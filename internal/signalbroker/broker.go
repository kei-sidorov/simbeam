// Package signalbroker is the simbeam signaling broker: a thin WSS rendezvous.
// A daemon registers persistently under its daemonID (its Ed25519 pubkey) and
// stays present; a client is routed to it by daemonID. The broker relays a
// mutual challenge-response (it authenticates only the client KEY, for the TURN
// gate — connection access is decided by the endpoints themselves, peer-pinning,
// broker untrusted), then relays one offer→answer. It hands each peer an
// iceServers config (STUN always; TURN only when the client's subscription is
// active in Store). Media never transits the broker. It also serves the
// subscription HTTP API (subscription.go).
package signalbroker

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/kei-sidorov/simbeam/internal/store"
)

// Config tunes ICE issuance, the subscription gate, and the subscription API.
type Config struct {
	STUNURLs   []string      // always handed out
	TURNURLs   []string      // handed out only when the client's subscription is active
	TURNSecret string        // coturn static-auth-secret (shared with coturn)
	TURNTTL    time.Duration // ephemeral credential lifetime; 0 → 1 minute
	Store      store.Store   // subscription gate + /v1/subscription persistence; nil → no TURN, API 503
	AppSecret  string        // SIMCAST_APP_SECRET: the weak app-sig barrier on the subscription API
	TURNOpen   bool          // TEMP: hand TURN to every authenticated client, bypassing the subscription gate (use while there are no subscriptions yet)
	Now        func() time.Time
}

// Broker holds live daemon presence.
type Broker struct {
	cfg      Config
	up       websocket.Upgrader
	mu       sync.Mutex
	daemons  map[string]*daemonConn // daemonID → registered daemon
	watchers map[*watcher]struct{}  // presence subscribers (guarded by mu, same as daemons)
}

// conn serializes writes to one websocket (gorilla forbids concurrent writers).
type conn struct {
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (c *conn) send(m signal.Msg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteJSON(m)
}

// daemonConn is a registered daemon plus its current (single) client session.
type daemonConn struct {
	c      *conn
	id     string
	mu     sync.Mutex
	client *clientConn
}

// clientConn is the in-flight client for a daemon, with the broker's gate nonce.
type clientConn struct {
	c      *conn
	pubKey string
	bNonce string
}

// New builds a Broker with sane defaults for the optional Config fields.
func New(cfg Config) *Broker {
	if cfg.TURNTTL == 0 {
		cfg.TURNTTL = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Broker{
		cfg:      cfg,
		up:       websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		daemons:  map[string]*daemonConn{},
		watchers: map[*watcher]struct{}{},
	}
}

// Handler serves the broker WS at /ws and the subscription API at /v1/...
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", b.handleWS)
	mux.HandleFunc("/v1/subscription", b.handleSubscription)
	mux.HandleFunc("/v1/subscription/me", b.handleSubscriptionMe)
	return mux
}

// iceServers builds the config for an authenticated client: STUN always, TURN
// only when the client's subscription is active in Store. The TURN credential
// userID is the verified client pubkey (decouples it from any room/token).
func (b *Broker) iceServers(clientPubKey string) []signal.ICEServer {
	out := []signal.ICEServer{{URLs: b.cfg.STUNURLs}}
	granted := false
	if len(b.cfg.TURNURLs) > 0 {
		switch {
		case b.cfg.TURNOpen:
			// Subscription gate disabled: hand TURN to every authenticated client.
			// Temporary — flip off once real subscriptions gate relay access.
			granted = true
		case b.cfg.Store != nil:
			ok, err := b.cfg.Store.Active(context.Background(), clientPubKey, b.cfg.Now())
			if err != nil {
				log.Printf("signalbroker: subscription gate lookup failed: %v", err)
			}
			granted = err == nil && ok
		}
	}
	if granted {
		cred := signal.MakeTURNCredential(b.cfg.TURNSecret, clientPubKey, b.cfg.Now(), b.cfg.TURNTTL)
		out = append(out, signal.ICEServer{URLs: b.cfg.TURNURLs, Username: cred.Username, Credential: cred.Credential})
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

	var first signal.Msg
	if err := ws.ReadJSON(&first); err != nil {
		return
	}
	switch first.Type {
	case signal.TypeRegister:
		b.serveDaemon(c, first)
	case signal.TypeJoin:
		b.serveClient(c, first)
	case signal.TypeWatch:
		b.serveWatcher(c, first)
	default:
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "first message must be register, join, or watch"})
	}
}

// serveDaemon keeps a daemon present under its daemonID and relays daemon→client
// messages (challenge/answer/error) to whichever client is currently in flight.
func (b *Broker) serveDaemon(c *conn, reg signal.Msg) {
	id := reg.Daemon
	if id == "" {
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "register missing daemon id"})
		return
	}
	d := &daemonConn{c: c, id: id}
	b.mu.Lock()
	b.daemons[id] = d // a re-register (after reconnect) overwrites the stale slot
	b.mu.Unlock()
	b.notifyPresence(id, true)

	// Ping/pong liveness: catches a half-open TCP from a hard Mac sleep that a
	// clean close (handled by the read loop below) would not.
	stopKA := keepalive(c, presencePingInterval)
	defer stopKA()

	defer func() {
		b.mu.Lock()
		removed := b.daemons[id] == d
		if removed {
			delete(b.daemons, id)
		}
		b.mu.Unlock()
		if removed { // a re-register stole the slot → its goroutine owns presence
			b.notifyPresence(id, false)
		}
		d.mu.Lock()
		cl := d.client
		d.mu.Unlock()
		if cl != nil {
			_ = cl.c.send(signal.Msg{Type: signal.TypePeerLeft})
		}
	}()

	for {
		var m signal.Msg
		if err := d.c.ws.ReadJSON(&m); err != nil {
			return
		}
		d.mu.Lock()
		cl := d.client
		d.mu.Unlock()
		if cl == nil {
			continue
		}
		switch m.Type {
		case signal.TypeChallenge:
			// Attach the broker's own gate nonce before forwarding to the client.
			_ = cl.c.send(signal.Msg{Type: signal.TypeChallenge, Nonce: m.Nonce, BrokerNonce: cl.bNonce})
		case signal.TypeAnswer, signal.TypeError, signal.TypePeerLeft:
			_ = cl.c.send(m)
		}
	}
}

// serveClient routes a client to its daemon and relays the handshake.
func (b *Broker) serveClient(c *conn, join signal.Msg) {
	if join.Daemon == "" || join.PubKey == "" {
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "join missing daemon or pubkey"})
		return
	}
	b.mu.Lock()
	d := b.daemons[join.Daemon]
	b.mu.Unlock()
	if d == nil {
		_ = c.send(signal.Msg{Type: signal.TypeError, Code: signal.CodeOffline, Msg: "device offline — wake your Mac"})
		return
	}

	bNonce, err := signal.NewNonce()
	if err != nil {
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "broker nonce error"})
		return
	}
	cl := &clientConn{c: c, pubKey: join.PubKey, bNonce: bNonce}
	d.mu.Lock()
	old := d.client
	d.client = cl // one client at a time; a new client replaces the previous
	d.mu.Unlock()
	if old != nil {
		// Stop the displaced client's goroutine so it can't write its
		// proof/offer into this new client's session on the shared daemon conn.
		_ = old.c.ws.Close()
	}

	// Keep the client socket warm through the long non-trickle ICE-gather window
	// (proof → offer sits idle for seconds; see clientPingInterval). Without this
	// an aggressive client-side NAT/idle timer can drop the socket before the
	// offer is sent, surfacing as a socket error on the client. Fast ping so a
	// ping lands inside the gather window; clients auto-pong.
	stopKA := keepalive(c, clientPingInterval)
	defer stopKA()

	defer func() {
		d.mu.Lock()
		mine := d.client == cl
		if mine {
			d.client = nil
		}
		d.mu.Unlock()
		// Only the CURRENT client tells the daemon its peer left; a displaced
		// client must not tear down the live session.
		if mine {
			_ = d.c.send(signal.Msg{Type: signal.TypePeerLeft})
		}
	}()

	// Ask the daemon to start the challenge (carry enrollment proof if present).
	_ = d.c.send(signal.Msg{Type: signal.TypeConnect, PubKey: join.PubKey, Nonce: join.Nonce, Pair: join.Pair})

	for {
		var m signal.Msg
		if err := c.ws.ReadJSON(&m); err != nil {
			return
		}
		d.mu.Lock()
		mine := d.client == cl
		d.mu.Unlock()
		if !mine {
			return // displaced by a newer client; stop relaying
		}
		switch m.Type {
		case signal.TypeProof:
			// Verify the broker-gate signature over bNonce: authenticates the
			// client KEY so the TURN gate can trust the subscription lookup.
			if !signal.Verify(cl.pubKey, []byte(cl.bNonce), m.BrokerSig) {
				_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "broker challenge failed"})
				return
			}
			ice := b.iceServers(cl.pubKey)
			_ = c.send(signal.Msg{Type: signal.TypeICEServers, ICEServers: ice})
			_ = d.c.send(signal.Msg{Type: signal.TypeICEServers, ICEServers: ice})
			// Relay the peer proof to the daemon (brokerSig stripped — the daemon
			// only cares about Sig over its own nonce + its pinned set).
			_ = d.c.send(signal.Msg{Type: signal.TypeProof, Sig: m.Sig})
		case signal.TypeOffer:
			_ = d.c.send(m)
		}
	}
}
