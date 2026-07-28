package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// AttachTimeout bounds one attach attempt end to end. Whatever the backend
// does, an accepted attach reaches "attached" or exactly one typed terminal
// failure inside this window — the client never has to invent its own deadline
// for a spawn that wedged.
//
// Every startup bound below it must be strictly smaller: simbeam-control's own
// --startup-timeout-ms and the daemon's wait for its handshake (see
// backend/sim, which asserts the ordering). Whoever's clock fires first owns
// the outcome, and a typed envelope from the helper ("display_not_ready",
// retryable) is worth far more to the client than "we killed a process that
// never spoke".
//
// A var, not a const, only so tests can shorten it; nothing in production
// writes it.
var AttachTimeout = 30 * time.Second

// attachment is one live video feed produced by the backend and pumped into the
// session's video track. Exactly one attachment exists per session at a time.
type attachment struct {
	cancel context.CancelFunc
	feed   Feed
	udid   string // device being streamed; lets doShutdown stop only its own feed
}

// doAttach starts streaming udid at quality q. The intent is claimed and
// acknowledged synchronously; the backend spawn — over a second on a real
// sidecar, and now potentially a cold-boot framebuffer wait on top — happens on
// its own goroutine.
//
// That split is the point. Claiming the generation here (not inside the
// goroutine) is what makes the asynchrony safe: a detach that lands while the
// sidecar spawns invalidates this generation, so the spawn finds itself
// superseded and drops its feed instead of installing one the client already
// dismissed. Deferring the claim to the goroutine would let it take a
// generation whenever the scheduler got round to it — after the detach.
//
// The "attaching" ack is what the asynchrony costs the client, and it earns its
// keep: it says the request was accepted and names the device that owns the
// outcome, so a client that fired two attaches knows which terminal reply is
// coming. Meanwhile the channel that carried the request is free again —
// nothing blocks detach, shutdown, restart, session teardown, or input.
func (d *rtcDispatch) doAttach(udid string, q QualityOpts, reply func(ctrlReply)) {
	if udid == "" {
		reply(ctrlReply{Type: "error", Operation: opAttach, Code: CodeBadRequest, Msg: "attach: missing udid"})
		return
	}
	gen, _ := d.claimAttach(udid)
	reply(ctrlReply{Type: "attaching", UDID: udid})
	go d.attachAs(udid, q, gen, reply)
}

// attachAs is doAttach's body for a caller that already claimed gen (doAttach
// itself, or doQuality via restartAttachment). It always ends in exactly one of:
// an "attached" reply, one typed terminal "error" — or silence, when a newer
// intent superseded this one and owns the client's next reply.
//
// attachAs MAY run concurrently with itself: quality changes arrive on bulk's
// goroutine while attach/detach arrive on control's, and the spawn is slow
// enough that overlap is ordinary. Every attempt compares the generation it
// claimed against d.gen before installing its feed, and drops it otherwise.
// Without that check the loser's attachment is overwritten and never cancelled,
// which no one can ever clean up: its pump waits on Frames(), and Frames() only
// closes when the ctx it never gets cancelled.
func (d *rtcDispatch) attachAs(udid string, q QualityOpts, gen uint64, reply func(ctrlReply)) {
	log.Printf("attach %s: starting", udid)
	ctx, cancel := context.WithCancel(d.baseCtx)
	feed, err := d.attachWithin(ctx, cancel, udid, q)
	if err != nil {
		cancel()
		// The client gets this as a typed error reply, but its rendering is the
		// client's business — the daemon log must not depend on it.
		log.Printf("attach %s: %v", udid, err)
		d.failAttach(gen, udid, err, reply)
		return
	}

	// Register the attachment BEFORE launching the pump so any concurrent or
	// subsequent stopAttachment (detach / switch / session end) always sees it.
	att := &attachment{cancel: cancel, feed: feed, udid: udid}
	d.mu.Lock()
	if d.gen != gen {
		// Superseded while the backend was spawning: the client has since
		// detached, shut this sim down, restarted it, or asked for a different
		// one. Drop what we built and stay silent — the newer intent owns the
		// reply, and pending belongs to whoever superseded us.
		d.mu.Unlock()
		cancel()
		feed.Close()
		return
	}
	d.att = att
	d.pending = "" // no longer in flight; it is the live feed now
	d.mu.Unlock()

	go d.pump(att)

	w, h := feed.Screen()
	log.Printf("attach %s: live (%dx%d)", udid, w, h)
	reply(ctrlReply{Type: "attached", UDID: udid, W: w, H: h})
}

// attachWithin runs backend.Attach under AttachTimeout, cancelling the feed's
// own ctx to unblock a backend that would otherwise never return (that ctx is
// the only handle we have on it), and reporting the deadline as the cause
// rather than the "context canceled" the backend saw.
//
// The mutex is not paranoia about a rare interleaving: at the deadline instant
// both outcomes are genuinely in flight, and without a decision point one of
// them silently wins. Whoever takes the lock first settles it — if the watchdog
// does, the spawn is a timeout even should a feed arrive a moment later (its
// ctx is already dead, so installing it would hand the client a stream that
// never produces a frame); if the caller does, the watchdog is disarmed and a
// healthy feed can never be cancelled out from under it.
func (d *rtcDispatch) attachWithin(ctx context.Context, cancel context.CancelFunc, udid string, q QualityOpts) (Feed, error) {
	var (
		mu       sync.Mutex
		settled  bool
		timedOut bool
	)
	watchdog := time.AfterFunc(AttachTimeout, func() {
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return
		}
		timedOut = true
		cancel()
	})
	feed, err := d.backend.Attach(ctx, udid, q)
	watchdog.Stop()
	mu.Lock()
	settled = true
	expired := timedOut
	mu.Unlock()

	if expired {
		if err == nil {
			feed.Close()
		}
		return nil, &AttachError{
			Code:      CodeAttachTimeout,
			Msg:       fmt.Sprintf("no video feed from %s within %s", udid, AttachTimeout),
			Retryable: false,
		}
	}
	return feed, err
}

// failAttach reports one typed terminal failure for the attach that claimed gen
// — unless a newer intent has superseded it, in which case that intent owns the
// client's next reply and this one stays silent.
//
// pending is cleared BEFORE the reply, not after. A client acts on a terminal
// failure immediately (restart the device, attach again), and a session still
// claiming to be busy with this udid would make that very restart believe a
// feed is live and needs stopping. The old code left pending set forever on a
// failed attach.
func (d *rtcDispatch) failAttach(gen uint64, udid string, err error, reply func(ctrlReply)) {
	d.mu.Lock()
	if d.gen != gen {
		d.mu.Unlock()
		return
	}
	d.pending = ""
	d.mu.Unlock()

	code, retryable := CodeAttachFailed, false
	var typed *AttachError
	if errors.As(err, &typed) && typed.Code != "" {
		code, retryable = typed.Code, typed.Retryable
	}
	reply(ctrlReply{Type: "error", Operation: opAttach, UDID: udid,
		Code: code, Retryable: retryable, Msg: err.Error()})
}

// pump copies att's frames into the video track until the feed ends, then tears
// the attachment down and — only if the feed died on its own — tells the client
// with a "stream_ended" event.
//
// Distinguishing "the feed died" from "we killed it" is the whole subtlety, and
// the attachment identity answers it: every intentional teardown (detach,
// replacement, shutdown, restart, session end) goes through claimAttach, which
// clears d.att before cancelling. So finding ourselves still installed means
// nobody asked for this — the sidecar exited, the simulator went away, or the
// track write failed — and the client, which would otherwise just see the
// picture freeze forever, gets told. The one exception is session teardown,
// which cancels baseCtx without touching d.att; there is no client left to tell.
func (d *rtcDispatch) pump(att *attachment) {
	var reason error
	for f := range att.feed.Frames() {
		if err := d.writeFrame(f.Data, f.Duration); err != nil {
			// A write error ends the feed for the client, who only ever sees
			// a frozen/black track — this line is the sole trace of why.
			log.Printf("attach %s: video pump stopped: %v", att.udid, err)
			reason = err
			break
		}
	}
	att.cancel()

	d.mu.Lock()
	current := d.att == att
	if current {
		d.att = nil
	}
	d.mu.Unlock()
	if !current {
		// A fast re-attach already swapped in a newer attachment, which we must
		// not disturb — and whose owner closed this feed, so we must not
		// double-close it either.
		return
	}
	att.feed.Close()

	if d.baseCtx.Err() != nil {
		return // session is going away; nobody to tell
	}
	msg := "video feed ended"
	if reason != nil {
		msg = reason.Error()
	}
	log.Printf("attach %s: stream ended (%s)", att.udid, msg)
	d.lifecycleReply()(ctrlReply{Type: "stream_ended", UDID: att.udid, Msg: msg})
}

// stopAttachment cancels the current feed (stops the pump, releases the feed)
// and reports which device it was streaming, "" if none. Safe to call when
// nothing is attached, and idempotent.
//
// It also bumps the generation, which is what lets a slow attach discover it was
// superseded: "detach", "shutdown", "restart" and a competing "attach" all
// invalidate an attach still waiting on the backend.
func (d *rtcDispatch) stopAttachment() string {
	_, prev := d.claimAttach("")
	return prev
}

// claimAttach cancels the current feed and claims a generation for attaching
// next (pass "" when nothing will be attached, i.e. a plain stop). prev is the
// device that was being streamed — or the one an attach was still spawning for
// — so callers can name it in their reply.
//
// It records next as pending because between here and attachAs installing the
// feed there IS no attachment — yet the session is very much busy with that
// device. Without pending, a shutdown arriving mid-spawn reads d.att == nil,
// concludes the sim isn't being streamed, and leaves the in-flight attach alone
// to spawn a sidecar against a simulator that is powering off.
func (d *rtcDispatch) claimAttach(next string) (gen uint64, prev string) {
	d.mu.Lock()
	d.gen++
	gen = d.gen
	att := d.att
	prev = d.pending
	if att != nil {
		prev = att.udid
	}
	d.att = nil
	d.pending = next
	d.mu.Unlock()
	if att != nil {
		att.cancel()
		att.feed.Close()
	}
	return gen, prev
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
