package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/kei-sidorov/simcast/internal/companion"
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
	hostName   string // Mac display name, sent in the hello
	osVersion  string // macOS version, sent in the hello

	mu  sync.Mutex
	att *attachment
}

// sendHello pushes the unsolicited "hello" greeting the moment the control
// channel opens: it carries the Mac's display name and macOS version so a
// paired client can render "Kirill's MacBook Pro" / "macOS 26.5" instead of a
// daemonID placeholder.
func (d *rtcDispatch) sendHello() {
	d.reply(ctrlReply{Type: "hello", Name: d.hostName, OSVersion: d.osVersion})
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

func (d *rtcDispatch) doShutdown(udid string) {
	if udid == "" {
		d.reply(ctrlReply{Type: "error", Msg: "shutdown: missing udid"})
		return
	}
	// If the live feed is this very simulator, stop it first — shutting the sim
	// out from under the sidecar would break the pump anyway. A feed of some
	// other simulator is left untouched.
	d.mu.Lock()
	current := d.att != nil && d.att.udid == udid
	d.mu.Unlock()
	if current {
		d.stopAttachment()
	}
	if err := d.comp.Shutdown(d.baseCtx, udid); err != nil {
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
	applyControl(d.baseCtx, att.client, att.screen, m)
}

func (d *rtcDispatch) reply(v ctrlReply) {
	b, err := json.Marshal(v)
	if err != nil || d.send == nil {
		return
	}
	d.send(b)
}
