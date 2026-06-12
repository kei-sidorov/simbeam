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
	"github.com/kei-sidorov/simcast/internal/store"
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

// startDaemon runs a stub-Companion daemon's ServeSignal against wsURL. Optional
// opts configure the Server (e.g. OnEnroll) before the serve goroutine starts.
func startDaemon(t *testing.T, ctx context.Context, wsURL string, id Identity, pinned *PinnedStore, win *pairingWindow, opts ...func(*Server)) {
	t.Helper()
	dsrv := New(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", State: "Booted", OSVersion: "17.0"},
		{UDID: "B", Name: "iPad", State: "Shutdown", OSVersion: "17.0"},
	}}, "")
	for _, o := range opts {
		o(dsrv)
	}
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

// expectSims waits for the control DataChannel to deliver a 2-sim list. The
// daemon also pushes an unsolicited "hello" on channel open, which may arrive
// before the sims reply, so non-sims frames are skipped.
func expectSims(t *testing.T, replies chan []byte, pc *webrtc.PeerConnection) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case raw := <-replies:
			var r ctrlReply
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("unmarshal reply %q: %v", raw, err)
			}
			if r.Type == "hello" {
				continue // unsolicited greeting; keep waiting for sims
			}
			if r.Type != "sims" || len(r.Sims) != 2 {
				t.Fatalf("want 2 sims, got type=%q n=%d (%s)", r.Type, len(r.Sims), raw)
			}
			return
		case <-deadline:
			t.Fatalf("control reply never arrived (state=%s)", pc.ConnectionState())
		}
	}
}

// expectHello waits for the daemon's unsolicited "hello" greeting and returns
// its Mac name + macOS version, skipping any other control frames.
func expectHello(t *testing.T, replies chan []byte, pc *webrtc.PeerConnection) ctrlReply {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case raw := <-replies:
			var r ctrlReply
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("unmarshal reply %q: %v", raw, err)
			}
			if r.Type == "hello" {
				return r
			}
		case <-deadline:
			t.Fatalf("hello never arrived (state=%s)", pc.ConnectionState())
		}
	}
}

// expectHelloAndSims drains control replies until it has seen BOTH the daemon's
// hello and the 2-sim list (they race on channel open), returning the hello.
func expectHelloAndSims(t *testing.T, replies chan []byte, pc *webrtc.PeerConnection) ctrlReply {
	t.Helper()
	var hello *ctrlReply
	sawSims := false
	deadline := time.After(15 * time.Second)
	for hello == nil || !sawSims {
		select {
		case raw := <-replies:
			var r ctrlReply
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("unmarshal reply %q: %v", raw, err)
			}
			switch r.Type {
			case "hello":
				h := r
				hello = &h
			case "sims":
				if len(r.Sims) != 2 {
					t.Fatalf("want 2 sims, got %d (%s)", len(r.Sims), raw)
				}
				sawSims = true
			}
		case <-deadline:
			t.Fatalf("hello+sims never both arrived (state=%s)", pc.ConnectionState())
		}
	}
	return *hello
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
	enrolled := make(chan string, 1)
	startDaemon(t, ctx, wsURL, id, pinned, win, func(s *Server) {
		s.OnEnroll(func(pub string) { enrolled <- pub })
	})

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pc, replies := newOfferer(t)
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret)
	t.Cleanup(func() { _ = ws.Close() })

	runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
	// The fresh enrollee receives the hello pin-ack (paired:true), confirming its
	// key is durably saved — the explicit confirmation iOS persists on (#3).
	if hello := expectHelloAndSims(t, replies, pc); !hello.Paired {
		t.Fatalf("enrolled client must receive hello paired:true, got %+v", hello)
	}

	if !pinned.Contains(clientPub) {
		t.Fatalf("client was not pinned after enrollment")
	}

	// The OnEnroll callback must fire exactly once, with the enrolled client's key.
	select {
	case got := <-enrolled:
		if got != clientPub {
			t.Fatalf("OnEnroll fired with %q, want enrolled client %q", got, clientPub)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("OnEnroll never fired on enrollment")
	}
}

// TestHelloCarriesHostInfo: on control-channel open the daemon pushes a hello
// carrying the Mac display name and macOS version (BLIND-SPOTS #2) so the client
// can render them instead of a daemonID placeholder.
func TestHelloCarriesHostInfo(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:stun.l.google.com:19302"}})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")
	_ = pinned.Add(clientPub, "iPad")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow(), func(s *Server) {
		s.WithHost("Kirill's MacBook Pro", "26.5")
	})

	pc, replies := newOfferer(t)
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "")
	t.Cleanup(func() { _ = ws.Close() })
	runHandshake(t, ws, pc, id.PubB64, clientPriv, first)

	hello := expectHello(t, replies, pc)
	if hello.Name != "Kirill's MacBook Pro" || hello.OSVersion != "26.5" {
		t.Fatalf("hello = {name:%q osVersion:%q}, want Mac name + macOS version", hello.Name, hello.OSVersion)
	}
	if !hello.Paired {
		t.Fatalf("hello must carry paired:true (pin-ack), got %+v", hello)
	}
}

// TestReconnectByDaemonID: a pre-pinned client connects with NO secret (key-only
// challenge), reaches the control plane, then reconnects a second time on the
// same daemon — proving the reconnect path needs no QR/secret.
func TestReconnectByDaemonID(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:stun.l.google.com:19302"}})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")
	_ = pinned.Add(clientPub, "iPad") // already enrolled

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow()) // window CLOSED

	for i := 0; i < 2; i++ {
		pc, replies := newOfferer(t)
		ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "")
		runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
		expectSims(t, replies, pc)
		_ = ws.Close()
		_ = pc.Close()
		time.Sleep(100 * time.Millisecond) // let the daemon release the prior session
	}
}

// TestUnpinnedClientRejected: with the window closed, a client the daemon has not
// pinned is refused (peer-pinning: the daemon decides access, not the broker).
func TestUnpinnedClientRejected(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:x"}})
	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow())

	stranger, strangerPriv, _ := signal.GenerateKeyPair()
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, stranger, strangerPriv, "")
	t.Cleanup(func() { _ = ws.Close() })

	// The daemon must answer the challenge with an error ("not paired"). Sign the
	// (empty) challenge if one came, then expect an error.
	if first.Type == signal.TypeChallenge {
		t.Fatalf("unpinned client should NOT receive a challenge")
	}
	if first.Type != signal.TypeError || !strings.Contains(first.Msg, "not paired") {
		t.Fatalf("want 'not paired' error, got %+v", first)
	}
}

// TestExpiredPairingCodeTyped: a client scanning a QR whose window has expired
// gets a typed CodePairExpired, not just opaque "not paired" text (BLIND-SPOTS
// #4) — even when its enrollment proof is otherwise valid.
func TestExpiredPairingCodeTyped(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:x"}})
	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")

	win := NewPairingWindow()
	const secret = "ENROLL-SECRET"
	win.Open(secret, time.Now().Add(-10*time.Minute), 5*time.Minute) // already past its TTL

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, win)

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret)
	t.Cleanup(func() { _ = ws.Close() })
	if first.Type != signal.TypeError || first.Code != signal.CodePairExpired {
		t.Fatalf("want typed %q error, got %+v", signal.CodePairExpired, first)
	}
}

// TestUsedPairingCodeTyped: a client presenting a secret whose single-use window
// was already consumed gets a typed CodePairUsed, distinct from "expired".
func TestUsedPairingCodeTyped(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:x"}})
	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")

	win := NewPairingWindow()
	const secret = "ENROLL-SECRET"
	now := time.Now()
	win.Open(secret, now, 5*time.Minute)
	// Burn the single use as if a first client had already paired.
	firstPub, _, _ := signal.GenerateKeyPair()
	n, _ := signal.NewNonce()
	if r := win.verify(firstPub, n, signal.EnrollProof(secret, firstPub, n), now); r != pairOK {
		t.Fatalf("setup: fresh window should accept the first proof, got %v", r)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, win)

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret)
	t.Cleanup(func() { _ = ws.Close() })
	if first.Type != signal.TypeError || first.Code != signal.CodePairUsed {
		t.Fatalf("want typed %q error, got %+v", signal.CodePairUsed, first)
	}
}

// TestTurnGateBySubscription: an active subscription for the client's key yields
// STUN+TURN; no subscription yields STUN only. The client key the broker gates on
// is the one authenticated by the challenge-response.
func TestTurnGateBySubscription(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/subs.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	wsURL := brokerFixture(t, signalbroker.Config{
		STUNURLs:   []string{"stun:stun.l.google.com:19302"},
		TURNURLs:   []string{"turn:relay.example:3478"},
		TURNSecret: "secret",
		Store:      st,
	})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow())

	connect := func(clientPub string, clientPriv ed25519.PrivateKey) []signal.ICEServer {
		_ = pinned.Add(clientPub, "")
		pc, replies := newOfferer(t)
		ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "")
		ice := runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
		expectSims(t, replies, pc)
		_ = ws.Close()
		_ = pc.Close()
		time.Sleep(100 * time.Millisecond)
		return ice
	}

	// Subscribed client → STUN + TURN.
	subPub, subPriv, _ := signal.GenerateKeyPair()
	if err := st.Upsert(ctx, store.Subscription{
		ClientPubKey: subPub, ProductID: "pro", ExpiresAt: "2099-01-01T00:00:00Z",
		IssuedAt: "2026-06-04T00:00:00Z", Source: "client", UpdatedAt: "2026-06-04T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if ice := connect(subPub, subPriv); len(ice) != 2 {
		t.Fatalf("subscribed client want STUN+TURN, got %d iceServers", len(ice))
	}

	// Unsubscribed client → STUN only.
	freePub, freePriv, _ := signal.GenerateKeyPair()
	if ice := connect(freePub, freePriv); len(ice) != 1 {
		t.Fatalf("free client want STUN only, got %d iceServers", len(ice))
	}
}
