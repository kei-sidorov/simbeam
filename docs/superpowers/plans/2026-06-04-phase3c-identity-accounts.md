# Phase 3C — Persistent Pairing, Key-Accounts & Subscriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pair a browser/iPad with a Mac once by exchanging permanent Ed25519 keys, then reconnect automatically by daemon identity (no QR), with a minimal SQLite subscription layer that gates TURN by a real (client-asserted) subscription instead of the `--grant-turn` stub.

**Architecture:** Three changes over Phase 3b. (1) The daemon gains a long-lived on-disk identity (`daemonID` = its Ed25519 pubkey) plus a pinned-clients set; it holds a *persistent* outbound WS to the broker and serves reconnecting clients one at a time, with an interactive **P-keypress** enrollment window minting a one-time secret `S`. (2) A mutual challenge-response runs before the WebRTC offer/answer: the broker authenticates the client's key for the TURN gate, the daemon verifies the client is pinned (or enrolling via `S`), and the existing signed answer (#54) proves the daemon to the client. (3) A new `internal/store` (SQLite behind a `Store` interface, pure-Go `modernc.org/sqlite`) backs a two-signature `POST /v1/subscription` endpoint on the broker; the TURN gate reads it by the already-verified `client_pubkey`.

**Tech Stack:** Go 1.25, `crypto/ed25519`, `crypto/hmac`+`sha256`, `gorilla/websocket`, `pion/webrtc/v4`, `modernc.org/sqlite` (new), `golang.org/x/term` (new), browser WebCrypto (Ed25519 + HMAC-SHA256) + `localStorage`.

**Read first:** `internal/signal/{auth,turn,pairing,message}.go`, `internal/signalbroker/broker.go`, `internal/server/{remote,rtc,session}.go`, `internal/server/remote_integration_test.go`, `web/debug/index.html`, `docs/decisions.md` (latest entry is #54; 3C adds #55+).

**Wire-contract invariants (every signature must agree across Go ↔ browser):**
- Nonces and the enrollment secret `S` are opaque base64/UTF-8 **strings**; signatures are computed over the **UTF-8/ASCII bytes of the string**, never the decoded bytes. Go `[]byte(nonce)` ≡ browser `new TextEncoder().encode(nonce)`.
- `EnrollProof` = `base64(HMAC-SHA256(S, clientPubKey ‖ 0x00 ‖ nonce))`.
- `CanonicalSubscription` = `clientPubKey ‖ 0x1f ‖ productID ‖ 0x1f ‖ expiresAt ‖ 0x1f ‖ issuedAt` (UTF-8 bytes; `0x1f` unit separator).
- Ed25519 pubkeys are base64 **StdEncoding**; `daemonID` == daemon pubkey (same string).

---

## File Structure

**New files:**
- `internal/server/identity.go` — load/create the daemon's on-disk Ed25519 identity (0600).
- `internal/server/pinned.go` — pinned-clients set persisted to JSON (0600).
- `internal/server/pairing_window.go` — one-time, TTL'd, single-use enrollment window.
- `internal/signal/enroll.go` — `NewNonce`, `NewPairingSecret`, `EnrollProof`/`VerifyEnrollProof`.
- `internal/signal/subscription.go` — `CanonicalSubscription`, `AppSig`/`VerifyAppSig`.
- `internal/store/store.go` — `Subscription` + `Store` interface.
- `internal/store/sqlite.go` — `modernc.org/sqlite` implementation + schema migration.
- `internal/signalbroker/subscription.go` — `POST /v1/subscription` + `GET /v1/subscription/me` + CORS.

**Modified files:**
- `internal/signal/message.go` — new types (`connect`/`challenge`/`proof`) + fields (`Daemon`, `Nonce`, `BrokerNonce`, `Pair`, `BrokerSig`).
- `internal/signal/pairing.go` — `PairingURL` fragment keys `signal`/`daemon`/`pair`; drop `NewToken`.
- `internal/signalbroker/broker.go` — persistent presence by `daemonID`, challenge/proof relay, gate via `Store`; `Config` gains `Store`+`AppSecret`, loses `GrantTURN`.
- `internal/server/remote.go` — replace one-shot `DialSignal` with persistent `ServeSignal`/`serveOnce`.
- `internal/server/remote_integration_test.go` — rewrite for the 3C handshake.
- `cmd/simbeam-signal/main.go` — `--db`, `SIMCAST_APP_SECRET`, drop `--grant-turn`.
- `cmd/simbeamd/main.go` — persistent serve, P-keypress pairing, identity/clients flags, `unpair` subcommand.
- `web/debug/index.html` — WebCrypto identity, enrollment, my-Macs, auto-reconnect, subscription panel, TURN indicator.
- `docs/decisions.md`, `README.md`, `docs/ROADMAP.md` — record decisions #55+ and Phase 3C status.

---

### Task 1: Daemon on-disk identity

**Files:**
- Create: `internal/server/identity.go`
- Test: `internal/server/identity_test.go`

- [ ] **Step 1: Write the failing test**

```go
package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateIdentity_CreatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "identity.key")
	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id.PubB64 == "" || id.Priv == nil {
		t.Fatalf("empty identity: %+v", id)
	}
	// File exists with 0600 perms.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
	// Second load returns the SAME public key (stable identity).
	id2, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id2.PubB64 != id.PubB64 {
		t.Fatalf("pubkey changed across loads: %q != %q", id2.PubB64, id.PubB64)
	}
}

func TestLoadOrCreateIdentity_RejectsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(path, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(path); err == nil {
		t.Fatalf("want error on malformed key file, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestLoadOrCreateIdentity -v`
Expected: FAIL — `undefined: LoadOrCreateIdentity`.

- [ ] **Step 3: Write minimal implementation**

```go
package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
)

// Identity is the daemon's long-lived Ed25519 keypair. PubB64 (base64 std) is
// the daemonID: simultaneously the stable address on the broker and the crypto
// credential a paired client pins (anti-MITM).
type Identity struct {
	PubB64 string
	Priv   ed25519.PrivateKey
}

// LoadOrCreateIdentity loads the daemon key from path, or generates and persists
// a fresh one (0600) on first run. The file stores the 64-byte Ed25519 private
// key as base64 (std); the public key is derived from it.
func LoadOrCreateIdentity(path string) (Identity, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		raw, derr := base64.StdEncoding.DecodeString(string(data))
		if derr != nil || len(raw) != ed25519.PrivateKeySize {
			return Identity{}, errors.New("identity: malformed key file")
		}
		priv := ed25519.PrivateKey(raw)
		pub := priv.Public().(ed25519.PublicKey)
		return Identity{PubB64: base64.StdEncoding.EncodeToString(pub), Priv: priv}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	pub, priv, gerr := ed25519.GenerateKey(rand.Reader)
	if gerr != nil {
		return Identity{}, gerr
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return Identity{}, mkErr
	}
	enc := base64.StdEncoding.EncodeToString(priv)
	if werr := os.WriteFile(path, []byte(enc), 0o600); werr != nil {
		return Identity{}, werr
	}
	return Identity{PubB64: base64.StdEncoding.EncodeToString(pub), Priv: priv}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/ -run TestLoadOrCreateIdentity -v`
Expected: PASS (both subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/server/identity.go internal/server/identity_test.go
git commit -m "feat(server): persistent on-disk daemon identity (daemonID = pubkey)"
```

---

### Task 2: Pinned-clients store

**Files:**
- Create: `internal/server/pinned.go`
- Test: `internal/server/pinned_test.go`

- [ ] **Step 1: Write the failing test**

```go
package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedStore_AddContainsRemovePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "clients.json")
	ps, err := LoadPinnedStore(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if ps.Contains("k1") {
		t.Fatalf("empty store should not contain k1")
	}
	if err := ps.Add("k1", "iPad"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !ps.Contains("k1") {
		t.Fatalf("k1 missing after add")
	}

	// Persisted with 0600 and reloads.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
	reloaded, err := LoadPinnedStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Contains("k1") {
		t.Fatalf("k1 missing after reload")
	}

	// Revocation removes and persists.
	if err := reloaded.Remove("k1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	again, _ := LoadPinnedStore(path)
	if again.Contains("k1") {
		t.Fatalf("k1 present after remove+reload")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestPinnedStore -v`
Expected: FAIL — `undefined: LoadPinnedStore`.

- [ ] **Step 3: Write minimal implementation**

```go
package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// PinnedClient is an enrolled client allowed to reconnect. Name is optional UI sugar.
type PinnedClient struct {
	PubKey string `json:"pubkey"`
	Name   string `json:"name,omitempty"`
}

// PinnedStore is the daemon's set of enrolled client public keys, persisted to a
// JSON file (0600). Safe for concurrent use. Revocation = Remove (local, no server).
type PinnedStore struct {
	path string
	mu   sync.Mutex
	set  map[string]PinnedClient
}

// LoadPinnedStore reads the set from path; a missing file yields an empty store.
func LoadPinnedStore(path string) (*PinnedStore, error) {
	ps := &PinnedStore{path: path, set: map[string]PinnedClient{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ps, nil
		}
		return nil, err
	}
	var list []PinnedClient
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, c := range list {
		ps.set[c.PubKey] = c
	}
	return ps, nil
}

// Contains reports whether pubKey is enrolled.
func (ps *PinnedStore) Contains(pubKey string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	_, ok := ps.set[pubKey]
	return ok
}

// Add enrolls pubKey (idempotent) and persists.
func (ps *PinnedStore) Add(pubKey, name string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.set[pubKey] = PinnedClient{PubKey: pubKey, Name: name}
	return ps.save()
}

// Remove revokes pubKey and persists.
func (ps *PinnedStore) Remove(pubKey string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.set, pubKey)
	return ps.save()
}

// save writes the set atomically-ish (caller holds mu).
func (ps *PinnedStore) save() error {
	list := make([]PinnedClient, 0, len(ps.set))
	for _, c := range ps.set {
		list = append(list, c)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ps.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(ps.path, data, 0o600)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/ -run TestPinnedStore -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/pinned.go internal/server/pinned_test.go
git commit -m "feat(server): pinned-clients store (add/remove/contains, JSON 0600)"
```

---

### Task 3: Enrollment & challenge crypto primitives

**Files:**
- Create: `internal/signal/enroll.go`
- Test: `internal/signal/enroll_test.go`

- [ ] **Step 1: Write the failing test**

```go
package signal

import "testing"

func TestEnrollProof_RoundTrip(t *testing.T) {
	const secret = "s3cr3t"
	const pub = "CLIENTPUBKEYBASE64=="
	nonce, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	proof := EnrollProof(secret, pub, nonce)
	if !VerifyEnrollProof(secret, pub, nonce, proof) {
		t.Fatalf("valid proof rejected")
	}
	// Wrong secret, pubkey, or nonce must all fail.
	if VerifyEnrollProof("wrong", pub, nonce, proof) {
		t.Fatalf("accepted wrong secret")
	}
	if VerifyEnrollProof(secret, "other", nonce, proof) {
		t.Fatalf("accepted wrong pubkey")
	}
	other, _ := NewNonce()
	if VerifyEnrollProof(secret, pub, other, proof) {
		t.Fatalf("accepted wrong nonce")
	}
}

func TestNoncesAndSecretsAreRandom(t *testing.T) {
	n1, _ := NewNonce()
	n2, _ := NewNonce()
	if n1 == n2 || n1 == "" {
		t.Fatalf("nonces not random/unique: %q %q", n1, n2)
	}
	s1, _ := NewPairingSecret()
	s2, _ := NewPairingSecret()
	if s1 == s2 || s1 == "" {
		t.Fatalf("secrets not random/unique: %q %q", s1, s2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signal/ -run 'TestEnrollProof|TestNonces' -v`
Expected: FAIL — `undefined: NewNonce`.

- [ ] **Step 3: Write minimal implementation**

```go
package signal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// NewNonce returns 16 random bytes, base64 (std). Used for the mutual
// challenge-response and to bind an enrollment proof to a single attempt.
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// NewPairingSecret returns a short one-time enrollment secret S: 9 random bytes,
// base64 URL (no padding) → 12 chars, easy to carry in a URL fragment or QR.
func NewPairingSecret() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// EnrollProof = base64(HMAC-SHA256(S, clientPubKey ‖ 0x00 ‖ nonce)). The client
// proves knowledge of the one-time secret S to the daemon WITHOUT revealing S to
// the untrusted broker; the nonce binds the proof to one attempt. The 0x00
// separator is part of the frozen wire contract (browser must match exactly).
func EnrollProof(secret, clientPubKey, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(clientPubKey))
	mac.Write([]byte{0})
	mac.Write([]byte(nonce))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyEnrollProof recomputes EnrollProof and compares in constant time.
func VerifyEnrollProof(secret, clientPubKey, nonce, proofB64 string) bool {
	want := EnrollProof(secret, clientPubKey, nonce)
	return hmac.Equal([]byte(want), []byte(proofB64))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/signal/ -run 'TestEnrollProof|TestNonces' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/signal/enroll.go internal/signal/enroll_test.go
git commit -m "feat(signal): nonce/secret gen + HMAC enrollment proof primitives"
```

---

### Task 4: Pairing window (TTL, single-use)

**Files:**
- Create: `internal/server/pairing_window.go`
- Test: `internal/server/pairing_window_test.go`

- [ ] **Step 1: Write the failing test**

```go
package server

import (
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

func TestPairingWindow_VerifyConsumesSingleUse(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	const pub = "CLIENTPUB=="
	var w pairingWindow
	w.open("S-secret", now, 5*time.Minute)

	nonce, _ := signal.NewNonce()
	proof := signal.EnrollProof("S-secret", pub, nonce)

	if !w.verify(pub, nonce, proof, now.Add(time.Minute)) {
		t.Fatalf("valid proof inside window rejected")
	}
	// Single-use: a second valid proof must fail (window consumed).
	if w.verify(pub, nonce, proof, now.Add(time.Minute)) {
		t.Fatalf("window accepted a second use")
	}
}

func TestPairingWindow_RejectsExpiredClosedAndBadProof(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	const pub = "CLIENTPUB=="
	nonce, _ := signal.NewNonce()

	// Closed window (never opened).
	var closed pairingWindow
	if closed.verify(pub, nonce, signal.EnrollProof("x", pub, nonce), now) {
		t.Fatalf("closed window accepted a proof")
	}

	// Expired.
	var w pairingWindow
	w.open("S", now, 5*time.Minute)
	proof := signal.EnrollProof("S", pub, nonce)
	if w.verify(pub, nonce, proof, now.Add(6*time.Minute)) {
		t.Fatalf("expired window accepted a proof")
	}

	// Bad proof inside a fresh window.
	var w2 pairingWindow
	w2.open("S", now, 5*time.Minute)
	if w2.verify(pub, nonce, "AAAA", now) {
		t.Fatalf("accepted a bad proof")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestPairingWindow -v`
Expected: FAIL — `undefined: pairingWindow`.

- [ ] **Step 3: Write minimal implementation**

```go
package server

import (
	"sync"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

// pairingWindow is a one-time, time-boxed enrollment authorization. While open,
// unexpired, and unused, a not-yet-pinned client that proves knowledge of the
// secret S may be enrolled. Outside the window the daemon accepts no new pins
// (anti-abuse). now is a parameter everywhere for deterministic tests.
type pairingWindow struct {
	mu      sync.Mutex
	secret  string
	expires time.Time
	used    bool
}

// open arms the window with secret S valid for ttl from now.
func (p *pairingWindow) open(secret string, now time.Time, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secret = secret
	p.expires = now.Add(ttl)
	p.used = false
}

// verify reports whether proof authorizes pinning clientPubKey right now, and on
// success consumes the window (single-use) so a replay cannot enroll again.
func (p *pairingWindow) verify(clientPubKey, nonce, proof string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.secret == "" || p.used || now.After(p.expires) {
		return false
	}
	if !signal.VerifyEnrollProof(p.secret, clientPubKey, nonce, proof) {
		return false
	}
	p.used = true
	p.secret = ""
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/ -run TestPairingWindow -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/pairing_window.go internal/server/pairing_window_test.go
git commit -m "feat(server): one-time TTL'd single-use pairing window"
```

---

### Task 5: Subscription canonicalization + app-secret signature

**Files:**
- Create: `internal/signal/subscription.go`
- Test: `internal/signal/subscription_test.go`

- [ ] **Step 1: Write the failing test**

```go
package signal

import "testing"

func TestCanonicalSubscription_StableAndOrdered(t *testing.T) {
	c := CanonicalSubscription("pub", "pro.monthly", "2026-12-31T00:00:00Z", "2026-06-04T00:00:00Z")
	want := "pub\x1fpro.monthly\x1f2026-12-31T00:00:00Z\x1f2026-06-04T00:00:00Z"
	if string(c) != want {
		t.Fatalf("canonical = %q, want %q", c, want)
	}
}

func TestAppSig_RoundTrip(t *testing.T) {
	const secret = "dev-app-secret"
	canon := CanonicalSubscription("pub", "p", "e", "i")
	sig := AppSig(secret, canon)
	if !VerifyAppSig(secret, canon, sig) {
		t.Fatalf("valid app-sig rejected")
	}
	if VerifyAppSig("other", canon, sig) {
		t.Fatalf("accepted wrong secret")
	}
	if VerifyAppSig(secret, CanonicalSubscription("pub", "p", "e", "DIFFERENT"), sig) {
		t.Fatalf("accepted tampered canonical")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signal/ -run 'TestCanonical|TestAppSig' -v`
Expected: FAIL — `undefined: CanonicalSubscription`.

- [ ] **Step 3: Write minimal implementation**

```go
package signal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// CanonicalSubscription is the fixed-separator byte string both subscription
// signatures cover. Field order and the 0x1f unit separator are a frozen wire
// contract (the browser builds the identical bytes). It is fed to the weak
// app-secret HMAC AND the real Ed25519 account signature.
func CanonicalSubscription(clientPubKey, productID, expiresAt, issuedAt string) []byte {
	const sep = "\x1f"
	return []byte(clientPubKey + sep + productID + sep + expiresAt + sep + issuedAt)
}

// AppSig = base64(HMAC-SHA256(appSecret, canonical)). HONESTLY this is
// obfuscation, not a crypto boundary — appSecret is extractable by reversing the
// client binary. The real "is this the account" auth is the Ed25519 account
// signature (signal.Verify). The strong "did they pay" boundary is a future
// Apple-receipt check that drops into the same endpoint by flipping source.
func AppSig(appSecret string, canonical []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(canonical)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyAppSig recomputes AppSig and compares in constant time.
func VerifyAppSig(appSecret string, canonical []byte, sigB64 string) bool {
	want := AppSig(appSecret, canonical)
	return hmac.Equal([]byte(want), []byte(sigB64))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/signal/ -run 'TestCanonical|TestAppSig' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/signal/subscription.go internal/signal/subscription_test.go
git commit -m "feat(signal): subscription canonicalization + app-secret HMAC"
```

---

### Task 6: Signaling wire types for the 3C handshake

**Files:**
- Modify: `internal/signal/message.go`
- Test: `internal/signal/message_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
package signal

import (
	"encoding/json"
	"testing"
)

func TestMsg_NewFieldsRoundTrip(t *testing.T) {
	in := Msg{
		Type:        TypeChallenge,
		Daemon:      "DAEMONID==",
		PubKey:      "CLIENTPUB==",
		Nonce:       "n1",
		BrokerNonce: "bn1",
		Pair:        "proofB64",
		Sig:         "sigB64",
		BrokerSig:   "bsigB64",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Msg
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
	// New message-type constants exist and are distinct.
	if TypeConnect == TypeChallenge || TypeChallenge == TypeProof {
		t.Fatalf("handshake type constants collide")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signal/ -run TestMsg_NewFields -v`
Expected: FAIL — `undefined: TypeChallenge` / `in.Daemon undefined`.

- [ ] **Step 3: Edit `internal/signal/message.go`**

Add three message-type constants after the existing `TypeError` block (keep `TypeRegister`/`TypeJoin`/`TypeICEServers`/`TypeOffer`/`TypeAnswer`/`TypePeerLeft`/`TypeError`):

```go
// 3C handshake additions: a mutual challenge-response runs before offer/answer.
const (
	TypeConnect   = "connect"   // broker → daemon: a client wants in (carries client pubkey + optional enrollment proof)
	TypeChallenge = "challenge" // daemon → broker → client: nonce to sign; broker adds BrokerNonce for its own gate
	TypeProof     = "proof"     // client → broker → daemon: Sig over daemon nonce (+ BrokerSig over broker nonce, stripped by broker)
)
```

Then extend the `Msg` struct with the new fields (append inside the struct, keeping the existing fields):

```go
	Daemon      string `json:"daemon,omitempty"`      // register/join: daemonID (= daemon Ed25519 pubkey, base64)
	Nonce       string `json:"nonce,omitempty"`       // join: client nonce binding the enroll proof; challenge: daemon nonce to sign
	BrokerNonce string `json:"brokerNonce,omitempty"` // challenge: broker nonce the client signs so the broker can gate TURN
	Pair        string `json:"pair,omitempty"`        // join: HMAC-SHA256(S, clientPubKey‖0x00‖nonce) enrollment proof
	BrokerSig   string `json:"brokerSig,omitempty"`   // proof: client Ed25519 signature over BrokerNonce (verified+stripped by broker)
```

> `PubKey` is reused for the **client** pubkey on `join`/`connect` (it carried the daemon pubkey on `register` in 3b; the daemon now uses `Daemon` for that). `Sig` is reused for the client's signature over the daemon `Nonce` on `proof` (and still the answer signature on `answer`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/signal/ -run TestMsg_NewFields -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/signal/message.go internal/signal/message_test.go
git commit -m "feat(signal): wire types for the 3C mutual challenge-response"
```

---

### Task 7: PairingURL fragment keys (daemon/pair); drop NewToken

**Files:**
- Modify: `internal/signal/pairing.go`
- Modify: `internal/signal/pairing_test.go`

- [ ] **Step 1: Rewrite the test**

Replace the entire contents of `internal/signal/pairing_test.go` with:

```go
package signal

import (
	"net/url"
	"strings"
	"testing"
)

func TestPairingURL_FragmentCarriesSignalDaemonPair(t *testing.T) {
	got := PairingURL("http://localhost:8080/", "wss://host/ws", "DAEMONPUB==", "S3cr3t")
	if !strings.HasPrefix(got, "http://localhost:8080/#") {
		t.Fatalf("missing client base + fragment: %q", got)
	}
	frag := got[strings.Index(got, "#")+1:]
	v, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatalf("parse fragment: %v", err)
	}
	if v.Get("signal") != "wss://host/ws" {
		t.Fatalf("signal = %q", v.Get("signal"))
	}
	if v.Get("daemon") != "DAEMONPUB==" {
		t.Fatalf("daemon = %q", v.Get("daemon"))
	}
	if v.Get("pair") != "S3cr3t" {
		t.Fatalf("pair = %q", v.Get("pair"))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signal/ -run TestPairingURL -v`
Expected: FAIL — `PairingURL` signature mismatch / still references `token`.

- [ ] **Step 3: Rewrite `internal/signal/pairing.go`**

Replace the whole file with (note: `NewToken` is removed — 3C uses the stable `daemonID` as the address and `NewPairingSecret` for `S`):

```go
package signal

import (
	"net/url"
)

// PairingURL builds the browser link the daemon prints when its enrollment
// window is open. The coordinates go in the URL *fragment* so they never reach
// the client web server's request line or logs:
//
//	<clientBase>#signal=<wss-url>&daemon=<daemonPubKey>&pair=<S>
//
// daemonPubKey (== daemonID) lets the client pin the Mac (anti-MITM); S is the
// one-time enrollment secret proving the client is authorized to be pinned.
func PairingURL(clientBase, signalingURL, daemonPubKey, secret string) string {
	frag := url.Values{}
	frag.Set("signal", signalingURL)
	frag.Set("daemon", daemonPubKey)
	frag.Set("pair", secret)
	return clientBase + "#" + frag.Encode()
}
```

- [ ] **Step 4: Run test + full signal package to verify it passes**

Run: `go test ./internal/signal/ -v`
Expected: PASS (any reference to the removed `NewToken` now lives only in `cmd/`, fixed in Task 12 — `internal/signal` compiles clean here).

- [ ] **Step 5: Commit**

```bash
git add internal/signal/pairing.go internal/signal/pairing_test.go
git commit -m "feat(signal): PairingURL fragment keys signal/daemon/pair; drop NewToken"
```

---

### Task 8: Subscriptions store (SQLite behind a Store interface)

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/sqlite.go`
- Test: `internal/store/sqlite_test.go`
- Modify: `go.mod` / `go.sum` (add `modernc.org/sqlite`)

- [ ] **Step 1: Add the pure-Go SQLite driver**

Run: `go get modernc.org/sqlite@latest`
Expected: `go.mod` gains `require modernc.org/sqlite vX.Y.Z`.

- [ ] **Step 2: Write the failing test**

```go
package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "subs.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLite_UpsertGetActive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	// Absent → inactive, not found.
	if active, err := s.Active(ctx, "pub", now); err != nil || active {
		t.Fatalf("absent active=%v err=%v, want false/nil", active, err)
	}
	if _, ok, err := s.Get(ctx, "pub"); err != nil || ok {
		t.Fatalf("absent get ok=%v err=%v", ok, err)
	}

	// Insert a future expiry → active.
	sub := Subscription{
		ClientPubKey: "pub", ProductID: "pro.monthly",
		ExpiresAt: "2026-12-31T00:00:00Z", IssuedAt: "2026-06-04T00:00:00Z",
		Source: "client", UpdatedAt: "2026-06-04T12:00:00Z",
	}
	if err := s.Upsert(ctx, sub); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if active, err := s.Active(ctx, "pub", now); err != nil || !active {
		t.Fatalf("active=%v err=%v, want true/nil", active, err)
	}

	// Expired expiry → inactive.
	past := Subscription{
		ClientPubKey: "pub", ProductID: "pro.monthly",
		ExpiresAt: "2026-01-01T00:00:00Z", IssuedAt: "2026-06-05T00:00:00Z",
		Source: "client", UpdatedAt: "2026-06-05T00:00:00Z",
	}
	if err := s.Upsert(ctx, past); err != nil {
		t.Fatalf("upsert past: %v", err)
	}
	if active, err := s.Active(ctx, "pub", now); err != nil || active {
		t.Fatalf("after expiry active=%v err=%v, want false", active, err)
	}
}

func TestSQLite_LastWriteWinsByIssuedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	newer := Subscription{ClientPubKey: "pub", ProductID: "p", ExpiresAt: "2026-12-31T00:00:00Z", IssuedAt: "2026-06-10T00:00:00Z", Source: "client", UpdatedAt: "2026-06-10T00:00:00Z"}
	if err := s.Upsert(ctx, newer); err != nil {
		t.Fatal(err)
	}
	// An OLDER issued_at must be ignored (out-of-order report).
	older := Subscription{ClientPubKey: "pub", ProductID: "p", ExpiresAt: "2026-07-01T00:00:00Z", IssuedAt: "2026-06-01T00:00:00Z", Source: "client", UpdatedAt: "2026-06-11T00:00:00Z"}
	if err := s.Upsert(ctx, older); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "pub")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.ExpiresAt != "2026-12-31T00:00:00Z" {
		t.Fatalf("older write clobbered newer: expires=%q", got.ExpiresAt)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `undefined: OpenSQLite`.

- [ ] **Step 4: Write `internal/store/store.go`**

```go
// Package store holds the simbeam subscription persistence behind a thin Store
// interface. The only durable server state is the subscriptions table; daemon
// keys and the list of paired Macs live on the endpoints (decision: minimal DB).
// SQLite now, Postgres later by swapping the implementation behind Store.
package store

import (
	"context"
	"time"
)

// Subscription is one row: a subscription bound to a client public key (the same
// key used for pairing). Dates are RFC3339 strings (normalized to UTC by the
// endpoint) so string comparison and portability hold across SQLite/Postgres.
type Subscription struct {
	ClientPubKey string
	ProductID    string
	ExpiresAt    string // RFC3339 UTC; from StoreKit (currently client-asserted)
	IssuedAt     string // RFC3339 UTC; client report time (ordering / last-write-wins)
	Source       string // "client" now; "apple-verified" later
	UpdatedAt    string // RFC3339 UTC; server clock at write
}

// Store is the persistence boundary. now is always a parameter (never
// CURRENT_TIMESTAMP) so logic is testable and SQL stays portable.
type Store interface {
	Upsert(ctx context.Context, sub Subscription) error
	Get(ctx context.Context, clientPubKey string) (Subscription, bool, error)
	Active(ctx context.Context, clientPubKey string, now time.Time) (bool, error)
	Close() error
}
```

- [ ] **Step 5: Write `internal/store/sqlite.go`**

```go
package store

import (
	"context"
	"database/sql"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers name "sqlite" (no cgo)
)

// SQLite is the database/sql-backed Store. Portable SQL (INSERT … ON CONFLICT …
// DO UPDATE) works on both SQLite and Postgres.
type SQLite struct{ db *sql.DB }

const schema = `CREATE TABLE IF NOT EXISTS subscriptions (
  client_pubkey TEXT PRIMARY KEY,
  product_id    TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  issued_at     TEXT NOT NULL,
  source        TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);`

// OpenSQLite opens (creating if needed) the database at path and ensures schema.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// Upsert inserts or updates the row, but ONLY when the incoming issued_at is
// strictly newer than the stored one (idempotent, last-write-wins, safe to spam
// from foreground/background). RFC3339 UTC strings compare lexicographically.
func (s *SQLite) Upsert(ctx context.Context, sub Subscription) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO subscriptions (client_pubkey, product_id, expires_at, issued_at, source, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(client_pubkey) DO UPDATE SET
  product_id = excluded.product_id,
  expires_at = excluded.expires_at,
  issued_at  = excluded.issued_at,
  source     = excluded.source,
  updated_at = excluded.updated_at
WHERE excluded.issued_at > subscriptions.issued_at`,
		sub.ClientPubKey, sub.ProductID, sub.ExpiresAt, sub.IssuedAt, sub.Source, sub.UpdatedAt)
	return err
}

// Get returns the row for clientPubKey (ok=false if absent).
func (s *SQLite) Get(ctx context.Context, clientPubKey string) (Subscription, bool, error) {
	var sub Subscription
	err := s.db.QueryRowContext(ctx,
		`SELECT client_pubkey, product_id, expires_at, issued_at, source, updated_at
		   FROM subscriptions WHERE client_pubkey = ?`, clientPubKey).
		Scan(&sub.ClientPubKey, &sub.ProductID, &sub.ExpiresAt, &sub.IssuedAt, &sub.Source, &sub.UpdatedAt)
	if err == sql.ErrNoRows {
		return Subscription{}, false, nil
	}
	if err != nil {
		return Subscription{}, false, err
	}
	return sub, true, nil
}

// Active reports whether the stored expires_at is in the future relative to now
// (server clock). Absent row → not active.
func (s *SQLite) Active(ctx context.Context, clientPubKey string, now time.Time) (bool, error) {
	var expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM subscriptions WHERE client_pubkey = ?`, clientPubKey).Scan(&expiresAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	exp, perr := time.Parse(time.RFC3339, expiresAt)
	if perr != nil {
		return false, perr
	}
	return exp.After(now), nil
}

// Close releases the database handle.
func (s *SQLite) Close() error { return s.db.Close() }
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/store/ -v`
Expected: PASS (both tests).

- [ ] **Step 7: Commit**

```bash
git add internal/store/ go.mod go.sum
git commit -m "feat(store): SQLite subscriptions Store (modernc, last-write-wins upsert)"
```

---

### Task 9: Broker — persistent presence by daemonID + challenge/proof relay + Store gate

This replaces the one-shot token room model with: a daemon registered persistently under `daemonID`, a client routed to it, a relayed mutual challenge-response, and a TURN gate that reads `Store` by the broker-verified `client_pubkey`. Media still never transits the broker.

**Files:**
- Modify: `internal/signalbroker/broker.go` (full rewrite below)
- Modify: `internal/signalbroker/broker_test.go` (full rewrite below)

**Handshake the broker mediates (one client at a time per daemon):**
1. Daemon: `register{role:daemon, daemon:<daemonID>}` → broker stores `daemons[daemonID]=conn`, keeps reading.
2. Client: `join{role:client, daemon:<daemonID>, pubkey:<clientPubKey>, nonce?:<cNonce>, pair?:<proof>}`. Daemon offline → `error`. Else broker mints `bNonce`, forwards `connect{pubkey, nonce, pair}` to the daemon.
3. Daemon decides allow (pinned OR enroll proof valid), mints `dNonce`, sends `challenge{nonce:dNonce}`. Broker adds `brokerNonce:bNonce` and forwards to client.
4. Client returns `proof{sig:Sign(dNonce), brokerSig:Sign(bNonce)}`. Broker **verifies `brokerSig` over `bNonce` against `clientPubKey`** (now the client key is authenticated for gating), sends `iceServers` (TURN gated by `Store.Active(clientPubKey)`) to both, and relays `proof{sig}` (brokerSig stripped) to the daemon for its own pinned check.
5. Client → `offer`; daemon → signed `answer` (existing #54, proves the daemon to the client). Done; the daemon WS stays open for the next client.

- [ ] **Step 1: Rewrite `internal/signalbroker/broker.go`**

```go
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
	Now        func() time.Time
}

// Broker holds live daemon presence.
type Broker struct {
	cfg     Config
	up      websocket.Upgrader
	mu      sync.Mutex
	daemons map[string]*daemonConn // daemonID → registered daemon
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
		cfg:     cfg,
		up:      websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		daemons: map[string]*daemonConn{},
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
	if b.cfg.Store != nil && len(b.cfg.TURNURLs) > 0 {
		ok, err := b.cfg.Store.Active(context.Background(), clientPubKey, b.cfg.Now())
		granted = err == nil && ok
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
	default:
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "first message must be register or join"})
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
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "device offline — wake your Mac"})
		return
	}

	bNonce, err := signal.NewNonce()
	if err != nil {
		_ = c.send(signal.Msg{Type: signal.TypeError, Msg: "broker nonce error"})
		return
	}
	cl := &clientConn{c: c, pubKey: join.PubKey, bNonce: bNonce}
	d.mu.Lock()
	d.client = cl // one client at a time; a new client replaces the previous
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		if d.client == cl {
			d.client = nil
		}
		d.mu.Unlock()
		_ = d.c.send(signal.Msg{Type: signal.TypePeerLeft})
	}()

	// Ask the daemon to start the challenge (carry enrollment proof if present).
	_ = d.c.send(signal.Msg{Type: signal.TypeConnect, PubKey: join.PubKey, Nonce: join.Nonce, Pair: join.Pair})

	for {
		var m signal.Msg
		if err := c.ws.ReadJSON(&m); err != nil {
			return
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
```

- [ ] **Step 2: Rewrite `internal/signalbroker/broker_test.go`**

```go
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
		// Daemon must receive the relayed proof (brokerSig stripped).
		pr := readMsg(t, daemon)
		if pr.Type != signal.TypeProof || pr.BrokerSig != "" || pr.Sig == "" {
			t.Fatalf("daemon want stripped proof, got %+v", pr)
		}
		// Client receives iceServers.
		ice := readMsg(t, client)
		if ice.Type != signal.TypeICEServers {
			t.Fatalf("client want iceServers, got %+v", ice)
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
```

- [ ] **Step 3: Run the broker tests**

Run: `go test ./internal/signalbroker/ -run 'TestClientWithoutDaemon|TestHandshakeRelayAndGate|TestBadBrokerSig' -v`
Expected: PASS. (The subscription-API tests are added in Task 10.)

- [ ] **Step 4: Commit**

```bash
git add internal/signalbroker/broker.go internal/signalbroker/broker_test.go
git commit -m "feat(broker): persistent daemonID presence, challenge/proof relay, Store TURN gate"
```

---

### Task 10: Broker — subscription HTTP API (two signatures, idempotent, CORS)

**Files:**
- Create: `internal/signalbroker/subscription.go`
- Create: `internal/signalbroker/subscription_test.go`

The browser bench is served from `localhost:8080` and the broker from `localhost:9000`, so the POST is cross-origin with custom headers → it needs CORS + an OPTIONS preflight.

- [ ] **Step 1: Write the failing test**

```go
package signalbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/kei-sidorov/simbeam/internal/store"
)

func TestSubscriptionEndpoint_TwoSigUpsertAndGate(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenSQLite(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const appSecret = "dev-app-secret"
	b := New(Config{Store: st, AppSecret: appSecret, Now: func() time.Time { return now }})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	pub, priv, _ := signal.GenerateKeyPair()
	product := "pro.monthly"
	expires := "2026-12-31T00:00:00Z"
	issued := now.UTC().Format(time.RFC3339)
	canon := signal.CanonicalSubscription(pub, product, expires, issued)

	post := func(appSig, accSig string) int {
		body, _ := json.Marshal(map[string]string{
			"client_pubkey": pub, "product_id": product, "expires_at": expires, "issued_at": issued,
		})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/subscription", bytes.NewReader(body))
		req.Header.Set("X-App-Sig", appSig)
		req.Header.Set("X-Account-Sig", accSig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	goodApp := signal.AppSig(appSecret, canon)
	goodAcc := signal.Sign(priv, canon)

	// Bad app sig → 401, nothing stored.
	if code := post("bad", goodAcc); code != http.StatusUnauthorized {
		t.Fatalf("bad app sig: code=%d want 401", code)
	}
	// Bad account sig → 401.
	if code := post(goodApp, "bad"); code != http.StatusUnauthorized {
		t.Fatalf("bad account sig: code=%d want 401", code)
	}
	// Both good → 200 and subscription becomes active.
	if code := post(goodApp, goodAcc); code != http.StatusOK {
		t.Fatalf("good: code=%d want 200", code)
	}
	if active, _ := st.Active(context.Background(), pub, now); !active {
		t.Fatalf("subscription not active after valid POST")
	}
}

func TestSubscriptionEndpoint_CORSPreflight(t *testing.T) {
	b := New(Config{Store: nil})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/subscription", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight code=%d want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("missing CORS allow-origin on preflight")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signalbroker/ -run TestSubscriptionEndpoint -v`
Expected: FAIL — `b.handleSubscription undefined` (referenced by `Handler` from Task 9; the methods don't exist yet, so the package won't compile — that is the failing state).

- [ ] **Step 3: Write `internal/signalbroker/subscription.go`**

```go
package signalbroker

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/kei-sidorov/simbeam/internal/store"
)

// replayWindow bounds how far issued_at may drift from the server clock. Generous
// because client clocks vary; with a static APP_SECRET this is hygiene, not a
// strong replay defense (the real boundary is a future Apple-receipt check).
const replayWindow = 48 * time.Hour

type subRequest struct {
	ClientPubKey string `json:"client_pubkey"`
	ProductID    string `json:"product_id"`
	ExpiresAt    string `json:"expires_at"`
	IssuedAt     string `json:"issued_at"`
}

// cors sets permissive headers and short-circuits OPTIONS preflight (the bench
// is cross-origin: served from :8080, broker on :9000, custom auth headers).
func cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-App-Sig, X-Account-Sig")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// handleSubscription verifies BOTH signatures (weak app-secret HMAC + real
// Ed25519 account signature) over the canonical fields, then idempotently upserts
// (last-write-wins by issued_at). Always 200 on valid signatures, whether it
// wrote or no-op'd — safe to spam from foreground/background.
func (b *Broker) handleSubscription(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if b.cfg.Store == nil {
		http.Error(w, "no store configured", http.StatusServiceUnavailable)
		return
	}
	var req subRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	canon := signal.CanonicalSubscription(req.ClientPubKey, req.ProductID, req.ExpiresAt, req.IssuedAt)
	if !signal.VerifyAppSig(b.cfg.AppSecret, canon, r.Header.Get("X-App-Sig")) {
		http.Error(w, "bad app signature", http.StatusUnauthorized)
		return
	}
	if !signal.Verify(req.ClientPubKey, canon, r.Header.Get("X-Account-Sig")) {
		http.Error(w, "bad account signature", http.StatusUnauthorized)
		return
	}
	issued, err := time.Parse(time.RFC3339, req.IssuedAt)
	if err != nil {
		http.Error(w, "bad issued_at", http.StatusBadRequest)
		return
	}
	now := b.cfg.Now()
	if issued.Before(now.Add(-replayWindow)) || issued.After(now.Add(replayWindow)) {
		http.Error(w, "issued_at outside accepted window", http.StatusBadRequest)
		return
	}
	exp, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		http.Error(w, "bad expires_at", http.StatusBadRequest)
		return
	}
	_ = b.cfg.Store.Upsert(r.Context(), store.Subscription{
		ClientPubKey: req.ClientPubKey,
		ProductID:    req.ProductID,
		ExpiresAt:    exp.UTC().Format(time.RFC3339),
		IssuedAt:     issued.UTC().Format(time.RFC3339),
		Source:       "client",
		UpdatedAt:    now.UTC().Format(time.RFC3339),
	})
	w.WriteHeader(http.StatusOK)
}

// handleSubscriptionMe returns the caller's current subscription so the bench can
// show state. Auth: an Ed25519 signature over "<pubkey>\x1f<ts>" proves key
// ownership; ts must be fresh. Optional convenience (the canonical inspection is
// the SQLite file itself).
func (b *Broker) handleSubscriptionMe(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if b.cfg.Store == nil {
		http.Error(w, "no store configured", http.StatusServiceUnavailable)
		return
	}
	pub := r.URL.Query().Get("pubkey")
	ts := r.URL.Query().Get("ts")
	sig := r.URL.Query().Get("sig")
	canon := []byte(pub + "\x1f" + ts)
	if pub == "" || !signal.Verify(pub, canon, sig) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if when, err := time.Parse(time.RFC3339, ts); err != nil || when.Before(b.cfg.Now().Add(-replayWindow)) || when.After(b.cfg.Now().Add(replayWindow)) {
		http.Error(w, "stale ts", http.StatusUnauthorized)
		return
	}
	sub, ok, err := b.cfg.Store.Get(r.Context(), pub)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no subscription", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"client_pubkey": sub.ClientPubKey, "product_id": sub.ProductID,
		"expires_at": sub.ExpiresAt, "issued_at": sub.IssuedAt,
		"source": sub.Source, "updated_at": sub.UpdatedAt,
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/signalbroker/ -v`
Expected: PASS (Task 9 + Task 10 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/signalbroker/subscription.go internal/signalbroker/subscription_test.go
git commit -m "feat(broker): two-signature /v1/subscription API + /me + CORS"
```

---

### Task 11: `cmd/simbeam-signal` — `--db`, `SIMCAST_APP_SECRET`, drop `--grant-turn`

**Files:**
- Modify: `cmd/simbeam-signal/main.go`

- [ ] **Step 1: Rewrite `cmd/simbeam-signal/main.go`**

```go
// Command simbeam-signal is the reference simbeam signaling broker: a thin WSS
// rendezvous that keeps a daemon present by daemonID, relays the mutual
// challenge-response + one offer→answer, issues iceServers (STUN always; TURN
// only when the client's subscription is active), and serves the subscription
// API. Media never transits it. The managed/production broker is the open-core
// moat (decisions #9, #47); this build is for local dev and self-host.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signalbroker"
	"github.com/kei-sidorov/simbeam/internal/store"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs (handed to everyone)")
	turn := flag.String("turn", "", "comma-separated TURN URLs (handed only to active subscribers)")
	turnSecret := flag.String("turn-secret", "", "coturn static-auth-secret for ephemeral credentials")
	turnTTL := flag.Duration("turn-ttl", time.Minute, "ephemeral TURN credential lifetime")
	db := flag.String("db", "simbeam.db", "SQLite path for the subscriptions store")
	flag.Parse()

	st, err := store.OpenSQLite(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer st.Close()

	appSecret := os.Getenv("SIMCAST_APP_SECRET")
	if appSecret == "" {
		fmt.Fprintln(os.Stderr, "WARNING: SIMCAST_APP_SECRET is empty — the subscription API app-sig barrier is disabled")
	}

	b := signalbroker.New(signalbroker.Config{
		STUNURLs:   splitNonEmpty(*stun),
		TURNURLs:   splitNonEmpty(*turn),
		TURNSecret: *turnSecret,
		TURNTTL:    *turnTTL,
		Store:      st,
		AppSecret:  appSecret,
	})

	fmt.Printf("simbeam-signal listening on %s (ws: /ws, api: /v1/subscription, db: %s)\n", *addr, *db)
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

- [ ] **Step 2: Build to verify it compiles**

Run: `go build ./cmd/simbeam-signal/`
Expected: builds clean, no `grant-turn` references remain.

- [ ] **Step 3: Commit**

```bash
git add cmd/simbeam-signal/main.go
git commit -m "feat(signal-cmd): --db + SIMCAST_APP_SECRET; remove --grant-turn stub"
```

---

### Task 12: Daemon — persistent serve + reconnect + 3C handshake

Replace the one-shot `DialSignal` with a persistent `ServeSignal` (auto-reconnect under `daemonID`) whose inner `serveOnce` runs the daemon side of the 3C handshake and serves reconnecting clients one at a time. The signed answer (existing #54) doubles as the daemon's proof-of-key to the client, so no separate daemon-nonce challenge is needed. This task is verified by the integration tests in Tasks 14–15; here we land the code and confirm the package compiles.

**Files:**
- Modify: `internal/server/remote.go` (replace `DialSignal`; keep `toWebRTC` + `signedAnswer`)

- [ ] **Step 1: Rewrite `internal/server/remote.go`**

```go
package server

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log"
	"sync"
	"time"

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
// the SDP under the daemon's permanent key. The browser verifies it against the
// pinned daemonPubKey (anti-MITM), which also proves the daemon controls its key
// — so a separate daemon-nonce challenge is unnecessary.
func signedAnswer(sdp string, priv ed25519.PrivateKey) signal.Msg {
	return signal.Msg{Type: signal.TypeAnswer, SDP: sdp, Sig: signal.Sign(priv, []byte(sdp))}
}

// ServeSignal keeps a persistent registration on the broker under the daemon's
// identity and serves reconnecting pinned clients one at a time, forever, with
// exponential-backoff auto-reconnect. win is the (possibly closed) enrollment
// window letting a not-yet-pinned client enroll with secret S. Returns when ctx
// is cancelled.
func (s *Server) ServeSignal(ctx context.Context, signalURL string, id Identity, pinned *PinnedStore, win *pairingWindow) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := s.serveOnce(ctx, signalURL, id, pinned, win)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("signaling connection lost: %v; reconnecting in %s", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
	return ctx.Err()
}

// serveOnce holds one broker connection: register, then process the relayed
// handshake for a single active client at a time. The live P2P peer runs in
// pion's own goroutines; the broker WS stays open for the next client (revises
// #51: signaling is now persistent presence, not handshake-then-close).
func (s *Server) serveOnce(ctx context.Context, signalURL string, id Identity, pinned *PinnedStore, win *pairingWindow) error {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, signalURL, nil)
	if err != nil {
		return fmt.Errorf("dial signaling: %w", err)
	}
	defer ws.Close()

	var wmu sync.Mutex
	send := func(m signal.Msg) error { wmu.Lock(); defer wmu.Unlock(); return ws.WriteJSON(m) }

	if err := send(signal.Msg{Type: signal.TypeRegister, Role: signal.RoleDaemon, Daemon: id.PubB64}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Single active client session state.
	var (
		sess       *rtc.SessionRef // assigned below; see note
		disp       *rtcDispatch
		sessCancel context.CancelFunc
		iceServers []webrtc.ICEServer
		authPub    string
		authNonce  string
		enrolling  bool
		authed     bool
	)
	_ = sess // (placeholder; replaced by the concrete type in the cleanup closure)

	// NOTE: use the concrete *rtc.Session type from startSession; the alias above
	// is only to keep this snippet readable. Declare it as:
	//   var sess *rtc.Session
	// (import "github.com/kei-sidorov/simbeam/internal/rtc" is already pulled in
	//  transitively via startSession's return type in rtc.go).

	cleanup := func() {
		if sessCancel != nil {
			sessCancel()
		}
		if disp != nil {
			disp.stopAttachment()
		}
		// sess.Close handled via the concrete variable; see Step 2 correction.
		sessCancel, disp = nil, nil
		authPub, authNonce, enrolling, authed = "", "", false, false
		iceServers = nil
	}
	defer cleanup()

	for {
		var m signal.Msg
		if err := ws.ReadJSON(&m); err != nil {
			return fmt.Errorf("read signaling: %w", err)
		}
		switch m.Type {
		case signal.TypeConnect:
			cleanup() // drop any prior client
			allow, enr := false, false
			if pinned.Contains(m.PubKey) {
				allow = true
			} else if win.verify(m.PubKey, m.Nonce, m.Pair, time.Now()) {
				allow, enr = true, true
			}
			if !allow {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: "not paired"})
				continue
			}
			nonce, nerr := signal.NewNonce()
			if nerr != nil {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: "nonce error"})
				continue
			}
			authPub, authNonce, enrolling, authed = m.PubKey, nonce, enr, false
			_ = send(signal.Msg{Type: signal.TypeChallenge, Nonce: nonce})
		case signal.TypeICEServers:
			iceServers = toWebRTC(m.ICEServers)
		case signal.TypeProof:
			if authPub == "" || !signal.Verify(authPub, []byte(authNonce), m.Sig) {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: "challenge failed"})
				cleanup()
				continue
			}
			if enrolling {
				_ = pinned.Add(authPub, "")
			}
			authed = true
		case signal.TypeOffer:
			if !authed {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: "unauthenticated"})
				continue
			}
			sctx, cancel := context.WithCancel(ctx)
			ns, nd, serr := s.startSession(sctx, iceServers)
			if serr != nil {
				cancel()
				_ = send(signal.Msg{Type: signal.TypeError, Msg: serr.Error()})
				continue
			}
			disp, sessCancel = nd, cancel
			ns.OnClose(cancel)
			answerSDP, aerr := ns.Answer(m.SDP)
			if aerr != nil {
				_ = ns.Close()
				cancel()
				_ = send(signal.Msg{Type: signal.TypeError, Msg: aerr.Error()})
				cleanup()
				continue
			}
			// Keep the session alive on a closer wired into cleanup.
			s.bindSession(&sessCancel, ns) // see Step 2 helper
			_ = send(signedAnswer(answerSDP, id.Priv))
		case signal.TypePeerLeft:
			cleanup()
		}
	}
}
```

> The snippet above intentionally flags the one awkward spot — holding the concrete `*rtc.Session` so `cleanup` can `Close()` it. Step 2 replaces the placeholder with the real, compiling version.

- [ ] **Step 2: Apply the concrete-type correction**

Replace the `var ( … sess *rtc.SessionRef … )` block, the `_ = sess` line, the long NOTE comment, the `cleanup` closure, and the `s.bindSession(...)` call with this compiling version (and add the `rtc` import):

Add to the import block:
```go
	"github.com/kei-sidorov/simbeam/internal/rtc"
```

Replace the state declaration with:
```go
	var (
		sess       *rtc.Session
		disp       *rtcDispatch
		sessCancel context.CancelFunc
		iceServers []webrtc.ICEServer
		authPub    string
		authNonce  string
		enrolling  bool
		authed     bool
	)
	cleanup := func() {
		if sessCancel != nil {
			sessCancel()
		}
		if disp != nil {
			disp.stopAttachment()
		}
		if sess != nil {
			_ = sess.Close()
		}
		sess, disp, sessCancel = nil, nil, nil
		authPub, authNonce, enrolling, authed = "", "", false, false
		iceServers = nil
	}
	defer cleanup()
```

In the `TypeOffer` case, replace the body after a successful `startSession` with:
```go
			sctx, cancel := context.WithCancel(ctx)
			ns, nd, serr := s.startSession(sctx, iceServers)
			if serr != nil {
				cancel()
				_ = send(signal.Msg{Type: signal.TypeError, Msg: serr.Error()})
				continue
			}
			sess, disp, sessCancel = ns, nd, cancel
			ns.OnClose(cancel)
			answerSDP, aerr := ns.Answer(m.SDP)
			if aerr != nil {
				_ = send(signal.Msg{Type: signal.TypeError, Msg: aerr.Error()})
				cleanup()
				continue
			}
			_ = send(signedAnswer(answerSDP, id.Priv))
```

Delete the `_ = sess`, the NOTE comment, the `s.bindSession(...)` line, and the `*rtc.SessionRef` placeholder — they were scaffolding.

- [ ] **Step 3: Build to verify it compiles**

Run: `go build ./internal/server/`
Expected: builds clean. (`cmd/simbeamd` still references the old `DialSignal` — fixed in Task 13. The integration test still references the old flow — rewritten in Task 14. Build the package alone here.)

- [ ] **Step 4: Commit**

```bash
git add internal/server/remote.go
git commit -m "feat(server): persistent ServeSignal with 3C handshake + auto-reconnect"
```

---

### Task 13: `cmd/simbeamd` — persistent serve, P-keypress pairing, `unpair`

**Files:**
- Modify: `cmd/simbeamd/main.go`
- Modify: `go.mod` / `go.sum` (add `golang.org/x/term`)

The daemon loads its identity + pinned store, serves persistently, and watches the terminal: pressing **`p`** opens a one-time enrollment window and prints the pairing URL; **`q`**/Ctrl-C quits. A separate `unpair <clientPubKey>` subcommand revokes a device.

- [ ] **Step 1: Add the terminal dependency**

Run: `go get golang.org/x/term@latest`
Expected: `go.mod` gains `golang.org/x/term`.

- [ ] **Step 2: Replace `runServe`/`runRemote` and add `unpair` in `cmd/simbeamd/main.go`**

Update the imports to include:
```go
	"path/filepath"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/server"
	"github.com/kei-sidorov/simbeam/internal/signal"
	"golang.org/x/term"
```
(remove the now-unused `net` import only if it is no longer referenced after this edit — `runRemote` still binds a local listener, so keep `net`.)

Add `unpair` to the command switch in `main` (alongside `list`/`serve`):
```go
	case "unpair":
		if err := runUnpair(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
```

Add the default identity/clients paths helper:
```go
// defaultStatePath returns ~/.simbeam/<name>, falling back to ./.simbeam/<name>.
func defaultStatePath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".simbeam", name)
	}
	return filepath.Join(home, ".simbeam", name)
}
```

Replace `runServe` with the version that adds identity/clients/pair-ttl flags and wires persistent serve:
```go
func runServe(argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	webDir := fs.String("web", "", "directory with debug client (served at /); empty = API only")
	signalURL := fs.String("signal", "", "remote rendezvous: signaling broker WS URL (e.g. wss://host/ws); empty = local-only")
	clientURL := fs.String("client-url", "", "base URL of the browser debug client for the pairing link; empty = http://localhost<addr>/")
	identityPath := fs.String("identity", defaultStatePath("identity.key"), "path to the daemon's persistent Ed25519 identity")
	clientsPath := fs.String("clients", defaultStatePath("clients.json"), "path to the pinned-clients store")
	pairTTL := fs.Duration("pair-ttl", 5*time.Minute, "how long an enrollment window stays open after pressing P")
	_ = fs.Parse(argv)

	c := companion.New()
	path, err := c.Resolve()
	if err != nil {
		return err
	}
	srv := server.New(c, *webDir).WithBinary(path)

	if *signalURL != "" {
		return runRemote(srv, *signalURL, *clientURL, *addr, *webDir, *identityPath, *clientsPath, *pairTTL)
	}

	fmt.Printf("simbeamd serving on %s (idb_companion: %s)\n", *addr, path)
	if *webDir != "" {
		fmt.Printf("debug client: http://localhost%s/\n", *addr)
	}
	return http.ListenAndServe(*addr, srv.Handler())
}
```

Replace `runRemote` with the persistent version + keypress loop:
```go
// runRemote loads the daemon's persistent identity + pinned clients, serves the
// debug client locally (so the browser can load it), connects persistently to the
// broker under daemonID, and watches the terminal: press P to open a one-time
// enrollment window (prints the pairing URL), Q/Ctrl-C to quit.
func runRemote(srv *server.Server, signalURL, clientURL, addr, webDir, identityPath, clientsPath string, pairTTL time.Duration) error {
	id, err := server.LoadOrCreateIdentity(identityPath)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	pinned, err := server.LoadPinnedStore(clientsPath)
	if err != nil {
		return fmt.Errorf("pinned store: %w", err)
	}
	win := server.NewPairingWindow()

	base := clientURL
	if base == "" {
		base = "http://localhost" + addr + "/"
	}
	if webDir != "" {
		ln, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return fmt.Errorf("local http listen on %s: %w", addr, lerr)
		}
		go func() {
			if err := http.Serve(ln, srv.Handler()); err != nil {
				log.Printf("local http: %v", err)
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("simbeamd remote mode — broker: %s\n", signalURL)
	fmt.Printf("daemonID: %s\n", id.PubB64)
	fmt.Println("Press P to pair a new device (opens a one-time window). Press Q to quit.")

	onPair := func() {
		secret, serr := signal.NewPairingSecret()
		if serr != nil {
			fmt.Printf("\rpairing error: %v\r\n", serr)
			return
		}
		win.Open(secret, time.Now(), pairTTL)
		fmt.Printf("\r\nPair this device by opening (window open %s):\r\n  %s\r\n",
			pairTTL, signal.PairingURL(base, signalURL, id.PubB64, secret))
	}

	go watchKeys(ctx, cancel, onPair)
	return srv.ServeSignal(ctx, signalURL, id, pinned, win)
}

// watchKeys reads single keystrokes from a terminal: P opens a pairing window,
// Q/Ctrl-C cancels. If stdin is not a TTY (piped/tests), it just blocks on ctx.
func watchKeys(ctx context.Context, cancel context.CancelFunc, onPair func()) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		<-ctx.Done()
		return
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		<-ctx.Done()
		return
	}
	defer term.Restore(fd, old)

	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			cancel()
			return
		}
		switch buf[0] {
		case 'p', 'P':
			onPair()
		case 'q', 'Q', 3: // 3 = Ctrl-C (raw mode delivers it as a byte)
			cancel()
			return
		}
	}
}

// runUnpair revokes a client by removing it from the pinned store.
func runUnpair(argv []string) error {
	fs := flag.NewFlagSet("unpair", flag.ExitOnError)
	clientsPath := fs.String("clients", defaultStatePath("clients.json"), "path to the pinned-clients store")
	_ = fs.Parse(argv)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: simbeamd unpair [--clients path] <clientPubKey>")
	}
	pinned, err := server.LoadPinnedStore(*clientsPath)
	if err != nil {
		return err
	}
	if err := pinned.Remove(fs.Arg(0)); err != nil {
		return err
	}
	fmt.Printf("revoked %s\n", fs.Arg(0))
	return nil
}
```

Add `"context"` and `"log"` to imports if not present, and update `usage` to mention the new subcommand and keys:
```go
	fmt.Fprintln(w, "  simbeamd serve   Serve REST API + WebSocket stream (flags: --addr, --web, --signal, --client-url, --identity, --clients, --pair-ttl)")
	fmt.Fprintln(w, "  simbeamd unpair  Revoke a paired client: simbeamd unpair <clientPubKey>")
```

- [ ] **Step 3: Add the exported `NewPairingWindow` constructor**

`runRemote` needs to construct a `pairingWindow` from outside the test file, and `pairing_window.go` only has the unexported type. Add an exported wrapper. In `internal/server/pairing_window.go`, add:
```go
// NewPairingWindow returns a closed pairing window the daemon arms on demand.
func NewPairingWindow() *pairingWindow { return &pairingWindow{} }

// Open arms the window (exported wrapper over open for cmd use).
func (p *pairingWindow) Open(secret string, now time.Time, ttl time.Duration) {
	p.open(secret, now, ttl)
}
```
And in `internal/server/remote.go`, change `ServeSignal`/`serveOnce` signatures' `win *pairingWindow` — already unexported-typed but reachable from cmd via the returned `*pairingWindow`. (Go allows passing an unexported-typed value obtained from an exported constructor.)

- [ ] **Step 4: Build to verify it compiles**

Run: `go build ./cmd/simbeamd/ ./internal/server/`
Expected: builds clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/simbeamd/main.go internal/server/pairing_window.go go.mod go.sum
git commit -m "feat(simbeamd): persistent serve, P-keypress pairing window, unpair, x/term"
```

---

### Task 14: Integration test — first enrollment end-to-end

Rewrite the 3b `remote_integration_test.go` for the 3C handshake. This task lands the shared helpers + the enrollment test; Task 15 appends the reconnect/TURN test to the same file. Hermetic: real broker + temp SQLite + a pion "browser" + stub Companion, no idb/network.

**Files:**
- Modify: `internal/server/remote_integration_test.go` (full replacement)

> `stubComp` is defined in `internal/server/rtcdispatch_test.go` (not this file), so replacing `remote_integration_test.go` keeps it available in-package.

- [ ] **Step 1: Replace the file with helpers + the enrollment test**

```go
package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/kei-sidorov/simbeam/internal/signalbroker"
	"github.com/kei-sidorov/simbeam/internal/store"
)

// brokerFixture starts a real broker (optionally with a Store + TURN) on httptest
// and returns its /ws URL.
func brokerFixture(t *testing.T, cfg signalbroker.Config) string {
	t.Helper()
	b := signalbroker.New(cfg)
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
}

// startDaemon runs a stub-Companion daemon's ServeSignal against wsURL.
func startDaemon(t *testing.T, ctx context.Context, wsURL string, id Identity, pinned *PinnedStore, win *pairingWindow) {
	t.Helper()
	dsrv := New(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", State: "Booted", OSVersion: "17.0"},
		{UDID: "B", Name: "iPad", State: "Shutdown", OSVersion: "17.0"},
	}}, "")
	go func() { _ = dsrv.ServeSignal(ctx, wsURL, id, pinned, win) }()
}

// newOfferer builds a pion "browser": a recvonly video transceiver + a control
// DataChannel that asks for the sim list on open and forwards replies.
func newOfferer(t *testing.T) (*webrtc.PeerConnection, chan []byte) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("AddTransceiverFromKind: %v", err)
	}
	replies := make(chan []byte, 4)
	dc, err := pc.CreateDataChannel("control", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	dc.OnOpen(func() { _ = dc.SendText(`{"type":"list"}`) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case replies <- m.Data:
		default:
		}
	})
	return pc, replies
}

// joinUntilPresent dials the broker and sends join (with an enrollment proof when
// pairSecret != ""), retrying until the daemon is registered. Returns the open ws
// and the first non-offline message (a challenge on success).
func joinUntilPresent(t *testing.T, ctx context.Context, wsURL, daemonID, clientPub string, clientPriv ed25519.PrivateKey, pairSecret string) (*websocket.Conn, signal.Msg) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("daemon never registered in time")
		}
		c, _, derr := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if derr != nil {
			t.Fatalf("dial broker: %v", derr)
		}
		join := signal.Msg{Type: signal.TypeJoin, Role: signal.RoleClient, Daemon: daemonID, PubKey: clientPub}
		if pairSecret != "" {
			nonce, _ := signal.NewNonce()
			join.Nonce = nonce
			join.Pair = signal.EnrollProof(pairSecret, clientPub, nonce)
		}
		_ = c.WriteJSON(join)
		_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
		var m signal.Msg
		if err := c.ReadJSON(&m); err != nil {
			_ = c.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if m.Type == signal.TypeError && strings.Contains(m.Msg, "offline") {
			_ = c.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return c, m
	}
}

// runHandshake completes the client side: sign the challenge nonces, send the
// offer when iceServers arrive, verify the signed answer against daemonID, and
// apply it. Returns the iceServers the broker issued (for gate assertions).
func runHandshake(t *testing.T, ws *websocket.Conn, pc *webrtc.PeerConnection, daemonID string, clientPriv ed25519.PrivateKey, first signal.Msg) []signal.ICEServer {
	t.Helper()
	var ice []signal.ICEServer
	offerSent := false

	handle := func(m signal.Msg) (done bool) {
		switch m.Type {
		case signal.TypeChallenge:
			_ = ws.WriteJSON(signal.Msg{
				Type:      signal.TypeProof,
				Sig:       signal.Sign(clientPriv, []byte(m.Nonce)),
				BrokerSig: signal.Sign(clientPriv, []byte(m.BrokerNonce)),
			})
		case signal.TypeICEServers:
			ice = m.ICEServers
			if !offerSent {
				offer, err := pc.CreateOffer(nil)
				if err != nil {
					t.Fatalf("CreateOffer: %v", err)
				}
				gathered := webrtc.GatheringCompletePromise(pc)
				if err := pc.SetLocalDescription(offer); err != nil {
					t.Fatalf("SetLocalDescription: %v", err)
				}
				select {
				case <-gathered:
				case <-time.After(5 * time.Second):
					t.Fatalf("ICE gathering did not complete")
				}
				_ = ws.WriteJSON(signal.Msg{Type: signal.TypeOffer, SDP: pc.LocalDescription().SDP})
				offerSent = true
			}
		case signal.TypeAnswer:
			if !signal.Verify(daemonID, []byte(m.SDP), m.Sig) {
				t.Fatalf("answer signature failed against daemonID (anti-MITM)")
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
				t.Fatalf("SetRemoteDescription: %v", err)
			}
			return true
		case signal.TypeError:
			t.Fatalf("handshake error: %s", m.Msg)
		case signal.TypePeerLeft:
			t.Fatalf("peer left mid-handshake")
		}
		return false
	}

	if handle(first) {
		return ice
	}
	for {
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		var m signal.Msg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("read signaling: %v", err)
		}
		if handle(m) {
			return ice
		}
	}
}

// expectSims waits for the control DataChannel to deliver a 2-sim list.
func expectSims(t *testing.T, replies chan []byte, pc *webrtc.PeerConnection) {
	t.Helper()
	select {
	case raw := <-replies:
		var r ctrlReply
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("unmarshal reply %q: %v", raw, err)
		}
		if r.Type != "sims" || len(r.Sims) != 2 {
			t.Fatalf("want 2 sims, got type=%q n=%d (%s)", r.Type, len(r.Sims), raw)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("control reply never arrived (state=%s)", pc.ConnectionState())
	}
}

// TestEnrollmentEndToEnd: open a pairing window, a brand-new client enrolls with
// secret S, the daemon pins it, and the control DataChannel works.
func TestEnrollmentEndToEnd(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:stun.l.google.com:19302"}})

	id, err := func() (Identity, error) {
		pub, priv, e := signal.GenerateKeyPair()
		return Identity{PubB64: pub, Priv: priv}, e
	}()
	if err != nil {
		t.Fatal(err)
	}

	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")
	win := NewPairingWindow()
	const secret = "ENROLL-SECRET"
	win.Open(secret, time.Now(), 5*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, win)

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pc, replies := newOfferer(t)
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, secret)
	t.Cleanup(func() { _ = ws.Close() })

	runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
	expectSims(t, replies, pc)

	if !pinned.Contains(clientPub) {
		t.Fatalf("client was not pinned after enrollment")
	}
}
```

- [ ] **Step 2: Run the enrollment test**

Run: `go test ./internal/server/ -run TestEnrollmentEndToEnd -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/server/remote_integration_test.go
git commit -m "test(server): hermetic enrollment-with-S end-to-end (3C handshake)"
```

---

### Task 15: Integration test — reconnect by daemonID + TURN gate by subscription

**Files:**
- Modify: `internal/server/remote_integration_test.go` (append two tests)

- [ ] **Step 1: Append the reconnect + gate tests**

```go
// TestReconnectByDaemonID: a pre-pinned client connects with NO secret (key-only
// challenge), reaches the control plane, then reconnects a second time on the
// same daemon — proving the reconnect path needs no QR/secret.
func TestReconnectByDaemonID(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:stun.l.google.com:19302"}})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}

	clientPub, clientPriv, _ := signal.GenerateKeyPair()
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")
	_ = pinned.Add(clientPub, "iPad") // already enrolled

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow()) // window CLOSED

	for i := 0; i < 2; i++ {
		pc, replies := newOfferer(t)
		ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "")
		runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
		expectSims(t, replies, pc)
		_ = ws.Close()
		_ = pc.Close()
		time.Sleep(100 * time.Millisecond) // let the daemon release the prior session
	}
}

// TestUnpinnedClientRejected: with the window closed, a client the daemon has not
// pinned is refused (peer-pinning: the daemon decides access, not the broker).
func TestUnpinnedClientRejected(t *testing.T) {
	wsURL := brokerFixture(t, signalbroker.Config{STUNURLs: []string{"stun:x"}})
	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow())

	stranger, strangerPriv, _ := signal.GenerateKeyPair()
	ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, stranger, strangerPriv, "")
	t.Cleanup(func() { _ = ws.Close() })

	// The daemon must answer the challenge with an error ("not paired"). Sign the
	// (empty) challenge if one came, then expect an error.
	if first.Type == signal.TypeChallenge {
		t.Fatalf("unpinned client should NOT receive a challenge")
	}
	if first.Type != signal.TypeError || !strings.Contains(first.Msg, "not paired") {
		t.Fatalf("want 'not paired' error, got %+v", first)
	}
}

// TestTurnGateBySubscription: an active subscription for the client's key yields
// STUN+TURN; no subscription yields STUN only. The client key the broker gates on
// is the one authenticated by the challenge-response.
func TestTurnGateBySubscription(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/subs.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	wsURL := brokerFixture(t, signalbroker.Config{
		STUNURLs:   []string{"stun:stun.l.google.com:19302"},
		TURNURLs:   []string{"turn:relay.example:3478"},
		TURNSecret: "secret",
		Store:      st,
	})

	pub, priv, _ := signal.GenerateKeyPair()
	id := Identity{PubB64: pub, Priv: priv}
	pinned, _ := LoadPinnedStore(t.TempDir() + "/clients.json")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startDaemon(t, ctx, wsURL, id, pinned, NewPairingWindow())

	connect := func(clientPub string, clientPriv ed25519.PrivateKey) []signal.ICEServer {
		_ = pinned.Add(clientPub, "")
		pc, replies := newOfferer(t)
		ws, first := joinUntilPresent(t, ctx, wsURL, id.PubB64, clientPub, clientPriv, "")
		ice := runHandshake(t, ws, pc, id.PubB64, clientPriv, first)
		expectSims(t, replies, pc)
		_ = ws.Close()
		_ = pc.Close()
		time.Sleep(100 * time.Millisecond)
		return ice
	}

	// Subscribed client → STUN + TURN.
	subPub, subPriv, _ := signal.GenerateKeyPair()
	if err := st.Upsert(ctx, store.Subscription{
		ClientPubKey: subPub, ProductID: "pro", ExpiresAt: "2099-01-01T00:00:00Z",
		IssuedAt: "2026-06-04T00:00:00Z", Source: "client", UpdatedAt: "2026-06-04T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if ice := connect(subPub, subPriv); len(ice) != 2 {
		t.Fatalf("subscribed client want STUN+TURN, got %d iceServers", len(ice))
	}

	// Unsubscribed client → STUN only.
	freePub, freePriv, _ := signal.GenerateKeyPair()
	if ice := connect(freePub, freePriv); len(ice) != 1 {
		t.Fatalf("free client want STUN only, got %d iceServers", len(ice))
	}
}
```

- [ ] **Step 2: Run the full server test suite**

Run: `go test ./internal/server/ -v`
Expected: PASS (identity, pinned, pairing-window, enrollment, reconnect, unpinned-reject, turn-gate, plus the pre-existing rtc/dispatch/control tests).

> If `TestUnpinnedClientRejected` sees the error arrive as a *second* message rather than `first`, adjust `joinUntilPresent` is not needed — the daemon emits the "not paired" error in response to the broker's `connect`, and the broker relays it via the daemon→client path; it is the first thing the client receives after a successful (daemon-present) join. If timing makes `first` a benign non-error, loop reading until an error or challenge before asserting.

- [ ] **Step 3: Commit**

```bash
git add internal/server/remote_integration_test.go
git commit -m "test(server): reconnect-by-daemonID, unpinned reject, TURN-gate-by-subscription"
```

---

### Task 16: Browser bench — WebCrypto identity + crypto helpers + UI panels

The bench plays the future iPad: a persistent Ed25519 account key in `localStorage`, a "my Macs" list, and a subscription panel. This task adds the identity layer, crypto helpers, and the HTML panels; Tasks 17–18 wire the connection and subscription flows. The bench's `verifyAnswer` already uses WebCrypto Ed25519 (line ~196), so generate/sign on the same browsers is supported.

**Files:**
- Modify: `web/debug/index.html`

- [ ] **Step 1: Add the HTML panels**

In `web/debug/index.html`, immediately after the `<div id="modes">…</div>` block (before `<h3>Simulators</h3>`), insert:

```html
  <div id="identity" style="margin:8px 0; padding:8px; border:1px solid #eee; font-size:13px;">
    <b>Account key:</b> <code id="pubkey">…</code>
    <button id="resetIdentity" title="new account for testing">reset</button>
  </div>
  <div id="macs" style="margin:8px 0;">
    <b>My Macs:</b>
    <div id="macList">none yet</div>
    <button id="pairBtn" style="display:none;">Pair this Mac</button>
  </div>
  <div id="subPanel" style="margin:8px 0; padding:8px; border:1px solid #eee; font-size:13px;">
    <b>Subscription (StoreKit emulation):</b>
    <input id="subProduct" value="pro.monthly" size="12">
    expires <input id="subExpires" type="date">
    <button id="subApply">Apply</button>
    <span id="subStatus"></span>
  </div>
  <div id="turnIndicator" style="margin:8px 0; font-size:13px;"></div>
```

- [ ] **Step 2: Add identity + crypto helpers**

Near the top of the `<script>`, after the existing element lookups (after the `let startGen = 0;` line ~42), add globals and helpers:

```javascript
// ---- 3C: account identity (WebCrypto Ed25519, persisted in localStorage) ----
const APP_SECRET = 'dev-app-secret'; // must equal the broker's SIMCAST_APP_SECRET in local dev
let clientPriv = null;               // CryptoKey (Ed25519 private, usage ['sign'])
let clientPub = null;                // base64 (std) raw public key = account id

function bytesToB64(b) { let s = ''; for (const x of b) s += String.fromCharCode(x); return btoa(s); }

async function loadOrCreateIdentity() {
  const stored = localStorage.getItem('simbeam_priv_pkcs8');
  const storedPub = localStorage.getItem('simbeam_pub');
  if (stored && storedPub) {
    clientPriv = await crypto.subtle.importKey('pkcs8', b64ToBytes(stored), {name: 'Ed25519'}, true, ['sign']);
    clientPub = storedPub;
    return;
  }
  const kp = await crypto.subtle.generateKey({name: 'Ed25519'}, true, ['sign', 'verify']);
  const pkcs8 = new Uint8Array(await crypto.subtle.exportKey('pkcs8', kp.privateKey));
  const raw = new Uint8Array(await crypto.subtle.exportKey('raw', kp.publicKey));
  clientPriv = kp.privateKey;
  clientPub = bytesToB64(raw);
  localStorage.setItem('simbeam_priv_pkcs8', bytesToB64(pkcs8));
  localStorage.setItem('simbeam_pub', clientPub);
}

async function signEd25519(bytes) {
  const sig = await crypto.subtle.sign('Ed25519', clientPriv, bytes);
  return bytesToB64(new Uint8Array(sig));
}

async function hmacSha256B64(secret, bytes) {
  const key = await crypto.subtle.importKey('raw', new TextEncoder().encode(secret), {name: 'HMAC', hash: 'SHA-256'}, false, ['sign']);
  const sig = await crypto.subtle.sign('HMAC', key, bytes);
  return bytesToB64(new Uint8Array(sig));
}

// EnrollProof = base64(HMAC-SHA256(S, clientPubKey ‖ 0x00 ‖ nonce)). Bytes must
// match the Go server byte-for-byte.
async function enrollProof(secret, pub, nonce) {
  const enc = new TextEncoder();
  const a = enc.encode(pub), b = enc.encode(nonce);
  const buf = new Uint8Array(a.length + 1 + b.length);
  buf.set(a, 0); buf[a.length] = 0; buf.set(b, a.length + 1);
  return hmacSha256B64(secret, buf);
}

// Canonical subscription bytes: pub ‖ 0x1f ‖ product ‖ 0x1f ‖ expires ‖ 0x1f ‖ issued.
function canonicalSub(pub, product, expires, issued) {
  return new TextEncoder().encode([pub, product, expires, issued].join('\x1f'));
}

function renderIdentity() {
  document.getElementById('pubkey').textContent = clientPub ? clientPub.slice(0, 16) + '…' : '…';
}

document.getElementById('resetIdentity').onclick = async () => {
  localStorage.removeItem('simbeam_priv_pkcs8');
  localStorage.removeItem('simbeam_pub');
  await loadOrCreateIdentity();
  renderIdentity();
  alert('New account key generated. You must re-pair your Macs.');
};

// ---- 3C: my Macs (saved pinned daemons) ----
function loadMacs() {
  try { return JSON.parse(localStorage.getItem('simbeam_macs') || '[]'); } catch (e) { return []; }
}
function saveMac(mac) {
  const macs = loadMacs().filter(m => m.daemon !== mac.daemon);
  macs.push(mac);
  localStorage.setItem('simbeam_macs', JSON.stringify(macs));
  renderMacs();
}
function renderMacs() {
  const macs = loadMacs();
  const el = document.getElementById('macList');
  el.innerHTML = '';
  if (!macs.length) { el.textContent = 'none yet'; return; }
  macs.forEach(m => {
    const b = document.createElement('button');
    b.textContent = `${m.name || 'Mac'} (${m.daemon.slice(0, 8)}…)`;
    b.onclick = () => connectRemote(m); // defined in Task 17
    el.appendChild(b);
  });
}
```

- [ ] **Step 3: Initialize on load**

Replace the final `enterMode();` line (~378) with:

```javascript
(async function init() {
  await loadOrCreateIdentity();
  renderIdentity();
  renderMacs();
  setupPairButton();          // defined in Task 17
  setupSubscriptionPanel();   // defined in Task 18
  if (mode === 'jpg') loadSimsREST();
})();
```

- [ ] **Step 4: Verify it loads (manual smoke)**

Run: `go run ./cmd/simbeamd serve --web web/debug --addr :8080` then open `http://localhost:8080/`.
Expected: the page loads, "Account key" shows a truncated base64 key, "My Macs: none yet", a subscription panel. Console has no errors. (Tasks 17–18 add the missing `setupPairButton`/`setupSubscriptionPanel`/`connectRemote` — until then those calls will throw; acceptable mid-build. To smoke-test Task 16 alone, temporarily stub them as `function setupPairButton(){}` etc., then remove the stubs in 17/18.)

- [ ] **Step 5: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(bench): WebCrypto Ed25519 account identity, my-Macs list, UI panels"
```

---

### Task 17: Browser bench — enrollment, reconnect, auto-reconnect

**Files:**
- Modify: `web/debug/index.html`

Replace the 3b remote signaling with the 3C handshake driven by enrollment (URL fragment with `pair`) or a saved Mac (reconnect, no `pair`), plus auto-reconnect.

- [ ] **Step 1: Rewrite `pairing()` and add the pair button + connect entry points**

Replace the existing `pairing()` function (~105-110) with:

```javascript
// pairing() returns enrollment coordinates iff the URL fragment carries a pair
// secret: {signal, daemon, pair}. Reconnects use saved Macs instead.
function pairing() {
  const f = new URLSearchParams(location.hash.slice(1));
  const signal = f.get('signal'), daemon = f.get('daemon'), pair = f.get('pair');
  return (signal && daemon && pair) ? {signal, daemon, pair} : null;
}

function setupPairButton() {
  const p = pairing();
  const btn = document.getElementById('pairBtn');
  if (!p) { btn.style.display = 'none'; return; }
  btn.style.display = 'inline-block';
  btn.textContent = `Pair this Mac (${p.daemon.slice(0, 8)}…)`;
  btn.onclick = () => enroll(p);
}

// enroll runs the first-pairing handshake (carries the HMAC(S) proof). On
// success the Mac is saved to "my Macs" and the live session continues.
function enroll(p) {
  startRemoteSession({signal: p.signal, daemon: p.daemon, daemonPub: p.daemon, pair: p.pair, save: true});
}

// connectRemote reconnects to a saved Mac (no secret, key-only challenge).
function connectRemote(mac) {
  startRemoteSession({signal: mac.signal, daemon: mac.daemon, daemonPub: mac.daemonPub, pair: null, save: false, name: mac.name});
}
```

- [ ] **Step 2: Replace `startControlPlane`/`signalRemote` with the 3C session builder**

Replace `startControlPlane` (~112-132) and `signalRemote` (~159-192) with a single `startRemoteSession` + the 3C signaling. Keep `signalLocal` (dev) untouched.

```javascript
let reconnectTimer = null;
let currentSession = null; // {signal, daemon, daemonPub, pair, save, name}

// startRemoteSession builds the peer + control DataChannel and runs the 3C
// handshake. On unexpected drop it auto-reconnects (reconnect path, no secret).
async function startRemoteSession(s) {
  teardown();
  currentSession = s;
  const gen = ++startGen;

  pc = new RTCPeerConnection();
  pc.addTransceiver('video', {direction: 'recvonly'});
  dc = pc.createDataChannel('control', {ordered: false, maxRetransmits: 0});
  pc.ontrack = (ev) => { vidEl.srcObject = ev.streams[0]; minimizeBuffer(pc); showSurface(); };
  dc.onopen = () => dc.send(JSON.stringify({type: 'list'}));
  dc.onmessage = (ev) => onCtrlReply(JSON.parse(ev.data));

  const onState = () => {
    if (pc && (pc.connectionState === 'failed' || pc.connectionState === 'disconnected')) {
      scheduleReconnect();
    }
  };
  pc.onconnectionstatechange = onState;

  await signal3C(s, gen);
}

function scheduleReconnect() {
  if (reconnectTimer || !currentSession || currentSession.pair) return; // don't auto-retry an enrollment
  const s = currentSession;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    startRemoteSession({...s, pair: null, save: false}); // reconnect path
  }, 1000);
}

// signal3C: join → challenge(sign nonces) → proof → iceServers(offer) →
// verify signed answer. daemonPub authenticates the Mac (anti-MITM).
async function signal3C(s, gen) {
  sig = new WebSocket(s.signal);
  let offerSent = false;

  const sendJoin = async () => {
    const join = {type: 'join', role: 'client', daemon: s.daemon, pubkey: clientPub};
    if (s.pair) {
      const nonce = bytesToB64(crypto.getRandomValues(new Uint8Array(16)));
      join.nonce = nonce;
      join.pair = await enrollProof(s.pair, clientPub, nonce);
    }
    sig.send(JSON.stringify(join));
  };

  sig.onopen = sendJoin;
  sig.onmessage = async (ev) => {
    const m = JSON.parse(ev.data);
    if (gen !== startGen) return; // superseded
    switch (m.type) {
      case 'challenge': {
        const enc = new TextEncoder();
        const proof = {
          type: 'proof',
          sig: await signEd25519(enc.encode(m.nonce)),
          brokerSig: await signEd25519(enc.encode(m.brokerNonce)),
        };
        sig.send(JSON.stringify(proof));
        break;
      }
      case 'iceServers': {
        window._iceServers = m.iceServers || [];
        refreshTurnIndicator(window._iceServers); // defined in Task 18
        pc.setConfiguration({iceServers: window._iceServers});
        if (!offerSent) {
          const offer = await pc.createOffer();
          await pc.setLocalDescription(offer);
          await iceGatheringComplete(pc);
          if (gen !== startGen) return;
          sig.send(JSON.stringify({type: 'offer', sdp: pc.localDescription.sdp}));
          offerSent = true;
        }
        break;
      }
      case 'answer': {
        const ok = await verifyAnswer(s.daemonPub, m.sdp, m.sig);
        if (!ok) { showAuthFail(); teardown(); return; }
        await pc.setRemoteDescription({type: 'answer', sdp: m.sdp});
        minimizeBuffer(pc);
        if (s.save) {
          saveMac({signal: s.signal, daemon: s.daemon, daemonPub: s.daemonPub, name: s.name || 'Mac'});
        }
        break;
      }
      case 'error':
        if (String(m.msg).includes('offline')) { scheduleReconnect(); }
        else { alert('pairing error: ' + m.msg); teardown(); }
        break;
      case 'peerLeft':
        scheduleReconnect();
        break;
    }
  };
  sig.onclose = () => { if (gen === startGen) scheduleReconnect(); };
}
```

- [ ] **Step 3: Make `teardown` cancel reconnects**

In the existing `teardown()` (~46-55), add at the top after `startGen++;`:

```javascript
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
```

And remove the now-unused `enterMode()` RTC auto-start: in `enterMode()` (~79-83), change the `rtc` branch so it no longer auto-starts a control plane:

```javascript
function enterMode() {
  simsEl.innerHTML = mode === 'rtc' ? 'pick or pair a Mac above' : 'loading…';
  if (mode === 'jpg') loadSimsREST();
}
```

- [ ] **Step 4: Manual smoke (enrollment + reconnect)**

Run a broker + daemon locally:
```bash
SIMCAST_APP_SECRET=dev-app-secret go run ./cmd/simbeam-signal --addr :9000 --db /tmp/simbeam.db
go run ./cmd/simbeamd serve --web web/debug --addr :8080 --signal ws://localhost:9000/ws
```
Press **P** in the daemon terminal, open the printed pairing URL, click "Pair this Mac" → simulators list appears, the Mac is saved. Reload the page (drop the fragment), click the saved Mac → reconnects with no QR. Kill+restart the daemon → the page auto-reconnects once the daemon is back.

- [ ] **Step 5: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(bench): 3C enrollment, reconnect by saved Mac, auto-reconnect"
```

---

### Task 18: Browser bench — subscription panel + TURN indicator

**Files:**
- Modify: `web/debug/index.html`

- [ ] **Step 1: Add the subscription + TURN functions**

Add near the other 3C helpers:

```javascript
// The broker base URL is derived from the pairing/saved signal WS URL
// (ws://host:9000/ws → http://host:9000).
function brokerHTTPBase() {
  const s = (currentSession && currentSession.signal) || (pairing() && pairing().signal);
  if (!s) return null;
  return s.replace(/^ws/, 'http').replace(/\/ws$/, '');
}

function setupSubscriptionPanel() {
  document.getElementById('subApply').onclick = applySubscription;
}

// applySubscription sends a REAL signed POST /v1/subscription (replaces StoreKit):
// X-App-Sig = HMAC-SHA256(APP_SECRET, canonical); X-Account-Sig = Ed25519(client).
async function applySubscription() {
  const base = brokerHTTPBase();
  if (!base) { alert('Pair or select a Mac first (need the broker URL).'); return; }
  const product = document.getElementById('subProduct').value;
  const dateVal = document.getElementById('subExpires').value; // yyyy-mm-dd
  if (!dateVal) { alert('pick an expiry date'); return; }
  const expires = new Date(dateVal + 'T00:00:00Z').toISOString().replace(/\.\d{3}Z$/, 'Z');
  const issued = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');

  const canon = canonicalSub(clientPub, product, expires, issued);
  const appSig = await hmacSha256B64(APP_SECRET, canon);
  const accSig = await signEd25519(canon);

  const resp = await fetch(base + '/v1/subscription', {
    method: 'POST',
    headers: {'Content-Type': 'application/json', 'X-App-Sig': appSig, 'X-Account-Sig': accSig},
    body: JSON.stringify({client_pubkey: clientPub, product_id: product, expires_at: expires, issued_at: issued}),
  });
  const status = document.getElementById('subStatus');
  status.textContent = resp.ok ? ` ✓ applied (expires ${expires})` : ` ✗ ${resp.status} ${await resp.text()}`;
}

// refreshTurnIndicator shows whether the issued iceServers include a TURN relay,
// making the subscription gate visible to the eye.
function refreshTurnIndicator(iceServers) {
  const hasTurn = (iceServers || []).some(s => {
    const urls = Array.isArray(s.urls) ? s.urls : [s.urls];
    return urls.some(u => String(u).startsWith('turn:'));
  });
  document.getElementById('turnIndicator').textContent =
    'iceServers contain TURN: ' + (hasTurn ? 'YES (subscriber)' : 'no (STUN only)');
}
```

- [ ] **Step 2: Manual smoke (gate flips by subscription)**

With the broker/daemon from Task 17 running and a TURN URL configured on the broker (`--turn turn:relay.example:3478 --turn-secret secret`):
- Without applying a subscription, connect a saved Mac → indicator reads "STUN only".
- Set an expiry date in the future, click **Apply** (status shows ✓), reconnect the Mac → indicator reads "YES (subscriber)".
- Set the expiry to a past date, Apply, reconnect → back to "STUN only". Inspect `/tmp/simbeam.db` with `sqlite3` to see the `subscriptions` row.

- [ ] **Step 3: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(bench): signed subscription POST + TURN indicator"
```

---

### Task 19: Documentation — decisions, README, ROADMAP

**Files:**
- Modify: `docs/decisions.md`
- Modify: `README.md`
- Modify: `docs/ROADMAP.md`

- [ ] **Step 1: Append decisions #55–#64 to `docs/decisions.md`**

Add these rows to the end of the decisions table (the latest existing row is #54):

```markdown
| 55 | Phase 3C: авторизация связи — **peer-pinning**, брокер untrusted. Демон пиннит клиентские ключи, клиент пиннит `daemonPubKey`; доступ к подключению решают концы по ключам, не брокер | анти-MITM без доверия к рандеву; медиа уже E2E (DTLS-SRTP, #7), защищаем рукопожатие криптографией концов |
| 56 | Phase 3C: **постоянная идентичность демона** — долгоживущий Ed25519 на диске (`~/.simbeam/identity.key`, 0600); `daemonID` = его pubkey. **Отменяет сессионный ключ из #54 и одноразовый token из #45/#51** | стабильный адрес на брокере + криптоудостоверение в одном; реконнект без повторного QR требует постоянного адреса |
| 57 | Phase 3C: **постоянное присутствие демона** — держит исходящий WSS к брокеру с auto-reconnect (экспон. backoff), всё время зарегистрирован под `daemonID`; presence у брокера в памяти `map[daemonID]→conn`. **Пересматривает #51** (signaling больше не «закрылся после рукопожатия») | Mac должен быть находим для реконнекта в любой момент; ноль открытых портов сохраняется (только исходящее) |
| 58 | Phase 3C: перед offer/answer — **взаимный challenge-response**. Демон challenge'ит клиента nonce'ом (клиент подписывает → демон проверяет владение ключом + pinned); брокер отдельным nonce'ом аутентифицирует клиентский ключ для гейта TURN; доказательство демона клиенту = **подписанный answer (#54)**, отдельный daemon-nonce не нужен | минимум сообщений: переиспользуем подпись answer'а как proof демона; брокер узнаёт проверенный `clientPubKey`, ничего не решая о доступе |
| 59 | Phase 3C: **первый пейринг — явное окно на демоне по нажатию `P` в терминале** (raw-mode, `golang.org/x/term`), одноразовый секрет `S` с TTL; клиент доказывает знание `S` через `HMAC-SHA256(S, clientPubKey‖0x00‖nonce)`, `S` брокеру не виден. Ревокация — `simbeamd unpair <pubkey>` (локально) | интерактивный триггер по запросу пользователя; HMAC-доказательство анти-«чужой», окно single-use/TTL анти-абьюз |
| 60 | Phase 3C: БД минимальна — **одна таблица `subscriptions`** в SQLite за интерфейсом `Store`; ключи и список Mac'ов живут на концах (localStorage / iCloud Keychain), серверного восстановления нет | соответствует open-core: durable только то, что нужно для гейта; миграция на Postgres = вторая реализация `Store` |
| 61 | Phase 3C: SQLite-драйвер — **`modernc.org/sqlite`** (чистый Go, без cgo) | герметичные `go test` без C-тулчейна, тривиальная кросс-компиляция под Homebrew-дистрибуцию Phase 4 |
| 62 | Phase 3C: endpoint подписки `POST /v1/subscription` с **двумя подписями** — app-secret HMAC (слабый барьер «наш билд», честно обфускация) + Ed25519 account-подпись (настоящая привязка к ключу); idempotent last-write-wins по `issued_at`, время сравнения — серверные часы. Усиление чеком Apple — Phase 4, флип `source` без смены схемы | разделяем «наш билд» и «это тот аккаунт»; идемпотентность делает спам foreground/background безопасным |
| 63 | Phase 3C: **гейт TURN читает `Store`** по уже проверенному (challenge-response) `clientPubKey`; **стаб `--grant-turn` убран**. Решалки разведены: доступ к *подключению* — концы по ключам (#55); доступ к *TURN-реле* — брокер по подписке (его законная работа, он выдаёт `iceServers`) | гейт «бесплатен» на рукопожатии (один read, не горячий медиапуть); заменяет стаб реальной подпиской |
| 64 | Phase 3C: тест-стенд — **браузер играет iPad целиком**: WebCrypto Ed25519 аккаунт-ключ в `localStorage`, пейринг по фрагменту URL, «мои Mac'ы», панель эмуляции подписки (настоящий подписанный POST), индикатор TURN, авто-reconnect. `http://localhost` = secure context → весь WebCrypto/WS работает без TLS | валидация всего флоу до строчки Swift; протухание не эмулируем — ставим `expires_at` вперёд/назад |
```

- [ ] **Step 2: Update `README.md`**

Add a "Remote pairing (Phase 3C)" subsection documenting the local run:

```markdown
## Remote pairing (Phase 3C)

Run the broker and the daemon, then pair a browser once and reconnect without QR:

```bash
# 1. Broker + subscription store (app secret must match the bench's dev value)
SIMCAST_APP_SECRET=dev-app-secret go run ./cmd/simbeam-signal \
  --addr :9000 --db /tmp/simbeam.db \
  --turn turn:relay.example:3478 --turn-secret secret   # TURN optional

# 2. Daemon: persistent identity + serve, debug client at :8080
go run ./cmd/simbeamd serve --web web/debug --addr :8080 --signal ws://localhost:9000/ws
```

Press **P** in the daemon terminal to open a one-time pairing window; open the
printed URL and click **Pair this Mac**. The browser saves the Mac and reconnects
automatically afterwards (no QR). Revoke a device with
`simbeamd unpair <clientPubKey>`. Inspect subscriptions by opening `/tmp/simbeam.db`
in `sqlite3` / DB Browser.

Identity files live in `~/.simbeam/` (`identity.key`, `clients.json`, both 0600).
```

- [ ] **Step 3: Update `docs/ROADMAP.md`**

Under Phase 3, note that 3a/3b/3C are complete and what 3C delivered, and that Phase 4 carries Apple-receipt verification (flip `source`), TLS/domain, Homebrew, and Postgres. Add after the Phase 3 DoD block:

```markdown
### Phase 3C — done

Постоянная парность (спарились ключами один раз → реконнект по `daemonID` без QR),
аккаунты-по-ключам, подписки (`POST /v1/subscription`, две подписи, SQLite за
`Store`), гейт TURN по реальной подписке (стаб `--grant-turn` убран). Дизайн —
`docs/superpowers/specs/2026-06-04-phase3c-identity-accounts-design.md`; план —
`docs/superpowers/plans/2026-06-04-phase3c-identity-accounts.md`. Решения #55–#64.
**Phase 4** несёт: серверную проверку чека Apple (флип `source`), TLS/домен,
Homebrew-дистрибуцию, Postgres (через `Store`).
```

- [ ] **Step 4: Commit**

```bash
git add docs/decisions.md README.md docs/ROADMAP.md
git commit -m "docs: record Phase 3C decisions #55-64, remote-pairing run, roadmap status"
```

---

### Task 20: Full-suite verification + gofmt + vet

**Files:** none (verification only)

- [ ] **Step 1: Format and vet**

Run: `gofmt -l . && go vet ./...`
Expected: `gofmt -l` prints nothing (all formatted); `go vet` reports no issues. If `gofmt -l` lists files, run `gofmt -w <files>` and re-commit.

- [ ] **Step 2: Run the whole hermetic suite**

Run: `go test ./...`
Expected: all packages PASS — `internal/signal`, `internal/store`, `internal/signalbroker`, `internal/server` (incl. the four integration tests), and the untouched Phase 0–2 packages. No idb/network/browser needed.

- [ ] **Step 3: Build all commands**

Run: `go build ./...`
Expected: clean build of `cmd/simbeamd` and `cmd/simbeam-signal`.

- [ ] **Step 4: Final manual end-to-end (the Phase 3C DoD)**

Following the README run block: press **P**, pair a browser, see the sims list, reload (drop fragment), reconnect a saved Mac with no QR, apply a future-dated subscription, reconnect and confirm the TURN indicator flips, then set a past date and confirm it flips back. Inspect the `subscriptions` row in the SQLite file.

- [ ] **Step 5: Commit any formatting fixes** (if Step 1 changed files)

```bash
git add -A
git commit -m "chore: gofmt + vet clean for Phase 3C"
```

---

## Self-Review Notes (for the executor)

- **Spec coverage:** §1 identities/connection → Tasks 1–2, 12; §1 challenge-response → Tasks 3, 6, 9, 12; §2 enrollment with `S` → Tasks 3, 4, 12, 13, 14; §3 subscriptions store → Task 8; §3 endpoint two-sig → Tasks 5, 10; §3 TURN gate → Task 9; §4 server process flags → Tasks 11, 13; §5 browser bench → Tasks 16–18; §6 testing → Tasks 1–10 (unit) + 14–15 (integration) + 17–18 (manual) + 20 (full suite).
- **Type consistency to watch:** `Identity{PubB64, Priv}`, `PinnedStore.{Contains,Add,Remove}`, `pairingWindow.{open/Open, verify}` + `NewPairingWindow`, `Store.{Upsert,Get,Active,Close}` with `now time.Time`, broker `Config.{Store, AppSecret}` (no `GrantTURN`), `signal.{NewNonce,NewPairingSecret,EnrollProof,VerifyEnrollProof,CanonicalSubscription,AppSig,VerifyAppSig}`, message types `TypeConnect/TypeChallenge/TypeProof` + fields `Daemon/Nonce/BrokerNonce/Pair/BrokerSig`, `Server.ServeSignal(ctx, signalURL, Identity, *PinnedStore, *pairingWindow)`.
- **Wire-contract parity (Go ↔ browser):** sign UTF-8 bytes of nonce strings; `EnrollProof` uses `pub‖0x00‖nonce`; `CanonicalSubscription` uses `0x1f`; Ed25519 pubkeys base64 std. A mismatch here fails the integration tests (Go side) before the browser ever sees it.
- **Known interaction:** the daemon WS now stays open across client sessions (revises #51) — `serveOnce` must not return after sending an answer; only a read error / ctx cancel ends it.

