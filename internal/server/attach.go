package server

import (
	"context"
	"log"
)

// attachment is one live video feed produced by the backend and pumped into the
// session's video track. Exactly one attachment exists per session at a time.
type attachment struct {
	cancel context.CancelFunc
	feed   Feed
	udid   string // device being streamed; lets doShutdown stop only its own feed
}

// doAttach tears down any current feed, asks the backend for a new one at
// quality q, and starts pumping its H.264 frames into the video track. Replies
// "attached" with screen dimensions, or "error".
//
// This is also the path a mid-session quality change takes (doQuality): the feed
// is torn down and respawned with new encoder arguments, since ffmpeg's argv is
// fixed at spawn. The track is not renegotiated — the fresh IDR ffmpeg emits on
// start resyncs the decoder, resolution change included (decision №50).
//
// doAttach MAY run concurrently with itself: quality changes arrive on bulk's
// goroutine while attach/detach arrive on control's. backend.Attach is slow
// enough (~1.2s for an idb sidecar) that overlap is ordinary, not exotic — so
// every attempt claims a generation from stopAttachment and drops its own feed
// if a newer intent landed while the backend was still spawning. Without that
// check the loser's attachment is overwritten and never cancelled, which no one
// can ever clean up: its pump waits on Frames(), and Frames() only closes when
// the ctx it never gets cancelled.
func (d *rtcDispatch) doAttach(udid string, q QualityOpts) {
	if udid == "" {
		d.reply(ctrlReply{Type: "error", Msg: "attach: missing udid"})
		return
	}
	d.attachAs(udid, q, d.claimAttach(udid))
}

// attachAs is doAttach's body for a caller that already claimed gen (via
// claimAttach or restartAttachment). Splitting it out is what makes an
// asynchronous re-attach safe: the generation must be claimed at the moment the
// decision is made, not inside the goroutine that carries it out. A
// `go doAttach(...)` claims it whenever the scheduler gets round to it, by which
// time a detach can have come and gone — the goroutine then finds the generation
// it just took still current and installs a feed the client already dismissed.
func (d *rtcDispatch) attachAs(udid string, q QualityOpts, gen uint64) {
	log.Printf("attach %s: starting", udid)
	ctx, cancel := context.WithCancel(d.baseCtx)
	feed, err := d.backend.Attach(ctx, udid, q)
	if err != nil {
		cancel()
		// The client gets this as an error reply, but its rendering is the
		// client's business — the daemon log must not depend on it.
		log.Printf("attach %s: %v", udid, err)
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}

	// Register the attachment BEFORE launching the pump so any concurrent or
	// subsequent stopAttachment (detach / switch / session end) always sees it.
	att := &attachment{cancel: cancel, feed: feed, udid: udid}
	d.mu.Lock()
	if d.gen != gen {
		// Superseded while the backend was spawning: the client has since
		// detached, shut this sim down, or asked for a different one. Drop what
		// we built and stay silent — the newer intent owns the reply, and
		// pending belongs to whoever superseded us.
		d.mu.Unlock()
		cancel()
		feed.Close()
		return
	}
	d.att = att
	d.pending = "" // no longer in flight; it is the live feed now
	d.mu.Unlock()

	go func() {
		for f := range feed.Frames() {
			if err := d.writeFrame(f.Data, f.Duration); err != nil {
				// A write error ends the feed for the client, who only ever sees
				// a frozen/black track — this line is the sole trace of why.
				log.Printf("attach %s: video pump stopped: %v", udid, err)
				break
			}
		}
		// Pump ended (stream closed, write failed, or ctx cancelled by
		// stopAttachment). Cancel our own ctx, then tear down THIS attachment
		// only if it is still current — a fast re-attach may have already
		// swapped in a newer one, which we must not disturb (and whose feed
		// we must not double-close).
		cancel()
		d.mu.Lock()
		if d.att == att {
			d.att = nil
			d.mu.Unlock()
			feed.Close()
		} else {
			d.mu.Unlock()
		}
	}()

	w, h := feed.Screen()
	log.Printf("attach %s: live (%dx%d)", udid, w, h)
	d.reply(ctrlReply{Type: "attached", W: w, H: h})
}

// stopAttachment cancels the current feed (stops the pump, releases the feed)
// and returns the generation this call established. Safe to call when nothing is
// attached.
//
// The generation is what lets a slow attach discover it was superseded. Every
// call bumps it, so "detach", "shutdown", and a competing "attach" all invalidate
// an attach still waiting on the backend. Callers with nothing to attach can
// ignore the value.
func (d *rtcDispatch) stopAttachment() uint64 {
	return d.claimAttach("")
}

// claimAttach cancels the current feed and claims a generation for attaching
// next (pass "" when nothing will be attached, i.e. a plain stop).
//
// It records next as pending because between here and attachAs installing the
// feed there IS no attachment — yet the session is very much busy with that
// device. Without pending, a shutdown arriving mid-spawn reads d.att == nil,
// concludes the sim isn't being streamed, and leaves the in-flight attach alone
// to spawn a sidecar against a simulator that is powering off.
func (d *rtcDispatch) claimAttach(next string) uint64 {
	d.mu.Lock()
	d.gen++
	gen := d.gen
	att := d.att
	d.att = nil
	d.pending = next
	d.mu.Unlock()
	if att != nil {
		att.cancel()
		att.feed.Close()
	}
	return gen
}

// restartAttachment stops the live feed and claims a generation for a
// replacement of the same device, reporting which device that was. ok is false
// when nothing was attached — and in that case nothing is claimed at all, since
// there is no feed to replace and invalidating an attach someone else has in
// flight would strand it (the client would get neither an "attached" nor an
// "error").
func (d *rtcDispatch) restartAttachment() (udid string, gen uint64, ok bool) {
	d.mu.Lock()
	att := d.att
	if att == nil {
		d.mu.Unlock()
		return "", 0, false
	}
	udid = att.udid
	d.gen++
	gen = d.gen
	d.att = nil
	d.pending = udid
	d.mu.Unlock()
	att.cancel()
	att.feed.Close()
	return udid, gen, true
}

// streaming reports whether udid is the device this session is streaming —
// counting one whose feed is still spawning. Callers must hold d.mu.
func (d *rtcDispatch) streaming(udid string) bool {
	return (d.att != nil && d.att.udid == udid) || d.pending == udid
}
