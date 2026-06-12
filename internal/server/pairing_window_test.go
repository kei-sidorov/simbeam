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

	if r := w.verify(pub, nonce, proof, now.Add(time.Minute)); r != pairOK {
		t.Fatalf("valid proof inside window rejected: %v", r)
	}
	// Single-use: a second valid proof must fail with pairUsed (window consumed).
	if r := w.verify(pub, nonce, proof, now.Add(time.Minute)); r != pairUsed {
		t.Fatalf("window accepted a second use: %v", r)
	}
}

func TestPairingWindow_RejectsExpiredClosedAndBadProof(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	const pub = "CLIENTPUB=="
	nonce, _ := signal.NewNonce()

	// Never opened → pairNoWindow (distinct from a bad-proof rejection).
	var closed pairingWindow
	if r := closed.verify(pub, nonce, signal.EnrollProof("x", pub, nonce), now); r != pairNoWindow {
		t.Fatalf("never-opened window: got %v, want pairNoWindow", r)
	}

	// Expired (TTL passed) → pairExpired.
	var w pairingWindow
	w.open("S", now, 5*time.Minute)
	proof := signal.EnrollProof("S", pub, nonce)
	if r := w.verify(pub, nonce, proof, now.Add(6*time.Minute)); r != pairExpired {
		t.Fatalf("expired window: got %v, want pairExpired", r)
	}

	// Cancelled (C key / TTL fired) → pairExpired, even before its TTL.
	var wc pairingWindow
	wc.open("S", now, 5*time.Minute)
	wc.Close()
	if r := wc.verify(pub, nonce, signal.EnrollProof("S", pub, nonce), now.Add(time.Minute)); r != pairExpired {
		t.Fatalf("cancelled window: got %v, want pairExpired", r)
	}

	// Bad proof inside a fresh window → pairBadProof.
	var w2 pairingWindow
	w2.open("S", now, 5*time.Minute)
	if r := w2.verify(pub, nonce, "AAAA", now); r != pairBadProof {
		t.Fatalf("bad proof: got %v, want pairBadProof", r)
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
	if r := w.verify(pub, nonce, proof, now.Add(5*time.Minute)); r != pairOK {
		t.Fatalf("proof at exact expiry instant should still be accepted: %v", r)
	}
}
