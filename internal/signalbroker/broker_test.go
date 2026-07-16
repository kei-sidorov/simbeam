package signalbroker

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/kei-sidorov/simbeam/internal/store"
)

func wsURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) signal.Msg {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var m signal.Msg
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

func TestClientWithoutDaemonGetsOffline(t *testing.T) {
	b := New(Config{STUNURLs: []string{"stun:x"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	c := dial(t, wsURL(t, srv))
	_ = c.WriteJSON(signal.Msg{Type: signal.TypeJoin, Daemon: "missing", PubKey: "pub", Role: signal.RoleClient})
	m := readMsg(t, c)
	if m.Type != signal.TypeError || !strings.Contains(m.Msg, "offline") {
		t.Fatalf("want offline error, got %+v", m)
	}
	if m.Code != signal.CodeOffline {
		t.Fatalf("want typed offline code %q, got %q", signal.CodeOffline, m.Code)
	}
}

// TestHandshakeRelayAndGate drives a fake daemon + fake client through the broker
// and asserts: connect reaches the daemon; the broker adds brokerNonce on the
// challenge; a bad brokerSig is rejected; a good one yields iceServers whose TURN
// presence follows the subscription Store.
func TestHandshakeRelayAndGate(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenSQLite(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	b := New(Config{
		STUNURLs:   []string{"stun:x"},
		TURNURLs:   []string{"turn:relay"},
		TURNSecret: "secret",
		Store:      st,
		Now:        func() time.Time { return now },
	})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := wsURL(t, srv)

	// Client keypair (Ed25519) so signatures verify.
	clientPub, clientPriv, err := signal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	// Fake daemon registers and stays present.
	daemon := dial(t, url)
	_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeRegister, Role: signal.RoleDaemon, Daemon: "DAEMONID"})

	// Helper: run one client handshake, return the iceServers it receives.
	run := func(active bool) signal.Msg {
		if active {
			_ = st.Upsert(context.Background(), store.Subscription{
				ClientPubKey: clientPub, ProductID: "p",
				ExpiresAt: "2026-12-31T00:00:00Z", IssuedAt: "2026-06-04T00:00:00Z",
				Source: "client", UpdatedAt: "2026-06-04T00:00:00Z",
			})
		}
		client := dial(t, url)
		_ = client.WriteJSON(signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: "DAEMONID", PubKey: clientPub})

		// Daemon receives connect, replies with its challenge nonce.
		conn := readMsg(t, daemon)
		if conn.Type != signal.TypeConnect || conn.PubKey != clientPub {
			t.Fatalf("daemon want connect for client, got %+v", conn)
		}
		_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeChallenge, Nonce: "DNONCE"})

		// Client receives challenge with the broker nonce attached.
		ch := readMsg(t, client)
		if ch.Type != signal.TypeChallenge || ch.Nonce != "DNONCE" || ch.BrokerNonce == "" {
			t.Fatalf("client want challenge+brokerNonce, got %+v", ch)
		}
		// Client proves both nonces.
		_ = client.WriteJSON(signal.Msg{
			Type:      signal.TypeProof,
			Sig:       signal.Sign(clientPriv, []byte(ch.Nonce)),
			BrokerSig: signal.Sign(clientPriv, []byte(ch.BrokerNonce)),
		})
		// Client receives iceServers.
		ice := readMsg(t, client)
		if ice.Type != signal.TypeICEServers {
			t.Fatalf("client want iceServers, got %+v", ice)
		}
		// Daemon also receives iceServers, then the relayed proof (brokerSig
		// stripped). The broker emits both peers' iceServers before relaying the
		// proof, so the daemon's iceServers arrives first.
		_ = readMsg(t, daemon) // daemon iceServers
		pr := readMsg(t, daemon)
		if pr.Type != signal.TypeProof || pr.BrokerSig != "" || pr.Sig == "" {
			t.Fatalf("daemon want stripped proof, got %+v", pr)
		}
		_ = client.Close()
		// Drain the peerLeft the broker sends the daemon on client close.
		_ = readMsg(t, daemon)
		return ice
	}

	// No subscription → STUN only.
	if ice := run(false); len(ice.ICEServers) != 1 {
		t.Fatalf("unsubscribed should get STUN only, got %d servers", len(ice.ICEServers))
	}
	// Active subscription → STUN + TURN.
	if ice := run(true); len(ice.ICEServers) != 2 {
		t.Fatalf("subscribed should get STUN+TURN, got %d servers", len(ice.ICEServers))
	}
}

// TestICEServers_TURNOpenBypassesGate: with TURNOpen the broker hands TURN to a
// client that has no subscription (temporary open-relay mode); with the gate on
// (default) the same client gets STUN only.
func TestICEServers_TURNOpenBypassesGate(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		STUNURLs:   []string{"stun:x"},
		TURNURLs:   []string{"turn:relay"},
		TURNSecret: "secret",
		Now:        func() time.Time { return now },
	}

	// Gate on, no store/subscription → STUN only.
	if ice := New(cfg).iceServers("CLIENTPUB"); len(ice) != 1 {
		t.Fatalf("gated (no sub) want STUN only, got %d servers", len(ice))
	}

	// Gate open → STUN + TURN with credentials, no subscription needed.
	cfg.TURNOpen = true
	ice := New(cfg).iceServers("CLIENTPUB")
	if len(ice) != 2 {
		t.Fatalf("turn-open want STUN+TURN, got %d servers", len(ice))
	}
	if ice[1].Username == "" || ice[1].Credential == "" {
		t.Fatalf("turn-open TURN server missing credentials: %+v", ice[1])
	}
}

func TestBadBrokerSigRejected(t *testing.T) {
	b := New(Config{STUNURLs: []string{"stun:x"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := wsURL(t, srv)

	clientPub, _, _ := signal.GenerateKeyPair()
	daemon := dial(t, url)
	_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeRegister, Role: signal.RoleDaemon, Daemon: "D"})

	client := dial(t, url)
	_ = client.WriteJSON(signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: "D", PubKey: clientPub})
	_ = readMsg(t, daemon) // connect
	_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeChallenge, Nonce: "DNONCE"})
	_ = readMsg(t, client) // challenge

	// Garbage brokerSig → broker must reject with an error.
	_ = client.WriteJSON(signal.Msg{Type: signal.TypeProof, Sig: "x", BrokerSig: "not-a-sig"})
	m := readMsg(t, client)
	if m.Type != signal.TypeError {
		t.Fatalf("want error on bad broker sig, got %+v", m)
	}
}

// TestSecondClientDisplacesFirst verifies the one-client-at-a-time takeover is
// clean: when a second client joins the same daemon, the first client's
// connection is closed (its goroutine stops) so it cannot inject proof/offer into
// the new client's session, and the daemon ends up handshaking the second client.
func TestSecondClientDisplacesFirst(t *testing.T) {
	b := New(Config{STUNURLs: []string{"stun:x"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := wsURL(t, srv)

	clientPub, _, _ := signal.GenerateKeyPair()
	daemon := dial(t, url)
	_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeRegister, Role: signal.RoleDaemon, Daemon: "D"})

	// Client A joins and reaches the daemon (connect #1).
	a, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	_ = a.WriteJSON(signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: "D", PubKey: clientPub})
	if c1 := readMsg(t, daemon); c1.Type != signal.TypeConnect {
		t.Fatalf("daemon want connect for A, got %+v", c1)
	}

	// Client B joins the same daemon → takes over.
	b2, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	t.Cleanup(func() { _ = b2.Close() })
	_ = b2.WriteJSON(signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: "D", PubKey: clientPub})

	// The broker closed A's connection: A's next read must error.
	_ = a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, rerr := a.ReadMessage(); rerr == nil {
		t.Fatalf("displaced client A should have been disconnected, but read succeeded")
	}

	// The daemon receives a peerLeft (from A's displacement) and/or a connect for
	// B; drain until we see the connect for B, proving B's handshake started.
	deadline := time.Now().Add(2 * time.Second)
	sawConnectForB := false
	for time.Now().Before(deadline) {
		m := readMsg(t, daemon)
		if m.Type == signal.TypeConnect {
			sawConnectForB = true
			break
		}
		// otherwise it's a peerLeft from A's displacement — keep reading
	}
	if !sawConnectForB {
		t.Fatalf("daemon never received connect for the second client")
	}
}
