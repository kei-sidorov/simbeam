package server

import (
	"testing"

	"github.com/kei-sidorov/simcast/internal/signal"
)

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
