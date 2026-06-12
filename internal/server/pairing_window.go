package server

import (
	"sync"
	"time"

	"github.com/kei-sidorov/simcast/internal/signal"
)

// pairResult is the outcome of a pairing-window verify. It lets the daemon send
// the client a typed error code instead of one opaque "not paired" (BLIND-SPOTS
// #4), so the UI can tell "code expired" from "code already used".
type pairResult int

const (
	pairOK       pairResult = iota // proof valid; the window is now consumed
	pairNoWindow                   // no window has ever been armed in this process
	pairExpired                    // window armed but its TTL has passed, or it was cancelled
	pairUsed                       // window already consumed by a successful pairing
	pairBadProof                   // window live, but the proof (secret) did not match
)

// pairingWindow is a one-time, time-boxed enrollment authorization. While open,
// unexpired, and unused, a not-yet-pinned client that proves knowledge of the
// secret S may be enrolled. Outside the window the daemon accepts no new pins
// (anti-abuse). Failed proofs do not consume the window; protection against
// brute force relies on the entropy of the secret S and the short TTL.
// now is a parameter everywhere for deterministic tests.
type pairingWindow struct {
	mu        sync.Mutex
	secret    string
	expires   time.Time // kept intact after consume/cancel so verify can report "expired"
	consumed  bool      // a successful pairing used it up
	cancelled bool      // manually disarmed (C key / TTL fired)
}

// open arms the window with secret S valid for ttl from now.
func (p *pairingWindow) open(secret string, now time.Time, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secret = secret
	p.expires = now.Add(ttl)
	p.consumed = false
	p.cancelled = false
}

// verify reports why proof does or does not authorize pinning clientPubKey right
// now, and on success consumes the window (single-use) so a replay cannot enroll
// again.
func (p *pairingWindow) verify(clientPubKey, nonce, proof string, now time.Time) pairResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.consumed:
		return pairUsed
	case p.secret == "" && p.expires.IsZero():
		return pairNoWindow
	case now.After(p.expires):
		return pairExpired
	case p.cancelled:
		return pairExpired // a cancelled code reads as "expired" to the client: get a fresh one
	}
	if !signal.VerifyEnrollProof(p.secret, clientPubKey, nonce, proof) {
		return pairBadProof
	}
	p.consumed = true
	p.secret = ""
	return pairOK
}

// Close disarms the window immediately (manual cancel): the secret is cleared so
// any in-flight or replayed proof is rejected. Idempotent.
func (p *pairingWindow) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secret = ""
	p.cancelled = true
}

// NewPairingWindow returns a closed pairing window the daemon arms on demand.
func NewPairingWindow() *pairingWindow { return &pairingWindow{} }

// Open arms the window (exported wrapper over open for cmd use).
func (p *pairingWindow) Open(secret string, now time.Time, ttl time.Duration) {
	p.open(secret, now, ttl)
}
