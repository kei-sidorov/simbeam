package sim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

// testControl builds a control whose NDJSON writer feeds an in-memory buffer, so
// the input-scaling paths can be exercised without spawning simbeam-control.
func testControl(d controlDims) (*control, *bytes.Buffer) {
	var buf bytes.Buffer
	c := &control{w: bufio.NewWriter(&buf)}
	c.setDims(d)
	return c, &buf
}

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line, err := buf.ReadBytes('\n')
	if err != nil {
		t.Fatalf("no control line written: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("bad control json %q: %v", line, err)
	}
	return m
}

// TestControlTapScalesAndClamps guards the normalized→point mapping that had a
// live off-screen regression before (decision №24): a tap in [0,1] must land at
// nx·widthPoints / ny·heightPoints, and out-of-range input must clamp, not wrap.
func TestControlTapScalesAndClamps(t *testing.T) {
	dims := controlDims{widthPoints: 402, heightPoints: 874}
	cases := []struct {
		nx, ny, wantX, wantY float64
	}{
		{0.5, 0.5, 201, 437}, // center
		{0, 0, 0, 0},         // top-left
		{1, 1, 402, 874},     // bottom-right
		{-0.5, 2.0, 0, 874},  // clamped into [0,1]
	}
	for _, tc := range cases {
		c, buf := testControl(dims)
		c.Tap(tc.nx, tc.ny)
		m := decodeLine(t, buf)
		if m["type"] != "tap" {
			t.Errorf("Tap type = %v, want tap", m["type"])
		}
		if m["x"] != tc.wantX || m["y"] != tc.wantY {
			t.Errorf("Tap(%v,%v) = (%v,%v), want (%v,%v)", tc.nx, tc.ny, m["x"], m["y"], tc.wantX, tc.wantY)
		}
	}
}

// TestControlSwipeScalesAndRoundsDuration checks both endpoints scale and the
// duration is converted to whole milliseconds.
func TestControlSwipeScalesAndRoundsDuration(t *testing.T) {
	c, buf := testControl(controlDims{widthPoints: 400, heightPoints: 800})
	c.Swipe(0.25, 0.75, 0.5, 0.5, 0.25)
	m := decodeLine(t, buf)
	if m["type"] != "swipe" {
		t.Fatalf("type = %v, want swipe", m["type"])
	}
	for k, want := range map[string]float64{
		"x1": 100, "y1": 600, "x2": 200, "y2": 400, "duration_ms": 250,
	} {
		if m[k] != want {
			t.Errorf("swipe %s = %v, want %v", k, m[k], want)
		}
	}
}

func TestControlHomeAndKey(t *testing.T) {
	c, buf := testControl(controlDims{})
	c.Home()
	if m := decodeLine(t, buf); m["type"] != "home" {
		t.Errorf("Home type = %v, want home", m["type"])
	}

	c.Key(4, true)
	m := decodeLine(t, buf)
	if m["type"] != "key" || m["usage"] != float64(4) || m["shift"] != true {
		t.Errorf("Key(4,true) = %v, want type=key usage=4 shift=true", m)
	}
}

func TestClamp01(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{-0.1, 0}, {0, 0}, {0.5, 0.5}, {1, 1}, {1.5, 1},
	} {
		if got := clamp01(tc.in); got != tc.want {
			t.Errorf("clamp01(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
