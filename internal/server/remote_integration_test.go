package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/signal"
	"github.com/kei-sidorov/simcast/internal/signalbroker"

	// store is used by the reconnect + TURN-gate test (Task 15) that shares
	// these helpers; kept here so the shared fixture file declares the dep.
	_ "github.com/kei-sidorov/simcast/internal/store"
)

// brokerFixture starts a real broker (optionally with a Store + TURN) on httptest
// and returns its /ws URL.
func brokerFixture(t *testing.T, cfg signalbroker.Config) string {
	t.Helper()
	b := signalbroker.New(cfg)
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
}

// startDaemon runs a stub-Companion daemon's ServeSignal against wsURL.
func startDaemon(t *testing.T, ctx context.Context, wsURL string, id Identity, pinned *PinnedStore, win *pairingWindow) {
	t.Helper()
	dsrv := New(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", State: "Booted", OSVersion: "17.0"},
		{UDID: "B", Name: "iPad", State: "Shutdown", OSVersion: "17.0"},
	}}, "")
	go func() { _ = dsrv.ServeSignal(ctx, wsURL, id, pinned, win) }()
}

// newOfferer builds a pion "browser": a recvonly video transceiver + a control
// DataChannel that asks for the sim list on open and forwards replies.
func newOfferer(t *testing.T) (*webrtc.PeerConnection, chan []byte) {
	t.Helper()
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
	dc.OnOpen(func() { _ = dc.SendText(`{"type":"list"}`) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case replies <- m.Data:
		default:
		}
	})
	return pc, replies
}

// joinUntilPresent dials the broker and sends join (with an enrollment proof when
// pairSecret != ""), retrying until the daemon is registered. Returns the open ws
// and the first non-offline message (a challenge on success).
func joinUntilPresent(t *testing.T, ctx context.Context, wsURL, daemonID, clientPub string, clientPriv ed25519.PrivateKey, pairSecret string) (*websocket.Conn, signal.Msg) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("daemon never registered in time")
		}
		c, _, derr := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if derr != nil {
			t.Fatalf("dial broker: %v", derr)
		}
		join := signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: daemonID, PubKey: clientPub}
		if pairSecret != "" {
			nonce, _ := signal.NewNonce()
			join.Nonce = nonce
			join.Pair = signal.EnrollProof(pairSecret, clientPub, nonce)
		}
		_ = c.WriteJSON(join)
		_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
		var m signal.Msg
		if err := c.ReadJSON(&m); err != nil {
			_ = c.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if m.Type == signal.TypeError && strings.Contains(m.Msg, "offline") {
			_ = c.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return c, m
	}
}

// runHandshake completes the client side: sign the challenge nonces, send the
// offer when iceServers arrive, verify the signed answer against daemonID, and
// apply it. Returns the iceServers the broker issued (for gate assertions).
func runHandshake(t *testing.T, ws *websocket.Conn, pc *webrtc.PeerConnection, daemonID string, clientPriv ed25519.PrivateKey, first signal.Msg) []signal.ICEServer {
	t.Helper()
	var ice []signal.ICEServer
	offerSent := false

	handle := func(m signal.Msg) (done bool) {
		switch m.Type {
		case signal.TypeChallenge:
			_ = ws.WriteJSON(signal.Msg{
				Type:      signal.TypeProof,
				Sig:       signal.Sign(clientPriv, []byte(m.Nonce)),
				BrokerSig: signal.Sign(clientPriv, []byte(m.BrokerNonce)),
			})
		case signal.TypeICEServers:
			ice = m.ICEServers
			if !offerSent {
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
				_ = ws.WriteJSON(signal.Msg{Type: signal.TypeOffer, SDP: pc.LocalDescription().SDP})
				offerSent = true
			}
		case signal.TypeAnswer:
			if !signal.Verify(daemonID, []byte(m.SDP), m.Sig) {
				t.Fatalf("answer signature failed against daemonID (anti-MITM)")
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
				t.Fatalf("SetRemoteDescription: %v", err)
			}
			return true
		case signal.TypeError:
			t.Fatalf("handshake error: %s", m.Msg)
		case signal.TypePeerLeft:
			t.Fatalf("peer left mid-handshake")
		}
		return false
	}

	if handle(first) {
		return ice
	}
	for {
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		var m signal.Msg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("read signaling: %v", err)
		}
		if handle(m) {
			return ice
		}
	}
}

// expectSims waits for the control DataChannel to deliver a 2-sim list.
func expectSims(t *testing.T, replies chan []byte, pc *webrtc.PeerConnection) {
	t.Helper()
	select {
	case raw := <-replies:
		var r ctrlReply
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("unmarshal reply %q: %v", raw, err)
		}
		if r.Type != "sims" || len(r.Sims) != 2 {
			t.Fatalf("want 2 sims, got type=%q n=%d (%s)", r.Type, len(r.Sims), raw)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("control reply never arrived (state=%s)", pc.ConnectionState())
	}
}

// TestEnrollmentEndToEnd: open a pairing window, a brand-new client enrolls with
// secret S, the daemon pins it, and the control DataChannel works.
func TestEnrollmentEndToEnd(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:stun.l.google.com:19302"}})

	id, err := func() (Identity, error) {
		pub, priv, e := signal.GenerateKeyPair()
		return Identity{PubB64: pub, Priv: priv}, e
	}()
	if err != nil {
		t.Fatal(err)
	}

	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")
	win := NewPairingWindow()
	const secret = "ENROLL-SECRET"
	win.Open(secret, time.Now(), 5*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, win)

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pc, replies := newOfferer(t)
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret)
	t.Cleanup(func() { _ = ws.Close() })

	runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
	expectSims(t, replies, pc)

	if !pinned.Contains(clientPub) {
		t.Fatalf("client was not pinned after enrollment")
	}
}
