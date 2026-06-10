package signalbroker

import (
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/kei-sidorov/simcast/internal/signal"
)

func watch(t *testing.T, c *websocket.Conn, ids ...string) {
	t.Helper()
	if err := c.WriteJSON(signal.Msg{Type: signal.TypeWatch, Daemons: ids}); err != nil {
		t.Fatalf("write watch: %v", err)
	}
}

func registerDaemon(t *testing.T, c *websocket.Conn, id string) {
	t.Helper()
	if err := c.WriteJSON(signal.Msg{Type: signal.TypeRegister, Role: signal.RoleDaemon, Daemon: id}); err != nil {
		t.Fatalf("write register: %v", err)
	}
}

// A fresh watcher must see an already-registered daemon as online in its
// SNAPSHOT. We synchronize on a prior watcher's delta to guarantee the daemon is
// in b.daemons before the second watcher subscribes.
func TestPresenceSnapshotReflectsRegisteredDaemon(t *testing.T) {
	b := New(Config{STUNURLs: []string{"stun:x"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := wsURL(t, srv)

	w1 := dial(t, url)
	watch(t, w1, "D")
	if m := readMsg(t, w1); m.Type != signal.TypePresence || m.States["D"] != false {
		t.Fatalf("want snapshot {D:false}, got %+v", m)
	}

	daemon := dial(t, url)
	registerDaemon(t, daemon, "D")
	if m := readMsg(t, w1); m.Type != signal.TypePresence || m.States["D"] != true {
		t.Fatalf("want delta {D:true}, got %+v", m)
	}

	w2 := dial(t, url)
	watch(t, w2, "D")
	if m := readMsg(t, w2); m.Type != signal.TypePresence || m.States["D"] != true {
		t.Fatalf("want snapshot {D:true}, got %+v", m)
	}
}

// A daemon dropping its WS produces an offline delta to watchers.
func TestPresenceDeltaOnDaemonDisconnect(t *testing.T) {
	b := New(Config{STUNURLs: []string{"stun:x"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := wsURL(t, srv)

	w := dial(t, url)
	watch(t, w, "D")
	_ = readMsg(t, w) // snapshot {D:false}

	daemon := dial(t, url)
	registerDaemon(t, daemon, "D")
	if m := readMsg(t, w); m.States["D"] != true {
		t.Fatalf("want delta {D:true}, got %+v", m)
	}

	_ = daemon.Close() // broker detects the closed WS → offline delta
	if m := readMsg(t, w); m.Type != signal.TypePresence || m.States["D"] != false {
		t.Fatalf("want delta {D:false}, got %+v", m)
	}
}

// No gap: a watcher subscribing BEFORE the daemon gets a false snapshot, then the
// true delta when the daemon registers — the delta is never lost.
func TestPresenceNoGapSubscribeBeforeDaemon(t *testing.T) {
	b := New(Config{STUNURLs: []string{"stun:x"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := wsURL(t, srv)

	w := dial(t, url)
	watch(t, w, "D")
	if m := readMsg(t, w); m.Type != signal.TypePresence || m.States["D"] != false {
		t.Fatalf("want snapshot {D:false}, got %+v", m)
	}

	daemon := dial(t, url)
	registerDaemon(t, daemon, "D")
	if m := readMsg(t, w); m.Type != signal.TypePresence || m.States["D"] != true {
		t.Fatalf("want delta {D:true}, got %+v", m)
	}
}
