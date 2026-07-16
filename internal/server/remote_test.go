package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

// TestReconnLogTransitionsOnly checks the default (non-verbose) narrative: one
// line when the daemon first drops off the broker, silence through the retry
// churn, one line when it recovers.
func TestReconnLogTransitionsOnly(t *testing.T) {
	var lines []string
	rl := &reconnLog{logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}

	rl.up()                                                   // first connect of the run — silent
	rl.lost(fmt.Errorf("reset"), time.Second, 14*time.Minute) // drop → announce once
	rl.lost(fmt.Errorf("dial"), 2*time.Second, 0)             // failed retry — silent
	rl.lost(fmt.Errorf("dial"), 4*time.Second, 0)             // failed retry — silent
	rl.up()                                                   // recovered → announce once
	rl.lost(fmt.Errorf("reset"), time.Second, 5*time.Minute)  // next drop → announce again

	want := []string{
		"broker connection lost (up 14m0s) — reconnecting",
		"broker back online",
		"broker connection lost (up 5m0s) — reconnecting",
	}
	if len(lines) != len(want) {
		t.Fatalf("want %d lines %q, got %d %q", len(want), want, len(lines), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d: want %q, got %q", i, want[i], lines[i])
		}
	}
}

// TestReconnLogVerbose checks that -v restores a line for every attempt, with the
// cause and backoff.
func TestReconnLogVerbose(t *testing.T) {
	var lines []string
	rl := &reconnLog{verbose: true, logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}

	rl.lost(fmt.Errorf("read: reset"), time.Second, time.Minute)
	rl.lost(fmt.Errorf("dial: refused"), 2*time.Second, 0)

	want := []string{
		"signaling connection lost: read: reset; reconnecting in 1s",
		"signaling connection lost: dial: refused; reconnecting in 2s",
	}
	if len(lines) != len(want) {
		t.Fatalf("want %d lines, got %d: %q", len(want), len(lines), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d: want %q, got %q", i, want[i], lines[i])
		}
	}
}

func TestToWebRTCConvertsICEServers(t *testing.T) {
	in := []signal.ICEServer{
		{URLs: []string{"stun:s:3478"}},
		{URLs: []string{"turn:t:3478"}, Username: "u", Credential: "p"},
	}
	out := toWebRTC(in)
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
	if out[1].Username != "u" || out[1].Credential != "p" {
		t.Fatalf("turn creds not carried: %+v", out[1])
	}
	if out[0].URLs[0] != "stun:s:3478" {
		t.Fatalf("urls not carried: %+v", out[0])
	}
}

func TestSignedAnswerVerifies(t *testing.T) {
	pub, priv, err := signal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	m := signedAnswer("ANSWER_SDP", priv)
	if m.Type != signal.TypeAnswer || m.SDP != "ANSWER_SDP" {
		t.Fatalf("bad answer msg: %+v", m)
	}
	if !signal.Verify(pub, []byte(m.SDP), m.Sig) {
		t.Fatal("browser-side verification of the signed answer failed")
	}
}
