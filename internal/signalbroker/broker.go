// Package signalbroker is the simcast signaling broker: a thin WSS rendezvous
// that pairs a daemon and a browser sharing a one-time pairing token, relays a
// single offer→answer between them, and hands each peer an iceServers config
// (STUN always; TURN only when the subscription stub grants it). Media never
// transits the broker — only the handshake does.
package signalbroker

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kei-sidorov/simcast/internal/signal"
)

// Config tunes ICE issuance and subscription gating.
type Config struct {
	STUNURLs   []string               // always handed out
	TURNURLs   []string               // handed out only when GrantTURN(room) is true
	TURNSecret string                 // coturn static-auth-secret (shared with coturn)
	TURNTTL    time.Duration          // ephemeral credential lifetime; 0 → 1 minute
	GrantTURN  func(room string) bool // subscription gate STUB (Phase 4 = real billing); nil → deny
	Now        func() time.Time       // injectable clock; nil → time.Now
}

// Broker holds the live rooms.
type Broker struct {
	cfg   Config
	up    websocket.Upgrader
	mu    sync.Mutex
	rooms map[string]*room
}

// room holds the two sides of one pairing. A connection is wrapped so writes
// are serialized (gorilla forbids concurrent writers on one conn).
type room struct {
	daemon *conn
	client *conn
}

// conn serializes writes to one websocket.
type conn struct {
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (c *conn) send(m signal.Msg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteJSON(m)
}

// New builds a Broker with sane defaults for the optional Config fields.
func New(cfg Config) *Broker {
	if cfg.TURNTTL == 0 {
		cfg.TURNTTL = time.Minute
	}
	if cfg.GrantTURN == nil {
		cfg.GrantTURN = func(string) bool { return false }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Broker{
		cfg:   cfg,
		up:    websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		rooms: map[string]*room{},
	}
}

// Handler serves the broker at /ws.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", b.handleWS)
	return mux
}

// iceServers builds the config for a room: STUN always, TURN if granted.
func (b *Broker) iceServers(roomID string) []signal.ICEServer {
	out := []signal.ICEServer{{URLs: b.cfg.STUNURLs}}
	if b.cfg.GrantTURN(roomID) && len(b.cfg.TURNURLs) > 0 {
		cred := signal.MakeTURNCredential(b.cfg.TURNSecret, roomID, b.cfg.Now(), b.cfg.TURNTTL)
		out = append(out, signal.ICEServer{
			URLs:       b.cfg.TURNURLs,
			Username:   cred.Username,
			Credential: cred.Credential,
		})
	}
	return out
}

func (b *Broker) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := b.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{ws: ws}
	defer ws.Close()

	// First message must be register (daemon) or join (client); it binds this
	// connection to a room.
	var first signal.Msg
	if err := ws.ReadJSON(&first); err != nil {
		return
	}
	switch first.Type {
	case signal.TypeRegister:
		b.serveDaemon(c, first)
	case signal.TypeJoin:
		b.serveClient(c, first)
	default:
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "first message must be register or join"})
	}
}

// serveDaemon claims a room and relays the client's offer to the daemon until
// the connection drops.
func (b *Broker) serveDaemon(c *conn, reg signal.Msg) {
	b.mu.Lock()
	rm := b.rooms[reg.Room]
	if rm == nil {
		rm = &room{}
		b.rooms[reg.Room] = rm
	}
	// One-shot pairing: a duplicate register for the same token simply
	// overwrites the slot. dropRoom uses connection identity, so the stale
	// connection's deferred cleanup will not clobber this one.
	rm.daemon = c
	b.mu.Unlock()

	defer b.dropRoom(reg.Room, c)

	for {
		var m signal.Msg
		if err := c.ws.ReadJSON(&m); err != nil {
			return
		}
		// Daemon → client: the signed answer (and nothing else needs relaying).
		if m.Type == signal.TypeAnswer {
			b.relay(reg.Room, signal.RoleDaemon, m)
		}
	}
}

// serveClient enters a room that a daemon must already hold, receives iceServers,
// and relays its offer to the daemon.
func (b *Broker) serveClient(c *conn, join signal.Msg) {
	b.mu.Lock()
	rm := b.rooms[join.Room]
	if rm == nil || rm.daemon == nil {
		b.mu.Unlock()
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "no daemon for this token — rescan/repair"})
		return
	}
	// One-shot pairing: a second client on the same token overwrites the slot
	// (no "room full" rejection by design).
	rm.client = c
	b.mu.Unlock()

	defer b.dropRoom(join.Room, c)

	// Hand both peers their ICE configuration (subscription-gated TURN). The
	// daemon needs matching servers to gather srflx/relay candidates too.
	ice := b.iceServers(join.Room)
	_ = c.send(signal.Msg{Type: signal.TypeICEServers, ICEServers: ice})
	b.mu.Lock()
	dmn := rm.daemon
	b.mu.Unlock()
	if dmn != nil {
		_ = dmn.send(signal.Msg{Type: signal.TypeICEServers, ICEServers: ice})
	}

	for {
		var m signal.Msg
		if err := c.ws.ReadJSON(&m); err != nil {
			return
		}
		if m.Type == signal.TypeOffer {
			b.relay(join.Room, signal.RoleClient, m)
		}
	}
}

// relay forwards m to the *other* side of the room.
func (b *Broker) relay(roomID, from string, m signal.Msg) {
	b.mu.Lock()
	rm := b.rooms[roomID]
	var dst *conn
	if rm != nil {
		if from == signal.RoleClient {
			dst = rm.daemon
		} else {
			dst = rm.client
		}
	}
	b.mu.Unlock()
	if dst != nil {
		_ = dst.send(m)
	}
}

// dropRoom removes c from its room and notifies the peer it left. The room is
// deleted once empty.
func (b *Broker) dropRoom(roomID string, c *conn) {
	b.mu.Lock()
	rm := b.rooms[roomID]
	if rm == nil {
		b.mu.Unlock()
		return
	}
	var peer *conn
	if rm.daemon == c {
		rm.daemon = nil
		peer = rm.client
	} else if rm.client == c {
		rm.client = nil
		peer = rm.daemon
	}
	if rm.daemon == nil && rm.client == nil {
		delete(b.rooms, roomID)
	}
	b.mu.Unlock()
	if peer != nil {
		_ = peer.send(signal.Msg{Type: signal.TypePeerLeft})
	}
}
