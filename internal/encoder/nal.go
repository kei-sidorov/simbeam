// Package encoder turns a stream of still images (PNG frames from idb's
// screenshot RPC) into an H.264 access-unit stream via an ffmpeg subprocess
// (hardware h264_videotoolbox). We own the encoder, so keyframe cadence is
// under our control — unlike idb's video_stream (fixed ~10s GOP, decisions
// №34-38). Knows nothing about idb, pion, or HTTP.
package encoder

import "time"

// Frame is one H.264 access unit (NAL units in Annex-B, start codes included)
// plus the duration it occupies, used to advance the RTP timestamp.
type Frame struct {
	Data     []byte
	Duration time.Duration
}

// startCodeLen returns 4 for a 00 00 00 01 prefix, 3 for 00 00 01, else 0.
func startCodeLen(b []byte) int {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return 4
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return 3
	}
	return 0
}

// indexStartCode returns the byte offset of the first start code in b, or -1 if none found.
func indexStartCode(b []byte) int {
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				return i
			}
			if i+3 < len(b) && b[i+2] == 0 && b[i+3] == 1 {
				return i
			}
		}
	}
	return -1
}

// splitNALUnits splits an Annex-B buffer into NAL units, each retaining its
// leading start code. The final (possibly incomplete) unit is returned as rest
// so it can be prepended to the next read — one NAL of latency, but unit
// boundaries stay correct across chunk splits. Fix vs spike: skip the FULL
// current start code (3 or 4 bytes) before scanning, else a 4-byte 00 00 00 01
// is mis-detected as a 3-byte code + a bogus 1-byte NAL.
func splitNALUnits(buf []byte) (nals [][]byte, rest []byte) {
	remaining := buf
	for len(remaining) > 0 {
		scLen := startCodeLen(remaining)
		if scLen == 0 {
			return nals, remaining // not at a start code; carry forward
		}
		next := indexStartCode(remaining[scLen:])
		if next < 0 {
			return nals, remaining // last (maybe incomplete) NAL
		}
		end := scLen + next
		nals = append(nals, remaining[:end])
		remaining = remaining[end:]
	}
	return nals, nil
}

// nalType returns the H.264 NAL unit type (low 5 bits of the header byte after
// the start code), or 0 if malformed.
func nalType(nal []byte) byte {
	sc := startCodeLen(nal)
	if sc == 0 || sc >= len(nal) {
		return 0
	}
	return nal[sc] & 0x1f
}

// isVCL reports whether a NAL type carries coded slice data (types 1..5).
func isVCL(t byte) bool { return t >= 1 && t <= 5 }

// vclStartsPicture reports whether a coded-slice NAL begins a new picture: its
// first_mb_in_slice — the first ue(v) field of the slice header — is 0. ue(v)=0
// is encoded as the single bit '1', so the first payload bit is set exactly for
// a picture's first slice; continuation slices carry a nonzero first_mb_in_slice
// (leading '0' bit). This matters for libx264: -tune zerolatency enables
// sliced-threads, emitting several slice NALs per picture, and only the first
// may open a new access unit (videotoolbox emits one slice per picture, so
// there this always reports true).
func vclStartsPicture(nal []byte) bool {
	sc := startCodeLen(nal)
	if sc == 0 || sc+1 >= len(nal) {
		return true // malformed/truncated: fall back to the per-VCL boundary
	}
	return nal[sc+1]&0x80 != 0
}

// startsNewAU reports whether this NAL begins a new access unit: an access-unit
// delimiter (9), a parameter set / SEI that prefixes a picture (7/8/6), or the
// FIRST coded slice of a picture (1..5 with first_mb_in_slice==0). Placing the
// boundary BEFORE these keeps an IDR together with its SPS/PPS in one sample
// (decision №38) and keeps all slices of one picture in one sample.
func startsNewAU(nal []byte, t byte) bool {
	switch {
	case t == 6 || t == 7 || t == 8 || t == 9:
		return true
	case isVCL(t):
		return vclStartsPicture(nal)
	default:
		return false
	}
}

// auAssembler groups NAL units into access units. It flushes the current unit
// when a NAL that starts a new access unit arrives while the current unit
// already holds a VCL NAL. An AU is therefore emitted only once the NEXT
// picture's leading NAL arrives — one AU of latency by design. The final
// in-progress AU is not emitted at stream end (the session is tearing down,
// so that last frame is moot); there is deliberately no Flush.
type auAssembler struct {
	cur    [][]byte
	hasVCL bool
}

// push adds a NAL and returns a completed access unit if this NAL starts a new
// one, otherwise nil. The flush is conditioned on an already-buffered VCL NAL,
// so leading non-VCL NALs (AUD/SEI/SPS/PPS) accumulate into the forming AU
// rather than splitting it.
func (a *auAssembler) push(nal []byte) []byte {
	var done []byte
	t := nalType(nal)
	if a.hasVCL && startsNewAU(nal, t) {
		done = flatten(a.cur)
		a.cur = nil
		a.hasVCL = false
	}
	a.cur = append(a.cur, nal)
	if isVCL(t) {
		a.hasVCL = true
	}
	return done
}

func flatten(nals [][]byte) []byte {
	var n int
	for _, b := range nals {
		n += len(b)
	}
	out := make([]byte, 0, n)
	for _, b := range nals {
		out = append(out, b...)
	}
	return out
}
