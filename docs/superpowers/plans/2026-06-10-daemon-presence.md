# Live Daemon Presence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show a live online/offline dot next to each paired Mac that updates in real time when the Mac sleeps/wakes.

**Architecture:** Add a lightweight `watch`/`presence` WS channel to the broker, independent of the heavy `join` streaming handshake. A client sends the list of `daemonID`s it cares about; the broker replies with a snapshot of their online states and pushes one-key deltas as daemons register/drop. "Daemon online" ≡ "its WS to the broker is alive" — the broker already tracks this in `b.daemons`; we expose it. Ping/pong keepalive on daemon conns detects a half-open TCP from a hard Mac sleep that a clean close would not. The `web/debug` page is the reference consumer.

**Tech Stack:** Go (`gorilla/websocket`), the existing `internal/signal` wire types and `internal/signalbroker` rendezvous, vanilla JS in `web/debug/index.html`.

**Spec:** `docs/superpowers/specs/2026-06-10-daemon-presence-design.md`

---

## File Structure

- `internal/signal/message.go` — add `TypeWatch`/`TypePresence` constants + `Daemons`/`States` fields on `Msg`.
- `internal/signal/message_test.go` — JSON round-trip for the new fields.
- `internal/signalbroker/presence.go` — **new**: `watcher` type, `serveWatcher`, `notifyPresence`, `keepalive`, presence write helper, constants.
- `internal/signalbroker/broker.go` — `watchers` field on `Broker`, init in `New`, `TypeWatch` dispatch in `handleWS`, `notifyPresence`/`keepalive` hooks in `serveDaemon`.
- `internal/signalbroker/presence_test.go` — **new**: the three protocol scenarios.
- `web/debug/index.html` — presence WS, `presenceState`, dots in `renderMacs`, re-subscribe on `saveMac`/`init`.
- `docs/decisions.md` — one decision row.

The daemon (`internal/server/remote.go`) is **not** modified: gorilla auto-replies pong to ping inside the `ReadJSON` loop that `serveOnce` already runs continuously.

---

## Task 1: Wire protocol — `watch` / `presence` types and fields

**Files:**
- Modify: `internal/signal/message.go`
- Test: `internal/signal/message_test.go`

- [ ] **Step 1: Add a failing round-trip test**

Add this function to `internal/signal/message_test.go`:

```go
func TestMsg_PresenceFieldsRoundTrip(t *testing.T) {
	in := Msg{
		Type:    TypePresence,
		Daemons: []string{"A==", "B=="},
		States:  map[string]bool{"A==": true, "B==": false},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Msg
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
	if TypeWatch == TypePresence {
		t.Fatalf("presence type constants collide")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails to compile**

Run: `go test ./internal/signal/ -run TestMsg_PresenceFieldsRoundTrip`
Expected: FAIL — `undefined: TypePresence`, `undefined: TypeWatch`, `Msg has no field Daemons/States`.

- [ ] **Step 3: Add the message-type constants**

In `internal/signal/message.go`, after the 3C handshake `const (...)` block (the one ending with `TypeProof`), add:

```go
// Live presence: a lightweight channel, independent of join, that streams
// daemon online/offline to subscribed clients.
const (
	TypeWatch    = "watch"    // client → broker (first message): observe a list of daemonIDs
	TypePresence = "presence" // broker → client: snapshot or one-key delta of online state
)
```

- [ ] **Step 4: Add the `Msg` fields**

In `internal/signal/message.go`, inside the `Msg` struct, after the `BrokerSig` field, add:

```go
	Daemons     []string        `json:"daemons,omitempty"`     // watch: daemonIDs to observe
	States      map[string]bool `json:"states,omitempty"`      // presence: daemonID → online (snapshot or delta)
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/signal/`
Expected: PASS (both `TestMsg_NewFieldsRoundTrip` and `TestMsg_PresenceFieldsRoundTrip`).

- [ ] **Step 6: Commit**

```bash
git add internal/signal/message.go internal/signal/message_test.go
git commit -m "feat(signal): add watch/presence wire types and fields"
```

---

## Task 2: Broker — watcher registry, presence channel, keepalive

**Files:**
- Create: `internal/signalbroker/presence.go`
- Modify: `internal/signalbroker/broker.go`
- Test: `internal/signalbroker/presence_test.go`

The tests speak only the WS protocol (no internal broker symbols), so the file compiles before the implementation exists and fails at runtime. They reuse `dial`/`readMsg`/`wsURL` from `broker_test.go` (same package).

- [ ] **Step 1: Write the three failing protocol tests**

Create `internal/signalbroker/presence_test.go`:

```go
package signalbroker

import (
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/kei-sidorov/simbeam/internal/signal"
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/signalbroker/ -run TestPresence`
Expected: FAIL — the broker's default branch answers `watch` with an error (`first message must be register or join`), so `readMsg` returns a `TypeError`, not `TypePresence`.

- [ ] **Step 3: Create `internal/signalbroker/presence.go`**

```go
package signalbroker

import (
	"time"

	"github.com/gorilla/websocket"

	"github.com/kei-sidorov/simbeam/internal/signal"
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
	_ = c.ws.SetWriteDeadline(time.Now().Add(presenceWriteTimeout))
	err := c.ws.WriteJSON(m)
	_ = c.ws.SetWriteDeadline(time.Time{})
	return err
}

// serveWatcher registers a watcher, emits its snapshot, then reads until close.
//
// Registration AND the snapshot write happen under the SAME b.mu that guards
// b.daemons. This closes two races at once:
//  1. a daemon registering between snapshot and registration would lose its
//     delta (the dot would stay stale forever);
//  2. a delta racing ahead of the snapshot on the socket could overwrite a fresh
//     true with a stale false.
// Holding b.mu across the snapshot write orders it strictly before any
// notifyPresence delta, because notifyPresence must re-acquire b.mu to find this
// watcher. The write is bounded by a deadline and the conn is freshly accepted,
// so the lock hold is brief.
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
	_ = c.sendPresence(signal.Msg{Type: signal.TypePresence, States: snap})
	b.mu.Unlock()

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
```

- [ ] **Step 4: Add the `watchers` field to `Broker` and init it**

In `internal/signalbroker/broker.go`, change the `Broker` struct:

```go
// Broker holds live daemon presence.
type Broker struct {
	cfg      Config
	up       websocket.Upgrader
	mu       sync.Mutex
	daemons  map[string]*daemonConn // daemonID → registered daemon
	watchers map[*watcher]struct{}  // presence subscribers (guarded by mu, same as daemons)
}
```

And in `New`, change the returned literal:

```go
	return &Broker{
		cfg:      cfg,
		up:       websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		daemons:  map[string]*daemonConn{},
		watchers: map[*watcher]struct{}{},
	}
```

- [ ] **Step 5: Dispatch `TypeWatch` in `handleWS`**

In `internal/signalbroker/broker.go`, add a case to the `switch first.Type` in `handleWS`:

```go
	switch first.Type {
	case signal.TypeRegister:
		b.serveDaemon(c, first)
	case signal.TypeJoin:
		b.serveClient(c, first)
	case signal.TypeWatch:
		b.serveWatcher(c, first)
	default:
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "first message must be register or join"})
	}
```

- [ ] **Step 6: Hook `notifyPresence` + `keepalive` into `serveDaemon`**

In `internal/signalbroker/broker.go`, replace this block of `serveDaemon`:

```go
	d := &daemonConn{c: c, id: id}
	b.mu.Lock()
	b.daemons[id] = d // a re-register (after reconnect) overwrites the stale slot
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if b.daemons[id] == d {
			delete(b.daemons, id)
		}
		b.mu.Unlock()
		d.mu.Lock()
		cl := d.client
		d.mu.Unlock()
		if cl != nil {
			_ = cl.c.send(signal.Msg{Type: signal.TypePeerLeft})
		}
	}()
```

with:

```go
	d := &daemonConn{c: c, id: id}
	b.mu.Lock()
	b.daemons[id] = d // a re-register (after reconnect) overwrites the stale slot
	b.mu.Unlock()
	b.notifyPresence(id, true)

	// Ping/pong liveness: catches a half-open TCP from a hard Mac sleep that a
	// clean close (handled by the read loop below) would not.
	stopKA := keepalive(c)
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
```

- [ ] **Step 7: Run the presence tests to verify they pass**

Run: `go test ./internal/signalbroker/ -run TestPresence -v`
Expected: PASS — all three (`...SnapshotReflectsRegisteredDaemon`, `...DeltaOnDaemonDisconnect`, `...NoGapSubscribeBeforeDaemon`).

- [ ] **Step 8: Run the full broker suite to verify no regression**

Run: `go test ./internal/signalbroker/`
Expected: PASS — the existing handshake/gate/displacement tests still pass (they read from the daemon conn, so gorilla auto-pongs the keepalive pings; all complete well inside the 25s read deadline).

- [ ] **Step 9: Commit**

```bash
git add internal/signalbroker/presence.go internal/signalbroker/broker.go internal/signalbroker/presence_test.go
git commit -m "feat(broker): live presence watch channel + daemon keepalive"
```

---

## Task 3: Web client — live online/offline dots

**Files:**
- Modify: `web/debug/index.html`

No automated test (vanilla JS in a single HTML file); verified manually in Step 5.

- [ ] **Step 1: Add the presence state + connection block**

In `web/debug/index.html`, immediately AFTER the `renderMacs()` function's closing brace (the line `}` that ends the function starting at `function renderMacs()`), insert:

```javascript
// ---- live presence (online/offline dots) ----
// A presence WS is separate from the streaming `sig` WS: the broker dispatches on
// the first message type (watch vs join), so both can target the same broker.
let presenceState = {};   // daemonID → bool; absent key = unknown
let presenceConns = [];   // [{ws, signal, ids, timer, closed, backoff}]

function presenceDot(daemon) {
  if (presenceState[daemon] === true) return '🟢';
  if (presenceState[daemon] === false) return '⚪';
  return '?'; // snapshot not yet received / link down
}

// subscribePresence tears down any existing presence WSs and opens one per
// distinct broker (grouping saved Macs by their `signal` URL — in practice one).
function subscribePresence() {
  presenceConns.forEach(pc => {
    pc.closed = true;
    clearTimeout(pc.timer);
    if (pc.ws) pc.ws.close();
  });
  presenceConns = [];

  const byBroker = {};
  loadMacs().forEach(m => { (byBroker[m.signal] = byBroker[m.signal] || []).push(m.daemon); });

  Object.keys(byBroker).forEach(signal => {
    const pc = {ws: null, signal, ids: byBroker[signal], timer: null, closed: false, backoff: 1000};
    presenceConns.push(pc);
    openPresence(pc);
  });
}

function openPresence(pc) {
  const ws = new WebSocket(pc.signal);
  pc.ws = ws;
  ws.onopen = () => {
    pc.backoff = 1000;
    ws.send(JSON.stringify({type: 'watch', daemons: pc.ids}));
  };
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if (m.type === 'presence' && m.states) {
      Object.assign(presenceState, m.states); // merge snapshot or one-key delta
      renderMacs();
    }
  };
  ws.onclose = () => {
    if (pc.closed) return; // intentional teardown by subscribePresence
    pc.ids.forEach(id => { delete presenceState[id]; }); // dots → '?' until reconnect
    renderMacs();
    pc.timer = setTimeout(() => openPresence(pc), pc.backoff);
    pc.backoff = Math.min(pc.backoff * 2, 30000);
  };
  ws.onerror = () => { /* onclose follows; reconnect handled there */ };
}
```

- [ ] **Step 2: Render the dot in `renderMacs`**

In `web/debug/index.html`, inside `renderMacs()`, change the button label line:

```javascript
    b.textContent = `${m.name || 'Mac'} (${m.daemon.slice(0, 8)}…)`;
```

to:

```javascript
    b.textContent = `${presenceDot(m.daemon)} ${m.name || 'Mac'} (${m.daemon.slice(0, 8)}…)`;
```

- [ ] **Step 3: Re-subscribe when the Mac list changes (`saveMac`)**

In `web/debug/index.html`, in `saveMac`, change:

```javascript
  localStorage.setItem('simbeam_macs', JSON.stringify(macs));
  renderMacs();
}
```

to:

```javascript
  localStorage.setItem('simbeam_macs', JSON.stringify(macs));
  renderMacs();
  subscribePresence(); // re-watch with the updated daemon list
}
```

- [ ] **Step 4: Subscribe on page load (`init`)**

In `web/debug/index.html`, in the `init` IIFE, change:

```javascript
  await loadOrCreateIdentity();
  renderIdentity();
  renderMacs();
```

to:

```javascript
  await loadOrCreateIdentity();
  renderIdentity();
  renderMacs();
  subscribePresence(); // light up dots for already-paired Macs immediately
```

- [ ] **Step 5: Manual verification**

```bash
# Terminal A: broker
go run ./cmd/simbeam-signal   # or the project's run target; note its ws URL

# Terminal B: daemon serving the debug page
go run ./cmd/simbeamd serve --web
```

Open `http://localhost:8080/`, pair a Mac (press `P` in `simbeamd`, open the URL).
Expected:
- After pairing, the Mac button shows a 🟢 dot.
- Stop the daemon (Ctrl-C in Terminal B) → within ~1s the dot turns ⚪ (clean close).
- Restart the daemon → dot returns to 🟢 (delta on re-register).
- Stop the broker → dot turns `?` (link down); restart broker → dot resolves from the snapshot.

- [ ] **Step 6: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(web): live online/offline presence dots per Mac"
```

---

## Task 4: Record the decision

**Files:**
- Modify: `docs/decisions.md`

- [ ] **Step 1: Append the decision row**

In `docs/decisions.md`, add a new row after row `75` (match the existing table format):

```markdown
| 76 | **Live presence демонов** — отдельный WS-канал `watch`/`presence` на брокере, не связанный с `join`: клиент шлёт список `daemonID`, брокер отдаёт снапшот `states:map[daemonID]bool` и одно-ключевые дельты при регистрации/отвале демона. «Демон онлайн» ≡ «его WS к брокеру жив» (`b.daemons`) — источник правды уже есть, выставляем наружу. `watchers` под **тем же** `b.mu`, что и `daemons`; **снапшот пишется под `b.mu`** (с write-deadline) — это строго упорядочивает его перед любой дельтой `notifyPresence` (она перезахватывает `b.mu`), закрывая и потерю дельты, и переворот «дельта обогнала снапшот» (усиление спека, где это покрывалось лишь «идемпотентным мёржем»). Keepalive ping/pong на conn'ах демонов (10s ping / 25s read-deadline) ловит half-open TCP при жёстком сне Mac; чистый выход рвёт WS и детектится мгновенно. **Демон не меняем** (gorilla авто-pong в `ReadJSON`-цикле `serveOnce`). Доступ по `daemonID` (публичный ключ), без отдельной auth — совпадает с моделью untrusted-брокера (#55). Веб-дебаг = референс-потребитель (🟢/⚪/?) | живой индикатор online/offline без тяжёлого `join`-рукопожатия; presence де-факто уже был виден только в момент полной попытки стрима — вынесли в лёгкий канал |
```

- [ ] **Step 2: Verify the table renders (no broken pipes)**

Run: `git diff docs/decisions.md`
Expected: a single added row, pipe-delimited, consistent with rows 74/75.

- [ ] **Step 3: Commit**

```bash
git add docs/decisions.md
git commit -m "docs(decisions): record live daemon presence channel"
```

---

## Self-Review Notes

- **Spec coverage:** wire types (Task 1) ✓; broker watcher registry + `serveWatcher` + `notifyPresence` + keepalive (Task 2) ✓; single-mutex gap closure + the three test scenarios (Task 2) ✓; web presence WS, `presenceState`, dots, reconnect-to-`?`, re-subscribe on `saveMac` (Task 3) ✓; `decisions.md` row (Task 4) ✓. Edge cases from the spec (empty `watch` → client opens no WS; unknown `daemonID` → `false` in snapshot; multiple tabs → independent watchers) are covered by the implemented behavior.
- **Deliberate strengthening:** the spec leaves snapshot/delta ordering to "idempotent merge"; this plan writes the snapshot under `b.mu` to make ordering deterministic (and test 3 reliable). Documented in the `decisions.md` row.
- **Out of scope (unchanged from spec):** HTTP polling `GET /v1/presence`; symmetric daemon→broker keepalive; the native client consumer; presence authentication.
- **Type consistency:** `States map[string]bool` / `Daemons []string` used identically across `message.go`, broker, and tests; `presenceDot`/`subscribePresence`/`openPresence` names consistent across the web edits.
