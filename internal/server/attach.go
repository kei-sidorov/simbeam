package server

import "context"

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
func (d *rtcDispatch) doAttach(udid string, q QualityOpts) {
	if udid == "" {
		d.reply(ctrlReply{Type: "error", Msg: "attach: missing udid"})
		return
	}
	d.stopAttachment()

	ctx, cancel := context.WithCancel(d.baseCtx)
	feed, err := d.backend.Attach(ctx, udid, q)
	if err != nil {
		cancel()
		d.reply(ctrlReply{Type: "error", Msg: err.Error()})
		return
	}

	// Register the attachment BEFORE launching the pump so any concurrent or
	// subsequent stopAttachment (detach / switch / session end) always sees it.
	att := &attachment{cancel: cancel, feed: feed, udid: udid}
	d.mu.Lock()
	d.att = att
	d.mu.Unlock()

	go func() {
		for f := range feed.Frames() {
			if err := d.writeFrame(f.Data, f.Duration); err != nil {
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
	d.reply(ctrlReply{Type: "attached", W: w, H: h})
}

// stopAttachment cancels the current feed (stops the pump, releases the feed).
// Safe to call when nothing is attached.
func (d *rtcDispatch) stopAttachment() {
	d.mu.Lock()
	att := d.att
	d.att = nil
	d.mu.Unlock()
	if att != nil {
		att.cancel()
		att.feed.Close()
	}
}
