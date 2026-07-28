package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/encoder"
)

// blockingBackend parks inside Attach until released (or until its ctx dies),
// which is what an attach in flight actually looks like: a sidecar spawning,
// a simulator that has not produced a framebuffer yet.
type blockingBackend struct {
	entered chan struct{} // closed/signalled when Attach is reached
	release chan struct{} // close to let Attach return a feed
	feed    Feed
	once    sync.Once
}

func (b *blockingBackend) DefaultScale() float64                               { return 0.5 }
func (b *blockingBackend) List(context.Context) ([]companion.Simulator, error) { return nil, nil }
func (b *blockingBackend) Boot(context.Context, string) error                  { return nil }
func (b *blockingBackend) Shutdown(context.Context, string) error              { return nil }
func (b *blockingBackend) Shake(context.Context, string) error                 { return nil }

func (b *blockingBackend) Attach(ctx context.Context, _ string, _ QualityOpts) (Feed, error) {
	b.once.Do(func() {
		if b.entered != nil {
			close(b.entered)
		}
	})
	select {
	case <-b.release:
		if b.feed != nil {
			return b.feed, nil
		}
		return &stubFeed{w: 10, h: 20}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// closingFeed is a feed whose frame channel the test closes by hand: the
// sidecar dying on its own, which is the only case that owes the client a
// stream_ended.
type closingFeed struct {
	ch     chan encoder.Frame
	closed chan struct{}
	once   sync.Once
}

func newClosingFeed() *closingFeed {
	return &closingFeed{ch: make(chan encoder.Frame), closed: make(chan struct{})}
}

func (f *closingFeed) Screen() (uint64, uint64)                   { return 10, 20 }
func (f *closingFeed) Frames() <-chan encoder.Frame               { return f.ch }
func (f *closingFeed) Input(context.Context, Input)               {}
func (f *closingFeed) Screenshot(context.Context) ([]byte, error) { return nil, nil }
func (f *closingFeed) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// die simulates the feed ending by itself (process exit, simulator gone).
func (f *closingFeed) die() { close(f.ch) }

// waitClosed reports whether the feed was released within the timeout — a feed
// left open holds a real simbeam-control process forever.
func (f *closingFeed) waitClosed(d time.Duration) bool {
	select {
	case <-f.closed:
		return true
	case <-time.After(d):
		return false
	}
}

// A typed backend failure must reach the client verbatim: the helper's own code
// and its retryable flag, not a daemon-invented approximation. display_not_ready
// is the case the whole issue is about — the simulator booted but never produced
// a framebuffer, and only a restart clears that.
func TestAttachTypedFailureForwardsHelperCode(t *testing.T) {
	var out []ctrlReply
	be := &stubComp{attachErr: &AttachError{
		Code:      "display_not_ready",
		Msg:       "the simulator display produced no framebuffer IOSurface within 15000 ms",
		Retryable: true,
	}}
	d := newTestDispatch(be, &out)

	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))

	got := waitReplies(t, &out, 2)
	e := got[1]
	if e.Type != "error" || e.Operation != "attach" || e.UDID != "ABC" {
		t.Fatalf("want a terminal attach error for ABC, got %+v", e)
	}
	if e.Code != "display_not_ready" || !e.Retryable {
		t.Fatalf("helper code/retryable must travel verbatim, got code=%q retryable=%v", e.Code, e.Retryable)
	}
	if !strings.Contains(e.Msg, "framebuffer") {
		t.Fatalf("want the helper's message carried through, got %q", e.Msg)
	}
}

// A failed attach must leave the session idle. Before, pending stayed set on the
// failure path, so the very restart the client sends next still believed a feed
// was live for that device.
func TestAttachFailureClearsPendingBeforeReplying(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{attachErr: errors.New("boom")}, &out)

	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))
	waitReplies(t, &out, 2)

	d.mu.Lock()
	pending, att := d.pending, d.att
	d.mu.Unlock()
	if pending != "" || att != nil {
		t.Fatalf("after a failed attach the session must be idle, got pending=%q att=%+v", pending, att)
	}
}

// Exactly one terminal reply per accepted attach — and none at all from an
// attach a newer intent has superseded. A detach that lands mid-spawn already
// answered the client; a late "attached" would resurrect a feed it has torn its
// UI down for, and a late "error" would report a failure it never asked about.
func TestSupersededAttachRepliesNothing(t *testing.T) {
	var out []ctrlReply
	entered, release := make(chan struct{}), make(chan struct{})
	d := newTestDispatch(&blockingBackend{entered: entered, release: release}, &out)

	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))
	<-entered
	d.handle([]byte(`{"type":"detach"}`)) // supersedes the attach still spawning
	close(release)                        // ... which only now gets its feed

	got := waitReplies(t, &out, 2)
	if len(got) != 2 || got[0].Type != "attaching" || got[1].Type != "detached" {
		t.Fatalf("want [attaching, detached] and nothing else, got %+v", got)
	}
	if got[1].UDID != "ABC" {
		t.Fatalf("detached must name the device it cancelled, got %+v", got[1])
	}
	time.Sleep(50 * time.Millisecond) // give a late reply every chance to appear
	if final := replies(&out); len(final) != 2 {
		t.Fatalf("superseded attach must stay silent, got %+v", final)
	}
}

// A backend that never returns must not strand the client: the attach is
// cancelled and reported as one typed timeout inside AttachTimeout.
func TestAttachTimesOutWithTypedError(t *testing.T) {
	restore := AttachTimeout
	AttachTimeout = 50 * time.Millisecond
	defer func() { AttachTimeout = restore }()

	var out []ctrlReply
	d := newTestDispatch(&blockingBackend{release: make(chan struct{})}, &out) // never released

	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))

	got := waitReplies(t, &out, 2)
	if got[1].Type != "error" || got[1].Code != CodeAttachTimeout || got[1].UDID != "ABC" {
		t.Fatalf("want a typed attach_timeout for ABC, got %+v", got[1])
	}
	if got[1].Retryable {
		t.Fatal("attach_timeout is not retryable: the same request would wedge the same way")
	}
}

// A feed that dies on its own owes the client a stream_ended — otherwise the
// picture simply freezes and the client waits on nothing.
func TestStreamEndedOnUnexpectedFeedDeath(t *testing.T) {
	var out []ctrlReply
	feed := newClosingFeed()
	d := newTestDispatch(&stubComp{}, &out)
	d.writeFrame = func([]byte, time.Duration) error { return nil }
	att := &attachment{cancel: func() {}, feed: feed, udid: "ABC"}
	d.att = att
	go d.pump(att)

	feed.die()

	got := waitReplies(t, &out, 1)
	if got[0].Type != "stream_ended" || got[0].UDID != "ABC" {
		t.Fatalf("want stream_ended for ABC, got %+v", got)
	}
	if !feed.waitClosed(time.Second) {
		t.Fatal("the dead feed must be released, not leaked")
	}
	d.mu.Lock()
	live := d.att
	d.mu.Unlock()
	if live != nil {
		t.Fatalf("attachment must be cleared after the feed died, got %+v", live)
	}
}

// The same feed death is silent when we caused it. A detach has already been
// confirmed; a stream_ended on top would read to the client as a second,
// unexplained event.
func TestNoStreamEndedOnIntentionalDetach(t *testing.T) {
	var out []ctrlReply
	feed := newClosingFeed()
	d := newTestDispatch(&stubComp{}, &out)
	d.writeFrame = func([]byte, time.Duration) error { return nil }
	att := &attachment{cancel: func() {}, feed: feed, udid: "ABC"}
	d.att = att
	go d.pump(att)

	d.handle([]byte(`{"type":"detach"}`)) // clears d.att, closes the feed
	feed.die()                            // the pump only notices afterwards

	waitReplies(t, &out, 1)
	time.Sleep(50 * time.Millisecond)
	for _, r := range replies(&out) {
		if r.Type == "stream_ended" {
			t.Fatalf("intentional teardown must be silent, got %+v", replies(&out))
		}
	}
}

// Session teardown is silent too: baseCtx is gone, and so is the client.
func TestNoStreamEndedOnSessionTeardown(t *testing.T) {
	var out []ctrlReply
	feed := newClosingFeed()
	ctx, cancel := context.WithCancel(context.Background())
	d := newTestDispatch(&stubComp{}, &out)
	d.baseCtx = ctx
	d.writeFrame = func([]byte, time.Duration) error { return nil }
	att := &attachment{cancel: func() {}, feed: feed, udid: "ABC"}
	d.att = att
	go d.pump(att)

	cancel()
	feed.die()

	time.Sleep(100 * time.Millisecond)
	if got := replies(&out); len(got) != 0 {
		t.Fatalf("session teardown must send nothing, got %+v", got)
	}
}

// restart power-cycles the device and confirms with "booted" so the client can
// attach again — the documented answer to a non-retryable display_not_ready.
func TestRestartCyclesDeviceAndReplies(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)

	d.handle([]byte(`{"type":"restart","udid":"ABC"}`))

	if len(c.shutdown) != 1 || c.shutdown[0] != "ABC" || len(c.booted) != 1 || c.booted[0] != "ABC" {
		t.Fatalf("want shutdown then boot of ABC, got shutdown=%v booted=%v", c.shutdown, c.booted)
	}
	got := replies(&out)
	if len(got) != 1 || got[0].Type != "booted" || got[0].UDID != "ABC" {
		t.Fatalf("want a single booted reply for ABC, got %+v", got)
	}
}

// A restart of the streaming device drops its feed first — the sidecar is about
// to lose its simulator — and does so silently: the client asked for this and is
// waiting on "booted".
func TestRestartDropsLiveFeedSilently(t *testing.T) {
	var out []ctrlReply
	feed := newClosingFeed()
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.writeFrame = func([]byte, time.Duration) error { return nil }
	att := &attachment{cancel: func() {}, feed: feed, udid: "ABC"}
	d.att = att
	go d.pump(att)

	d.handle([]byte(`{"type":"restart","udid":"ABC"}`))

	if !feed.waitClosed(time.Second) {
		t.Fatal("the feed must be released before the device is power-cycled")
	}
	got := waitReplies(t, &out, 1)
	if len(got) != 1 || got[0].Type != "booted" {
		t.Fatalf("want only a booted reply, got %+v", got)
	}
}

// A restart must also cancel an attach that is still spawning: that attach
// would otherwise come back holding a sidecar for a simulator being rebooted.
func TestRestartCancelsInFlightAttach(t *testing.T) {
	var out []ctrlReply
	entered, release := make(chan struct{}), make(chan struct{})
	feed := newClosingFeed()
	d := newTestDispatch(&blockingBackend{entered: entered, release: release, feed: feed}, &out)

	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))
	<-entered
	d.handle([]byte(`{"type":"restart","udid":"ABC"}`))
	close(release)

	if !feed.waitClosed(time.Second) {
		t.Fatal("the superseded attach must drop the feed it built")
	}
	got := waitReplies(t, &out, 2)
	if len(got) != 2 || got[0].Type != "attaching" || got[1].Type != "booted" {
		t.Fatalf("want [attaching, booted], got %+v", got)
	}
}

func TestRestartMissingUDID(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"restart"}`))
	got := replies(&out)
	if len(got) != 1 || got[0].Type != "error" || got[0].Operation != "restart" || got[0].Code != CodeBadRequest {
		t.Fatalf("want a bad_request error for restart, got %+v", got)
	}
	if len(c.shutdown) != 0 || len(c.booted) != 0 {
		t.Fatal("a restart without a udid must touch no device")
	}
}

func TestRestartShutdownFailureReplies(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{shutdownErr: errors.New("boom")}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"restart","udid":"ABC"}`))
	got := replies(&out)
	if len(got) != 1 || got[0].Type != "error" || got[0].Code != CodeRestartFailed {
		t.Fatalf("want a restart_failed error, got %+v", got)
	}
	if len(c.booted) != 0 {
		t.Fatal("boot must not run after a failed shutdown")
	}
}

// bulkText captures the text frames the reliable channel carries.
type bulkText struct {
	mu    sync.Mutex
	lines []string
}

func (b *bulkText) send(s string) error {
	b.mu.Lock()
	b.lines = append(b.lines, s)
	b.mu.Unlock()
	return nil
}

func (b *bulkText) wait(t *testing.T, n int) []ctrlReply {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		got := append([]string(nil), b.lines...)
		b.mu.Unlock()
		if len(got) >= n {
			out := make([]ctrlReply, len(got))
			for i, line := range got {
				if err := json.Unmarshal([]byte(line), &out[i]); err != nil {
					t.Fatalf("bulk frame %d is not JSON: %q", i, line)
				}
			}
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d bulk frames, got %v", n, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// A lifecycle request on "bulk" is answered on "bulk". Answering on the lossy
// channel a client that migrated to the reliable one would reintroduce exactly
// the drop this move exists to prevent.
func TestLifecycleOverBulkRepliesOnBulk(t *testing.T) {
	var control []ctrlReply
	sink := &bulkText{}
	d := newTestDispatch(&stubComp{feed: &stubFeed{w: 100, h: 200}}, &control)
	d.sendBulkText = sink.send

	d.handleBulk([]byte(`{"type":"attach","udid":"ABC"}`))

	got := sink.wait(t, 2)
	if got[0].Type != "attaching" || got[1].Type != "attached" || got[1].UDID != "ABC" {
		t.Fatalf("want [attaching, attached] on bulk, got %+v", got)
	}
	if c := replies(&control); len(c) != 0 {
		t.Fatalf("nothing may go to control for a bulk request, got %+v", c)
	}
}

// Unsolicited events follow the client: once it has spoken lifecycle on "bulk",
// a stream_ended must not be dropped on the lossy channel instead.
func TestStreamEndedFollowsTheReliableRoute(t *testing.T) {
	var control []ctrlReply
	sink := &bulkText{}
	feed := newClosingFeed()
	d := newTestDispatch(&stubComp{}, &control)
	d.sendBulkText = sink.send
	d.writeFrame = func([]byte, time.Duration) error { return nil }

	d.handleBulk([]byte(`{"type":"detach"}`)) // teaches the session the route
	att := &attachment{cancel: func() {}, feed: feed, udid: "ABC"}
	d.att = att
	go d.pump(att)
	feed.die()

	got := sink.wait(t, 2)
	if got[1].Type != "stream_ended" || got[1].UDID != "ABC" {
		t.Fatalf("want stream_ended on bulk, got %+v", got)
	}
	if c := replies(&control); len(c) != 0 {
		t.Fatalf("stream_ended must not go to control, got %+v", c)
	}
}

// A client old enough to send `quality` on "bulk" but not lifecycle is
// documented to read the rebuild's outcome on "control". Routing it to "bulk"
// just because the request that triggered it arrived there would drop that
// client's attached/error into a channel it never reads.
func TestQualityRebuildFollowsTheClientsLifecycleRoute(t *testing.T) {
	var control []ctrlReply
	sink := &bulkText{}
	c := &stubComp{feed: &stubFeed{w: 100, h: 200}}
	d := newTestDispatch(c, &control)
	d.sendBulkText = sink.send

	d.handle([]byte(`{"type":"attach","udid":"ABC"}`)) // lifecycle on control
	waitReplies(t, &control, 2)
	d.mu.Lock()
	installed := d.att != nil
	d.mu.Unlock()
	if !installed {
		t.Fatal("attach did not install a feed")
	}

	d.handleBulk([]byte(`{"type":"quality","scale":0.25}`))

	got := waitReplies(t, &control, 3)
	if got[2].Type != "attached" || got[2].UDID != "ABC" {
		t.Fatalf("want the rebuild's attached on control, got %+v", got[2])
	}
	// The echo itself still belongs on bulk — it answers the bulk request.
	if frames := sink.wait(t, 1); frames[0].Type != "quality" {
		t.Fatalf("want the quality echo on bulk, got %+v", frames[0])
	}
}

// An unknown bulk type must still be rejected as unknown — and must not teach
// the session that this client reads lifecycle on "bulk".
func TestUnknownBulkTypeIsNotLifecycle(t *testing.T) {
	var control []ctrlReply
	sink := &bulkText{}
	d := newTestDispatch(&stubComp{}, &control)
	d.sendBulkText = sink.send

	d.handleBulk([]byte(`{"type":"teleport","udid":"ABC"}`))

	got := sink.wait(t, 1)
	if got[0].Type != "error" || got[0].Code != CodeUnknownType {
		t.Fatalf("want an unknown_type error, got %+v", got)
	}
	d.mu.Lock()
	reliable := d.bulkLifecycle
	d.mu.Unlock()
	if reliable {
		t.Fatal("an unrecognized type must not switch the session's event route")
	}
}

// Every lifecycle frame must fit one SCTP packet. A backend error can carry a
// whole simctl dump, and an oversized frame black-holes on an IPv6 path (issue
// #3) — silently undoing the reason lifecycle moved to this channel.
func TestLifecycleBulkFrameFitsOnePacket(t *testing.T) {
	var control []ctrlReply
	sink := &bulkText{}
	huge := strings.Repeat("отладочный вывод симулятора ", 500)
	d := newTestDispatch(&stubComp{attachErr: errors.New(huge)}, &control)
	d.sendBulkText = sink.send

	d.handleBulk([]byte(`{"type":"attach","udid":"ABCDEFGH-1234-5678-9ABC-DEF012345678"}`))

	got := sink.wait(t, 2)
	if got[1].Type != "error" {
		t.Fatalf("want the failure on bulk, got %+v", got[1])
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i, line := range sink.lines {
		if len(line) > bulkFrameMax {
			t.Fatalf("bulk frame %d is %d bytes, over the %d cap", i, len(line), bulkFrameMax)
		}
	}
	if !strings.Contains(got[1].Msg, "…") {
		t.Fatalf("an over-long message must be truncated visibly, got %q", got[1].Msg)
	}
}
