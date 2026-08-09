package signalbroker

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// Stats is an aggregate snapshot of broker activity. Aggregates ONLY — no
// daemonIDs, pubkeys or IPs — so the payload stays boring if it ever leaks.
//
// Gauges are counted live; totals are process-lifetime and reset on restart
// (the broker keeps no metrics store, and the updater restarts it on release).
type Stats struct {
	DaemonsOnline  int `json:"daemons_online"`  // registered daemons (Macs) present right now
	SessionsActive int `json:"sessions_active"` // daemons with a client in flight right now
	Watchers       int `json:"watchers"`        // presence subscribers right now

	JoinsTotal     int64 `json:"joins_total"`                // clients that asked to reach a daemon
	PairingsTotal  int64 `json:"pairing_attempts_total"`     // subset of joins carrying an enrollment secret
	ProofsOKTotal  int64 `json:"proofs_ok_total"`            // client key authenticated to the broker gate
	ProofsBadTotal int64 `json:"proofs_failed_total"`        // broker-challenge signature rejected
	TURNGranted    int64 `json:"turn_granted_total"`         // iceServers handed out WITH a relay
	OfflineTotal   int64 `json:"joins_daemon_offline_total"` // join for a daemon that was not present
}

// counters are the process-lifetime totals behind Stats.
type counters struct {
	joins, pairings, proofsOK, proofsBad, turnGranted, offline atomic.Int64
}

// Stats snapshots the gauges and totals.
//
// The daemon pointers are collected under b.mu and inspected after releasing it,
// so this never nests b.mu → d.mu (no new lock-order edge) and a stalled session
// cannot block registrations while we count. The cost is that a daemon dropping
// mid-scan may be counted stale by one sample — fine for a stats endpoint.
func (b *Broker) Stats() Stats {
	b.mu.Lock()
	ds := make([]*daemonConn, 0, len(b.daemons))
	for _, d := range b.daemons {
		ds = append(ds, d)
	}
	s := Stats{DaemonsOnline: len(b.daemons), Watchers: len(b.watchers)}
	b.mu.Unlock()

	for _, d := range ds {
		d.mu.Lock()
		if d.client != nil {
			s.SessionsActive++
		}
		d.mu.Unlock()
	}

	s.JoinsTotal = b.n.joins.Load()
	s.PairingsTotal = b.n.pairings.Load()
	s.ProofsOKTotal = b.n.proofsOK.Load()
	s.ProofsBadTotal = b.n.proofsBad.Load()
	s.TURNGranted = b.n.turnGranted.Load()
	s.OfflineTotal = b.n.offline.Load()
	return s
}

// StatsHandler serves Stats as JSON. It is deliberately NOT mounted on Handler():
// that mux sits behind the public reverse proxy. Serve this on a separate
// loopback-only listener (see --stats-addr) so it cannot be exposed by a
// Caddyfile edit.
func (b *Broker) StatsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(b.Stats())
	})
}
