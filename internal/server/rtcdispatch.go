package server

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/kei-sidorov/simbeam/internal/companion"
)

// ctrlReply is a downstream control message: daemon → client over the
// "control" DataChannel.
type ctrlReply struct {
	Type      string                `json:"type"` // sims|booted|attached|detached|shutdown|hello|error
	Msg       string                `json:"msg,omitempty"`
	Sims      []companion.Simulator `json:"sims,omitempty"`
	UDID      string                `json:"udid,omitempty"`
	W         uint64                `json:"w,omitempty"`         // frame dimensions, set in the "attached" reply
	H         uint64                `json:"h,omitempty"`         // frame dimensions, set in the "attached" reply
	Name      string                `json:"name,omitempty"`      // hello: Mac display name
	OSVersion string                `json:"osVersion,omitempty"` // hello: macOS version
	Paired    bool                  `json:"paired,omitempty"`    // hello: this client's key is pinned (enrollment confirmed)
}

// rtcDispatch is the per-session control plane. It owns at most one video
// "attachment" (a live backend Feed) and routes inbound control messages:
// management (list/boot/attach/detach) and input (tap/swipe/...).
//
// It depends on plain func values (send, writeFrame) rather than *rtc.Session
// so management/input logic is unit-testable without a live pion handshake.
//
// handle() runs on pion's DataChannel goroutine; boot/attach block it briefly
// (backend.Attach waits for feed readiness). Acceptable for the debug/local
// scope — revisit if it stalls input during attach.
type rtcDispatch struct {
	backend      Backend
	baseCtx      context.Context
	send         func([]byte)       // control reply (text)
	sendBulk     func([]byte) error // bulk reply, image chunk (binary)
	sendBulkText func(string) error // bulk reply, transfer header or error envelope (text)
	bulkMaxMsg   func() int         // peer's negotiated max message size, the hard cap on one sendBulk
	writeFrame   func([]byte, time.Duration) error
	hostName     string // Mac display name, sent in the hello
	osVersion    string // macOS version, sent in the hello

	mu  sync.Mutex
	att *attachment
	// gen counts attach intents. An attach can run concurrently with another
	// (quality arrives on bulk's goroutine, attach/detach on control's), and
	// backend.Attach is slow, so an attempt compares the generation it claimed
	// against gen before installing its feed. See claimAttach.
	gen uint64
	// pending is the udid of an attach in flight — after the old feed is gone
	// and before the new one is installed. Without it that window looks idle to
	// shutdown, which would then let the spawn race a powering-off simulator.
	pending string
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
	d.reply(ctrlReply{Type: "hello", Name: d.hostName, OSVersion: d.osVersion, Paired: true})
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
	case "shutdown":
		d.doShutdown(m.UDID)
	case "attach":
		d.doAttach(m.UDID, m.QualityOpts)
	case "detach":
		d.stopAttachment()
		d.reply(ctrlReply{Type: "detached"})
	case "tap", "home", "swipe", "key":
		d.doInput(m)
	case "shake":
		d.doShake()
	}
}

func (d *rtcDispatch) doList() {
	sims, err := d.backend.List(d.baseCtx)
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
	if err := d.backend.Boot(d.baseCtx, udid); err != nil {
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}
	d.reply(ctrlReply{Type: "booted", UDID: udid})
}

func (d *rtcDispatch) doShutdown(udid string) {
	if udid == "" {
		d.reply(ctrlReply{Type: "error", Msg: "shutdown: missing udid"})
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
		d.reply(ctrlReply{Type: "detached"})
	}
	if err := d.backend.Shutdown(d.baseCtx, udid); err != nil {
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}
	d.reply(ctrlReply{Type: "shutdown", UDID: udid})
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

func (d *rtcDispatch) reply(v ctrlReply) {
	b, err := json.Marshal(v)
	if err != nil || d.send == nil {
		return
	}
	d.send(b)
}
