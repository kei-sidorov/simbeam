package server

import (
	"context"
	"time"

	"github.com/kei-sidorov/simcast/internal/encoder"
	"github.com/kei-sidorov/simcast/internal/idb"
)

// attachment is one live video feed: a spawned idb_companion sidecar whose
// screenshots are encoded to H.264 and pumped into the session's video track.
// Exactly one attachment exists per session at a time.
type attachment struct {
	cancel  context.CancelFunc
	sidecar *idb.Sidecar
	client  *idb.Client // routes input (taps/swipes/keys) to this feed via doInput
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

	// Register the attachment BEFORE launching the pump so any concurrent or
	// subsequent stopAttachment (detach / switch / session end) always sees it.
	att := &attachment{cancel: cancel, sidecar: sidecar, client: client, screen: screen}
	d.mu.Lock()
	d.att = att
	d.mu.Unlock()

	go func() {
		for f := range frames {
			if err := d.writeFrame(f.Data, f.Duration); err != nil {
				break
			}
		}
		// Pump ended (stream closed, write failed, or ctx cancelled by
		// stopAttachment). Cancel our own ctx, then tear down THIS attachment
		// only if it is still current — a fast re-attach may have already
		// swapped in a newer one, which we must not disturb (and whose sidecar
		// we must not double-close).
		cancel()
		d.mu.Lock()
		if d.att == att {
			d.att = nil
			d.mu.Unlock()
			sidecar.Close()
		} else {
			d.mu.Unlock()
		}
	}()

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
