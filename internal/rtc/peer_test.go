package rtc

import (
	"errors"
	"strings"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
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

// An offer announcing playout-delay (as the browser/iOS client does) must get
// it back in the answer — that is what arms the receiver's zero-delay mode.
func TestSessionAnswerNegotiatesPlayoutDelay(t *testing.T) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: playoutDelayURI}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
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

	sess, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	answer, err := sess.Answer(pc.LocalDescription().SDP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, playoutDelayURI) {
		t.Fatalf("answer must negotiate playout-delay:\n%s", answer)
	}
}

// A client that does not offer playout-delay must not see it in the answer —
// SDP answers may only accept, never introduce.
func TestSessionAnswerWithoutPlayoutDelayOffer(t *testing.T) {
	sess, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	answer, err := sess.Answer(makeOffer(t)) // stock pion offer: no playout-delay
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(answer, playoutDelayURI) {
		t.Fatalf("answer offers playout-delay the client never asked for:\n%s", answer)
	}
}

// The interceptor must stamp min=max=0 on negotiated video streams and leave
// everything else — no negotiated id, non-video — completely untouched.
func TestPlayoutDelayInterceptorStamps(t *testing.T) {
	var got *rtp.Header
	sink := interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		got = h
		return 0, nil
	})
	pd := &playoutDelay{}

	video := &interceptor.StreamInfo{
		MimeType:            "video/H264",
		RTPHeaderExtensions: []interceptor.RTPHeaderExtension{{URI: playoutDelayURI, ID: 5}},
	}
	w := pd.BindLocalStream(video, sink)
	if _, err := w.Write(&rtp.Header{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ext := got.GetExtension(5); len(ext) != 3 || ext[0] != 0 || ext[1] != 0 || ext[2] != 0 {
		t.Fatalf("want 3 zero bytes on ext id 5, got %v", ext)
	}

	noExt := &interceptor.StreamInfo{MimeType: "video/H264"}
	if w := pd.BindLocalStream(noExt, sink); w.(interceptor.RTPWriterFunc) == nil {
		t.Fatal("nil writer for un-negotiated stream")
	} else {
		got = nil
		_, _ = w.Write(&rtp.Header{}, nil, nil)
		if got.Extensions != nil {
			t.Fatalf("un-negotiated stream must pass through untouched, got extensions %v", got.Extensions)
		}
	}

	audio := &interceptor.StreamInfo{
		MimeType:            "audio/opus",
		RTPHeaderExtensions: []interceptor.RTPHeaderExtension{{URI: playoutDelayURI, ID: 5}},
	}
	got = nil
	_, _ = pd.BindLocalStream(audio, sink).Write(&rtp.Header{}, nil, nil)
	if got.Extensions != nil {
		t.Fatalf("audio stream must pass through untouched, got extensions %v", got.Extensions)
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
