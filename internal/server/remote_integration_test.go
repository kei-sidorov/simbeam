package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/kei-sidorov/simbeam/internal/signalbroker"
	"github.com/kei-sidorov/simbeam/internal/store"
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

// newOfferer builds a pion "browser": a recvonly video transceiver plus the two
// DataChannels the client speaks — "control" (lossy: input + small acks) and
// "bulk" (reliable ordered). The sim list rides bulk (issue #2), so the offerer
// asks for it on the bulk channel's open and forwards each channel's replies to
// its own queue: ctrl carries hello/acks, bulk carries the sims reply.
func newOfferer(t *testing.T) (pc *webrtc.PeerConnection, ctrl, bulk chan []byte) {
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
	ctrl = make(chan []byte, 4)
	bulk = make(chan []byte, 4)
	forward := func(label string, out chan []byte) *webrtc.DataChannel {
		dc, err := pc.CreateDataChannel(label, nil)
		if err != nil {
			t.Fatalf("CreateDataChannel(%s): %v", label, err)
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			select {
			case out <- m.Data:
			default:
			}
		})
		return dc
	}
	forward("control", ctrl)
	bulkDC := forward("bulk", bulk)
	bulkDC.OnOpen(func() { _ = bulkDC.SendText(`{"type":"list"}`) })
	return pc, ctrl, bulk
}

// joinUntilPresent dials the broker and sends join (with an enrollment proof when
// pairSecret != "", declaring trickle ICE when trickle), retrying until the daemon
// is registered. Returns the open ws and the first non-offline message (a
// challenge on success).
func joinUntilPresent(t *testing.T, ctx context.Context, wsURL, daemonID, clientPub string, clientPriv ed25519.PrivateKey, pairSecret string, trickle bool) (*websocket.Conn, signal.Msg) {
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
		join := signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: daemonID, PubKey: clientPub, Trickle: trickle}
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
// apply it. When the broker's iceServers says trickle, the offer goes out
// immediately and candidates are exchanged as messages (buffering the daemon's
// until the answer is applied, as a real client must). Returns the iceServers
// the broker issued (for gate assertions).
func runHandshake(t *testing.T, ws *websocket.Conn, pc *webrtc.PeerConnection, daemonID string, clientPriv ed25519.PrivateKey, first signal.Msg) []signal.ICEServer {
	t.Helper()
	var ice []signal.ICEServer
	offerSent := false
	// pion fires OnICECandidate on its own goroutine, so writes must serialize.
	var wmu sync.Mutex
	send := func(m signal.Msg) { wmu.Lock(); defer wmu.Unlock(); _ = ws.WriteJSON(m) }
	trickle, remoteSet, eoc := false, false, false
	var pending []signal.Msg
	// Under trickle the handshake isn't over at the answer: the daemon's
	// candidates arrive after it, and without them the client has nothing to
	// pair against. Keep reading until it says end-of-candidates.
	finished := func() bool { return remoteSet && (!trickle || eoc) }
	addCand := func(m signal.Msg) {
		init := webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMLineIndex: m.SDPMLineIndex}
		if m.SDPMid != "" {
			init.SDPMid = &m.SDPMid
		}
		if err := pc.AddICECandidate(init); err != nil {
			t.Errorf("AddICECandidate: %v", err)
		}
	}

	handle := func(m signal.Msg) (done bool) {
		switch m.Type {
		case signal.TypeChallenge:
			send(signal.Msg{
				Type:      signal.TypeProof,
				Sig:       signal.Sign(clientPriv, []byte(m.Nonce)),
				BrokerSig: signal.Sign(clientPriv, []byte(m.BrokerNonce)),
			})
		case signal.TypeICEServers:
			ice, trickle = m.ICEServers, m.Trickle
			if !offerSent {
				offer, err := pc.CreateOffer(nil)
				if err != nil {
					t.Fatalf("CreateOffer: %v", err)
				}
				gathered := webrtc.GatheringCompletePromise(pc)
				if m.Trickle {
					pc.OnICECandidate(func(c *webrtc.ICECandidate) {
						if c == nil {
							send(signal.Msg{Type: signal.TypeCandidate}) // end-of-candidates
							return
						}
						i := c.ToJSON()
						cm := signal.Msg{Type: signal.TypeCandidate, Candidate: i.Candidate, SDPMLineIndex: i.SDPMLineIndex}
						if i.SDPMid != nil {
							cm.SDPMid = *i.SDPMid
						}
						send(cm)
					})
				}
				if err := pc.SetLocalDescription(offer); err != nil {
					t.Fatalf("SetLocalDescription: %v", err)
				}
				if !m.Trickle {
					select {
					case <-gathered:
					case <-time.After(5 * time.Second):
						t.Fatalf("ICE gathering did not complete")
					}
				}
				send(signal.Msg{Type: signal.TypeOffer, SDP: pc.LocalDescription().SDP})
				offerSent = true
			}
		case signal.TypeCandidate:
			if m.Candidate == "" {
				eoc = true
			}
			// May overtake the answer: pion rejects candidates until the remote
			// description is set, so hold them like a real client does.
			if !remoteSet {
				pending = append(pending, m)
				return finished()
			}
			addCand(m)
		case signal.TypeAnswer:
			if !signal.Verify(daemonID, []byte(m.SDP), m.Sig) {
				t.Fatalf("answer signature failed against daemonID (anti-MITM)")
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
				t.Fatalf("SetRemoteDescription: %v", err)
			}
			remoteSet = true
			for _, c := range pending {
				addCand(c)
			}
			pending = nil
			return finished()
		case signal.TypeError:
			t.Fatalf("handshake error: %s", m.Msg)
		case signal.TypePeerLeft:
			t.Fatalf("peer left mid-handshake")
		}
		return finished()
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

// simsReassembler collects a chunked "sims" transfer off the bulk channel: the
// {"type":"sims","bytes":N} header followed by binary chunks totalling N bytes
// (issue #3). bulk is reliable + ordered, so feed sees the header before its
// chunks. done flips true once N bytes have arrived.
type simsReassembler struct {
	want int
	seen bool
	buf  []byte
	sims []bulkSim
	done bool
}

// feed consumes one bulk frame. Before the header, non-sims frames are ignored;
// after it, chunks accumulate until the announced byte count is reached.
func (r *simsReassembler) feed(t *testing.T, raw []byte) {
	if !r.seen {
		var h bulkHeader
		if err := json.Unmarshal(raw, &h); err != nil || h.Type != "sims" {
			return // hello race, or a non-sims frame; keep waiting for the header
		}
		r.seen, r.want = true, h.Bytes
	} else {
		r.buf = append(r.buf, raw...)
	}
	if !r.seen || len(r.buf) < r.want {
		return
	}
	if err := json.Unmarshal(r.buf, &r.sims); err != nil {
		t.Fatalf("sims payload does not decode: %v (%s)", err, r.buf)
	}
	r.done = true
}

// expectSims drains the bulk DataChannel until the full chunked sims reply has
// arrived (issue #3) and asserts it lists both simulators.
func expectSims(t *testing.T, bulk chan []byte, pc *webrtc.PeerConnection) {
	t.Helper()
	var r simsReassembler
	deadline := time.After(15 * time.Second)
	for !r.done {
		select {
		case raw := <-bulk:
			r.feed(t, raw)
		case <-deadline:
			t.Fatalf("bulk sims reply never arrived (state=%s)", pc.ConnectionState())
		}
	}
	if len(r.sims) != 2 {
		t.Fatalf("want 2 sims, got %d (%+v)", len(r.sims), r.sims)
	}
}

// expectHello waits for the daemon's unsolicited "hello" greeting and returns
// its Mac name + macOS version, skipping any other control frames.
func expectHello(t *testing.T, ctrl chan []byte, pc *webrtc.PeerConnection) ctrlReply {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case raw := <-ctrl:
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

// expectHelloAndSims drains both channels until it has seen BOTH the daemon's
// hello (on control) and the 2-sim list (on bulk — issue #2), returning the
// hello. The two race on channel open.
func expectHelloAndSims(t *testing.T, ctrl, bulk chan []byte, pc *webrtc.PeerConnection) ctrlReply {
	t.Helper()
	var hello *ctrlReply
	var sims simsReassembler
	deadline := time.After(15 * time.Second)
	for hello == nil || !sims.done {
		select {
		case raw := <-ctrl:
			var r ctrlReply
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("unmarshal control reply %q: %v", raw, err)
			}
			if r.Type == "hello" {
				h := r
				hello = &h
			}
		case raw := <-bulk:
			sims.feed(t, raw)
		case <-deadline:
			t.Fatalf("hello+sims never both arrived (state=%s)", pc.ConnectionState())
		}
	}
	if len(sims.sims) != 2 {
		t.Fatalf("want 2 sims, got %d (%+v)", len(sims.sims), sims.sims)
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
	pc, ctrl, bulk := newOfferer(t)
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret, false)
	t.Cleanup(func() { _ = ws.Close() })

	runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
	// The fresh enrollee receives the hello pin-ack (paired:true), confirming its
	// key is durably saved — the explicit confirmation iOS persists on (#3).
	if hello := expectHelloAndSims(t, ctrl, bulk, pc); !hello.Paired {
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

	pc, ctrl, _ := newOfferer(t)
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "", false)
	t.Cleanup(func() { _ = ws.Close() })
	runHandshake(t, ws, pc, id.PubB64, clientPriv, first)

	hello := expectHello(t, ctrl, pc)
	if hello.Name != "Kirill's MacBook Pro" || hello.OSVersion != "26.5" {
		t.Fatalf("hello = {name:%q osVersion:%q}, want Mac name + macOS version", hello.Name, hello.OSVersion)
	}
	if !hello.Paired {
		t.Fatalf("hello must carry paired:true (pin-ack), got %+v", hello)
	}
}

// TestTrickleICEEndToEnd: with both peers declaring trickle, the daemon's answer
// carries no candidates (they arrive as candidate messages instead) and the peer
// still connects — the control channel opens and the hello lands (issue #5).
func TestTrickleICEEndToEnd(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:stun.l.google.com:19302"}})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")
	_ = pinned.Add(clientPub, "iPad")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow())

	pc, ctrl, _ := newOfferer(t)
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "", true)
	t.Cleanup(func() { _ = ws.Close() })
	runHandshake(t, ws, pc, id.PubB64, clientPriv, first)

	if sdp := pc.RemoteDescription().SDP; strings.Contains(sdp, "a=candidate") {
		t.Fatalf("trickle answer should carry no candidates:\n%s", sdp)
	}
	expectHello(t, ctrl, pc)
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
		pc, _, bulk := newOfferer(t)
		ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "", false)
		runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
		expectSims(t, bulk, pc)
		_ = ws.Close()
		_ = pc.Close()
		time.Sleep(100 * time.Millisecond) // let the daemon release the prior session
	}
}

// TestServeSignalReturnsOnCancelWhileConnected reproduces the Ctrl-C hang: while
// the daemon is connected to the broker and parked in ws.ReadJSON (no messages
// pending), cancelling ctx must make ServeSignal return promptly. Before the fix
// the blocked read ignored ctx, so quitting hung until the connection dropped.
func TestServeSignalReturnsOnCancelWhileConnected(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")
	_ = pinned.Add(clientPub, "iPad") // already enrolled: join without a secret

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dsrv := New(&stubComp{}, "")
	done := make(chan error, 1)
	go func() { done <- dsrv.ServeSignal(ctx, wsURL, id, pinned, NewPairingWindow()) }()

	// Gate on the daemon actually being registered — so serveOnce is now blocked
	// in the read loop, not still dialing (which has its own ctx-aware path).
	ws, _ := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "", false)
	t.Cleanup(func() { _ = ws.Close() })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeSignal did not return within 2s of ctx cancel while connected — Ctrl-C would hang")
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
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, stranger, strangerPriv, "", false)
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
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret, false)
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
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret, false)
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

	// Stand-in for Cloudflare's credential endpoint (see internal/signalbroker/cfturn.go).
	turnAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"iceServers":[{"urls":["turn:relay.example:3478"],"username":"U","credential":"C"}]}`)
	}))
	t.Cleanup(turnAPI.Close)

	wsURL := brokerFixture(t, signalbroker.Config{
		STUNURLs:     []string{"stun:stun.l.google.com:19302"},
		TURNEndpoint: turnAPI.URL,
		TURNAPIToken: "TOKEN",
		Store:        st,
	})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow())

	connect := func(clientPub string, clientPriv ed25519.PrivateKey) []signal.ICEServer {
		_ = pinned.Add(clientPub, "")
		pc, _, bulk := newOfferer(t)
		ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "", false)
		ice := runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
		expectSims(t, bulk, pc)
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
