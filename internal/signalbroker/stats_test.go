package signalbroker

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

// waitFor polls Stats until ok holds: registration and teardown happen on the
// broker's own goroutines, so the numbers land shortly after the write returns.
func waitFor(t *testing.T, b *Broker, ok func(Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok(b.Stats()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stats never satisfied the condition; last: %+v", b.Stats())
}

// TestStatsCountsSessionsAndPairings drives real joins through the broker and
// asserts the gauges follow presence and the totals follow what actually
// happened — including that a pairing attempt is counted from join.Pair and that
// a bad broker-challenge signature lands in proofs_failed, not proofs_ok.
func TestStatsCountsSessionsAndPairings(t *testing.T) {
	b := New(Config{STUNURLs: []string{"stun:x"}})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := wsURL(t, srv)

	if s := b.Stats(); s.DaemonsOnline != 0 || s.SessionsActive != 0 {
		t.Fatalf("fresh broker should be empty, got %+v", s)
	}

	// A join for an absent daemon: counted as a join and as offline, no session.
	dead := dial(t, url)
	_ = dead.WriteJSON(signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: "nope", PubKey: "pub"})
	readMsg(t, dead) // offline error
	if s := b.Stats(); s.JoinsTotal != 1 || s.OfflineTotal != 1 || s.SessionsActive != 0 {
		t.Fatalf("offline join: want joins=1 offline=1 sessions=0, got %+v", s)
	}

	// Daemon registers → gauge follows presence.
	daemon := dial(t, url)
	_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeRegister, Role: signal.RoleDaemon, Daemon: "D"})
	waitFor(t, b, func(s Stats) bool { return s.DaemonsOnline == 1 })

	// Client joins carrying an enrollment secret → session + pairing attempt.
	clientPub, clientPriv, err := signal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	client := dial(t, url)
	_ = client.WriteJSON(signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: "D", PubKey: clientPub, Pair: "SECRET"})
	readMsg(t, daemon) // connect
	waitFor(t, b, func(s Stats) bool { return s.SessionsActive == 1 })
	if s := b.Stats(); s.PairingsTotal != 1 || s.JoinsTotal != 2 {
		t.Fatalf("want pairings=1 joins=2, got %+v", s)
	}

	// A bad brokerSig must count as a failed proof, never a good one.
	_ = daemon.WriteJSON(signal.Msg{Type: signal.TypeChallenge, Nonce: "N"})
	ch := readMsg(t, client)
	_ = client.WriteJSON(signal.Msg{
		Type:      signal.TypeProof,
		Sig:       signal.Sign(clientPriv, []byte(ch.Nonce)),
		BrokerSig: signal.Sign(clientPriv, []byte("wrong-nonce")),
	})
	readMsg(t, client) // rejection
	waitFor(t, b, func(s Stats) bool { return s.ProofsBadTotal == 1 })
	if s := b.Stats(); s.ProofsOKTotal != 0 || s.TURNGranted != 0 {
		t.Fatalf("bad proof must not count as ok/granted, got %+v", s)
	}

	// Session gauge drops when the client goes away; totals stay.
	_ = client.Close()
	waitFor(t, b, func(s Stats) bool { return s.SessionsActive == 0 })
	if s := b.Stats(); s.JoinsTotal != 2 {
		t.Fatalf("totals must not decay, got %+v", s)
	}

	// The handler serves the same numbers as Stats().
	rec := httptest.NewRecorder()
	b.StatsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))
	var got Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("handler json: %v (%s)", err, rec.Body)
	}
	if got.JoinsTotal != 2 || got.PairingsTotal != 1 {
		t.Fatalf("handler payload disagrees: %+v", got)
	}
}
