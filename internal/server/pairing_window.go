package server

import (
	"sync"
	"time"

	"github.com/kei-sidorov/simcast/internal/signal"
)

// pairingWindow is a one-time, time-boxed enrollment authorization. While open,
// unexpired, and unused, a not-yet-pinned client that proves knowledge of the
// secret S may be enrolled. Outside the window the daemon accepts no new pins
// (anti-abuse). Failed proofs do not consume the window; protection against
// brute force relies on the entropy of the secret S and the short TTL.
// now is a parameter everywhere for deterministic tests.
type pairingWindow struct {
	mu      sync.Mutex
	secret  string
	expires time.Time
	used    bool
}

// open arms the window with secret S valid for ttl from now.
func (p *pairingWindow) open(secret string, now time.Time, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secret = secret
	p.expires = now.Add(ttl)
	p.used = false
}

// verify reports whether proof authorizes pinning clientPubKey right now, and on
// success consumes the window (single-use) so a replay cannot enroll again.
func (p *pairingWindow) verify(clientPubKey, nonce, proof string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.secret == "" || p.used || now.After(p.expires) {
		return false
	}
	if !signal.VerifyEnrollProof(p.secret, clientPubKey, nonce, proof) {
		return false
	}
	p.used = true
	p.secret = ""
	return true
}

// Close disarms the window immediately (manual cancel): the secret is cleared so
// any in-flight or replayed proof is rejected. Idempotent.
func (p *pairingWindow) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secret = ""
	p.used = true
}

// NewPairingWindow returns a closed pairing window the daemon arms on demand.
func NewPairingWindow() *pairingWindow { return &pairingWindow{} }

// Open arms the window (exported wrapper over open for cmd use).
func (p *pairingWindow) Open(secret string, now time.Time, ttl time.Duration) {
	p.open(secret, now, ttl)
}
