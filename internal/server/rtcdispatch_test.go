package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/encoder"
)

// stubFeed is an inert Feed that records the input routed to it.
type stubFeed struct {
	w, h   uint64
	inputs []Input
}

func (f *stubFeed) Screen() (uint64, uint64)         { return f.w, f.h }
func (f *stubFeed) Frames() <-chan encoder.Frame     { return nil }
func (f *stubFeed) Input(_ context.Context, i Input) { f.inputs = append(f.inputs, i) }
func (f *stubFeed) Close() error                     { return nil }

type stubComp struct {
	sims        []companion.Simulator
	listErr     error
	booted      []string
	bootErr     error
	shutdown    []string
	shutdownErr error
	shook       []string
	shakeErr    error
	feed        *stubFeed
	attachErr   error
	attached    []string
}

func (c *stubComp) Attach(_ context.Context, udid string) (Feed, error) {
	c.attached = append(c.attached, udid)
	if c.attachErr != nil {
		return nil, c.attachErr
	}
	if c.feed == nil {
		c.feed = &stubFeed{}
	}
	return c.feed, nil
}

func (c *stubComp) List(context.Context) ([]companion.Simulator, error) {
	return c.sims, c.listErr
}
func (c *stubComp) Boot(_ context.Context, udid string) error {
	c.booted = append(c.booted, udid)
	return c.bootErr
}
func (c *stubComp) Shutdown(_ context.Context, udid string) error {
	c.shutdown = append(c.shutdown, udid)
	return c.shutdownErr
}
func (c *stubComp) Shake(_ context.Context, udid string) error {
	c.shook = append(c.shook, udid)
	return c.shakeErr
}

// newTestDispatch returns a dispatcher whose replies are captured into *out.
func newTestDispatch(backend Backend, out *[]ctrlReply) *rtcDispatch {
	return &rtcDispatch{
		backend: backend,
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

func TestDoShutdownMissingUDID(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"shutdown"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want error reply for missing udid, got %+v", out)
	}
	if len(c.shutdown) != 0 {
		t.Fatalf("Shutdown should not be called, got %v", c.shutdown)
	}
}

func TestDoShutdownSuccess(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"shutdown","udid":"ABC"}`))
	if len(out) != 1 || out[0].Type != "shutdown" || out[0].UDID != "ABC" {
		t.Fatalf("want shutdown reply for ABC, got %+v", out)
	}
	if len(c.shutdown) != 1 || c.shutdown[0] != "ABC" {
		t.Fatalf("Shutdown(ABC) not called, got %v", c.shutdown)
	}
}

func TestDoShutdownErrorReply(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{shutdownErr: errors.New("boom")}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"shutdown","udid":"ABC"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want one error reply, got %+v", out)
	}
}

func TestDoDetachReplies(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{}, &out)
	d.handle([]byte(`{"type":"detach"}`))
	if len(out) != 1 || out[0].Type != "detached" {
		t.Fatalf("want one detached reply, got %+v", out)
	}
}

// Shutting down the simulator that is currently being streamed must both tear
// down the feed AND emit "detached" before "shutdown", so the client's
// attachment state doesn't go stale (a silent video with a no-op detach).
func TestDoShutdownOfCurrentFeedDetaches(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	// Pretend "ABC" is the live feed. Feed.Close()/cancel are no-ops here.
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{}, udid: "ABC"}

	d.handle([]byte(`{"type":"shutdown","udid":"ABC"}`))

	if len(out) != 2 || out[0].Type != "detached" || out[1].Type != "shutdown" {
		t.Fatalf("want [detached, shutdown], got %+v", out)
	}
	if d.att != nil {
		t.Fatalf("feed should be torn down, att still set")
	}
}

// Shutting down a DIFFERENT simulator than the one streaming must leave the feed
// (and its attachment state) untouched — only a plain "shutdown" reply.
func TestDoShutdownOfOtherSimLeavesFeed(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	att := &attachment{cancel: func() {}, feed: &stubFeed{}, udid: "ABC"}
	d.att = att

	d.handle([]byte(`{"type":"shutdown","udid":"XYZ"}`))

	if len(out) != 1 || out[0].Type != "shutdown" || out[0].UDID != "XYZ" {
		t.Fatalf("want single shutdown reply for XYZ, got %+v", out)
	}
	if d.att != att {
		t.Fatalf("feed for ABC should be left untouched")
	}
}

func TestSendHelloCarriesHostInfo(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{}, &out)
	d.hostName, d.osVersion = "Kirill's MacBook Pro", "26.5"
	d.sendHello()
	if len(out) != 1 || out[0].Type != "hello" {
		t.Fatalf("want one hello reply, got %+v", out)
	}
	if out[0].Name != "Kirill's MacBook Pro" || out[0].OSVersion != "26.5" {
		t.Fatalf("hello = {name:%q osVersion:%q}, want host info", out[0].Name, out[0].OSVersion)
	}
	if !out[0].Paired {
		t.Fatalf("hello must carry paired:true (pin-ack), got %+v", out[0])
	}
}

// doAttach must ask the backend for a feed and reply "attached" with the feed's
// screen dimensions.
func TestDoAttachRepliesWithFeedScreen(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{feed: &stubFeed{w: 100, h: 200}}
	d := newTestDispatch(c, &out)

	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))

	if len(c.attached) != 1 || c.attached[0] != "ABC" {
		t.Fatalf("Attach(ABC) not called, got %v", c.attached)
	}
	if len(out) != 1 || out[0].Type != "attached" || out[0].W != 100 || out[0].H != 200 {
		t.Fatalf("want attached reply 100x200, got %+v", out)
	}
}

func TestDoAttachBackendErrorReply(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{attachErr: errors.New("boom")}, &out)
	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want one error reply, got %+v", out)
	}
}

// Gestures must be routed to the live feed with the wire fields intact.
func TestInputRoutedToFeed(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{}, &out)
	feed := &stubFeed{}
	d.att = &attachment{cancel: func() {}, feed: feed, udid: "ABC"}

	d.handle([]byte(`{"type":"tap","x":0.25,"y":0.75}`))
	d.handle([]byte(`{"type":"key","key":"Enter"}`))

	if len(feed.inputs) != 2 {
		t.Fatalf("want 2 inputs routed to feed, got %+v", feed.inputs)
	}
	if in := feed.inputs[0]; in.Type != "tap" || in.X != 0.25 || in.Y != 0.75 {
		t.Fatalf("tap not routed verbatim, got %+v", in)
	}
	if in := feed.inputs[1]; in.Type != "key" || in.Key != "Enter" {
		t.Fatalf("key not routed verbatim, got %+v", in)
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

func TestDoShakeBeforeAttachIgnored(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	// No attachment → shake is a silent no-op like the other gestures: no reply,
	// and the companion is never reached.
	d.handle([]byte(`{"type":"shake"}`))
	if len(out) != 0 {
		t.Fatalf("shake before attach should produce no reply, got %+v", out)
	}
	if len(c.shook) != 0 {
		t.Fatalf("Shake should not be called, got %v", c.shook)
	}
}

// shake is fire-and-forget like tap/home/swipe/key: it reaches the companion for
// the attached sim but sends no reply — an error reply would wrongly drop the
// iOS client's UI to "disconnected".
func TestDoShakeShakesAttachedSimWithoutReply(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{}, udid: "ABC"}

	d.handle([]byte(`{"type":"shake"}`))

	if len(out) != 0 {
		t.Fatalf("shake should produce no reply, got %+v", out)
	}
	if len(c.shook) != 1 || c.shook[0] != "ABC" {
		t.Fatalf("Shake(ABC) not called, got %v", c.shook)
	}
}

func TestDoShakeErrorIsSwallowed(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{shakeErr: errors.New("boom")}
	d := newTestDispatch(c, &out)
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{}, udid: "ABC"}

	d.handle([]byte(`{"type":"shake"}`))

	if len(out) != 0 {
		t.Fatalf("shake failure must not reply (would disconnect the client), got %+v", out)
	}
}
