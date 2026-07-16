# Phase 3b — Signaling / Rendezvous Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a browser reach the daemon's simulator from anywhere by pairing through a thin Go WSS signaling broker (rooms keyed by `pairingToken`), with STUN open to all and TURN gated to "subscribers" via ephemeral HMAC credentials — reusing the unchanged 3a `rtc.Session` / `rtcDispatch` session mechanics.

**Architecture:** A new `cmd/simbeam-signal` broker relays one offer→answer between a daemon and a browser that share a `pairingToken`, and hands each peer an `iceServers` config (STUN always; TURN only when the subscription stub grants it). The daemon stops *accepting* WebRTC and starts *dialing out*: it registers a room on the broker, waits for the client's offer, answers it (reusing the 3a control-plane peer), and signs the answer with a per-session Ed25519 key so the browser can authenticate it (anti-MITM). **The signaling socket is handshake-only** — it carries exactly one offer/answer and then closes; the P2P connection (video track + control DataChannel) carries everything else, and disconnect detection stays on pion's `OnConnectionStateChange` (decision #39, #50). No SDP ever returns to signaling after pairing; `attach`/`detach` remain renegotiation-free (Option B, decision #50).

**Tech Stack:** Go 1.25, `gorilla/websocket` (broker server + daemon outbound dial — already the repo's WS lib, reused instead of pulling in `coder/websocket`), `crypto/ed25519` + `crypto/hmac`/`crypto/sha1` (stdlib), pion/webrtc v4 (existing `internal/rtc`), `coturn` (off-the-shelf, config-only — not built here), vanilla-JS debug client with WebCrypto Ed25519 verification.

---

## What is in scope vs deferred

**Validated locally (this plan, on localhost):** broker room/relay logic, TURN HMAC credential derivation, Ed25519 sign/verify, pairing-URL build/parse, the daemon's outbound dial + room registration, the browser's remote pairing path, `iceServers` plumbing into both peers, and the subscription-gating *decision logic* (stub). Localhost gathers only host candidates, so the **functional pairing flow** (token → broker-brokered handshake → control plane over the P2P peer → video on `attach`) is fully exercised end-to-end.

**Requires real deployment — explicitly NOT verified here, only built + flagged:** actual NAT traversal via `srflx`/`relay`, a running `coturn` relaying media, real cross-internet latency, and the `iceConnectionState === "failed"` → upsell path under a hostile symmetric NAT. Task 8 produces the `coturn` config + deploy notes; Task 9 marks each network scenario that needs a deployed broker/TURN as **deploy-only**.

**Out of scope (Phase 4, do not build):** real accounts/billing (subscription check stays a stub), QR rendering (browser uses a URL fragment), native iPad client (separate repo), broker production hardening (rate-limiting, room TTL eviction, horizontal store) beyond a noted stub, and trickle ICE (non-trickle full-SDP exchange is reused from 3a — see the ICE note below).

**ICE model (reused from decision #32, unchanged):** non-trickle. Each side gathers all candidates (`GatheringCompletePromise`) and embeds them in its single offer/answer SDP. This works with STUN/TURN too — it just waits for the full gather before sending. Trickle ICE would lower real-NAT setup latency but is a larger change; it is deferred and flagged in Task 9. Keeping non-trickle lets the daemon reuse `rtc.Session.Answer` verbatim.

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `internal/signal/message.go` | Signaling wire envelope (`Msg`) + `ICEServer` JSON shape + message-type constants. Shared by broker and daemon. | **Create** |
| `internal/signal/turn.go` | Ephemeral coturn REST credential derivation (HMAC-SHA1). | **Create** |
| `internal/signal/turn_test.go` | TURN credential unit tests. | **Create** |
| `internal/signal/auth.go` | Ed25519 keypair gen, detached sign, verify (base64 wire form). | **Create** |
| `internal/signal/auth_test.go` | Sign/verify unit tests. | **Create** |
| `internal/signal/pairing.go` | One-time token generation + pairing-URL builder (coordinates in URL fragment). | **Create** |
| `internal/signal/pairing_test.go` | Token + pairing-URL unit tests. | **Create** |
| `internal/signalbroker/broker.go` | The WSS broker: rooms by token, register/join, relay offer→answer, issue `iceServers` with subscription-gated TURN. | **Create** |
| `internal/signalbroker/broker_test.go` | Integration tests via `httptest` + two real WS clients. | **Create** |
| `cmd/simbeam-signal/main.go` | Thin entrypoint: flags (`--addr`, `--stun`, `--turn`, `--turn-secret`, `--grant-turn`), starts the broker. | **Create** |
| `internal/rtc/peer.go` | Accept `iceServers` so the peer can gather srflx/relay candidates. | Modify: `New` signature |
| `internal/rtc/peer_test.go` | Fix `New` callers; add an iceServers-plumbing test. | Modify |
| `internal/server/rtc.go` | Extract `startSession` helper (shared peer+dispatch wiring); local `/rtc` reuses it with `nil` iceServers. | Modify |
| `internal/server/remote.go` | Daemon outbound dial: register room, relay one offer→signed answer, reuse `startSession`. | **Create** |
| `internal/server/remote_test.go` | Unit test for the answer-signing envelope + iceServers conversion (no live broker). | **Create** |
| `cmd/simbeamd/main.go` | `serve --signal/--client-url`: generate keypair+token, dial broker, print pairing URL. | Modify |
| `web/debug/index.html` | Remote pairing mode from URL fragment: join broker, verify signed answer, use `iceServers`, upsell on `failed`. | Modify |
| `deploy/coturn/turnserver.conf` | Reference `coturn` config (REST-API/long-term creds). Deploy-only. | **Create** |
| `deploy/README.md` | How to deploy broker + coturn; what each `iceServers` entry means. | **Create** |
| `README.md` | Document remote pairing + `simbeam-signal`. | Modify |
| `docs/decisions.md` | Record 3b decisions (#51–#54). | Modify |

**Boundary preserved (decision #30):** `internal/signal` is pure data + crypto and imports **neither** webrtc nor the broker. The broker imports only `internal/signal`. The daemon (`internal/server`) imports `internal/signal` and converts `signal.ICEServer` → `webrtc.ICEServer` locally. `rtcDispatch`/`applyControl` are untouched — 3b changes only *how peers find each other*, never the session mechanics.

---

## Task 1: `internal/signal` — wire envelope + TURN credentials

**Files:**
- Create: `internal/signal/message.go`
- Create: `internal/signal/turn.go`
- Test: `internal/signal/turn_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/signal/turn_test.go`:

```go
package signal

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestMakeTURNCredentialFormat(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := MakeTURNCredential("s3cr3t", "room-abc", now, 60*time.Second)

	// username = "<expiry>:<userID>", expiry = now+ttl in unix seconds.
	wantUser := "1000060:room-abc"
	if c.Username != wantUser {
		t.Fatalf("username = %q, want %q", c.Username, wantUser)
	}

	// credential = base64(HMAC-SHA1(secret, username)) — recompute independently.
	mac := hmac.New(sha1.New, []byte("s3cr3t"))
	mac.Write([]byte(wantUser))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if c.Credential != want {
		t.Fatalf("credential = %q, want %q", c.Credential, want)
	}
}

func TestMakeTURNCredentialDeterministic(t *testing.T) {
	now := time.Unix(42, 0)
	a := MakeTURNCredential("k", "u", now, time.Minute)
	b := MakeTURNCredential("k", "u", now, time.Minute)
	if a != b {
		t.Fatalf("same inputs gave different creds: %+v vs %+v", a, b)
	}
	if strings.Contains(a.Credential, "=") && !strings.HasSuffix(a.Credential, "=") {
		t.Fatalf("credential not valid base64 padding: %q", a.Credential)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signal/ -run TestMakeTURNCredential -v`
Expected: FAIL — package/`MakeTURNCredential` undefined (compile error).

- [ ] **Step 3: Create the envelope and the credential deriver**

Create `internal/signal/message.go`:

```go
// Package signal holds the simbeam signaling wire types and the crypto
// primitives shared by the signaling broker (cmd/simbeam-signal) and the
// daemon (internal/server). It imports neither webrtc nor the broker so both
// sides depend on the same definitions without a dependency cycle.
package signal

// Message types carried by Msg.Type.
const (
	TypeRegister   = "register"   // daemon → broker: claim a room by token
	TypeJoin       = "join"       // client → broker: enter a room by token
	TypeICEServers = "iceServers" // broker → peer: ICE configuration
	TypeOffer      = "offer"      // client → broker → daemon
	TypeAnswer     = "answer"     // daemon → broker → client (carries Sig)
	TypePeerLeft   = "peerLeft"   // broker → peer: the other side dropped
	TypeError      = "error"      // broker/peer → peer: fatal, text in Msg
)

// Roles carried by Msg.Role on register/join.
const (
	RoleDaemon = "daemon"
	RoleClient = "client"
)

// Msg is the single JSON envelope for every signaling message in both
// directions; unused fields stay zero. Non-trickle ICE: all candidates ride
// inside SDP, so there is no separate candidate message.
type Msg struct {
	Type       string      `json:"type"`
	Room       string      `json:"room,omitempty"`       // register/join: the pairing token
	Role       string      `json:"role,omitempty"`       // register/join: daemon|client
	SDP        string      `json:"sdp,omitempty"`        // offer/answer
	PubKey     string      `json:"pubkey,omitempty"`     // register: daemon Ed25519 public key (base64)
	Sig        string      `json:"sig,omitempty"`        // answer: Ed25519 signature of SDP (base64)
	ICEServers []ICEServer `json:"iceServers,omitempty"` // broker → peer
	Msg        string      `json:"msg,omitempty"`        // error text
}

// ICEServer is the subset of the WebRTC RTCIceServer JSON shape we transmit.
// The browser consumes it directly as an RTCIceServer; the daemon converts it
// to webrtc.ICEServer (in internal/server, to keep this package webrtc-free).
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}
```

Create `internal/signal/turn.go`:

```go
package signal

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"time"
)

// TURNCredential is an ephemeral coturn long-term credential, derived per the
// TURN REST API mechanism. coturn validates it by recomputing the same HMAC
// with its shared static-auth-secret, so no per-credential state is stored.
type TURNCredential struct {
	Username   string
	Credential string
}

// MakeTURNCredential derives a credential valid for ttl after now:
//
//	username   = "<unixExpiry>:<userID>"
//	credential = base64( HMAC-SHA1( secret, username ) )
//
// now is a parameter (not time.Now) so callers can test deterministically and
// the broker can inject a clock.
func MakeTURNCredential(secret, userID string, now time.Time, ttl time.Duration) TURNCredential {
	expiry := now.Add(ttl).Unix()
	username := strconv.FormatInt(expiry, 10) + ":" + userID
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return TURNCredential{
		Username:   username,
		Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/signal/ -run TestMakeTURNCredential -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/signal/message.go internal/signal/turn.go internal/signal/turn_test.go
git commit -m "feat(signal): wire envelope + ephemeral TURN HMAC credentials"
```

---

## Task 2: `internal/signal` — Ed25519 handshake auth + pairing

**Files:**
- Create: `internal/signal/auth.go`
- Create: `internal/signal/pairing.go`
- Test: `internal/signal/auth_test.go`
- Test: `internal/signal/pairing_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/signal/auth_test.go`:

```go
package signal

import "testing"

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("v=0\r\no=- 42 2 IN IP4 127.0.0.1\r\n")
	sig := Sign(priv, msg)
	if sig == "" {
		t.Fatal("empty signature")
	}
	if !Verify(pub, msg, sig) {
		t.Fatal("Verify rejected a valid signature")
	}
}

func TestVerifyRejectsTamper(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sig := Sign(priv, []byte("real answer sdp"))
	if Verify(pub, []byte("forged answer sdp"), sig) {
		t.Fatal("Verify accepted a signature over different bytes")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv, _ := GenerateKeyPair()
	otherPub, _, _ := GenerateKeyPair()
	msg := []byte("answer")
	if Verify(otherPub, msg, Sign(priv, msg)) {
		t.Fatal("Verify accepted a signature under the wrong public key")
	}
}

func TestVerifyRejectsGarbageInput(t *testing.T) {
	pub, _, _ := GenerateKeyPair()
	if Verify(pub, []byte("x"), "not-base64-!!") {
		t.Fatal("Verify accepted non-base64 signature")
	}
	if Verify("not-base64-!!", []byte("x"), "AAAA") {
		t.Fatal("Verify accepted non-base64 pubkey")
	}
}
```

Create `internal/signal/pairing_test.go`:

```go
package signal

import (
	"net/url"
	"strings"
	"testing"
)

func TestNewTokenUnique(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if a == "" || a == b {
		t.Fatalf("tokens not unique/non-empty: %q %q", a, b)
	}
}

func TestPairingURLCarriesCoordinatesInFragment(t *testing.T) {
	got := PairingURL("http://localhost:8080/", "wss://sig.example/ws", "tok123", "PUBKEYB64==")

	// Coordinates must live in the fragment (#...), not the query, so they are
	// never sent to (or logged by) the client's HTTP server.
	hash := got[strings.Index(got, "#")+1:]
	if strings.Contains(got[:strings.Index(got, "#")], "tok123") {
		t.Fatalf("token leaked into non-fragment part: %q", got)
	}
	q, err := url.ParseQuery(hash)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("signal") != "wss://sig.example/ws" {
		t.Fatalf("signal = %q", q.Get("signal"))
	}
	if q.Get("token") != "tok123" {
		t.Fatalf("token = %q", q.Get("token"))
	}
	if q.Get("pubkey") != "PUBKEYB64==" {
		t.Fatalf("pubkey = %q", q.Get("pubkey"))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/signal/ -run 'TestSign|TestVerify|TestNewToken|TestPairingURL' -v`
Expected: FAIL — `GenerateKeyPair`/`Sign`/`Verify`/`NewToken`/`PairingURL` undefined.

- [ ] **Step 3: Implement auth + pairing**

Create `internal/signal/auth.go`:

```go
package signal

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
)

// GenerateKeyPair returns a fresh Ed25519 keypair. The public key is base64
// (StdEncoding) for the wire/QR/URL; the private key stays in the daemon.
func GenerateKeyPair() (pubB64 string, priv ed25519.PrivateKey, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	return base64.StdEncoding.EncodeToString(pub), priv, nil
}

// Sign returns a base64 detached Ed25519 signature of msg.
func Sign(priv ed25519.PrivateKey, msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// Verify reports whether sigB64 is a valid Ed25519 signature of msg under the
// base64 public key pubB64. Any decoding/size error returns false (never
// panics) — a malformed signature is just an invalid one.
func Verify(pubB64 string, msg []byte, sigB64 string) bool {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}
```

Create `internal/signal/pairing.go`:

```go
package signal

import (
	"crypto/rand"
	"encoding/base64"
	"net/url"
)

// NewToken returns a one-time pairing token: 16 random bytes, URL-safe base64
// (no padding). The broker treats it as an opaque room key.
func NewToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// PairingURL builds the browser link the daemon prints/renders. The signaling
// coordinates (signalingURL, token, daemonPubKey) go in the URL *fragment* so
// they never reach the client web server's request line or logs:
//
//	<clientBase>#signal=<wss-url>&token=<token>&pubkey=<base64>
//
// clientBase is where the debug client is served (e.g. http://localhost:8080/).
func PairingURL(clientBase, signalingURL, token, pubKeyB64 string) string {
	frag := url.Values{}
	frag.Set("signal", signalingURL)
	frag.Set("token", token)
	frag.Set("pubkey", pubKeyB64)
	return clientBase + "#" + frag.Encode()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/signal/ -v`
Expected: PASS across Task 1 + Task 2 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/signal/auth.go internal/signal/auth_test.go internal/signal/pairing.go internal/signal/pairing_test.go
git commit -m "feat(signal): Ed25519 handshake auth + one-time pairing token/URL"
```

---

## Task 3: `internal/signalbroker` — rooms, relay, gated iceServers

**Files:**
- Create: `internal/signalbroker/broker.go`
- Test: `internal/signalbroker/broker_test.go`

The broker is verified by an integration test: a real `httptest` server with two `gorilla/websocket` clients (one daemon, one client) that pair, receive `iceServers`, and relay offer→answer. This mirrors how the codebase already tests live components rather than mocking them.

- [ ] **Step 1: Write the failing test**

Create `internal/signalbroker/broker_test.go`:

```go
package signalbroker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kei-sidorov/simbeam/internal/signal"
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signalbroker/ -v`
Expected: FAIL — `New`/`Config`/`Handler` undefined.

- [ ] **Step 3: Implement the broker**

Create `internal/signalbroker/broker.go`:

```go
// Package signalbroker is the simbeam signaling broker: a thin WSS rendezvous
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
	"github.com/kei-sidorov/simbeam/internal/signal"
)

// Config tunes ICE issuance and subscription gating.
type Config struct {
	STUNURLs   []string             // always handed out
	TURNURLs   []string             // handed out only when GrantTURN(room) is true
	TURNSecret string               // coturn static-auth-secret (shared with coturn)
	TURNTTL    time.Duration        // ephemeral credential lifetime; 0 → 1 minute
	GrantTURN  func(room string) bool // subscription gate STUB (Phase 4 = real billing); nil → deny
	Now        func() time.Time     // injectable clock; nil → time.Now
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
	rm.client = c
	b.mu.Unlock()

	defer b.dropRoom(join.Room, c)

	// Hand the client its ICE configuration (subscription-gated TURN).
	_ = c.send(signal.Msg{Type: signal.TypeICEServers, ICEServers: b.iceServers(join.Room)})

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
	b.mu.Unlock()
	if rm == nil {
		return
	}
	var dst *conn
	if from == signal.RoleClient {
		dst = rm.daemon
	} else {
		dst = rm.client
	}
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
```

Remove the `var _ = http.StatusOK` line from the test if `go vet` flags the unused import; otherwise leave it. (It exists only to keep the test compiling if you trim imports while iterating.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/signalbroker/ -v`
Expected: PASS (`TestPairRelaysOfferAndAnswer`, `TestSubscriberGetsTURN`, `TestJoinUnknownRoomErrors`).

- [ ] **Step 5: Commit**

```bash
git add internal/signalbroker/
git commit -m "feat(signalbroker): WSS room broker — relay offer/answer, gated iceServers"
```

---

## Task 4: `cmd/simbeam-signal` — broker entrypoint

**Files:**
- Create: `cmd/simbeam-signal/main.go`

- [ ] **Step 1: Write the entrypoint**

Create `cmd/simbeam-signal/main.go`:

```go
// Command simbeam-signal is the reference simbeam signaling broker: a thin WSS
// rendezvous that pairs a daemon and a browser by pairing token, relays one
// offer→answer, and issues iceServers (STUN always; TURN only when granted).
// Media never transits it. The managed/production broker is the open-core moat
// (decision #9, #47); this reference build is for local dev and self-host.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signalbroker"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs (handed to everyone)")
	turn := flag.String("turn", "", "comma-separated TURN URLs (handed only to granted rooms)")
	turnSecret := flag.String("turn-secret", "", "coturn static-auth-secret for ephemeral credentials")
	turnTTL := flag.Duration("turn-ttl", time.Minute, "ephemeral TURN credential lifetime")
	grantTURN := flag.Bool("grant-turn", false, "STUB subscription gate: grant TURN to every room (Phase 4 = real billing)")
	flag.Parse()

	b := signalbroker.New(signalbroker.Config{
		STUNURLs:   splitNonEmpty(*stun),
		TURNURLs:   splitNonEmpty(*turn),
		TURNSecret: *turnSecret,
		TURNTTL:    *turnTTL,
		GrantTURN:  func(string) bool { return *grantTURN },
	})

	fmt.Printf("simbeam-signal listening on %s (ws path: /ws)\n", *addr)
	if *grantTURN {
		fmt.Println("WARNING: --grant-turn is a STUB that grants TURN to every room (dev/testing only)")
	}
	if err := http.ListenAndServe(*addr, b.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func splitNonEmpty(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./cmd/simbeam-signal/`
Expected: no output (success).

- [ ] **Step 3: Smoke-run it (manual)**

Run: `go run ./cmd/simbeam-signal --addr :9000`
Expected: prints `simbeam-signal listening on :9000 (ws path: /ws)`. Stop it with Ctrl-C.

- [ ] **Step 4: Commit**

```bash
git add cmd/simbeam-signal/main.go
git commit -m "feat(cmd): simbeam-signal broker entrypoint"
```

---

## Task 5: `rtc.New` accepts iceServers; extract `startSession`

**Files:**
- Modify: `internal/rtc/peer.go`
- Modify: `internal/rtc/peer_test.go`
- Modify: `internal/server/rtc.go`

The daemon's peer currently uses `webrtc.Configuration{}` (host candidates only — fine for localhost, decision #32). Remote peers must gather `srflx`/`relay` candidates, which requires ICE servers. We thread them through `rtc.New`, and extract the shared peer+dispatch wiring into `startSession` so both the local `/rtc` handler and the new remote dial build the session identically.

- [ ] **Step 1: Update the rtc test for the new signature + plumbing**

In `internal/rtc/peer_test.go`, change every `New(nil)` call to `New(nil, nil)` (there are three: `TestSessionAnswer`, `TestSessionWriteFrameNoPanic`, `TestSessionSendBeforeChannel`). Then add a plumbing test:

```go
func TestNewWithICEServersBuilds(t *testing.T) {
	sess, err := New(nil, []webrtc.ICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// A valid configuration must still answer an offer (STUN unreachable in the
	// test is fine — non-trickle gathering completes with host candidates).
	if _, err := sess.Answer(makeOffer(t)); err != nil {
		t.Fatalf("Answer with iceServers configured: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/rtc/ -v`
Expected: FAIL — `New` takes one argument (compile error on the `New(nil, nil)` calls and the new test).

- [ ] **Step 3: Change `New` to accept iceServers**

In `internal/rtc/peer.go`, update the signature and the `NewPeerConnection` call:

```go
// New creates a peer with one H.264 video track and routes inbound "control"
// DataChannel messages to onControl (nil to ignore). iceServers configures ICE
// gathering: nil/empty yields host candidates only (localhost dev); STUN/TURN
// entries enable srflx/relay for remote rendezvous.
func New(onControl func([]byte), iceServers []webrtc.ICEServer) (*Session, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, err
	}
```

(The rest of `New` is unchanged.)

- [ ] **Step 4: Run rtc tests to verify they pass**

Run: `go test ./internal/rtc/ -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Extract `startSession` and fix the local `/rtc` caller**

In `internal/server/rtc.go`, add the `webrtc` import and a shared builder, then have `handleRTC` use it. Replace the file with:

```go
package server

import (
	"context"
	"net/http"

	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simbeam/internal/rtc"
)

// rtcFPS is the screenshot/encode frame rate for the WebRTC path.
const rtcFPS = 15

// sdpMsg is the signaling envelope exchanged over the local /rtc WebSocket
// (dev mode). Remote mode uses internal/signal instead.
type sdpMsg struct {
	Type string `json:"type"` // "offer" | "answer" | "error"
	SDP  string `json:"sdp,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// startSession builds a session-scoped control plane: a pre-negotiated
// (initially silent) H.264 peer + a bidirectional "control" DataChannel wired
// to a fresh dispatcher. iceServers is nil for local/dev (host candidates) and
// populated from the broker for remote rendezvous. The caller owns ctx and must
// call sess.Close()/d.stopAttachment() on teardown.
func (s *Server) startSession(ctx context.Context, iceServers []webrtc.ICEServer) (*rtc.Session, *rtcDispatch, error) {
	d := &rtcDispatch{comp: s.comp, binary: s.binary, baseCtx: ctx}
	sess, err := rtc.New(d.handle, iceServers)
	if err != nil {
		return nil, nil, err
	}
	d.send = func(b []byte) { _ = sess.Send(b) }
	d.writeFrame = sess.WriteFrame
	return sess, d, nil
}

// handleRTC negotiates one session-scoped WebRTC peer over the local /rtc
// WebSocket (dev mode): a pre-negotiated silent H.264 track plus a control
// DataChannel. No simulator is bound up front — the client drives
// list/boot/attach/detach over the control channel. The JPEG /session path is
// untouched. Remote rendezvous uses DialSignal (remote.go) instead.
func (s *Server) handleRTC(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())

	sess, d, err := s.startSession(ctx, nil) // dev: host candidates only (decision #32)
	if err != nil {
		cancel()
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sess.Close()
	defer d.stopAttachment()
	defer cancel() // cancels baseCtx so the pump/stream drain before sess.Close
	sess.OnClose(cancel)

	var offer sdpMsg
	if err := conn.ReadJSON(&offer); err != nil || offer.Type != "offer" {
		return
	}
	answerSDP, err := sess.Answer(offer.SDP)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	if err := conn.WriteJSON(sdpMsg{Type: "answer", SDP: answerSDP}); err != nil {
		return
	}

	// Block until the client disconnects; all control travels over the
	// DataChannel, so any WS read here is just the disconnect signal.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			cancel()
			return
		}
	}
}
```

- [ ] **Step 6: Run the full server + rtc suites and build**

Run: `go test ./internal/server/ ./internal/rtc/ -v && go build ./... && go vet ./...`
Expected: PASS, then no output. (`TestHandleRTCRejectsNonWebsocket` and all 3a dispatcher tests still pass — local mode is behavior-identical.)

- [ ] **Step 7: Commit**

```bash
git add internal/rtc/peer.go internal/rtc/peer_test.go internal/server/rtc.go
git commit -m "feat(rtc): iceServers config + shared startSession wiring"
```

---

## Task 6: Daemon outbound dial — register room, sign + relay answer

**Files:**
- Create: `internal/server/remote.go`
- Test: `internal/server/remote_test.go`
- Modify: `cmd/simbeamd/main.go`

The daemon dials the broker, registers a room under the pairing token (announcing its public key), waits for the client's offer, answers it with the reused control-plane peer, and **signs the answer SDP** so the browser can authenticate the daemon (anti-MITM). After the answer is sent the handshake is done: the daemon keeps the live P2P peer and closes the signaling socket. The live `idb`/`ffmpeg` attach path is exercised in Task 9; this task unit-tests the signing/conversion seams that don't need a live handshake.

- [ ] **Step 1: Write the failing test**

Create `internal/server/remote_test.go`:

```go
package server

import (
	"testing"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

func TestToWebRTCConvertsICEServers(t *testing.T) {
	in := []signal.ICEServer{
		{URLs: []string{"stun:s:3478"}},
		{URLs: []string{"turn:t:3478"}, Username: "u", Credential: "p"},
	}
	out := toWebRTC(in)
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
	if out[1].Username != "u" || out[1].Credential != "p" {
		t.Fatalf("turn creds not carried: %+v", out[1])
	}
	if out[0].URLs[0] != "stun:s:3478" {
		t.Fatalf("urls not carried: %+v", out[0])
	}
}

func TestSignedAnswerVerifies(t *testing.T) {
	pub, priv, err := signal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	// signedAnswer is what the daemon puts on the wire: an answer Msg whose Sig
	// authenticates its SDP under the daemon key the browser holds from the URL.
	m := signedAnswer("ANSWER_SDP", priv)
	if m.Type != signal.TypeAnswer || m.SDP != "ANSWER_SDP" {
		t.Fatalf("bad answer msg: %+v", m)
	}
	if !signal.Verify(pub, []byte(m.SDP), m.Sig) {
		t.Fatal("browser-side verification of the signed answer failed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run 'TestToWebRTC|TestSignedAnswer' -v`
Expected: FAIL — `toWebRTC`/`signedAnswer` undefined.

- [ ] **Step 3: Implement the remote dial**

Create `internal/server/remote.go`:

```go
package server

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

// toWebRTC converts broker iceServers to pion's type (kept here so
// internal/signal stays webrtc-free, preserving the decision #30 boundary).
func toWebRTC(in []signal.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return out
}

// signedAnswer wraps an answer SDP into a signaling Msg whose Sig authenticates
// the SDP under the daemon's session key (anti-MITM: the browser verifies it
// against the pubkey carried in the pairing URL).
func signedAnswer(sdp string, priv ed25519.PrivateKey) signal.Msg {
	return signal.Msg{Type: signal.TypeAnswer, SDP: sdp, Sig: signal.Sign(priv, []byte(sdp))}
}

// DialSignal connects to the signaling broker, registers a room under token,
// and serves exactly one client: it relays the client's offer into a fresh
// session-scoped control-plane peer (reusing startSession) and sends back a
// signed answer. The signaling socket is handshake-only — once the answer is
// sent it closes, and the live P2P peer carries everything else (decision #50;
// disconnect detection stays on pion via sess.OnClose). Blocks until the peer
// closes or ctx is cancelled.
func (s *Server) DialSignal(ctx context.Context, signalURL, token, pubKeyB64 string, priv ed25519.PrivateKey) error {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, signalURL, nil)
	if err != nil {
		return fmt.Errorf("dial signaling: %w", err)
	}
	defer ws.Close()

	if err := ws.WriteJSON(signal.Msg{
		Type: signal.TypeRegister, Room: token, Role: signal.RoleDaemon, PubKey: pubKeyB64,
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Wait for messages from the broker. We need iceServers (sent to the client,
	// but the daemon side must gather with matching servers too) and the offer.
	// The broker forwards the client's offer; iceServers for the daemon side are
	// derived from the same room, so we read until we see the offer and use any
	// iceServers msg that arrives first.
	var iceServers []webrtc.ICEServer
	var offerSDP string
	for offerSDP == "" {
		var m signal.Msg
		if err := ws.ReadJSON(&m); err != nil {
			return fmt.Errorf("read signaling: %w", err)
		}
		switch m.Type {
		case signal.TypeICEServers:
			iceServers = toWebRTC(m.ICEServers)
		case signal.TypeOffer:
			offerSDP = m.SDP
		case signal.TypeError:
			return fmt.Errorf("signaling: %s", m.Msg)
		case signal.TypePeerLeft:
			return fmt.Errorf("signaling: client left before offer")
		}
	}

	sessCtx, cancel := context.WithCancel(ctx)
	sess, d, err := s.startSession(sessCtx, iceServers)
	if err != nil {
		cancel()
		_ = ws.WriteJSON(signal.Msg{Type: signal.TypeError, Msg: err.Error()})
		return err
	}
	defer sess.Close()
	defer d.stopAttachment()
	defer cancel()
	sess.OnClose(cancel)

	answerSDP, err := sess.Answer(offerSDP)
	if err != nil {
		_ = ws.WriteJSON(signal.Msg{Type: signal.TypeError, Msg: err.Error()})
		return err
	}
	if err := ws.WriteJSON(signedAnswer(answerSDP, priv)); err != nil {
		return fmt.Errorf("send answer: %w", err)
	}

	// Handshake complete. Close the signaling socket (defer) and hold the live
	// peer until it drops or ctx is cancelled.
	_ = ws.Close()
	<-sessCtx.Done()
	return nil
}
```

> **Broker note (must hold for this to work):** the broker sends `iceServers` to the **client** on join; the daemon needs them too. In `internal/signalbroker/broker.go`, after a daemon registers AND a client is present, also send the daemon its `iceServers`. Add this to `serveClient` right after computing the client's config — send the *same* config to `rm.daemon`:
>
> ```go
> ice := b.iceServers(join.Room)
> _ = c.send(signal.Msg{Type: signal.TypeICEServers, ICEServers: ice})
> b.mu.Lock()
> dmn := rm.daemon
> b.mu.Unlock()
> if dmn != nil {
> 	_ = dmn.send(signal.Msg{Type: signal.TypeICEServers, ICEServers: ice})
> }
> ```
>
> Replace the single `c.send(... iceServers ...)` line in `serveClient` with the block above. Re-run `go test ./internal/signalbroker/ -v` — the existing tests still pass (the client still receives exactly one iceServers msg; the daemon test client in `TestPairRelaysOfferAndAnswer` will now also receive one before the offer, so update that test to drain it):
>
> In `TestPairRelaysOfferAndAnswer`, after the client reads its `iceServers` and before asserting the relayed offer, the daemon must first drain its own `iceServers` msg:
> ```go
> 	// Daemon also receives iceServers once the client joins.
> 	if dice := readMsg(t, daemon); dice.Type != signal.TypeICEServers {
> 		t.Fatalf("daemon want iceServers, got %+v", dice)
> 	}
> ```
> Place that block immediately before `client.WriteJSON(... TypeOffer ...)`.

- [ ] **Step 4: Apply the broker change above, then run tests**

Run: `go test ./internal/server/ -run 'TestToWebRTC|TestSignedAnswer' ./internal/signalbroker/ -v`
Expected: PASS (server seam tests + updated broker integration tests).

- [ ] **Step 5: Wire `serve --signal` into the daemon**

In `cmd/simbeamd/main.go`, extend `runServe`. Add imports `context`, `log`, and the `signal` package, then replace `runServe` with:

```go
func runServe(argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	webDir := fs.String("web", "", "directory with debug client (served at /); empty = API only")
	signalURL := fs.String("signal", "", "remote rendezvous: signaling broker WS URL (e.g. wss://host/ws); empty = local-only")
	clientURL := fs.String("client-url", "", "base URL of the browser debug client for the pairing link; empty = http://localhost<addr>/")
	_ = fs.Parse(argv)

	c := companion.New()
	path, err := c.Resolve()
	if err != nil {
		return err
	}
	srv := server.New(c, *webDir).WithBinary(path)

	// Remote rendezvous: dial the broker, print a pairing URL, serve one client.
	if *signalURL != "" {
		return runRemote(srv, *signalURL, *clientURL, *addr, *webDir)
	}

	fmt.Printf("simbeamd serving on %s (idb_companion: %s)\n", *addr, path)
	if *webDir != "" {
		fmt.Printf("debug client: http://localhost%s/\n", *addr)
	}
	return http.ListenAndServe(*addr, srv.Handler())
}

// runRemote dials the signaling broker and serves a single paired client. It
// also serves the local HTTP (debug client) so the browser has somewhere to
// load from; the pairing URL points there with the signaling coordinates in
// the fragment.
func runRemote(srv *server.Server, signalURL, clientURL, addr, webDir string) error {
	pubKey, priv, err := signal.GenerateKeyPair()
	if err != nil {
		return err
	}
	token, err := signal.NewToken()
	if err != nil {
		return err
	}
	base := clientURL
	if base == "" {
		base = "http://localhost" + addr + "/"
	}

	// Serve the debug client locally (so the browser can load it) in the
	// background; pairing coordinates travel via the URL fragment, not this server.
	if webDir != "" {
		go func() {
			if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
				log.Printf("local http: %v", err)
			}
		}()
	}

	fmt.Printf("simbeamd remote mode — broker: %s\n", signalURL)
	fmt.Println("Pair this device by opening:")
	fmt.Println("  " + signal.PairingURL(base, signalURL, token, pubKey))
	fmt.Println("(token is one-time; restart to pair again)")

	ctx := context.Background()
	return srv.DialSignal(ctx, signalURL, token, pubKey, priv)
}
```

Also update `usage()` to mention the flags:

```go
	fmt.Fprintln(w, "  simbeamd serve   Serve REST API + WebSocket stream (flags: --addr, --web, --signal, --client-url)")
```

- [ ] **Step 6: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/server/remote.go internal/server/remote_test.go internal/signalbroker/broker.go internal/signalbroker/broker_test.go cmd/simbeamd/main.go
git commit -m "feat(server): outbound signaling dial — register room, sign + relay answer"
```

---

## Task 7: Browser debug client — remote pairing mode

**Files:**
- Modify: `web/debug/index.html`

When the page loads with a `#signal=...&token=...&pubkey=...` fragment, the client enters **remote mode**: it connects to the broker, joins the room, receives `iceServers`, builds the peer with them, exchanges offer/answer through the broker, **verifies the answer's signature against the daemon pubkey** (WebCrypto Ed25519), and then runs the identical 3a control plane over the P2P DataChannel. Without a fragment, the page keeps its existing local-mode behavior (`/rtc` WS) unchanged. On `iceConnectionState`/`connectionState === "failed"`, it shows the upsell message.

The control-plane logic (`onCtrlReply`, `pickRTC`, input handlers, `minimizeBuffer`) is unchanged — only the *signaling transport* differs. We refactor `startControlPlane` to build the peer + DataChannel once, then hand off to either the local or remote signaling exchange.

- [ ] **Step 1: Replace the RTC signaling section of `web/debug/index.html`**

Replace the `// ---- RTC path: control plane + video on demand ----` block (the `startControlPlane` function and nothing else around it) with the following. Everything else in the file stays as-is.

```javascript
// ---- RTC path: control plane + video on demand ----
// Signaling is pluggable: local mode talks to /rtc; remote mode pairs through
// the broker carried in the URL fragment (#signal&token&pubkey). The peer +
// control DataChannel and all downstream control-plane logic are identical.

function pairing() {
  // Coordinates live in the URL fragment so they never hit a server log.
  const f = new URLSearchParams(location.hash.slice(1));
  const signal = f.get('signal'), token = f.get('token'), pubkey = f.get('pubkey');
  return (signal && token && pubkey) ? {signal, token, pubkey} : null;
}

async function startControlPlane() {
  const gen = ++startGen;
  pc = new RTCPeerConnection(window._iceServers ? {iceServers: window._iceServers} : undefined);
  pc.addTransceiver('video', {direction: 'recvonly'});
  dc = pc.createDataChannel('control', {ordered: false, maxRetransmits: 0});

  pc.ontrack = (ev) => { vidEl.srcObject = ev.streams[0]; minimizeBuffer(pc); };
  const onFail = () => {
    if (['failed'].includes(pc.iceConnectionState) || ['failed'].includes(pc.connectionState)) {
      showUpsell();
    }
  };
  pc.oniceconnectionstatechange = onFail;
  pc.onconnectionstatechange = onFail;
  dc.onopen = () => dc.send(JSON.stringify({type: 'list'}));
  dc.onmessage = (ev) => onCtrlReply(JSON.parse(ev.data));

  const p = pairing();
  if (p) await signalRemote(p, gen);
  else await signalLocal(gen);
}

// Local dev signaling: one /rtc WS, offer→answer, no auth (localhost).
async function signalLocal(gen) {
  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await iceGatheringComplete(pc);
  if (gen !== startGen) return;

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  sig = new WebSocket(`${proto}://${location.host}/rtc`);
  sig.onopen = () => sig.send(JSON.stringify({type: 'offer', sdp: pc.localDescription.sdp}));
  sig.onmessage = async (ev) => {
    const m = JSON.parse(ev.data);
    if (m.type === 'answer') {
      await pc.setRemoteDescription({type: 'answer', sdp: m.sdp});
      minimizeBuffer(pc);
    } else if (m.type === 'error') {
      alert('rtc signaling error: ' + m.msg);
      teardown();
    }
  };
}

// Remote rendezvous: join the broker room, receive iceServers, exchange a
// signed offer/answer. The daemon signs its answer SDP; we verify it against
// the pubkey from the pairing URL before trusting the connection (anti-MITM).
async function signalRemote(p, gen) {
  sig = new WebSocket(p.signal);
  let haveIce = false, sentOffer = false;

  const makeAndSendOffer = async () => {
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    await iceGatheringComplete(pc);
    if (gen !== startGen) return;
    sig.send(JSON.stringify({type: 'offer', sdp: pc.localDescription.sdp}));
    sentOffer = true;
  };

  sig.onopen = () => sig.send(JSON.stringify({type: 'join', room: p.token, role: 'client'}));
  sig.onmessage = async (ev) => {
    const m = JSON.parse(ev.data);
    if (m.type === 'iceServers') {
      window._iceServers = m.iceServers || [];
      // Rebuild the peer with the issued iceServers, then offer.
      // (Recreate so the configuration takes effect before gathering.)
      pc.setConfiguration({iceServers: window._iceServers});
      haveIce = true;
      if (!sentOffer) await makeAndSendOffer();
    } else if (m.type === 'answer') {
      const ok = await verifyAnswer(p.pubkey, m.sdp, m.sig);
      if (!ok) { showAuthFail(); teardown(); return; }
      await pc.setRemoteDescription({type: 'answer', sdp: m.sdp});
      minimizeBuffer(pc);
    } else if (m.type === 'error') {
      alert('pairing error: ' + m.msg + '\nRescan / re-pair.');
      teardown();
    } else if (m.type === 'peerLeft') {
      console.log('daemon left the room');
    }
  };
  // If iceServers somehow precede the open handler's join race, offer once we have them.
  void haveIce;
}

// verifyAnswer checks the daemon's Ed25519 signature over the answer SDP using
// WebCrypto. Returns false on any decode/verify failure.
async function verifyAnswer(pubB64, sdp, sigB64) {
  try {
    const pub = b64ToBytes(pubB64), sig = b64ToBytes(sigB64);
    const key = await crypto.subtle.importKey('raw', pub, {name: 'Ed25519'}, false, ['verify']);
    return await crypto.subtle.verify('Ed25519', key, sig, new TextEncoder().encode(sdp));
  } catch (e) {
    console.error('answer verification error', e);
    return false;
  }
}

function b64ToBytes(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function showUpsell() {
  alert('Could not connect peer-to-peer.\n\n' +
        'Free tier has no relay (TURN) infrastructure — be on the same network ' +
        'as your Mac, or subscribe for connect-from-anywhere.');
}

function showAuthFail() {
  alert('Pairing authentication failed: the signaling answer was not signed by ' +
        'the expected device key. Possible man-in-the-middle — do not proceed. Re-pair.');
}
```

> **Note on `setConfiguration` vs recreate:** `RTCPeerConnection.setConfiguration({iceServers})` updates ICE servers before the first offer/`setLocalDescription`, which is exactly our ordering (we receive `iceServers` on join, *then* offer). This avoids tearing down and rebuilding the peer. If a target browser rejects `setConfiguration` for iceServers, fall back to constructing `pc` with the servers — but verify in Task 9 first; do not pre-optimize.

- [ ] **Step 2: Confirm local mode is untouched**

Read the diff: the only changes are inside the RTC signaling section. `enterMode`, `onCtrlReply`, `pickRTC`, `loadSimsREST`, `pickJPG`, `startJPG`, input handlers, `iceGatheringComplete`, `minimizeBuffer` are unchanged. Local mode (no fragment) routes through `signalLocal`, which is the original `/rtc` exchange.

- [ ] **Step 3: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(web): remote pairing mode — broker join, signed-answer verify, upsell"
```

---

## Task 8: coturn config + deploy notes (deploy-only)

**Files:**
- Create: `deploy/coturn/turnserver.conf`
- Create: `deploy/README.md`

This task produces deployment artifacts. Per the spec, the TURN engine is off-the-shelf `coturn`; we only configure it and issue HMAC creds (done in Task 1). Nothing here runs in local validation — it is flagged deploy-only.

- [ ] **Step 1: Write the coturn config**

Create `deploy/coturn/turnserver.conf`:

```ini
# simbeam reference coturn config (deploy-only — NOT exercised by local tests).
#
# coturn validates the ephemeral credentials the broker issues
# (internal/signal.MakeTURNCredential) by recomputing the same HMAC with the
# shared secret below. The broker's --turn-secret MUST equal static-auth-secret.

listening-port=3478
tls-listening-port=5349

# Advertise the public IP of the TURN host (replace at deploy time).
external-ip=YOUR.PUBLIC.IP

# TURN REST API / long-term credential mechanism: time-limited HMAC creds.
use-auth-secret
static-auth-secret=REPLACE_WITH_SAME_SECRET_AS_BROKER_--turn-secret

realm=simbeam

# Relay only what we need; lock down the rest.
no-multicast-peers
no-cli
fingerprint

# TLS (recommended in prod): point at real certs.
# cert=/etc/coturn/cert.pem
# pkey=/etc/coturn/pkey.pem

# Optional: restrict relay port range for firewalling.
# min-port=49152
# max-port=65535
```

- [ ] **Step 2: Write the deploy README**

Create `deploy/README.md`:

```markdown
# Deploying simbeam remote access (Phase 3b)

> **Deploy-only.** None of this is exercised by the repo's local tests. Local
> validation (see the plan's Task 9) proves the *functional* pairing flow over
> localhost (host candidates only). Real NAT traversal, `srflx`/`relay`
> candidates, and a running coturn require this deployment.

## Components

1. **Signaling broker** — `cmd/simbeam-signal`. Stateless WSS rendezvous.
2. **coturn** — off-the-shelf TURN relay. We only configure it (`coturn/turnserver.conf`).
3. **Daemon** — `simbeamd serve --signal wss://<broker-host>/ws --web ./web/debug`.

## Broker

```bash
go build -o simbeam-signal ./cmd/simbeam-signal
./simbeam-signal \
  --addr :9000 \
  --stun stun:<stun-host>:3478 \
  --turn turn:<turn-host>:3478 \
  --turn-secret "<SAME-AS-COTURN-static-auth-secret>" \
  --turn-ttl 1m \
  --grant-turn=false   # STUB gate; true grants TURN to every room (dev only)
```

Put the broker behind TLS (`wss://`) in production — terminate TLS at a reverse
proxy or extend the broker. The pairing URL embeds `wss://`.

## coturn

Install coturn (`apt install coturn` / `brew install coturn`), set
`static-auth-secret` in `coturn/turnserver.conf` to the **same value** as the
broker's `--turn-secret`, set `external-ip`, and run `turnserver -c turnserver.conf`.

## Subscription gating (Phase 4)

`--grant-turn` is a STUB: it grants TURN to all rooms or none. Real per-user
billing/subscription checks (`GrantTURN(room)` keyed to an account) are Phase 4.

## ICE entries the browser receives

| Entry | When | Cost |
|-------|------|------|
| `stun:` | always | ~free (stateless) |
| `turn:` + ephemeral HMAC creds | only when `GrantTURN(room)` is true | relays media — the metered resource |

Free tier (STUN only): works on the same LAN and on friendly NATs; a hostile
symmetric NAT yields `iceConnectionState === "failed"` and the client shows the
upsell.
```

- [ ] **Step 3: Commit**

```bash
git add deploy/coturn/turnserver.conf deploy/README.md
git commit -m "docs(deploy): coturn config + remote-access deploy notes"
```

---

## Task 9: End-to-end localhost validation + docs + decisions

**Files:**
- Modify: `README.md`
- Modify: `docs/decisions.md`

- [ ] **Step 1: Build everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: no build/vet output; all tests PASS.

- [ ] **Step 2: Run the broker + daemon in remote mode (localhost)**

Terminal 1 — broker (free tier, STUN only):
```bash
go run ./cmd/simbeam-signal --addr :9000 --stun stun:stun.l.google.com:19302
```
Expected: `simbeam-signal listening on :9000 (ws path: /ws)`.

Terminal 2 — daemon dialing the local broker (note `ws://` for localhost; the
client base must match where the browser loads the page):
```bash
go run ./cmd/simbeamd serve --addr :8080 --web ./web/debug \
  --signal ws://localhost:9000/ws \
  --client-url http://localhost:8080/
```
Expected: prints `simbeamd remote mode — broker: ws://localhost:9000/ws` and a
`Pair this device by opening:` URL of the form
`http://localhost:8080/#signal=ws%3A...&token=...&pubkey=...`.

- [ ] **Step 3: Pair from the browser and verify the functional flow**

Open the printed pairing URL in Chrome (Ed25519 WebCrypto support required —
Chrome 137+/Safari 17+; if your browser lacks it, see the caveat in Step 6).
Observe the browser console, the broker terminal, and the daemon terminal, and verify:
- The peer connects (`pc.connectionState === "connected"`) over **host candidates** (localhost).
- The **answer signature verifies** — no "Pairing authentication failed" alert. (To prove the check works, temporarily corrupt one byte of `pubkey` in the URL → expect the auth-fail alert and no connection.)
- The simulator **list renders over the DataChannel** (not REST): the broker relayed only `join`/`iceServers`/`offer`/`answer`, then went silent — no media or control crossed it.
- Click a **shutdown** sim → boots → video appears (`boot`→`booted`→`attach`→`attached`).
- Click a **booted** sim → video appears directly. Tap/swipe/keys/Home work. Switching sims swaps the feed within ~1–2s.
- Close the tab → daemon tears down the sidecar/ffmpeg (peer drop cancels the session); the daemon process exits `DialSignal` (one-shot pairing).

If any step fails, debug before continuing — do not mark complete on assumption.

- [ ] **Step 4: Verify local dev mode still works (no fragment)**

Stop the remote daemon. Run the plain local daemon:
```bash
go run ./cmd/simbeamd serve --addr :8080 --web ./web/debug
```
Open `http://localhost:8080/` (no fragment). Verify RTC mode pairs over `/rtc`
exactly as in 3a (list over DataChannel, attach shows video) and the JPG
fallback still works. This proves Task 5's refactor is behavior-preserving.

- [ ] **Step 5: Note what remains deploy-only**

Record (in the commit message and your report) the scenarios **not** provable on
localhost, all requiring a deployed broker + public STUN/TURN + real NAT:
- `srflx` path on a friendly NAT (different networks).
- `relay` path for a subscriber on a symmetric NAT (run broker with `--grant-turn` + a real coturn).
- `iceConnectionState === "failed"` → upsell under a hostile symmetric NAT, free tier.
- Trickle ICE (deferred): non-trickle works but adds real-NAT setup latency.

- [ ] **Step 6: Update README**

In `README.md`, add a "Удалёнка (Phase 3b — рандеву)" subsection after the WebRTC
section. Apply this addition:

```markdown
### Удалёнка / рандеву (Phase 3b)

Демон дозванивается до signaling-брокера (исходящий WSS, ноль открытых портов на
Mac), регистрирует «комнату» по одноразовому `pairingToken` и печатает pairing-URL.
Браузер открывает URL (координаты — `signalingURL` + `token` + `daemonPubKey` — в
**фрагменте** `#…`, не в query), входит в комнату, обменивается offer/answer через
брокер и **проверяет подпись answer'а** по `daemonPubKey` (анти-MITM, Ed25519). Видео
и control идут P2P (DTLS-SRTP E2E); через брокер течёт только рукопожатие.

Reference-брокер — `cmd/simbeam-signal` (в этом репо). STUN раздаётся всем; TURN —
только «подписчикам» (в этой фазе — стаб `--grant-turn`), по короткоживущим HMAC-кредам
для готового `coturn` (свой TURN не пишем). Деплой — `deploy/README.md`.

Локально (один хост, host-кандидаты):

```bash
# терминал 1 — брокер
go run ./cmd/simbeam-signal --addr :9000 --stun stun:stun.l.google.com:19302
# терминал 2 — демон в remote-режиме
go run ./cmd/simbeamd serve --addr :8080 --web ./web/debug \
  --signal ws://localhost:9000/ws --client-url http://localhost:8080/
# открыть напечатанный pairing-URL в браузере
```

Сигналинг — **только рукопожатие**: один offer/answer, затем сокет закрывается;
renegotiation нет (видео-трек pre-negotiated, решение #50). Реальный NAT/relay
локально не проверить — см. `deploy/README.md` и «deploy-only» сценарии.
```

Also add `simbeam-signal` to any command list in the README that enumerates binaries.

- [ ] **Step 7: Record decisions**

Append rows to `docs/decisions.md` (after #50):

```
| 51 | Phase 3b: сигналинг — **только рукопожатие**. Брокер релеит один offer→answer, затем сокет закрывается; пост-пейринговый обмен SDP отсутствует (видео-трек pre-negotiated, #50), renegotiation (если когда-нибудь понадобится) — по DataChannel, не по сигналингу. Disconnect ловит pion `OnConnectionStateChange` (#39) | разрешает «развилку» плана 3b: Option B (#50) убирает renegotiation вообще, поэтому signaling нужен только для знакомства и может закрыться сразу после connect — брокер остаётся по-настоящему тонким |
| 52 | Phase 3b: демон делает **исходящий** дозвон до брокера и остаётся **answerer** (браузер — offerer, как в 3a). Переиспуем `rtc.Session.Answer` без изменений; non-trickle ICE (#32) сохраняется — кандидаты в SDP, работает и со STUN/TURN (медленнее сбор). Trickle ICE отложен | минимальная дельта к 3a: меняется только транспорт знакомства, механика сессии (`rtcDispatch`/attach) нетронута; роль answerer'а сохраняет переиспользование кода |
| 53 | Phase 3b: WSS-библиотека для брокера и исходящего дозвона демона — `gorilla/websocket` (уже зависимость репо), а не `coder/websocket` из спеки #47 | спека называла lib как ориентир, не как архитектурное решение; gorilla уже вендорится и используется (`/session`, `/rtc`), даёт и Upgrader, и Dialer — ноль новых зависимостей |
| 54 | Phase 3b: аутентификация рукопожатия — **Ed25519** (stdlib `crypto/ed25519`); демон подписывает answer-SDP сессионным ключом, браузер проверяет по `daemonPubKey` из URL-фрагмента (WebCrypto Ed25519). TURN-креды — HMAC-SHA1 REST (`username=<expiry>:<userID>`, #44). Гейтинг подписки — стаб `--grant-turn` (реальный биллинг — Phase 4) | асимметрия нужна против MITM на сигналинге (брокер не должен уметь подделать сторону демона); координаты в фрагменте URL не попадают в логи веб-сервера; медиа уже E2E (DTLS-SRTP, #7) |
```

- [ ] **Step 8: Commit**

```bash
git add README.md docs/decisions.md
git commit -m "docs: document Phase 3b remote rendezvous + decisions #51-54"
```

---

## Self-Review

**Spec coverage (against `2026-06-03-phase4-remote-access-design.md`, the Phase 3b parts):**
- ✅ Signaling service on Go, reference in-repo (`cmd/simbeam-signal`): Tasks 3–4. Rooms by `pairingToken`, relay offer/answer, issues `iceServers`, zero-knowledge of media (handshake-only, decision #51).
- ✅ Daemon outbound dial, zero open ports, reuses 3a session: Task 6 (`DialSignal` + `startSession` from Task 5). Decision #52.
- ✅ Pairing: `signalingURL` + one-time `pairingToken` + `daemonPubKey` via URL fragment; pubkey authenticates the handshake (Ed25519 signed answer): Tasks 2, 6, 7. Decision #54.
- ✅ STUN/TURN gating: STUN to all, TURN to subscribers via ephemeral HMAC creds; subscription = stub (`--grant-turn`); free + `failed` → upsell: Tasks 1, 3, 7, 8. Decision #44/#54.
- ✅ coturn off-the-shelf, config + creds only: Tasks 1 + 8.
- ✅ Browser validation, no native client; QR replaced by URL fragment: Task 7. Localhost functional flow: Task 9.
- ✅ The plan's flagged fork (post-pairing SDP location): resolved as handshake-only signaling (decision #51), stated up front.
- ⏭️ Correctly deferred/flagged deploy-only: real NAT `srflx`/`relay`, running coturn, hostile-NAT upsell, trickle ICE (Tasks 8–9). Phase 4 (not here): real billing, QR, native client, broker prod hardening.

**Placeholder scan:** No TBD/TODO in implementation steps. `deploy/coturn/turnserver.conf` contains `YOUR.PUBLIC.IP`/`REPLACE_WITH...` placeholders **by design** — they are deploy-time secrets/addresses, not code gaps, and the deploy README says so. The broker iceServers-to-daemon addition in Task 6 is given as full replacement code, not hand-waved.

**Type consistency:** `signal.Msg` fields (`Type/Room/Role/SDP/PubKey/Sig/ICEServers/Msg`) and constants (`TypeRegister/TypeJoin/TypeICEServers/TypeOffer/TypeAnswer/TypePeerLeft/TypeError`, `RoleDaemon/RoleClient`) defined in Task 1, used by broker (Task 3), daemon (Task 6), browser (Task 7). `signal.ICEServer{URLs,Username,Credential}` ↔ browser RTCIceServer ↔ `toWebRTC`→`webrtc.ICEServer` (Task 6). `MakeTURNCredential`/`GenerateKeyPair`/`Sign`/`Verify`/`NewToken`/`PairingURL` (Tasks 1–2) consumed consistently. `rtc.New(onControl, iceServers)` new signature (Task 5) used by `startSession` (Task 5) and indirectly by `handleRTC`/`DialSignal`. `startSession`/`stopAttachment`/`rtcDispatch` reused unchanged from 3a. Broker `Config{STUNURLs,TURNURLs,TURNSecret,TURNTTL,GrantTURN,Now}` consistent across Tasks 3–4.

**Granularity / TDD:** Tasks 1–3 and the seams of 5–6 are test-first with real assertions. Broker is integration-tested with live WS clients (Task 3), matching the codebase's no-mock-for-live-components style. The parts needing real `idb_companion`/`ffmpeg`/NAT (attach pump, relay) are verified live in Task 9 with explicit run/observe steps, and genuinely un-localhost-able scenarios are flagged deploy-only rather than faked.

---

## Execution Handoff

After this plan, **Phase 4** (GoReleaser + Homebrew, deployed managed signaling + coturn, real accounts/billing replacing the `--grant-turn` stub) builds on this foundation.
