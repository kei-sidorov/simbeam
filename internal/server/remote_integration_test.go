package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/signal"
	"github.com/kei-sidorov/simcast/internal/signalbroker"

	"net/http/httptest"
)

// TestRemotePairingEndToEnd exercises the whole server-side remote path with no
// browser and no idb_companion: a real broker, a daemon dialing it, a pion
// "browser" offerer joining by token, a signed answer that verifies against the
// daemon pubkey, a real DTLS+ICE handshake on loopback, and a control
// DataChannel that answers a {"type":"list"} with {"type":"sims"}.
func TestRemotePairingEndToEnd(t *testing.T) {
	// 1. Broker.
	b := signalbroker.New(signalbroker.Config{STUNURLs: []string{"stun:stun.l.google.com:19302"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	// 2. Keys + token.
	pub, priv, err := signal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	const token = "test-token"

	// 3. Daemon with a stub companion (two sims, no idb needed).
	dsrv := New(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", State: "Booted", OSVersion: "17.0"},
		{UDID: "B", Name: "iPad", State: "Shutdown", OSVersion: "17.0"},
	}}, "")

	// 4. Daemon dials the broker.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dialErr := make(chan error, 1)
	go func() { dialErr <- dsrv.DialSignal(ctx, wsURL, token, pub, priv) }()

	// 5. Build the pion offerer (host candidates on loopback; empty config).
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("AddTransceiverFromKind: %v", err)
	}

	replies := make(chan []byte, 4)
	dc, err := pc.CreateDataChannel("control", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	dc.OnOpen(func() {
		_ = dc.SendText(`{"type":"list"}`)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case replies <- msg.Data:
		default:
		}
	})

	// 6. Connect to the broker as the client. The broker only relays the offer
	// to the daemon once the daemon has registered, so retry the join until we
	// don't get a "no daemon" error. Bounded so the test can't hang.
	var ws *websocket.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("daemon never registered the room in time")
		}
		c, _, derr := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if derr != nil {
			t.Fatalf("dial broker: %v", derr)
		}
		if werr := c.WriteJSON(signal.Msg{Type: signal.TypeJoin, Room: token, Role: signal.RoleClient}); werr != nil {
			t.Fatalf("write join: %v", werr)
		}
		// First reply after join is either iceServers (daemon present) or error
		// ("no daemon"). Read with a deadline so a stuck broker fails the test.
		_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
		var m signal.Msg
		if rerr := c.ReadJSON(&m); rerr != nil {
			_ = c.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if m.Type == signal.TypeError {
			_ = c.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// Got iceServers (or some non-error first message): the daemon is in.
		ws = c
		break
	}
	t.Cleanup(func() { _ = ws.Close() })

	// 7. Create + gather the offer, then send it. The daemon is already blocked
	// in its read loop waiting for the offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatalf("ICE gathering did not complete")
	}
	if err := ws.WriteJSON(signal.Msg{Type: signal.TypeOffer, SDP: pc.LocalDescription().SDP}); err != nil {
		t.Fatalf("write offer: %v", err)
	}

	// 8. Read the signed answer, verify the signature, apply it.
	var sigVerified bool
	for {
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		var m signal.Msg
		if rerr := ws.ReadJSON(&m); rerr != nil {
			t.Fatalf("read answer: %v", rerr)
		}
		switch m.Type {
		case signal.TypeICEServers:
			// Daemon-side ICE config arrives for the client too; ignore (pc
			// already built with host candidates on loopback).
			continue
		case signal.TypeAnswer:
			if !signal.Verify(pub, []byte(m.SDP), m.Sig) {
				t.Fatalf("answer signature failed to verify against daemon pubkey (anti-MITM check)")
			}
			sigVerified = true
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer, SDP: m.SDP,
			}); err != nil {
				t.Fatalf("SetRemoteDescription: %v", err)
			}
		case signal.TypeError:
			t.Fatalf("broker/daemon error: %s", m.Msg)
		case signal.TypePeerLeft:
			t.Fatalf("peer left before answer")
		default:
			continue
		}
		break
	}
	if !sigVerified {
		t.Fatalf("never verified an answer signature")
	}

	// 9. Wait for the control DataChannel to open + the sims reply.
	select {
	case raw := <-replies:
		var r ctrlReply
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("unmarshal control reply %q: %v", raw, err)
		}
		if r.Type != "sims" {
			t.Fatalf("want reply type %q, got %q (%s)", "sims", r.Type, raw)
		}
		if len(r.Sims) != 2 {
			t.Fatalf("want 2 sims in reply, got %d (%s)", len(r.Sims), raw)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("control DataChannel reply never arrived (state=%s)", pc.ConnectionState())
	}

	// 10. Teardown. Closing pc drops the live peer; DialSignal should return.
	_ = pc.Close()
	_ = ws.Close()
	cancel()

	select {
	case err := <-dialErr:
		// A post-cancel error (context-cancelled read) is expected and benign.
		if err != nil {
			t.Logf("DialSignal returned after teardown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Logf("DialSignal did not return within 5s after teardown")
	}
}
