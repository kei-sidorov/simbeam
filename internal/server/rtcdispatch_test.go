package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/encoder"
)

// stubFeed is an inert Feed that records the input routed to it and serves a
// canned screenshot (or error).
type stubFeed struct {
	w, h    uint64
	inputs  []Input
	shot    []byte
	shotErr error
}

func (f *stubFeed) Screen() (uint64, uint64)         { return f.w, f.h }
func (f *stubFeed) Frames() <-chan encoder.Frame     { return nil }
func (f *stubFeed) Input(_ context.Context, i Input) { f.inputs = append(f.inputs, i) }
func (f *stubFeed) Screenshot(context.Context) ([]byte, error) {
	return f.shot, f.shotErr
}
func (f *stubFeed) Close() error { return nil }

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

	mu         sync.Mutex
	attached   []string
	attachedAt []QualityOpts // quality of each Attach, parallel to attached
}

// stubDefaultScale is what the fake backend reports as its default multiplier —
// deliberately neither MinScale nor MaxScale, so tests can tell "defaulted" from
// "clamped".
const stubDefaultScale = 0.5

func (c *stubComp) DefaultScale() float64 { return stubDefaultScale }

// Attach records the quality it was handed. It takes a lock because doQuality
// re-attaches on its own goroutine, so tests race the dispatch otherwise.
func (c *stubComp) Attach(_ context.Context, udid string, q QualityOpts) (Feed, error) {
	c.mu.Lock()
	c.attached = append(c.attached, udid)
	c.attachedAt = append(c.attachedAt, q)
	c.mu.Unlock()
	if c.attachErr != nil {
		return nil, c.attachErr
	}
	if c.feed == nil {
		c.feed = &stubFeed{}
	}
	return c.feed, nil
}

// attaches returns a snapshot of the udids attached so far.
func (c *stubComp) attaches() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.attached...)
}

// qualities returns a snapshot of the quality each attach was handed.
func (c *stubComp) qualities() []QualityOpts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]QualityOpts(nil), c.attachedAt...)
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

// list/sims ride the reliable "bulk" channel, not control (issue #2): the sims
// reply is the largest control message and was dropped with no retransmission on
// a cellular/relay path, hanging the list screen.
func TestDoListSendsSimsOnBulk(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", OSVersion: "26.1", State: "Booted"},
		{UDID: "B", Name: "iPad", OSVersion: "18.5", State: "Shutdown"},
	}}, &sink)
	d.handleBulk([]byte(`{"type":"list"}`))
	sims := sink.sims()
	if len(sims) != 2 {
		t.Fatalf("want 2 sims, got %+v (frames %+v)", sims, sink.txt)
	}
	if sims[0] != (bulkSim{UDID: "A", Name: "iPhone", OSVersion: "26.1", State: "Booted"}) {
		t.Fatalf("first sim decoded wrong: %+v", sims[0])
	}
	for _, frame := range sink.txt {
		if len(frame) > bulkFrameMax {
			t.Fatalf("sims header frame is %d bytes — over the %d one-packet cap", len(frame), bulkFrameMax)
		}
	}
	for i, c := range sink.chunks {
		if len(c) > bulkFrameMax {
			t.Fatalf("sims chunk %d is %d bytes — over the %d one-packet cap", i, len(c), bulkFrameMax)
		}
	}
}

// The sims reply carries only the four fields a client renders — model,
// architecture and type are dropped to keep it small (issue #3).
func TestDoListSimsReplyOmitsUnusedFields(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", Model: "iPhone17,1", OSVersion: "26.1",
			State: "Booted", Architecture: "arm64", Type: "Simulator"},
	}}, &sink)
	d.handleBulk([]byte(`{"type":"list"}`))
	payload := string(sink.image())
	for _, dropped := range []string{"model", "architecture", `"type"`, "iPhone17,1", "arm64"} {
		if strings.Contains(payload, dropped) {
			t.Fatalf("sims payload must not carry %q, got %s", dropped, payload)
		}
	}
}

func TestDoListErrorReplyOnBulk(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{listErr: errors.New("boom")}, &sink)
	d.handleBulk([]byte(`{"type":"list"}`))
	errs := sink.errors()
	if len(errs) != 1 || errs[0].Code != CodeListFailed {
		t.Fatalf("want one %q error, got %+v", CodeListFailed, sink.txt)
	}
	if len(sink.sims()) != 0 {
		t.Fatalf("list error must not emit a sims reply, got %+v", sink.txt)
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

	if got := c.attaches(); len(got) != 1 || got[0] != "ABC" {
		t.Fatalf("Attach(ABC) not called, got %v", got)
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

// bulkSink captures the bulk reply paths: chunks (binary image frames, in order)
// and txt (raw text frames — header or error envelope). sendErr, when set, fails
// every binary send to model pion rejecting an oversized frame. maxMsg models
// the peer's negotiated max-message-size: a send above it is rejected exactly as
// pion/sctp does, so a chunker that ignores the cap fails these tests.
type bulkSink struct {
	chunks  [][]byte
	txt     []string
	sendErr error
	maxMsg  int
}

// image is the reassembled payload the client would decode: the binary chunks
// concatenated in arrival order.
func (s *bulkSink) image() []byte {
	var out []byte
	for _, c := range s.chunks {
		out = append(out, c...)
	}
	return out
}

// errors returns just the error envelopes among the text frames, so tests can
// assert on failures without matching the header.
func (s *bulkSink) errors() []bulkErr {
	var out []bulkErr
	for _, raw := range s.txt {
		var e bulkErr
		if err := json.Unmarshal([]byte(raw), &e); err != nil || e.Type != "error" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// sims reassembles the chunked "sims" reply — the {"type":"sims","bytes":N}
// header plus the binary chunks that follow it (issue #3) — and returns the
// decoded simulator array. nil if no sims header was sent (e.g. a list error).
// A list test performs a single operation, so every binary chunk in the sink
// belongs to this one transfer.
func (s *bulkSink) sims() []bulkSim {
	var bytes int
	sawHeader := false
	for _, raw := range s.txt {
		var h bulkHeader
		if err := json.Unmarshal([]byte(raw), &h); err == nil && h.Type == "sims" {
			bytes, sawHeader = h.Bytes, true
		}
	}
	if !sawHeader {
		return nil
	}
	payload := s.image() // chunks concatenated; only the sims transfer ran
	if len(payload) != bytes {
		return nil // announced byte count and delivered chunks disagree
	}
	var out []bulkSim
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil
	}
	return out
}

// newBulkDispatch wires a dispatch onto sink. The peer's negotiated cap defaults
// to what libwebrtc advertises (256 KiB) unless the sink overrides it.
func newBulkDispatch(backend Backend, sink *bulkSink) *rtcDispatch {
	if sink.maxMsg == 0 {
		sink.maxMsg = 256 * 1024
	}
	return &rtcDispatch{
		backend: backend,
		baseCtx: context.Background(),
		sendBulk: func(b []byte) error {
			if sink.sendErr != nil {
				return sink.sendErr
			}
			// Mirror pion/sctp: len(payload) > maxMessageSize is a hard reject.
			if len(b) > sink.maxMsg {
				return fmt.Errorf("outbound packet larger than maximum message size: %d", sink.maxMsg)
			}
			sink.chunks = append(sink.chunks, append([]byte(nil), b...))
			return nil
		},
		sendBulkText: func(s string) error {
			sink.txt = append(sink.txt, s)
			return nil
		},
		bulkMaxMsg: func() int { return sink.maxMsg },
	}
}

// An image smaller than the chunk size still follows the framing: a header
// announcing the byte count, then the payload.
func TestScreenshotRepliesWithHeaderThenChunks(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{}, &sink)
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{shot: []byte("PNGDATA")}, udid: "ABC"}

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	if string(sink.image()) != "PNGDATA" {
		t.Fatalf("want reassembled PNGDATA, got %q", sink.image())
	}
	if len(sink.txt) != 1 {
		t.Fatalf("want exactly one text frame (the header), got %+v", sink.txt)
	}
	var h bulkHeader
	if err := json.Unmarshal([]byte(sink.txt[0]), &h); err != nil {
		t.Fatalf("header does not decode: %v", err)
	}
	if h.Type != "screenshot" || h.Bytes != len("PNGDATA") {
		t.Fatalf("want header screenshot/%d, got %+v", len("PNGDATA"), h)
	}
	if len(sink.errors()) != 0 {
		t.Fatalf("success must not emit an error envelope, got %+v", sink.errors())
	}
}

// The whole point of the framing: an image far larger than the peer's
// max-message-size goes out as several chunks, each within the cap, and
// reassembles to the original bytes.
func TestScreenshotSplitsLargeImageIntoCappedChunks(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{}, &sink)
	img := make([]byte, bulkFrameMax*2+7) // two full chunks and a short tail
	for i := range img {
		img[i] = byte(i)
	}
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{shot: img}, udid: "ABC"}

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	if len(sink.chunks) != 3 {
		t.Fatalf("want 3 chunks for %d bytes, got %d", len(img), len(sink.chunks))
	}
	for i, c := range sink.chunks {
		if len(c) > bulkFrameMax {
			t.Fatalf("chunk %d is %d bytes — over the %d one-packet cap", i, len(c), bulkFrameMax)
		}
	}
	if !bytes.Equal(sink.image(), img) {
		t.Fatalf("reassembled image differs from the capture")
	}
	var h bulkHeader
	if err := json.Unmarshal([]byte(sink.txt[0]), &h); err != nil || h.Bytes != len(img) {
		t.Fatalf("header must announce %d bytes, got %+v (err %v)", len(img), h, err)
	}
}

// The core of issue #3: even against a peer that would accept a far larger
// message, no frame — header or chunk — exceeds one packet, so nothing
// black-holes on an IPv6 path that cannot fragment. The huge maxMsg here would
// have let the old ceiling-sized chunks through the sink, hiding the bug.
func TestScreenshotNeverExceedsOnePacket(t *testing.T) {
	sink := bulkSink{maxMsg: 1 << 30} // peer would accept anything; assert on what we send
	d := newBulkDispatch(&stubComp{}, &sink)
	img := make([]byte, 300*1024) // several hundred packets' worth
	for i := range img {
		img[i] = byte(i)
	}
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{shot: img}, udid: "ABC"}

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	if errs := sink.errors(); len(errs) != 0 {
		t.Fatalf("transfer must succeed, got %+v", errs)
	}
	for i, frame := range sink.txt {
		if len(frame) > bulkFrameMax {
			t.Fatalf("text frame %d is %d bytes — over the %d one-packet cap", i, len(frame), bulkFrameMax)
		}
	}
	for i, c := range sink.chunks {
		if len(c) > bulkFrameMax {
			t.Fatalf("chunk %d is %d bytes — over the %d one-packet cap", i, len(c), bulkFrameMax)
		}
	}
	if !bytes.Equal(sink.image(), img) {
		t.Fatalf("reassembled image differs from the capture")
	}
}

// With no association yet the negotiated cap is unknown (0). The one-packet
// ceiling still applies — the fix does not depend on knowing the peer's cap.
func TestScreenshotUnknownCapStillCapsAtOnePacket(t *testing.T) {
	sink := bulkSink{maxMsg: 1 << 30} // sink accepts anything; assert on the size we choose
	d := newBulkDispatch(&stubComp{}, &sink)
	d.bulkMaxMsg = func() int { return 0 } // association not up
	img := make([]byte, bulkFrameMax+1)
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{shot: img}, udid: "ABC"}

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	if len(sink.chunks) != 2 {
		t.Fatalf("want a %d-byte one-packet split (2 chunks), got %d", bulkFrameMax, len(sink.chunks))
	}
	if len(sink.chunks[0]) != bulkFrameMax {
		t.Fatalf("want a %d-byte first chunk, got %d", bulkFrameMax, len(sink.chunks[0]))
	}
}

// A chunk send that pion rejects must stop the stream and tell the client why —
// otherwise it waits out its failsafe staring at a half-delivered image.
func TestScreenshotSendFailureRepliesError(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{}, &sink)
	sink.sendErr = errors.New("outbound packet larger than maximum message size: 262144")
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{shot: []byte("PNGDATA")}, udid: "ABC"}

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	errs := sink.errors()
	if len(errs) != 1 || errs[0].Type != "error" {
		t.Fatalf("want one error envelope, got %+v", errs)
	}
	if !strings.Contains(errs[0].Msg, "maximum message size") {
		t.Fatalf("error must carry the reason, got %q", errs[0].Msg)
	}
}

// An empty capture is a daemon bug, not an image: say so rather than shipping a
// header promising zero bytes that the client cannot decode.
func TestScreenshotEmptyCaptureRepliesError(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{}, &sink)
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{shot: []byte{}}, udid: "ABC"}

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	if len(sink.chunks) != 0 {
		t.Fatalf("empty capture must not send chunks, got %d", len(sink.chunks))
	}
	if len(sink.errors()) != 1 {
		t.Fatalf("want one error envelope, got %+v", sink.errors())
	}
}

// With nothing attached the daemon must still answer — a text error, never
// silence (the client would otherwise hit its ~20s failsafe).
func TestScreenshotNoAttachmentRepliesError(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{}, &sink)

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	if len(sink.chunks) != 0 {
		t.Fatalf("no attachment must not send an image, got %d chunks", len(sink.chunks))
	}
	errs := sink.errors()
	if len(errs) != 1 || errs[0].Msg == "" {
		t.Fatalf("want one non-empty error envelope, got %+v", errs)
	}
}

// A capture failure surfaces as a text error (unlike fire-and-forget input).
func TestScreenshotFeedErrorRepliesError(t *testing.T) {
	var sink bulkSink
	d := newBulkDispatch(&stubComp{}, &sink)
	d.att = &attachment{cancel: func() {}, feed: &stubFeed{shotErr: errors.New("grpc down")}, udid: "ABC"}

	d.handleBulk([]byte(`{"type":"screenshot"}`))

	if len(sink.chunks) != 0 {
		t.Fatalf("capture error must not send an image, got %d chunks", len(sink.chunks))
	}
	if len(sink.errors()) != 1 {
		t.Fatalf("want one error envelope, got %+v", sink.errors())
	}
}

func TestBulkUnknownTypeAndBadJSONReplyError(t *testing.T) {
	for _, in := range []string{`{"type":"nope"}`, `not json`} {
		var sink bulkSink
		d := newBulkDispatch(&stubComp{}, &sink)
		d.att = &attachment{cancel: func() {}, feed: &stubFeed{shot: []byte("x")}, udid: "ABC"}

		d.handleBulk([]byte(in))

		if len(sink.chunks) != 0 {
			t.Fatalf("%s: must not send an image, got %d chunks", in, len(sink.chunks))
		}
		if len(sink.errors()) != 1 {
			t.Fatalf("%s: want one error envelope, got %+v", in, sink.errors())
		}
	}
}
