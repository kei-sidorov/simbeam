package signalbroker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kei-sidorov/simcast/internal/signal"
)

// dial connects a WS client to the broker's /ws endpoint.
func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) signal.Msg {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var m signal.Msg
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	return m
}

func newTestServer(grant bool) *httptest.Server {
	b := New(Config{
		STUNURLs:   []string{"stun:stun.example:3478"},
		TURNURLs:   []string{"turn:turn.example:3478"},
		TURNSecret: "shared-secret",
		TURNTTL:    time.Minute,
		GrantTURN:  func(string) bool { return grant },
		Now:        func() time.Time { return time.Unix(1000, 0) },
	})
	return httptest.NewServer(b.Handler())
}

func TestPairRelaysOfferAndAnswer(t *testing.T) {
	srv := newTestServer(false)
	defer srv.Close()

	daemon := dial(t, srv)
	defer daemon.Close()
	if err := daemon.WriteJSON(signal.Msg{
		Type: signal.TypeRegister, Room: "tok", Role: signal.RoleDaemon, PubKey: "PK==",
	}); err != nil {
		t.Fatal(err)
	}

	client := dial(t, srv)
	defer client.Close()
	if err := client.WriteJSON(signal.Msg{
		Type: signal.TypeJoin, Room: "tok", Role: signal.RoleClient,
	}); err != nil {
		t.Fatal(err)
	}

	// On join the client receives iceServers. Free tier (grant=false): STUN only.
	ice := readMsg(t, client)
	if ice.Type != signal.TypeICEServers || len(ice.ICEServers) != 1 {
		t.Fatalf("want one STUN-only iceServers msg, got %+v", ice)
	}
	if len(ice.ICEServers[0].URLs) == 0 || !strings.HasPrefix(ice.ICEServers[0].URLs[0], "stun:") {
		t.Fatalf("want STUN url, got %+v", ice.ICEServers[0])
	}

	// Client offer is relayed to the daemon.
	if err := client.WriteJSON(signal.Msg{Type: signal.TypeOffer, SDP: "OFFER_SDP"}); err != nil {
		t.Fatal(err)
	}
	got := readMsg(t, daemon)
	if got.Type != signal.TypeOffer || got.SDP != "OFFER_SDP" {
		t.Fatalf("daemon got %+v, want offer OFFER_SDP", got)
	}

	// Daemon's signed answer is relayed to the client.
	if err := daemon.WriteJSON(signal.Msg{Type: signal.TypeAnswer, SDP: "ANSWER_SDP", Sig: "SIG=="}); err != nil {
		t.Fatal(err)
	}
	got = readMsg(t, client)
	if got.Type != signal.TypeAnswer || got.SDP != "ANSWER_SDP" || got.Sig != "SIG==" {
		t.Fatalf("client got %+v, want signed answer", got)
	}
}

func TestSubscriberGetsTURN(t *testing.T) {
	srv := newTestServer(true)
	defer srv.Close()

	daemon := dial(t, srv)
	defer daemon.Close()
	_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeRegister, Room: "tok", Role: signal.RoleDaemon, PubKey: "PK=="})

	client := dial(t, srv)
	defer client.Close()
	_ = client.WriteJSON(signal.Msg{Type: signal.TypeJoin, Room: "tok", Role: signal.RoleClient})

	ice := readMsg(t, client)
	if ice.Type != signal.TypeICEServers || len(ice.ICEServers) != 2 {
		t.Fatalf("subscriber wants STUN+TURN (2 entries), got %+v", ice)
	}
	turn := ice.ICEServers[1]
	if len(turn.URLs) == 0 || !strings.HasPrefix(turn.URLs[0], "turn:") {
		t.Fatalf("want TURN url, got %+v", turn)
	}
	if turn.Username == "" || turn.Credential == "" {
		t.Fatalf("TURN entry missing ephemeral creds: %+v", turn)
	}
	// username = "<expiry>:<room>", expiry = injected now(1000) + ttl(60).
	if turn.Username != "1060:tok" {
		t.Fatalf("TURN username = %q, want 1060:tok", turn.Username)
	}
}

func TestJoinUnknownRoomErrors(t *testing.T) {
	srv := newTestServer(false)
	defer srv.Close()

	client := dial(t, srv)
	defer client.Close()
	_ = client.WriteJSON(signal.Msg{Type: signal.TypeJoin, Room: "nope", Role: signal.RoleClient})

	got := readMsg(t, client)
	if got.Type != signal.TypeError {
		t.Fatalf("joining a room with no daemon should error (rescan), got %+v", got)
	}
}

var _ = http.StatusOK // keep net/http import if unused after edits
