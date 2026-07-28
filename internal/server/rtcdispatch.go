package server

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Optional control features a daemon advertises in the hello's caps. A client
// gates on membership; an absent list means a daemon too old to advertise
// anything (≤ v0.11) and therefore the v1 gesture set only.
const (
	CapTouch       = "touch"        // the streamed touch phases (down/move/up)
	CapAppSwitcher = "app_switcher" // the serialized double-Home gesture
	// CapLifecycle says the lifecycle requests and replies (boot, restart,
	// attach/attaching/attached, detach, shutdown, their terminal errors and
	// stream_ended) may ride the reliable ordered "bulk" channel instead of the
	// lossy "control" one. Unlike the other two this describes the dispatcher,
	// not the backend, so every backend advertises it.
	CapLifecycle = "lifecycle"
)

// Operation values in a lifecycle error reply: which request failed. A client
// with several lifecycle requests outstanding routes the failure by this plus
// udid, rather than by guessing from arrival order.
const (
	opAttach   = "attach"
	opBoot     = "boot"
	opRestart  = "restart"
	opShutdown = "shutdown"
)

// Terminal lifecycle error codes the daemon itself produces. A failure that
// came from simbeam-control carries the helper's own code instead (see
// AttachError) — those are protocol surface there and are forwarded verbatim.
const (
	CodeAttachFailed   = "attach_failed"   // the backend refused the feed, with no code of its own
	CodeAttachTimeout  = "attach_timeout"  // no feed and no typed failure within AttachTimeout
	CodeBootFailed     = "boot_failed"     // powering the simulator on failed
	CodeShutdownFailed = "shutdown_failed" // powering the simulator off failed
	CodeRestartFailed  = "restart_failed"  // the shutdown+boot cycle failed
)

// restartTimeout bounds a restart's shutdown+boot cycle so the client always
// gets a reply. `simctl boot` returns once the device reports Booted (~1s on a
// warm machine) and shutdown is quicker still, so this is a backstop for a
// wedged CoreSimulator, not a normal wait.
const restartTimeout = 60 * time.Second

// ctrlReply is a downstream lifecycle message: daemon → client, over "bulk"
// when the client speaks CapLifecycle and over "control" otherwise.
type ctrlReply struct {
	Type string `json:"type"` // booted|attaching|attached|detached|shutdown|stream_ended|hello|error
	Msg  string `json:"msg,omitempty"`
	UDID string `json:"udid,omitempty"`
	// Operation names the request an error belongs to (attach|boot|restart|
	// shutdown), so a failure can be routed to the right piece of UI.
	Operation string `json:"operation,omitempty"`
	// Code is the stable machine value to branch on; Msg is for humans and may
	// be reworded freely.
	Code string `json:"code,omitempty"`
	// Retryable says whether re-sending the same request unchanged is worth it.
	// Only meaningful on an error, and only false is the default — an absent
	// field means "no".
	Retryable bool   `json:"retryable,omitempty"`
	W         uint64 `json:"w,omitempty"`         // frame dimensions, set in the "attached" reply
	H         uint64 `json:"h,omitempty"`         // frame dimensions, set in the "attached" reply
	Name      string `json:"name,omitempty"`      // hello: Mac display name
	OSVersion string `json:"osVersion,omitempty"` // hello: macOS version
	Paired    bool   `json:"paired,omitempty"`    // hello: this client's key is pinned (enrollment confirmed)
	// LatestVersion, when set in a hello, says a newer simbeamd release exists
	// (bare semver) — the client may nudge the user to upgrade the Mac side.
	LatestVersion string `json:"latestVersion,omitempty"`
	// Version is the daemon's own version in the hello (e.g. "0.12.0", "dev").
	Version string `json:"version,omitempty"`
	// Caps lists the optional control features this daemon+backend forwards
	// (e.g. "touch", "app_switcher", "lifecycle") in the hello. Clients gate
	// features on membership; an absent list = a daemon too old to advertise →
	// v1 gesture set only (tap/swipe/home/key/shake).
	Caps []string `json:"caps,omitempty"`
}

// rtcDispatch is the per-session control plane. It owns at most one video
// "attachment" (a live backend Feed) and routes inbound messages from two
// channels: "control" (lossy — input, and lifecycle from clients that predate
// CapLifecycle) and "bulk" (reliable ordered — list/screenshot/quality, and
// lifecycle from clients that speak it).
//
// It depends on plain func values (send, writeFrame) rather than *rtc.Session
// so management/input logic is unit-testable without a live pion handshake.
//
// handle() runs on pion's DataChannel goroutine. Nothing on it blocks for long
// any more: attach acknowledges and spawns asynchronously, so only boot/
// shutdown/restart (a simctl call each) hold the goroutine, and those are what
// the reliable channel is for.
type rtcDispatch struct {
	backend      Backend
	baseCtx      context.Context
	send         func([]byte)       // control reply (text)
	sendBulk     func([]byte) error // bulk reply, image chunk (binary)
	sendBulkText func(string) error // bulk reply, transfer header or error envelope (text)
	bulkMaxMsg   func() int         // peer's negotiated max message size, the hard cap on one sendBulk
	writeFrame   func([]byte, time.Duration) error
	hostName     string   // Mac display name, sent in the hello
	osVersion    string   // macOS version, sent in the hello
	version      string   // daemon version, sent in the hello
	caps         []string // backend's optional control features, sent in the hello
	// latestVersion reports a newer available daemon release for the hello, or
	// "". A func, not a string: the update checker may learn of a release while
	// the daemon runs, and later sessions should carry it. nil → never.
	latestVersion func() string

	mu  sync.Mutex
	att *attachment
	// gen counts attach intents. An attach runs asynchronously and can overlap
	// another (quality arrives on bulk's goroutine, attach/detach on control's),
	// so an attempt compares the generation it claimed against gen before
	// installing its feed or reporting a failure. See claimAttach.
	gen uint64
	// pending is the udid of an attach in flight — after the old feed is gone
	// and before the new one is installed. Without it that window looks idle to
	// shutdown, which would then let the spawn race a powering-off simulator.
	pending string
	// bulkLifecycle records that this client sends lifecycle over "bulk", which
	// is also where it expects unsolicited lifecycle events (stream_ended). A
	// reply to a request always goes back on the channel that carried it; only
	// events nobody asked for need this.
	bulkLifecycle bool
}

// sendHello pushes the unsolicited "hello" greeting the moment the control
// channel opens: it carries the Mac's display name and macOS version so a
// paired client can render "Kirill's MacBook Pro" / "macOS 26.5" instead of a
// daemonID placeholder.
//
// hello also doubles as the explicit pin-ack (paired:true): reaching the
// control channel is only possible past the authentication gate, which an
// enrolling client clears only after its key is durably pinned — so the greeting
// is proof to the client that its key is saved. A client that persisted a Mac
// optimistically on scan uses this to confirm the pairing actually took (a dial
// that drops before the hello means the pin is unconfirmed).
func (d *rtcDispatch) sendHello() {
	var latest string
	if d.latestVersion != nil {
		latest = d.latestVersion()
	}
	d.reply(ctrlReply{Type: "hello", Name: d.hostName, OSVersion: d.osVersion, Paired: true,
		Version: d.version, Caps: d.caps, LatestVersion: latest})
}

func (d *rtcDispatch) handle(data []byte) {
	m, err := parseControl(data)
	if err != nil {
		return // ignore malformed/unknown
	}
	// Lifecycle is still accepted here for clients that predate CapLifecycle,
	// and answered on this same lossy channel — answering on "bulk" a client
	// that isn't listening there would be worse than the drop it risks.
	if d.lifecycle(m, false) {
		return
	}
	switch m.Type {
	case "tap", "touch", "home", "swipe", "key", "app_switcher":
		d.doInput(m)
	case "shake":
		d.doShake()
	case "hello":
		// Re-request of the unsolicited greeting: it rides the lossy control
		// channel and doubles as the pin-ack, so a client that missed it asks
		// again. Idempotent — same reply as the on-open push.
		d.sendHello()
	}
}

// lifecycle dispatches one lifecycle request, whichever channel carried it, and
// reports whether it was one. reliable says it arrived on "bulk": the answer
// goes back there rather than on "control", because a reply on a channel the
// client isn't listening to is worse than no reply at all.
func (d *rtcDispatch) lifecycle(m controlMsg, reliable bool) bool {
	reply := d.reply
	if reliable {
		reply = func(v ctrlReply) {
			// Remember the route on the first answer rather than up front: an
			// unknown type falls through this switch untouched, and it must not
			// convince the session that this client reads lifecycle on "bulk"
			// — that is where its unsolicited stream_ended would then go.
			d.mu.Lock()
			d.bulkLifecycle = true
			d.mu.Unlock()
			d.replyBulk(v)
		}
	}
	switch m.Type {
	case "boot":
		d.doBoot(m.UDID, reply)
	case "restart":
		d.doRestart(m.UDID, reply)
	case "attach":
		d.doAttach(m.UDID, m.QualityOpts, reply)
	case "detach":
		d.doDetach(reply)
	case "shutdown":
		d.doShutdown(m.UDID, reply)
	default:
		return false
	}
	return true
}

func (d *rtcDispatch) doBoot(udid string, reply func(ctrlReply)) {
	if udid == "" {
		reply(ctrlReply{Type: "error", Operation: opBoot, Code: CodeBadRequest, Msg: "boot: missing udid"})
		return
	}
	if err := d.backend.Boot(d.baseCtx, udid); err != nil {
		reply(ctrlReply{Type: "error", Operation: opBoot, UDID: udid, Code: CodeBootFailed, Msg: err.Error()})
		return
	}
	reply(ctrlReply{Type: "booted", UDID: udid})
}

// doRestart power-cycles a simulator and confirms with "booted", so the client
// can attach again. It is the documented answer to a non-retryable attach
// failure — display_not_ready above all, which means the device booted but its
// framebuffer never appeared, a state only a fresh boot clears.
//
// The feed goes first, in-flight attach included: the sidecar is about to lose
// its simulator, and an attach still spawning would race the shutdown. That
// teardown is intentional, so it is silent — no "detached", no "stream_ended".
// The client asked for this and is waiting on "booted", which it must read as
// "your feed is gone; attach again".
func (d *rtcDispatch) doRestart(udid string, reply func(ctrlReply)) {
	if udid == "" {
		reply(ctrlReply{Type: "error", Operation: opRestart, Code: CodeBadRequest, Msg: "restart: missing udid"})
		return
	}
	d.mu.Lock()
	current := d.streaming(udid)
	d.mu.Unlock()
	if current {
		d.stopAttachment()
	}

	// Bounded so a wedged CoreSimulator cannot swallow the reply: the client is
	// blocked on this one, with no feed left to fall back to.
	ctx, cancel := context.WithTimeout(d.baseCtx, restartTimeout)
	defer cancel()
	log.Printf("restart %s: shutting down", udid)
	if err := d.backend.Shutdown(ctx, udid); err != nil {
		reply(ctrlReply{Type: "error", Operation: opRestart, UDID: udid, Code: CodeRestartFailed, Msg: err.Error()})
		return
	}
	log.Printf("restart %s: booting", udid)
	if err := d.backend.Boot(ctx, udid); err != nil {
		reply(ctrlReply{Type: "error", Operation: opRestart, UDID: udid, Code: CodeRestartFailed, Msg: err.Error()})
		return
	}
	log.Printf("restart %s: booted", udid)
	reply(ctrlReply{Type: "booted", UDID: udid})
}

// doDetach stops the feed and confirms with the device it stopped — including
// one whose attach was still spawning, which is precisely when the client most
// needs to be told which intent the confirmation cancels.
func (d *rtcDispatch) doDetach(reply func(ctrlReply)) {
	reply(ctrlReply{Type: "detached", UDID: d.stopAttachment()})
}

func (d *rtcDispatch) doShutdown(udid string, reply func(ctrlReply)) {
	if udid == "" {
		reply(ctrlReply{Type: "error", Operation: opShutdown, Code: CodeBadRequest, Msg: "shutdown: missing udid"})
		return
	}
	// If the live feed is this very simulator, stop it first — shutting the sim
	// out from under the sidecar would break the pump anyway. A feed of some
	// other simulator is left untouched.
	d.mu.Lock()
	current := d.streaming(udid)
	d.mu.Unlock()
	if current {
		// Tear down the feed AND tell the client it ended, so its attachment
		// state doesn't go stale (mirrors doDetach's "detached" contract) —
		// otherwise the video just goes silent and a later detach is a no-op.
		d.stopAttachment()
		reply(ctrlReply{Type: "detached", UDID: udid})
	}
	if err := d.backend.Shutdown(d.baseCtx, udid); err != nil {
		reply(ctrlReply{Type: "error", Operation: opShutdown, UDID: udid, Code: CodeShutdownFailed, Msg: err.Error()})
		return
	}
	reply(ctrlReply{Type: "shutdown", UDID: udid})
}

func (d *rtcDispatch) doInput(m controlMsg) {
	d.mu.Lock()
	att := d.att
	d.mu.Unlock()
	if att == nil {
		return // no simulator attached → ignore input
	}
	att.feed.Input(d.baseCtx, m.input())
}

// doShake triggers a shake gesture on the currently attached simulator. Shake is
// a gesture like tap/home, so it targets the sim the client is viewing (its udid)
// rather than taking one off the wire, and it is fire-and-forget: no attachment
// is a silent no-op and a failure is only logged. It runs through simctl (see
// companion.Shake) rather than idb HID, but deliberately mirrors the HID gestures'
// contract — an error reply would wrongly drop the client's UI to "disconnected".
func (d *rtcDispatch) doShake() {
	d.mu.Lock()
	att := d.att
	d.mu.Unlock()
	if att == nil {
		return // no simulator attached → ignore, matching doInput
	}
	if err := d.backend.Shake(d.baseCtx, att.udid); err != nil {
		log.Printf("shake: %v", err)
	}
}

// lifecycleReply picks the route for an event nobody requested (stream_ended):
// the reliable channel once this client has proven it speaks lifecycle there,
// the lossy one otherwise. A request's own reply never uses this — it goes back
// on the channel that carried it.
func (d *rtcDispatch) lifecycleReply() func(ctrlReply) {
	d.mu.Lock()
	reliable := d.bulkLifecycle
	d.mu.Unlock()
	if reliable {
		return d.replyBulk
	}
	return d.reply
}

func (d *rtcDispatch) reply(v ctrlReply) {
	b, err := json.Marshal(v)
	if err != nil || d.send == nil {
		return
	}
	d.send(b)
}
