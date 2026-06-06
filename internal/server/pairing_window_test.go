package server

import (
	"testing"
	"time"

	"github.com/kei-sidorov/simcast/internal/signal"
)

func TestPairingWindow_VerifyConsumesSingleUse(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	const pub = "CLIENTPUB=="
	var w pairingWindow
	w.open("S-secret", now, 5*time.Minute)

	nonce, _ := signal.NewNonce()
	proof := signal.EnrollProof("S-secret", pub, nonce)

	if !w.verify(pub, nonce, proof, now.Add(time.Minute)) {
		t.Fatalf("valid proof inside window rejected")
	}
	// Single-use: a second valid proof must fail (window consumed).
	if w.verify(pub, nonce, proof, now.Add(time.Minute)) {
		t.Fatalf("window accepted a second use")
	}
}

func TestPairingWindow_RejectsExpiredClosedAndBadProof(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	const pub = "CLIENTPUB=="
	nonce, _ := signal.NewNonce()

	// Closed window (never opened): secret=="" so verify returns false regardless
	// of proof correctness — this is NOT a bad-proof rejection.
	var closed pairingWindow
	if closed.verify(pub, nonce, signal.EnrollProof("x", pub, nonce), now) {
		t.Fatalf("closed window accepted a proof")
	}

	// Expired.
	var w pairingWindow
	w.open("S", now, 5*time.Minute)
	proof := signal.EnrollProof("S", pub, nonce)
	if w.verify(pub, nonce, proof, now.Add(6*time.Minute)) {
		t.Fatalf("expired window accepted a proof")
	}

	// Bad proof inside a fresh window.
	var w2 pairingWindow
	w2.open("S", now, 5*time.Minute)
	if w2.verify(pub, nonce, "AAAA", now) {
		t.Fatalf("accepted a bad proof")
	}
}

func TestPairingWindow_AtExactExpiryStillValid(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	const pub = "CLIENTPUB=="
	var w pairingWindow
	w.open("S", now, 5*time.Minute)
	nonce, err := signal.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	proof := signal.EnrollProof("S", pub, nonce)
	// Exactly at expires (now+5m): now.After(expires) is false, so still valid.
	if !w.verify(pub, nonce, proof, now.Add(5*time.Minute)) {
		t.Fatalf("proof at exact expiry instant should still be accepted")
	}
}
