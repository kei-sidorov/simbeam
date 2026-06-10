package signalbroker

import (
	"time"

	"github.com/gorilla/websocket"

	"github.com/kei-sidorov/simcast/internal/signal"
)

// Keepalive tuning. A clean daemon exit (Ctrl-C, brew stop, crash) closes the WS
// and the read loop errors at once; ping/pong only adds detection of a half-open
// TCP from a hard Mac sleep. Ping every 10s, read deadline 25s → hard sleep
// detected in ~10–25s.
const (
	presencePingInterval = 10 * time.Second
	presenceReadTimeout  = 25 * time.Second
	presenceWriteTimeout = 5 * time.Second
)

// watcher is one client observing a set of daemonIDs over a presence WS.
type watcher struct {
	c   *conn
	ids map[string]bool // daemonIDs this watcher tracks
}

// sendPresence writes a snapshot/delta bounded by a deadline so a dead watcher
// cannot wedge the goroutine (daemon or broker mutex holder) that pushes to it.
func (c *conn) sendPresence(m signal.Msg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writePresenceLocked(m)
}

// writePresenceLocked writes a presence frame bounded by a deadline; the caller
// must already hold c.wmu (serveWatcher holds it across the b.mu release to order
// the snapshot ahead of any delta without pinning the global lock during I/O).
func (c *conn) writePresenceLocked(m signal.Msg) error {
	_ = c.ws.SetWriteDeadline(time.Now().Add(presenceWriteTimeout))
	err := c.ws.WriteJSON(m)
	_ = c.ws.SetWriteDeadline(time.Time{})
	return err
}

// serveWatcher registers a watcher, emits its snapshot, then reads until close.
//
// Registration and the snapshot computation happen under the SAME b.mu that
// guards b.daemons. This closes two races at once:
//  1. a daemon registering between snapshot and registration would lose its
//     delta (the dot would stay stale forever);
//  2. a delta racing ahead of the snapshot on the socket could overwrite a fresh
//     true with a stale false.
//
// We take this conn's write lock (c.wmu) BEFORE releasing b.mu, then send the
// snapshot after the release. That orders the snapshot strictly before any
// notifyPresence delta — notifyPresence can only target this watcher after it
// scans b.watchers (populated under b.mu), and its delta send blocks on the same
// c.wmu until the snapshot write completes — WITHOUT holding the global b.mu
// across the network write. (Holding b.mu over a write would let one stalled
// watcher freeze every daemon register/drop and join broker-wide.) c.wmu is
// uncontended here: the conn is brand new and only reachable via b.watchers,
// which we just populated under the lock we still hold.
func (b *Broker) serveWatcher(c *conn, first signal.Msg) {
	w := &watcher{c: c, ids: make(map[string]bool, len(first.Daemons))}
	for _, id := range first.Daemons {
		w.ids[id] = true
	}

	b.mu.Lock()
	b.watchers[w] = struct{}{}
	snap := make(map[string]bool, len(w.ids))
	for id := range w.ids {
		snap[id] = b.daemons[id] != nil
	}
	c.wmu.Lock()
	b.mu.Unlock()
	_ = c.writePresenceLocked(signal.Msg{Type: signal.TypePresence, States: snap})
	c.wmu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.watchers, w)
		b.mu.Unlock()
	}()

	stop := keepalive(c)
	defer stop()

	// Watchers send nothing after `watch`; read only to detect the close.
	for {
		var m signal.Msg
		if err := c.ws.ReadJSON(&m); err != nil {
			return
		}
	}
}

// notifyPresence pushes a one-key delta to every watcher tracking id. Targets are
// collected under b.mu, then written after the lock is released — a dead watcher
// must not wedge the daemon goroutine that calls this, and each write is bounded
// by a deadline. Ordering vs. the snapshot is guaranteed because the snapshot is
// written while b.mu is held (see serveWatcher).
func (b *Broker) notifyPresence(id string, online bool) {
	b.mu.Lock()
	var targets []*conn
	for w := range b.watchers {
		if w.ids[id] {
			targets = append(targets, w.c)
		}
	}
	b.mu.Unlock()

	msg := signal.Msg{Type: signal.TypePresence, States: map[string]bool{id: online}}
	for _, c := range targets {
		_ = c.sendPresence(msg)
	}
}

// keepalive arms ping/pong liveness on a long-lived conn: it sets a read deadline
// that each pong extends, and pings on an interval. It detects a half-open TCP
// (hard Mac sleep) that a clean close would not. Returns a stop func to halt the
// pinger; call it when the read loop exits.
func keepalive(c *conn) (stop func()) {
	_ = c.ws.SetReadDeadline(time.Now().Add(presenceReadTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(presenceReadTimeout))
	})
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(presencePingInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c.wmu.Lock()
				err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(presenceWriteTimeout))
				c.wmu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	return func() { close(done) }
}
