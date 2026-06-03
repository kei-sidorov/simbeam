package rtc

import (
	"errors"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func makeOffer(t *testing.T) string {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.CreateDataChannel("control", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-done
	return pc.LocalDescription().SDP
}

func TestSessionAnswer(t *testing.T) {
	sess, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	answerSDP, err := sess.Answer(makeOffer(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answerSDP, "m=video") {
		t.Fatalf("answer missing video media section:\n%s", answerSDP)
	}
}

func TestSessionWriteFrameNoPanic(t *testing.T) {
	sess, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.WriteFrame([]byte{0, 0, 0, 1, 0x65, 0x00}, 66)
}

func TestSessionSendBeforeChannel(t *testing.T) {
	sess, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// No control DataChannel has been opened by a remote peer yet.
	if err := sess.Send([]byte(`{"type":"sims"}`)); !errors.Is(err, ErrNoControlChannel) {
		t.Fatalf("want ErrNoControlChannel, got %v", err)
	}
}

func TestNewWithICEServersBuilds(t *testing.T) {
	sess, err := New(nil, []webrtc.ICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// A valid configuration must still answer an offer (STUN unreachable in the
	// test is fine — non-trickle gathering completes with host candidates).
	if _, err := sess.Answer(makeOffer(t)); err != nil {
		t.Fatalf("Answer with iceServers configured: %v", err)
	}
}
