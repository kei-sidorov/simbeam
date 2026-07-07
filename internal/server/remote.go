package server

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simcast/internal/rtc"
	"github.com/kei-sidorov/simcast/internal/signal"
)

// toWebRTC converts broker iceServers to pion's type (kept here so
// internal/signal stays webrtc-free, preserving the decision #30 boundary).
func toWebRTC(in []signal.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return out
}

// signedAnswer wraps an answer SDP into a signaling Msg whose Sig authenticates
// the SDP under the daemon's permanent key. The browser verifies it against the
// pinned daemonPubKey (anti-MITM), which also proves the daemon controls its key
// — so a separate daemon-nonce challenge is unnecessary.
func signedAnswer(sdp string, priv ed25519.PrivateKey) signal.Msg {
	return signal.Msg{Type: signal.TypeAnswer, SDP: sdp, Sig: signal.Sign(priv, []byte(sdp))}
}

// ServeSignal keeps a persistent registration on the broker under the daemon's
// identity and serves reconnecting pinned clients one at a time, forever, with
// exponential-backoff auto-reconnect. win is the (possibly closed) enrollment
// window letting a not-yet-pinned client enroll with secret S. Returns when ctx
// is cancelled.
func (s *Server) ServeSignal(ctx context.Context, signalURL string, id Identity, pinned *PinnedStore, win *pairingWindow) error {
	backoff := time.Second
	rl := &reconnLog{logf: log.Printf, verbose: s.verbose}
	for ctx.Err() == nil {
		start := time.Now()
		err := s.serveOnce(ctx, signalURL, id, pinned, win, rl.up)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A connection that stayed up well past the max backoff was healthy;
		// reset so the first drop after a long stable period retries promptly
		// instead of inheriting a stale 30s penalty.
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		rl.lost(err, backoff, time.Since(start))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
	return ctx.Err()
}

// reconnLog renders the broker reconnect narrative. By default it reports only
// state transitions — one line when the daemon first drops off the broker, one
// when it comes back — so routine Mac sleep/wake churn doesn't fill the console.
// With -v (verbose) it logs every failed attempt with its cause and backoff, as
// the daemon always did. logf is injected (log.Printf in production) so the line
// sequence is unit-testable without a broker.
type reconnLog struct {
	logf    func(format string, args ...any)
	verbose bool
	down    bool // currently in the announced-offline state
}

// up marks the connection live (called after a successful register). It announces
// recovery only if we had previously announced going offline; the first connect
// of a run stays silent because the startup banner already reported presence.
func (r *reconnLog) up() {
	if r.down {
		r.logf("broker back online")
		r.down = false
	}
}

// lost records a dropped connection. Verbose logs every attempt; the default logs
// a single "offline" line on the healthy→down transition and then stays quiet
// through the retry backoff until up() reports recovery.
func (r *reconnLog) lost(err error, backoff, upFor time.Duration) {
	if r.verbose {
		r.logf("signaling connection lost: %v; reconnecting in %s", err, backoff)
		return
	}
	if !r.down {
		r.logf("broker connection lost (up %s) — reconnecting", upFor.Round(time.Second))
		r.down = true
	}
}

// serveOnce holds one broker connection: register, then process the relayed
// handshake for a single active client at a time. The live P2P peer runs in
// pion's own goroutines; the broker WS stays open for the next client (revises
// #51: signaling is now persistent presence, not handshake-then-close).
func (s *Server) serveOnce(ctx context.Context, signalURL string, id Identity, pinned *PinnedStore, win *pairingWindow, onUp func()) error {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, signalURL, nil)
	if err != nil {
		return fmt.Errorf("dial signaling: %w", err)
	}
	defer ws.Close()

	var wmu sync.Mutex
	send := func(m signal.Msg) error { wmu.Lock(); defer wmu.Unlock(); return ws.WriteJSON(m) }

	if err := send(signal.Msg{Type: signal.TypeRegister, Role: signal.RoleDaemon, Daemon: id.PubB64}); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	// Dialed and announced presence on the broker: we're live. Reports recovery
	// if a prior drop had been announced.
	onUp()

	// Single active client session state.
	var (
		sess       *rtc.Session
		disp       *rtcDispatch
		sessCancel context.CancelFunc
		iceServers []webrtc.ICEServer
		authPub    string
		authNonce  string
		enrolling  bool
		authed     bool
	)
	cleanup := func() {
		// sessCancel may already have fired via OnClose; CancelFunc is idempotent.
		if sessCancel != nil {
			sessCancel()
		}
		if disp != nil {
			disp.stopAttachment()
		}
		if sess != nil {
			_ = sess.Close()
		}
		sess, disp, sessCancel = nil, nil, nil
		authPub, authNonce, enrolling, authed = "", "", false, false
		iceServers = nil
	}
	defer cleanup()

	for {
		var m signal.Msg
		if err := ws.ReadJSON(&m); err != nil {
			return fmt.Errorf("read signaling: %w", err)
		}
		switch m.Type {
		case signal.TypeConnect:
			cleanup() // drop any prior client
			allow, enr := false, false
			var code string
			if pinned.Contains(m.PubKey) {
				allow = true
			} else {
				// NOTE: a valid enrollment proof consumes the single-use window here,
				// even if the client subsequently fails the key challenge below. An
				// attacker would need to know S to reach this path; the user re-arms
				// with P. (Acceptable for the self-host scope.) The verify reason is
				// surfaced as a typed Code so the client can tell expired from used.
				switch win.verify(m.PubKey, m.Nonce, m.Pair, time.Now()) {
				case pairOK:
					allow, enr = true, true
				case pairExpired:
					code = signal.CodePairExpired
				case pairUsed:
					code = signal.CodePairUsed
				default: // pairNoWindow, pairBadProof
					code = signal.CodePairInvalid
				}
			}
			if !allow {
				_ = send(signal.Msg{Type: signal.TypeError, Code: code, Msg: "not paired"})
				continue
			}
			nonce, nerr := signal.NewNonce()
			if nerr != nil {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: "nonce error"})
				continue
			}
			authPub, authNonce, enrolling, authed = m.PubKey, nonce, enr, false
			_ = send(signal.Msg{Type: signal.TypeChallenge, Nonce: nonce})
		case signal.TypeICEServers:
			iceServers = toWebRTC(m.ICEServers)
		case signal.TypeProof:
			if authPub == "" || !signal.Verify(authPub, []byte(authNonce), m.Sig) {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: "challenge failed"})
				cleanup()
				continue
			}
			if enrolling {
				// Pin BEFORE authenticating: only a durably-saved key may pass the
				// gate, so the hello pin-ack (paired:true) the client later receives
				// is never a lie. If the save fails the client stays unpaired and
				// can re-pair, rather than becoming "saved but forever offline".
				if err := pinned.Add(authPub, ""); err != nil {
					_ = send(signal.Msg{Type: signal.TypeError, Msg: "could not save pairing"})
					cleanup()
					continue
				}
				if s.onEnroll != nil {
					s.onEnroll(authPub)
				}
			}
			authed = true
		case signal.TypeOffer:
			if !authed {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: "unauthenticated"})
				continue
			}
			sctx, cancel := context.WithCancel(ctx)
			ns, nd, serr := s.startSession(sctx, iceServers)
			if serr != nil {
				cancel()
				_ = send(signal.Msg{Type: signal.TypeError, Msg: serr.Error()})
				continue
			}
			sess, disp, sessCancel = ns, nd, cancel
			// On peer death, cancel the session AND reap its sidecar eagerly —
			// don't wait for the broker's peerLeft (which may never come on a
			// silent ICE failure). stopAttachment is idempotent, so the later
			// cleanup() calling it again is harmless.
			ns.OnClose(func() { cancel(); nd.stopAttachment() })
			answerSDP, aerr := ns.Answer(m.SDP)
			if aerr != nil {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: aerr.Error()})
				cleanup()
				continue
			}
			_ = send(signedAnswer(answerSDP, id.Priv))
		case signal.TypePeerLeft:
			cleanup()
		}
	}
}
