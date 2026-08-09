package rtc

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	sess, err := New(nil, nil, nil)
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
	sess, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.WriteFrame([]byte{0, 0, 0, 1, 0x65, 0x00}, 66)
}

func TestSessionSendBeforeChannel(t *testing.T) {
	sess, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// No control DataChannel has been opened by a remote peer yet.
	if err := sess.Send([]byte(`{"type":"sims"}`)); !errors.Is(err, ErrNoControlChannel) {
		t.Fatalf("want ErrNoControlChannel, got %v", err)
	}
}

func TestSessionSendBulkBeforeChannel(t *testing.T) {
	sess, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// No bulk DataChannel has been opened by a remote peer yet — both the binary
	// and text reply paths must report it rather than panic.
	if err := sess.SendBulk([]byte{0x89, 0x50}); !errors.Is(err, ErrNoBulkChannel) {
		t.Fatalf("SendBulk: want ErrNoBulkChannel, got %v", err)
	}
	if err := sess.SendBulkText(`{"type":"error"}`); !errors.Is(err, ErrNoBulkChannel) {
		t.Fatalf("SendBulkText: want ErrNoBulkChannel, got %v", err)
	}
}

func TestNewWithICEServersBuilds(t *testing.T) {
	sess, err := New(nil, nil, []webrtc.ICEServer{
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

// TestAnswerTrickle: with OnCandidate set, Answer returns without waiting for
// gathering (the answer SDP carries no candidates) and the candidates arrive on
// the callback instead, terminated by nil.
func TestAnswerTrickle(t *testing.T) {
	sess, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	cands := make(chan *webrtc.ICECandidate, 32)
	sess.OnCandidate(func(c *webrtc.ICECandidate) { cands <- c })

	answerSDP, err := sess.Answer(makeOffer(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(answerSDP, "a=candidate") {
		t.Fatalf("trickle answer should not carry candidates:\n%s", answerSDP)
	}
	var got int
	for {
		select {
		case c := <-cands:
			if c == nil { // end-of-gathering
				if got == 0 {
					t.Fatal("gathering completed without a single candidate")
				}
				return
			}
			got++
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d candidates, want end-of-gathering nil", got)
		}
	}
}

// TestAddCandidateBuffersUntilRemoteDescription: a candidate that overtakes the
// offer must not be dropped (pion rejects it outright before the remote
// description is set) — it is buffered and applied by Answer.
func TestAddCandidateBuffersUntilRemoteDescription(t *testing.T) {
	sess, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	early := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2130706431 10.9.8.7 51000 typ host"}
	if err := sess.AddCandidate(early); err != nil {
		t.Fatalf("early candidate: %v", err)
	}
	if _, err := sess.Answer(makeOffer(t)); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range sess.pc.GetStats() {
		if cs, ok := st.(webrtc.ICECandidateStats); ok && cs.IP == "10.9.8.7" {
			found = true
		}
	}
	if !found {
		t.Fatal("buffered candidate never reached the peer connection")
	}
}
