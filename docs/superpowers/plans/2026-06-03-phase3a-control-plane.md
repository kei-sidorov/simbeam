# Phase 3a — Control-Plane / Video-on-Demand Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decouple the WebRTC PeerConnection from a specific simulator: one session-scoped peer comes up with only a control DataChannel, carries `list`/`boot`/`attach`/`detach` plus taps, and gets a video track fed on demand when a simulator is selected.

**Architecture:** Today `/rtc?udid=X` binds a whole peer (sidecar + ffmpeg + video track) to one UDID up front. We invert it: `/rtc` (no UDID) establishes a peer with a pre-negotiated **silent** H.264 track + a control DataChannel. Management and input flow over the DataChannel; on `attach <udid>` the daemon spawns the sidecar + ffmpeg and pumps frames into the existing track; on `detach` (or a new `attach`) it stops the pump and kills the sidecar. The control DataChannel is now **bidirectional** (daemon sends `sims`/`attached`/`booted`/`error` replies). This is the prerequisite for remote access (Plan 3b): signaling will only change how the two peers find each other — the session mechanics are identical.

**Decision locked here (resolves the spec's attach spike, decisions.md §"open questions"):** use **Option B — pre-negotiated silent video track**, not runtime renegotiation. The video m-line exists from the initial answer; `attach` just starts writing H.264 (each new stream starts with an IDR from our short-GOP ffmpeg, so the decoder re-syncs on switch). This avoids all renegotiation machinery.

**Tech Stack:** Go 1.25, pion/webrtc v4 (existing `internal/rtc`), gorilla/websocket, idb_companion sidecar + ffmpeg (existing `internal/idb`, `internal/encoder`), vanilla-JS debug client.

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `internal/rtc/peer.go` | pion peer: H.264 track out, control DataChannel in **and out** | Modify: capture the control channel, add `Send([]byte)` |
| `internal/rtc/peer_test.go` | rtc unit tests | Modify: add `Send`-before-channel test |
| `internal/server/control.go` | parse/apply control messages | Modify: add `UDID` field + new message types to `parseControl` |
| `internal/server/control_test.go` | control parsing tests | Modify: cover new types |
| `internal/server/rtcdispatch.go` | session-scoped control dispatcher: `list`/`boot`/input routing + reply envelope | **Create** |
| `internal/server/rtcdispatch_test.go` | dispatcher unit tests (stub Companion, captured `send`) | **Create** |
| `internal/server/attach.go` | attachment lifecycle: spawn sidecar + ffmpeg pump on `attach`, tear down on `detach` | **Create** |
| `internal/server/rtc.go` | `/rtc` HTTP handler: UDID-less, wires dispatcher into the peer | Modify: rewrite handler |
| `internal/server/rtc_test.go` | `/rtc` handler test | Modify: drop the missing-udid expectation |
| `web/debug/index.html` | debug client: RTC control plane (list/attach over DataChannel) | Modify: rewrite RTC path |
| `README.md` | RTC run docs | Modify: describe control-plane flow |

The dispatcher takes a plain `send func([]byte)` and `writeFrame func([]byte, time.Duration) error` (not the `*rtc.Session` directly) so `list`/`boot`/input logic is unit-testable without a live pion handshake — mirroring the existing boundary where `internal/rtc` "knows nothing about the meaning of control messages."

---

## Task 1: `rtc.Session.Send` — bidirectional control channel

**Files:**
- Modify: `internal/rtc/peer.go`
- Test: `internal/rtc/peer_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/rtc/peer_test.go`:

```go
func TestSessionSendBeforeChannel(t *testing.T) {
	sess, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// No control DataChannel has been opened by a remote peer yet.
	if err := sess.Send([]byte(`{"type":"sims"}`)); err == nil {
		t.Fatal("want error sending before control channel exists, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/rtc/ -run TestSessionSendBeforeChannel -v`
Expected: FAIL — `sess.Send` undefined (compile error).

- [ ] **Step 3: Add the control-channel ref and `Send`**

In `internal/rtc/peer.go`, add an `errors` import and a sentinel:

```go
import (
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// ErrNoControlChannel is returned by Send before the remote peer has opened
// the "control" DataChannel.
var ErrNoControlChannel = errors.New("rtc: control channel not open")
```

Add a field to `Session` (guarded by the existing `mu`):

```go
type Session struct {
	pc        *webrtc.PeerConnection
	track     *webrtc.TrackLocalStaticSample
	mu        sync.Mutex // guards onClose and ctrl
	onClose   func()
	ctrl      *webrtc.DataChannel
	closeOnce sync.Once
}
```

In `New`, capture the channel inside the existing `OnDataChannel` callback:

```go
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "control" {
			return
		}
		s.mu.Lock()
		s.ctrl = dc
		s.mu.Unlock()
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if onControl != nil {
				onControl(msg.Data)
			}
		})
	})
```

Add the method (after `WriteFrame`):

```go
// Send delivers a control message to the remote peer over the "control"
// DataChannel. Returns ErrNoControlChannel if the peer has not opened it yet.
func (s *Session) Send(b []byte) error {
	s.mu.Lock()
	dc := s.ctrl
	s.mu.Unlock()
	if dc == nil {
		return ErrNoControlChannel
	}
	return dc.SendText(string(b))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/rtc/ -v`
Expected: PASS (`TestSessionAnswer`, `TestSessionWriteFrameNoPanic`, `TestSessionSendBeforeChannel`).

- [ ] **Step 5: Commit**

```bash
git add internal/rtc/peer.go internal/rtc/peer_test.go
git commit -m "feat(rtc): bidirectional control channel (Session.Send)"
```

---

## Task 2: Extend the control protocol (parse new message types)

**Files:**
- Modify: `internal/server/control.go`
- Test: `internal/server/control_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/server/control_test.go`:

```go
func TestParseControlManagementTypes(t *testing.T) {
	cases := []struct {
		in   string
		typ  string
		udid string
	}{
		{`{"type":"list"}`, "list", ""},
		{`{"type":"boot","udid":"ABC"}`, "boot", "ABC"},
		{`{"type":"attach","udid":"XYZ"}`, "attach", "XYZ"},
		{`{"type":"detach"}`, "detach", ""},
	}
	for _, c := range cases {
		m, err := parseControl([]byte(c.in))
		if err != nil {
			t.Fatalf("parseControl(%s): %v", c.in, err)
		}
		if m.Type != c.typ || m.UDID != c.udid {
			t.Fatalf("parseControl(%s) = {%q,%q}, want {%q,%q}", c.in, m.Type, m.UDID, c.typ, c.udid)
		}
	}
}

func TestParseControlUnknownStillErrors(t *testing.T) {
	if _, err := parseControl([]byte(`{"type":"explode"}`)); err == nil {
		t.Fatal("want error for unknown type, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestParseControl -v`
Expected: FAIL — `parseControl` rejects `"list"`/`"boot"`/`"attach"`/`"detach"` (unknown type) and `controlMsg` has no `UDID` field.

- [ ] **Step 3: Add the `UDID` field and accept the new types**

In `internal/server/control.go`, add a `UDID` field to `controlMsg`:

```go
type controlMsg struct {
	Type     string  `json:"type"` // tap|home|swipe|key|list|boot|attach|detach
	UDID     string  `json:"udid"` // boot, attach
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	X1       float64 `json:"x1"`
	Y1       float64 `json:"y1"`
	X2       float64 `json:"x2"`
	Y2       float64 `json:"y2"`
	Duration float64 `json:"duration"`
	Key      string  `json:"key"`
}
```

Extend the `switch` in `parseControl`:

```go
	switch m.Type {
	case "tap", "home", "swipe", "key", "list", "boot", "attach", "detach":
		return m, nil
	default:
		return m, fmt.Errorf("unknown control type %q", m.Type)
	}
```

`applyControl` is unchanged — it still only handles the input types.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run TestParseControl -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/control.go internal/server/control_test.go
git commit -m "feat(server): accept list/boot/attach/detach control messages"
```

---

## Task 3: Control dispatcher — `list` and `boot` over the channel

**Files:**
- Create: `internal/server/rtcdispatch.go`
- Test: `internal/server/rtcdispatch_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/server/rtcdispatch_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kei-sidorov/simbeam/internal/companion"
)

type stubComp struct {
	sims    []companion.Simulator
	listErr error
	booted  []string
	bootErr error
}

func (c *stubComp) List(context.Context) ([]companion.Simulator, error) {
	return c.sims, c.listErr
}
func (c *stubComp) Boot(_ context.Context, udid string) error {
	c.booted = append(c.booted, udid)
	return c.bootErr
}

// newTestDispatch returns a dispatcher whose replies are captured into *out.
func newTestDispatch(comp Companion, out *[]ctrlReply) *rtcDispatch {
	return &rtcDispatch{
		comp:    comp,
		baseCtx: context.Background(),
		send: func(b []byte) {
			var r ctrlReply
			_ = json.Unmarshal(b, &r)
			*out = append(*out, r)
		},
	}
}

func TestDoListSendsSims(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", State: "Booted"},
		{UDID: "B", Name: "iPad", State: "Shutdown"},
	}}, &out)
	d.handle([]byte(`{"type":"list"}`))
	if len(out) != 1 || out[0].Type != "sims" || len(out[0].Sims) != 2 {
		t.Fatalf("want one sims reply with 2 sims, got %+v", out)
	}
}

func TestDoListErrorReply(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{listErr: errors.New("boom")}, &out)
	d.handle([]byte(`{"type":"list"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want one error reply, got %+v", out)
	}
}

func TestDoBootMissingUDID(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"boot"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want error reply for missing udid, got %+v", out)
	}
	if len(c.booted) != 0 {
		t.Fatalf("Boot should not be called, got %v", c.booted)
	}
}

func TestDoBootSuccess(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"boot","udid":"ABC"}`))
	if len(out) != 1 || out[0].Type != "booted" || out[0].UDID != "ABC" {
		t.Fatalf("want booted reply for ABC, got %+v", out)
	}
	if len(c.booted) != 1 || c.booted[0] != "ABC" {
		t.Fatalf("Boot(ABC) not called, got %v", c.booted)
	}
}

func TestInputBeforeAttachIgnored(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{}, &out)
	// No attachment yet → a tap must be a safe no-op (no panic, no reply).
	d.handle([]byte(`{"type":"tap","x":0.5,"y":0.5}`))
	if len(out) != 0 {
		t.Fatalf("tap before attach should produce no reply, got %+v", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run 'TestDoList|TestDoBoot|TestInputBeforeAttach' -v`
Expected: FAIL — `rtcDispatch`, `ctrlReply`, `handle` undefined.

- [ ] **Step 3: Create the dispatcher (without attach yet)**

Create `internal/server/rtcdispatch.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/kei-sidorov/simbeam/internal/companion"
)

// ctrlReply is a downstream control message: daemon → client over the
// "control" DataChannel.
type ctrlReply struct {
	Type string                 `json:"type"` // sims|booted|attached|detached|error
	Msg  string                 `json:"msg,omitempty"`
	Sims []companion.Simulator  `json:"sims,omitempty"`
	UDID string                 `json:"udid,omitempty"`
	W    uint64                 `json:"w,omitempty"`
	H    uint64                 `json:"h,omitempty"`
}

// rtcDispatch is the per-session control plane. It owns at most one video
// "attachment" (a spawned sidecar + ffmpeg pump) and routes inbound control
// messages: management (list/boot/attach/detach) and input (tap/swipe/...).
//
// It depends on plain func values (send, writeFrame) rather than *rtc.Session
// so management/input logic is unit-testable without a live pion handshake.
//
// handle() runs on pion's DataChannel goroutine; boot/attach block it briefly
// (sidecar spawn waits for readiness). Acceptable for the debug/local scope —
// revisit if it stalls input during attach.
type rtcDispatch struct {
	comp       Companion
	binary     string
	baseCtx    context.Context
	send       func([]byte)
	writeFrame func([]byte, time.Duration) error

	mu  sync.Mutex
	att *attachment
}

func (d *rtcDispatch) handle(data []byte) {
	m, err := parseControl(data)
	if err != nil {
		return // ignore malformed/unknown
	}
	switch m.Type {
	case "list":
		d.doList()
	case "boot":
		d.doBoot(m.UDID)
	case "attach":
		d.doAttach(m.UDID)
	case "detach":
		d.stopAttachment()
		d.reply(ctrlReply{Type: "detached"})
	case "tap", "home", "swipe", "key":
		d.doInput(m)
	}
}

func (d *rtcDispatch) doList() {
	sims, err := d.comp.List(d.baseCtx)
	if err != nil {
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}
	d.reply(ctrlReply{Type: "sims", Sims: sims})
}

func (d *rtcDispatch) doBoot(udid string) {
	if udid == "" {
		d.reply(ctrlReply{Type: "error", Msg: "boot: missing udid"})
		return
	}
	if err := d.comp.Boot(d.baseCtx, udid); err != nil {
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}
	d.reply(ctrlReply{Type: "booted", UDID: udid})
}

func (d *rtcDispatch) doInput(m controlMsg) {
	d.mu.Lock()
	att := d.att
	d.mu.Unlock()
	if att == nil {
		return // no simulator attached → ignore input
	}
	applyControl(d.baseCtx, att.client, att.screen, m)
}

func (d *rtcDispatch) reply(v ctrlReply) {
	b, err := json.Marshal(v)
	if err != nil || d.send == nil {
		return
	}
	d.send(b)
}
```

Note: `doAttach`, `stopAttachment`, and the `attachment` type are added in Task 4. This task will not compile until Task 4 is done **iff** those identifiers are referenced — they are (`handle` calls `doAttach`/`stopAttachment`, `doInput` reads `att.client`/`att.screen`). To keep this task self-contained and green, add a **temporary stub** at the bottom of `rtcdispatch.go` now and replace it in Task 4:

```go
// --- temporary stubs, replaced in Task 4 (attach.go) ---

type attachment struct {
	client interface{ /* replaced by *idb.Client */ }
	screen interface{ /* replaced by idb.Screen */ }
}

func (d *rtcDispatch) doAttach(udid string)  { d.reply(ctrlReply{Type: "error", Msg: "attach not wired yet"}) }
func (d *rtcDispatch) stopAttachment()        {}
```

Because `doInput` reads `att.client`/`att.screen` with concrete types, the stub `attachment` above won't satisfy `applyControl`. To avoid that coupling in this task, temporarily make `doInput` guard-only:

```go
func (d *rtcDispatch) doInput(m controlMsg) {
	d.mu.Lock()
	att := d.att
	d.mu.Unlock()
	if att == nil {
		return
	}
	// real routing wired in Task 4
}
```

Task 4 replaces the stubs and restores the real `doInput` body.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestDoList|TestDoBoot|TestInputBeforeAttach' -v`
Expected: PASS (attachment is always nil in these tests, so the stub paths are never exercised).

- [ ] **Step 5: Commit**

```bash
git add internal/server/rtcdispatch.go internal/server/rtcdispatch_test.go
git commit -m "feat(server): control-plane dispatcher (list/boot over DataChannel)"
```

---

## Task 4: Attachment lifecycle — video pump on `attach`

**Files:**
- Create: `internal/server/attach.go`
- Modify: `internal/server/rtcdispatch.go` (remove the temporary stubs from Task 3; restore real `doInput`)

- [ ] **Step 1: Create `attach.go` with the real attachment + `doAttach`/`stopAttachment`**

Create `internal/server/attach.go`:

```go
package server

import (
	"context"
	"time"

	"github.com/kei-sidorov/simbeam/internal/encoder"
	"github.com/kei-sidorov/simbeam/internal/idb"
)

// attachment is one live video feed: a spawned idb_companion sidecar whose
// screenshots are encoded to H.264 and pumped into the session's video track.
// Exactly one attachment exists per session at a time.
type attachment struct {
	cancel  context.CancelFunc
	sidecar *idb.Sidecar
	client  *idb.Client
	screen  idb.Screen
}

// doAttach tears down any current feed, spawns a sidecar for udid, and starts
// pumping H.264 frames into the video track. Replies "attached" with screen
// dimensions, or "error".
func (d *rtcDispatch) doAttach(udid string) {
	if udid == "" {
		d.reply(ctrlReply{Type: "error", Msg: "attach: missing udid"})
		return
	}
	d.stopAttachment()

	if err := encoder.Available(); err != nil {
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}

	ctx, cancel := context.WithCancel(d.baseCtx)
	sidecar, err := idb.Spawn(ctx, d.binary, udid)
	if err != nil {
		cancel()
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}
	client := sidecar.Client()

	screen, err := client.Describe(ctx)
	if err != nil {
		sidecar.Close()
		cancel()
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}

	png := client.ScreenshotStream(ctx, time.Second/rtcFPS)
	frames, err := encoder.Encode(ctx, png, rtcFPS)
	if err != nil {
		sidecar.Close()
		cancel()
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}

	go func() {
		for f := range frames {
			if d.writeFrame == nil {
				cancel()
				return
			}
			if err := d.writeFrame(f.Data, f.Duration); err != nil {
				cancel()
				return
			}
		}
		cancel() // encoder/stream ended
	}()

	d.mu.Lock()
	d.att = &attachment{cancel: cancel, sidecar: sidecar, client: client, screen: screen}
	d.mu.Unlock()

	d.reply(ctrlReply{Type: "attached", W: screen.Width, H: screen.Height})
}

// stopAttachment cancels the current feed (stops the pump, kills the sidecar).
// Safe to call when nothing is attached.
func (d *rtcDispatch) stopAttachment() {
	d.mu.Lock()
	att := d.att
	d.att = nil
	d.mu.Unlock()
	if att != nil {
		att.cancel()
		att.sidecar.Close()
	}
}
```

- [ ] **Step 2: Remove the Task 3 stubs and restore real `doInput` in `rtcdispatch.go`**

Delete the entire `// --- temporary stubs ... ---` block (the stub `attachment` type, stub `doAttach`, stub `stopAttachment`) from `internal/server/rtcdispatch.go`. Restore `doInput`'s real body:

```go
func (d *rtcDispatch) doInput(m controlMsg) {
	d.mu.Lock()
	att := d.att
	d.mu.Unlock()
	if att == nil {
		return // no simulator attached → ignore input
	}
	applyControl(d.baseCtx, att.client, att.screen, m)
}
```

- [ ] **Step 3: Run tests to verify they still pass**

Run: `go test ./internal/server/ -run 'TestDoList|TestDoBoot|TestInputBeforeAttach' -v`
Expected: PASS — `attachment` is now the real type, `doInput` routes to `applyControl` but the tests never attach, so the nil-guard returns early.

- [ ] **Step 4: Verify the whole package builds**

Run: `go build ./...`
Expected: no output (success).

- [ ] **Step 5: Commit**

```bash
git add internal/server/attach.go internal/server/rtcdispatch.go
git commit -m "feat(server): on-demand attachment (spawn sidecar + ffmpeg pump on attach)"
```

---

## Task 5: Rewrite `/rtc` handler — UDID-less, dispatcher-wired

**Files:**
- Modify: `internal/server/rtc.go`
- Test: `internal/server/rtc_test.go`

- [ ] **Step 1: Update the handler test (drop missing-udid 400)**

Replace the body of `internal/server/rtc_test.go`:

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// /rtc no longer requires ?udid= (UDID is chosen later via the control
// DataChannel). A plain GET without WebSocket upgrade headers must be rejected
// by the upgrader (400), not panic.
func TestHandleRTCRejectsNonWebsocket(t *testing.T) {
	s := New(&stubComp{}, "")
	req := httptest.NewRequest(http.MethodGet, "/rtc", nil) // no upgrade headers
	rec := httptest.NewRecorder()
	s.handleRTC(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-websocket GET, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestHandleRTC -v`
Expected: FAIL — old `handleRTC` returns 400 only for missing udid; with no udid it currently returns 400 too, so this *may* pass for the wrong reason. Confirm by reading the failure: after Step 3 the handler must still 400 here because `gorilla/websocket`'s `Upgrade` writes 400 on a non-upgrade request. (If it passes before Step 3, that's fine — Step 3 must keep it passing.)

- [ ] **Step 3: Rewrite `handleRTC`**

Replace `internal/server/rtc.go` entirely:

```go
package server

import (
	"context"
	"net/http"

	"github.com/kei-sidorov/simbeam/internal/rtc"
)

// rtcFPS is the screenshot/encode frame rate for the WebRTC path.
const rtcFPS = 15

// sdpMsg is the signaling envelope exchanged over the /rtc WebSocket.
type sdpMsg struct {
	Type string `json:"type"` // "offer" | "answer" | "error"
	SDP  string `json:"sdp,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// handleRTC negotiates one session-scoped WebRTC peer: a pre-negotiated
// (initially silent) H.264 video track plus a bidirectional "control"
// DataChannel. No simulator is bound up front — the client drives
// list/boot/attach/detach over the control channel, and the daemon starts the
// video pump on attach. The JPEG /session path is untouched (fallback).
func (s *Server) handleRTC(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	d := &rtcDispatch{comp: s.comp, binary: s.binary, baseCtx: ctx}

	sess, err := rtc.New(d.handle)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sess.Close()
	defer d.stopAttachment()
	sess.OnClose(cancel)

	// Wire the peer into the dispatcher before negotiation completes; control
	// messages can only arrive after ICE connects (post-answer).
	d.send = func(b []byte) { _ = sess.Send(b) }
	d.writeFrame = sess.WriteFrame

	var offer sdpMsg
	if err := conn.ReadJSON(&offer); err != nil || offer.Type != "offer" {
		return
	}
	answerSDP, err := sess.Answer(offer.SDP)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	if err := conn.WriteJSON(sdpMsg{Type: "answer", SDP: answerSDP}); err != nil {
		return
	}

	// Block until the client disconnects or teardown cancels ctx.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			cancel()
			return
		}
	}
}
```

Note: imports `time`, `encoder`, `idb` are now used only by `attach.go`, not `rtc.go` — they are removed from `rtc.go` here (the rewrite drops them). `rtcFPS` stays in `rtc.go` and is referenced by `attach.go` (same package).

- [ ] **Step 4: Run the full server + rtc test suites**

Run: `go test ./internal/server/ ./internal/rtc/ -v`
Expected: PASS across all tests.

- [ ] **Step 5: Verify the whole build**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add internal/server/rtc.go internal/server/rtc_test.go
git commit -m "feat(server): UDID-less /rtc — control plane up first, video on attach"
```

---

## Task 6: Browser debug client — RTC control plane

**Files:**
- Modify: `web/debug/index.html`

The RTC path now: open `/rtc` (no udid) → establish peer with recvonly video + control DataChannel → on channel open, request `list` → render sims from the `sims` reply → clicking a sim sends `boot` (if shutdown) then `attach` → on `attached`, show video. The JPG path keeps REST (`/api/simulators`) + `/session?udid=` unchanged.

- [ ] **Step 1: Replace `web/debug/index.html`**

Write the full file:

```html
<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>simbeam debug</title>
  <style>
    body { font-family: -apple-system, sans-serif; margin: 16px; }
    #sims button { display: block; margin: 4px 0; }
    #modes { margin: 8px 0; }
    #modes button.active { font-weight: bold; text-decoration: underline; }
    .surface { border: 1px solid #ccc; max-height: 80vh; cursor: crosshair; display: none; touch-action: none; }
    #home { margin-top: 8px; display: none; }
    #hint { margin-top: 6px; font-size: 12px; color: #666; display: none; }
  </style>
</head>
<body>
  <div id="modes">
    Mode:
    <button id="modeRtc" class="active">RTC</button>
    <button id="modeJpg">JPG</button>
  </div>
  <h3>Simulators</h3>
  <div id="sims">loading…</div>
  <img id="screenImg" class="surface" alt="simulator">
  <video id="screenVid" class="surface" autoplay playsinline muted></video>
  <div id="hint">Click&nbsp;= tap · drag&nbsp;= swipe · keys typed here go to the device</div>
  <div><button id="home">Home</button></div>

<script>
const simsEl = document.getElementById('sims');
const imgEl = document.getElementById('screenImg');
const vidEl = document.getElementById('screenVid');
const homeBtn = document.getElementById('home');
const hintEl = document.getElementById('hint');
const modeRtcBtn = document.getElementById('modeRtc');
const modeJpgBtn = document.getElementById('modeJpg');

let mode = 'rtc';
let currentUDID = null;
let ws = null;                          // jpg path
let pc = null, dc = null, sig = null;   // rtc path
let startGen = 0;

function surface() { return mode === 'rtc' ? vidEl : imgEl; }

function teardown() {
  if (ws) { ws.close(); ws = null; }
  if (sig) { try { sig.close(); } catch (e) {} sig = null; }
  if (dc) { try { dc.close(); } catch (e) {} dc = null; }
  if (pc) { try { pc.close(); } catch (e) {} pc = null; }
  imgEl.style.display = 'none';
  vidEl.style.display = 'none';
  vidEl.srcObject = null;
}

function showSurface() {
  surface().style.display = 'block';
  homeBtn.style.display = 'inline-block';
  hintEl.style.display = 'block';
}

modeRtcBtn.onclick = () => setMode('rtc');
modeJpgBtn.onclick = () => setMode('jpg');

function setMode(m) {
  if (m === mode) return;
  mode = m;
  currentUDID = null;
  modeRtcBtn.classList.toggle('active', m === 'rtc');
  modeJpgBtn.classList.toggle('active', m === 'jpg');
  teardown();
  enterMode();
}

// Each mode populates the simulator list its own way:
//   rtc → control plane comes up first, list arrives over the DataChannel
//   jpg → REST (Phase 1 fallback)
function enterMode() {
  simsEl.innerHTML = 'loading…';
  if (mode === 'rtc') startControlPlane();
  else loadSimsREST();
}

function renderSims(sims) {
  simsEl.innerHTML = '';
  sims.forEach(s => {
    const b = document.createElement('button');
    b.textContent = `${s.state === 'Booted' ? '▶' : '○'} ${s.name} (${s.os_version})`;
    b.onclick = () => onPick(s);
    simsEl.appendChild(b);
  });
}

function onPick(s) {
  if (mode === 'rtc') pickRTC(s);
  else pickJPG(s);
}

// ---- RTC path: control plane + video on demand ----
async function startControlPlane() {
  const gen = ++startGen;
  pc = new RTCPeerConnection();
  pc.addTransceiver('video', {direction: 'recvonly'});
  dc = pc.createDataChannel('control', {ordered: false, maxRetransmits: 0});

  pc.ontrack = (ev) => { vidEl.srcObject = ev.streams[0]; minimizeBuffer(pc); };
  pc.onconnectionstatechange = () => {
    if (['failed', 'disconnected', 'closed'].includes(pc.connectionState)) {
      console.log('rtc state', pc.connectionState);
    }
  };
  dc.onopen = () => dc.send(JSON.stringify({type: 'list'}));
  dc.onmessage = (ev) => onCtrlReply(JSON.parse(ev.data));

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await iceGatheringComplete(pc);
  if (gen !== startGen) return; // superseded

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  sig = new WebSocket(`${proto}://${location.host}/rtc`);
  sig.onopen = () => sig.send(JSON.stringify({type: 'offer', sdp: pc.localDescription.sdp}));
  sig.onmessage = async (ev) => {
    const m = JSON.parse(ev.data);
    if (m.type === 'answer') {
      await pc.setRemoteDescription({type: 'answer', sdp: m.sdp});
      minimizeBuffer(pc);
    } else if (m.type === 'error') {
      alert('rtc signaling error: ' + m.msg);
      teardown();
    }
  };
}

function onCtrlReply(m) {
  switch (m.type) {
    case 'sims':
      renderSims(m.sims || []);
      break;
    case 'booted':
      // simulator is up; now attach video to it
      dc.send(JSON.stringify({type: 'attach', udid: m.udid}));
      break;
    case 'attached':
      currentUDID = currentUDID; // already set in pickRTC
      showSurface();
      break;
    case 'detached':
      vidEl.style.display = 'none';
      break;
    case 'error':
      alert('rtc error: ' + m.msg);
      break;
  }
}

function pickRTC(s) {
  if (!dc || dc.readyState !== 'open') { alert('control channel not ready'); return; }
  currentUDID = s.udid;
  vidEl.style.display = 'none'; // hide until new feed arrives
  if (s.state !== 'Booted') {
    dc.send(JSON.stringify({type: 'boot', udid: s.udid})); // → booted → attach
  } else {
    dc.send(JSON.stringify({type: 'attach', udid: s.udid}));
  }
}

// ---- JPG path (Phase 1, unchanged transport) ----
async function loadSimsREST() {
  const r = await fetch('/api/simulators');
  renderSims(await r.json());
}

async function pickJPG(s) {
  if (s.state !== 'Booted') {
    const r = await fetch('/api/boot', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({udid: s.udid}),
    });
    if (!r.ok) { alert('boot failed: ' + (await r.text())); return; }
  }
  currentUDID = s.udid;
  teardown();
  startJPG(s.udid);
}

function startJPG(udid) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/session?udid=${encodeURIComponent(udid)}`);
  ws.binaryType = 'blob';
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') { console.log('status', ev.data); return; }
    const url = URL.createObjectURL(ev.data);
    const old = imgEl.src;
    imgEl.src = url;
    showSurface();
    if (old) URL.revokeObjectURL(old);
  };
  ws.onclose = () => { console.log('ws closed'); };
}

// Minimize the receiver playout buffer for low latency (Chrome; try/catch for Safari).
function minimizeBuffer(pc) {
  pc.getReceivers().forEach(r => {
    try { r.jitterBufferTarget = 0; } catch (e) {}
    try { r.playoutDelayHint = 0; } catch (e) {}
  });
}

function iceGatheringComplete(pc) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise(resolve => {
    const check = () => {
      if (pc.iceGatheringState === 'complete') {
        pc.removeEventListener('icegatheringstatechange', check);
        resolve();
      }
    };
    pc.addEventListener('icegatheringstatechange', check);
  });
}

// ---- Shared input ----
function sendControl(obj) {
  const s = JSON.stringify(obj);
  if (mode === 'rtc') {
    if (dc && dc.readyState === 'open') dc.send(s);
  } else {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(s);
  }
}

function controlReady() {
  return mode === 'rtc'
    ? (dc && dc.readyState === 'open' && currentUDID)
    : (ws && ws.readyState === WebSocket.OPEN);
}

function normCoords(e) {
  const rect = surface().getBoundingClientRect();
  const x = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
  const y = Math.min(1, Math.max(0, (e.clientY - rect.top) / rect.height));
  return {x, y};
}

const SWIPE_THRESHOLD_PX = 6;
let drag = null;

for (const el of [imgEl, vidEl]) {
  el.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    const c = normCoords(e);
    drag = {x: c.x, y: c.y, clientX: e.clientX, clientY: e.clientY, t: Date.now()};
    el.setPointerCapture(e.pointerId);
  });
  el.addEventListener('pointerup', (e) => {
    if (e.button !== 0 || !drag) return;
    if (!controlReady()) { drag = null; return; }
    const dx = e.clientX - drag.clientX;
    const dy = e.clientY - drag.clientY;
    if (Math.sqrt(dx * dx + dy * dy) < SWIPE_THRESHOLD_PX) {
      sendControl({type: 'tap', x: drag.x, y: drag.y});
    } else {
      const end = normCoords(e);
      const duration = Math.max(0.05, (Date.now() - drag.t) / 1000);
      sendControl({type: 'swipe', x1: drag.x, y1: drag.y, x2: end.x, y2: end.y, duration});
    }
    drag = null;
  });
  el.addEventListener('pointercancel', () => { drag = null; });
}

const MODIFIER_KEYS = new Set(['Shift', 'Control', 'Alt', 'Meta']);
window.addEventListener('keydown', (e) => {
  if (!controlReady()) return;
  if (MODIFIER_KEYS.has(e.key)) return;
  if (e.ctrlKey || e.metaKey) return;
  e.preventDefault();
  sendControl({type: 'key', key: e.key});
});

homeBtn.onclick = () => sendControl({type: 'home'});

enterMode();
</script>
</body>
</html>
```

- [ ] **Step 2: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(web): RTC control plane — list/boot/attach over DataChannel"
```

---

## Task 7: End-to-end verification + docs

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Build and run the daemon**

Run: `make run` (or `go run ./cmd/simbeamd serve --web ./web/debug`)
Expected: prints `simbeamd serving on :8080` and `debug client: http://localhost:8080/`.

- [ ] **Step 2: Verify the control plane in the browser (RTC mode, default)**

Open `http://localhost:8080/` in Chrome. Verify, observing the browser console and the daemon log:
- The simulator list renders **without** a REST call to `/api/simulators` (check Network tab: the list arrives over the WebRTC DataChannel; only `/rtc` WS + the SDP exchange appear, no `/api/simulators`).
- Click a **shutdown** simulator → it boots, then video appears (boot → `booted` → `attach` → `attached`).
- Click a **booted** simulator → video appears directly.
- Click → tap reaches the simulator; drag → swipe; type → keystrokes; Home button works.
- Click a **different** simulator → video switches to it within ~1–2s (old sidecar killed, new feed starts on the same track).
- Close the tab → daemon log shows the sidecar/ffmpeg torn down (peer disconnect cancels the session).

If any step fails, debug before continuing — do not mark this step complete on assumption.

- [ ] **Step 3: Verify JPG fallback still works**

In the browser, click **JPG**. Verify the list reloads (REST), clicking a simulator shows polled screenshots, and taps work — i.e. the Phase 1 path is untouched.

- [ ] **Step 4: Update README RTC section**

In `README.md`, in the "Запуск стрима (Phase 2 — WebRTC)" section, replace the paragraph describing the per-UDID `/rtc?udid=X` signaling with the control-plane model. Apply this edit:

Find:
```
Сигналинг: один WS-заход `/rtc?udid=X` — браузер шлёт offer, сервер отвечает answer (non-trickle,
только host-кандидаты, без STUN/TURN — скоуп localhost). Закрытие вкладки или смена симулятора
рвёт сессию и убивает сайдкар + ffmpeg-процесс.
```

Replace with:
```
Сигналинг и управление разведены (Phase 3a): один WS-заход `/rtc` (без udid) поднимает
WebRTC-пир с control-DataChannel; по нему идут `list`/`boot`/`attach`/`detach` и тачи. Видео-трек
молчит, пока не выбран симулятор: `attach <udid>` спавнит сайдкар + ffmpeg и начинает писать H.264
в существующий трек, `detach` (или новый `attach`) — останавливает и убивает сайдкар. Закрытие
вкладки рвёт пир и тушит текущий сайдкар + ffmpeg. STUN/TURN и удалёнка — Plan 3b.
```

Also update the `/rtc` row in the endpoint table. Find:
```
| `WS /rtc?udid=X` | сигналинг WebRTC (offer→answer, non-trickle); видео H.264 и control-тачи идут по P2P (медиа-трек + DataChannel) |
```
Replace with:
```
| `WS /rtc` | сигналинг WebRTC (offer→answer, non-trickle); по control-DataChannel — `list`/`boot`/`attach`/`detach` + тачи; видео H.264 по медиа-треку после `attach` |
```

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document /rtc control-plane model (Phase 3a)"
```

- [ ] **Step 6: Record the resolved spike as a decision**

Add a row to `docs/decisions.md` (after §49):

```
| 50 | Phase 3a: видео-трек **pre-negotiated и молчит** до `attach` (Option B), без рантайм-renegotiation. PeerConnection поднимается per-session с control-DataChannel; `list`/`boot`/`attach`/`detach` + тачи — по DataChannel (двунаправленному); `attach <udid>` спавнит сайдкар+ffmpeg и пишет H.264 в существующий трек, `detach`/новый `attach` — останавливает | разрешает spike из §49: renegotiation не нужен — новый H.264-поток начинается с IDR (короткий GOP, №35), декодер ресинкается на смене сима. Проще и надёжнее. `/rtc` теперь без udid; JPG `/session` не трогаем (№28) |
```

```bash
git add docs/decisions.md
git commit -m "docs(decisions): #50 — pre-negotiated silent track, control plane over DataChannel"
```

---

## Self-Review

**Spec coverage (against `2026-06-03-phase4-remote-access-design.md`, the Phase 3 parts implemented here):**
- ✅ Control-плоскость vs видео-плоскость: PeerConnection + DataChannel up first (Task 5), video on `attach` (Task 4).
- ✅ `list`/`boot`/`describe` over DataChannel: `list`/`boot` in Task 3; `describe` is folded into `attach` (screen dims returned in the `attached` reply, used for tap scaling) — matches spec step 3.
- ✅ `attach <udid>` / `detach` lifecycle, sidecar on-demand (Task 4); reuses `parseControl`/`ScaleTap`/`applyControl`/`hid` (decision §30).
- ✅ PeerConnection per-client-session, not per-UDID (Task 5).
- ✅ Browser validation, no native client (Task 6–7); QR/signaling intentionally **out of scope** here → Plan 3b.
- ⏭️ Deferred to Plan 3b (correctly not in this plan): signaling server, daemon outbound dial, `pairingToken`/`daemonPubKey`, STUN/TURN gating + upsell, `iceServers`.

**Placeholder scan:** No TBD/TODO. The only "temporary" code is the explicit Task 3 stub that Task 4 removes — its full replacement code is given in both tasks, not hand-waved.

**Type consistency:** `ctrlReply` fields (`Type/Msg/Sims/UDID/W/H`) defined in Task 3, marshaled by `reply`, consumed by browser `onCtrlReply` in Task 6 (`sims`/`booted`/`attached`/`detached`/`error`). `attachment{cancel,sidecar,client,screen}` defined in Task 4, read by `doInput`. `Screen.Width/Height` are `uint64` (confirmed in `internal/idb/client.go`) → `ctrlReply.W/H uint64`. `rtcDispatch` fields (`comp/binary/baseCtx/send/writeFrame/mu/att`) consistent across Tasks 3–5. `rtc.Session.Send`/`ErrNoControlChannel` (Task 1) used by handler in Task 5.

**Granularity / TDD:** Tasks 1–3 are test-first with real assertions. Tasks 4–5 are guarded by the Task 3 tests plus build/vet; the parts that need a real `idb_companion`/`ffmpeg` (attach pump) are verified live in Task 7 — matching how the codebase already integration-tests sidecars (no mock idb exists). Task 7 has explicit run/observe steps, not "verify it works."

---

## Execution Handoff

After this plan, **Plan 3b** (signaling server `cmd/simbeam-signal` + daemon outbound dial + STUN/TURN gating + upsell) gets written and executed on top of this foundation.
