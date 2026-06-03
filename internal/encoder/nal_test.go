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
	pSlice := p(0x41, 0x01) // type 1 (VCL, non-IDR)
	sps := p(0x67, 0x02)    // type 7
	pps := p(0x68, 0x03)    // type 8
	sei := p(0x06, 0x04)    // type 6
	idr := p(0x65, 0x05)    // type 5 (VCL, IDR)
	next := p(0x41, 0x06)   // type 1 — opens the AU AFTER the keyframe

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
	sps1, pps1, idr1 := p(0x67, 0x01), p(0x68, 0x02), p(0x65, 0x03)
	sps2, pps2, idr2 := p(0x67, 0x04), p(0x68, 0x05), p(0x65, 0x06)

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
