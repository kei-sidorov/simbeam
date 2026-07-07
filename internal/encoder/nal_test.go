package encoder

import (
	"bytes"
	"testing"
)

func TestSplitNALUnits4ByteStartCode(t *testing.T) {
	// NAL A с 4-байтным start code, NAL B с 3-байтным, частичный третий.
	buf := []byte{
		0, 0, 0, 1, 0x67, 0xAA, // NAL A (SPS, type 7) — 4-байтный код
		0, 0, 1, 0x68, 0xBB, // NAL B (PPS, type 8) — 3-байтный код
		0, 0, 0, 1, 0x65, // NAL C начало (неполный → rest)
	}
	nals, rest := splitNALUnits(buf)
	if len(nals) != 2 {
		t.Fatalf("want 2 nals, got %d: %v", len(nals), nals)
	}
	if !bytes.Equal(nals[0], []byte{0, 0, 0, 1, 0x67, 0xAA}) {
		t.Fatalf("nal0 = % x", nals[0])
	}
	if !bytes.Equal(nals[1], []byte{0, 0, 1, 0x68, 0xBB}) {
		t.Fatalf("nal1 = % x", nals[1])
	}
	if !bytes.Equal(rest, []byte{0, 0, 0, 1, 0x65}) {
		t.Fatalf("rest = % x", rest)
	}
}

func TestSplitNALUnitsCleanEnd(t *testing.T) {
	// Two complete NALs where the second's payload is followed by the next
	// start code, so the first NAL is fully delimited and nothing carries over.
	buf := []byte{
		0, 0, 0, 1, 0x67, 0xAA, // NAL A (SPS)
		0, 0, 1, 0x68, 0xBB, // NAL B (PPS)
		0, 0, 0, 1, // start code of NAL C with no payload yet
	}
	nals, rest := splitNALUnits(buf)
	if len(nals) != 2 {
		t.Fatalf("want 2 nals, got %d", len(nals))
	}
	// The trailing bare start code has no following start code to delimit it,
	// so splitNALUnits carries it forward as rest (it is an incomplete NAL).
	if !bytes.Equal(rest, []byte{0, 0, 0, 1}) {
		t.Fatalf("rest = % x", rest)
	}
}

func TestNALTypeAndVCL(t *testing.T) {
	if nalType([]byte{0, 0, 0, 1, 0x65}) != 5 {
		t.Fatal("4-byte IDR should be type 5")
	}
	if nalType([]byte{0, 0, 1, 0x67}) != 7 {
		t.Fatal("3-byte SPS should be type 7")
	}
	if !isVCL(5) || isVCL(7) {
		t.Fatal("VCL classification wrong")
	}
}

func TestAUAssemblerKeyframeGrouping(t *testing.T) {
	p := func(b ...byte) []byte { return append([]byte{0, 0, 0, 1}, b...) }
	pSlice := p(0x41, 0x81) // type 1 (VCL, non-IDR)
	sps := p(0x67, 0x02)    // type 7
	pps := p(0x68, 0x03)    // type 8
	sei := p(0x06, 0x04)    // type 6
	idr := p(0x65, 0x85)    // type 5 (VCL, IDR)
	next := p(0x41, 0x86)   // type 1 — opens the AU AFTER the keyframe

	var a auAssembler
	// First P-frame: no flush yet.
	if au := a.push(pSlice); au != nil {
		t.Fatalf("first slice should not complete an AU")
	}
	// SPS arrives → it starts a new AU, so the prior P-frame flushes alone.
	au := a.push(sps)
	if !bytes.Equal(au, pSlice) {
		t.Fatalf("expected prior P-frame flushed, got % x", au)
	}
	// PPS, SEI, IDR accumulate into the keyframe AU (no flush).
	for _, n := range [][]byte{pps, sei, idr} {
		if au := a.push(n); au != nil {
			t.Fatalf("keyframe AU should still accumulate, flushed % x", au)
		}
	}
	// Next picture's slice flushes the keyframe AU = SPS+PPS+SEI+IDR together.
	au = a.push(next)
	want := bytes.Join([][]byte{sps, pps, sei, idr}, nil)
	if !bytes.Equal(au, want) {
		t.Fatalf("keyframe AU = % x\nwant % x", au, want)
	}
}

func TestAUAssemblerBackToBackKeyframes(t *testing.T) {
	p := func(b ...byte) []byte { return append([]byte{0, 0, 0, 1}, b...) }
	sps1, pps1, idr1 := p(0x67, 0x01), p(0x68, 0x02), p(0x65, 0x83)
	sps2, pps2, idr2 := p(0x67, 0x04), p(0x68, 0x05), p(0x65, 0x86)

	var a auAssembler
	for _, n := range [][]byte{sps1, pps1, idr1} {
		if au := a.push(n); au != nil {
			t.Fatalf("first keyframe should still accumulate, flushed % x", au)
		}
	}
	// The second keyframe's SPS flushes the first complete keyframe AU.
	au := a.push(sps2)
	want := bytes.Join([][]byte{sps1, pps1, idr1}, nil)
	if !bytes.Equal(au, want) {
		t.Fatalf("first keyframe AU = % x\nwant % x", au, want)
	}
	// Remaining NALs of the second keyframe accumulate without flushing.
	for _, n := range [][]byte{pps2, idr2} {
		if au := a.push(n); au != nil {
			t.Fatalf("second keyframe should still accumulate, flushed % x", au)
		}
	}
}

// x264's -tune zerolatency enables sliced-threads: several slice NALs per
// picture. All slices of one picture must land in ONE access unit — only a
// slice with first_mb_in_slice==0 (leading payload bit set) opens a new AU.
// Regression test for the "green frame" artifact: each slice used to be
// emitted as its own frame.
func TestAUAssemblerMultiSlicePicture(t *testing.T) {
	p := func(b ...byte) []byte { return append([]byte{0, 0, 0, 1}, b...) }
	sps := p(0x67, 0x01)
	pps := p(0x68, 0x02)
	idrS0 := p(0x65, 0x88) // IDR slice 0: first_mb_in_slice==0 (MSB set)
	idrS1 := p(0x65, 0x22) // IDR slice 1: continuation (MSB clear)
	idrS2 := p(0x65, 0x11) // IDR slice 2: continuation
	pS0 := p(0x41, 0x9A)   // next picture, slice 0
	pS1 := p(0x41, 0x3C)   // next picture, slice 1

	var a auAssembler
	for _, n := range [][]byte{sps, pps, idrS0, idrS1, idrS2} {
		if au := a.push(n); au != nil {
			t.Fatalf("keyframe slices should accumulate into one AU, flushed % x", au)
		}
	}
	// First slice of the NEXT picture flushes the whole keyframe AU.
	au := a.push(pS0)
	want := bytes.Join([][]byte{sps, pps, idrS0, idrS1, idrS2}, nil)
	if !bytes.Equal(au, want) {
		t.Fatalf("keyframe AU = % x\nwant % x", au, want)
	}
	// Continuation slice of the same picture must NOT flush.
	if au := a.push(pS1); au != nil {
		t.Fatalf("continuation slice must not flush, got % x", au)
	}
}

func TestVCLStartsPicture(t *testing.T) {
	if !vclStartsPicture([]byte{0, 0, 0, 1, 0x65, 0x88}) {
		t.Fatal("slice with first_mb_in_slice==0 must start a picture")
	}
	if vclStartsPicture([]byte{0, 0, 1, 0x41, 0x22}) {
		t.Fatal("continuation slice must not start a picture")
	}
	if !vclStartsPicture([]byte{0, 0, 0, 1, 0x65}) {
		t.Fatal("truncated slice falls back to starting a picture")
	}
}
