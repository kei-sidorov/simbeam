package sim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/server"
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

// TestControlTouchScalesAndClamps mirrors the tap contract for the streamed
// touch phases: same normalized→point mapping, action forwarded verbatim. An
// out-of-bounds up still clamps into the screen — simbeam-control releases the
// finger at the last valid point.
func TestControlTouchScalesAndClamps(t *testing.T) {
	dims := controlDims{widthPoints: 402, heightPoints: 874}
	cases := []struct {
		action               string
		nx, ny, wantX, wantY float64
	}{
		{"down", 0.5, 0.5, 201, 437},
		{"move", 0, 1, 0, 874},
		{"up", -0.5, 2.0, 0, 874}, // clamped into [0,1]
	}
	for _, tc := range cases {
		c, buf := testControl(dims)
		c.Touch(tc.action, tc.nx, tc.ny)
		m := decodeLine(t, buf)
		if m["type"] != "touch" || m["action"] != tc.action {
			t.Errorf("Touch(%s) type/action = %v/%v", tc.action, m["type"], m["action"])
		}
		if m["x"] != tc.wantX || m["y"] != tc.wantY {
			t.Errorf("Touch(%s,%v,%v) = (%v,%v), want (%v,%v)", tc.action, tc.nx, tc.ny, m["x"], m["y"], tc.wantX, tc.wantY)
		}
	}
}

func TestControlAppSwitcher(t *testing.T) {
	c, buf := testControl(controlDims{})
	c.AppSwitcher()
	if m := decodeLine(t, buf); m["type"] != "app_switcher" {
		t.Errorf("AppSwitcher type = %v, want app_switcher", m["type"])
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

// TestControlProtocolMapping pins the --protocol preflight contract: a helper
// that predates the flag dies on the unknown argument (any exec error → 1),
// garbage output is 1, and a printed integer wins. Brew cannot pin dependency
// versions, so this mapping is what catches a stale helper after a daemon
// upgrade.
func TestControlProtocolMapping(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want int
	}{
		{"new helper prints version", "3\n", nil, 3},
		{"future version passes through", "7\n", nil, 7},
		{"pre-flag helper exits non-zero", "", errors.New("exit status 2"), 1},
		{"garbage output", "Usage: simbeam-control ...", nil, 1},
		{"nonsense zero", "0\n", nil, 1},
	}
	for _, tc := range cases {
		if got := controlProtocol([]byte(tc.out), tc.err); got != tc.want {
			t.Errorf("%s: controlProtocol = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The startup lines must be told apart by their `ready` field, not by position:
// on a cold boot the framebuffer-wait notice comes first, so anything reading
// stderr line 1 as the handshake breaks on every cold start.
func TestParseControlEventDiscriminatesOnReady(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantParsed bool
		wantReady  *bool // nil = no `ready` field at all
	}{
		{"waiting notice has no ready", `{"waiting":"framebuffer","protocol":3,"timeout_ms":15000}`, true, nil},
		{"handshake", `{"ready":true,"width":402,"height":874,"scale":3,"protocol":3}`, true, ptr(true)},
		{"failure envelope", `{"ready":false,"error":"display_not_ready","message":"no surface","retryable":true}`, true, ptr(false)},
		{"plain text is not an event", `startup cancelled before the simulator framebuffer was available`, false, nil},
		{"broken json is not an event", `{"ready":`, false, nil},
	}
	for _, tc := range cases {
		e, ok := parseControlEvent(tc.line)
		if ok != tc.wantParsed {
			t.Errorf("%s: parsed = %v, want %v", tc.name, ok, tc.wantParsed)
			continue
		}
		if !ok {
			continue
		}
		switch {
		case tc.wantReady == nil && e.Ready != nil:
			t.Errorf("%s: ready = %v, want absent", tc.name, *e.Ready)
		case tc.wantReady != nil && e.Ready == nil:
			t.Errorf("%s: ready absent, want %v", tc.name, *tc.wantReady)
		case tc.wantReady != nil && *e.Ready != *tc.wantReady:
			t.Errorf("%s: ready = %v, want %v", tc.name, *e.Ready, *tc.wantReady)
		}
	}
}

func ptr(b bool) *bool { return &b }

// Key order is not stable in the helper's output (JSONSerialization), so the
// envelope must survive being reordered — a text match would not.
func TestParseControlEventIgnoresKeyOrder(t *testing.T) {
	line := `{"retryable":true,"message":"no framebuffer within 15000 ms","protocol":3,"error":"display_not_ready","ready":false}`
	e, ok := parseControlEvent(line)
	if !ok || e.Ready == nil || *e.Ready {
		t.Fatalf("reordered envelope not parsed as a failure: %+v ok=%v", e, ok)
	}
	err := e.attachError("ABC")
	if err.Code != "display_not_ready" || !err.Retryable {
		t.Fatalf("attachError = %+v, want the helper's code and retryable flag", err)
	}
	if !strings.Contains(err.Msg, "ABC") || !strings.Contains(err.Msg, "framebuffer") {
		t.Fatalf("message must name the device and carry the helper's text, got %q", err.Msg)
	}
}

// The helper's codes are its protocol surface: forwarded verbatim, never
// remapped, so a client can branch on them. Retryable travels with them —
// device_not_booted means "call again", display_not_ready means "restart".
func TestAttachErrorForwardsCodesVerbatim(t *testing.T) {
	for _, tc := range []struct {
		code      string
		retryable bool
	}{
		{"invalid_arguments", false},
		{"core_simulator_unavailable", false},
		{"device_not_found", false},
		{"device_not_booted", true},
		{"display_not_ready", true},
		{"encoder_failed", false},
		{"hid_unavailable", false},
	} {
		e := controlEvent{Error: tc.code, Message: "…", Retryable: tc.retryable}
		got := e.attachError("ABC")
		if got.Code != tc.code || got.Retryable != tc.retryable {
			t.Errorf("attachError(%s) = code %q retryable %v, want %q/%v",
				tc.code, got.Code, got.Retryable, tc.code, tc.retryable)
		}
	}
}

// A handshake still yields the full retina geometry (points × scale), falling
// back to the encoded size when the helper reports no scale.
func TestControlEventDims(t *testing.T) {
	e, _ := parseControlEvent(`{"ready":true,"width":402,"height":874,"scale":3}`)
	if d := e.dims(); d.widthPoints != 402 || d.pixelW != 1206 || d.pixelH != 2622 {
		t.Fatalf("dims = %+v, want 402pt → 1206x2622px", d)
	}
	e, _ = parseControlEvent(`{"ready":true,"width":402,"height":874,"encoded_width":600,"encoded_height":1300}`)
	if d := e.dims(); d.pixelW != 600 || d.pixelH != 1300 {
		t.Fatalf("dims without scale = %+v, want the encoded size", d)
	}
}

// The three startup deadlines must nest, and the ordering is what decides who
// reports a stuck attach. If the daemon's fired first the client would get "we
// killed a process that never spoke" instead of the helper's typed
// display_not_ready — the whole point of issue #4.
func TestStartupTimeoutsNest(t *testing.T) {
	if !(controlStartupTimeout < controlHandshakeTimeout) {
		t.Fatalf("helper startup %s must be under our handshake wait %s", controlStartupTimeout, controlHandshakeTimeout)
	}
	if !(controlHandshakeTimeout < server.AttachTimeout) {
		t.Fatalf("handshake wait %s must be under the session's attach bound %s", controlHandshakeTimeout, server.AttachTimeout)
	}
	// The helper rejects 0 (it means "wait forever" to nobody) with
	// invalid_arguments, so whatever we pass must round to at least 1ms.
	if controlStartupTimeout/time.Millisecond < 1 {
		t.Fatal("--startup-timeout-ms must be at least 1")
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
