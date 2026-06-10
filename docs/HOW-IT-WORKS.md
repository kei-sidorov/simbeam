# How simcast works

A plain-language tour of the protocol: the actors, what happens on first connect,
how pairing works, how a paired session talks, and how TURN and subscriptions fit in.

This document describes **observable behavior and wire messages**, not code internals.
It is meant to be readable on its own and complete enough to implement a client (e.g. an
iPad app) against. Exact field-level constructions are included where a client must
reproduce them byte-for-byte.

A [glossary](#glossary) at the bottom explains every abbreviation.

---

## The three actors

| Actor | Where it runs | What it does |
|-------|---------------|--------------|
| **Daemon** (`simcastd`) | on the Mac | Owns the simulators. Streams H.264 video, accepts touch/keyboard input. Has a permanent cryptographic identity. |
| **Broker** (`simcast-signal`) | on a server (public or self-hosted) | A meeting point. Helps a client and a daemon find each other and exchange a WebRTC handshake. Also issues TURN credentials and stores subscriptions. |
| **Client** | iPad app / browser | Watches which Macs are online, pairs with a Mac once, then connects to see and control a simulator. |

### What the broker is — and isn't

The broker is **only a rendezvous point**. Think of it as a phone switchboard: it connects
two parties and then steps aside. Specifically:

- It relays a handful of small JSON messages so a client and a daemon can complete a WebRTC
  handshake.
- After the handshake, **video and control flow directly peer-to-peer** between client and
  Mac. The broker never sees them.
- All media is end-to-end encrypted by WebRTC itself (DTLS-SRTP) — true even if traffic is
  relayed through TURN. **A malicious or compromised broker cannot eavesdrop or impersonate
  either side.** It can only refuse to connect you.

Because the broker can't be fully trusted, both ends authenticate each other with their own
keys (see [Pairing](#pairing) and [Connecting](#connecting-a-paired-session)). The broker is
*untrusted by design*.

Everything the daemon does is **outbound**: it dials the broker, it never listens for inbound
connections. The Mac opens **zero ports**. This is why simcast works from behind a home
router without any port forwarding.

---

## First connect

Everyone connects to the broker over a single WebSocket endpoint: `wss://<broker>/ws`.
The very first message you send declares who you are and what you want.

### The daemon comes online

When `simcastd` starts (or wakes from sleep), it dials the broker and **registers**:

```json
{ "type": "register", "role": "daemon", "daemon": "<daemonID>" }
```

- `daemonID` is the daemon's **public key** (Ed25519, base64). It is stable — the same Mac
  always has the same `daemonID` — so it doubles as the Mac's permanent address on the broker.
- The daemon then *stays connected*, holding this WebSocket open with automatic reconnect and
  keepalive pings. As long as that socket is alive, the Mac is considered **online**.

The daemon does not need a client to be present. It registers and waits.

### The client comes online

A client doesn't have to authenticate to *look around*. Its first action is usually to ask
which of its known Macs are online — see [Presence](#presence-who-is-online) next. To actually
connect, it sends a `join` (see [Connecting](#connecting-a-paired-session)).

---

## Presence: who is online

A client can check whether a Mac is online **without any authentication**, knowing only its
`daemonID` (which is a public key — not a secret). This powers the green/grey online dots in
the UI.

The client opens a WebSocket and its **first message** is a watch request listing the Macs it
cares about:

```json
{ "type": "watch", "daemons": ["<daemonID-A>", "<daemonID-B>"] }
```

The broker immediately replies with a **snapshot** of their current state:

```json
{ "type": "presence", "states": { "<daemonID-A>": true, "<daemonID-B>": false } }
```

After that, the broker pushes a small **delta** whenever any watched Mac comes or goes — one
key at a time:

```json
{ "type": "presence", "states": { "<daemonID-A>": false } }
```

`true` = that Mac's daemon currently holds a live WebSocket to the broker. `false` = offline.
The watch connection stays open; the client just listens. No keys, no signatures — presence is
public information keyed by a public key.

> **Why this is safe:** knowing a `daemonID` lets you see if a Mac is online and *attempt* to
> connect, but you still can't connect unless that Mac has pinned your key during pairing. The
> daemonID is an address, not a credential.

---

## Pairing

Before a client can ever connect to a Mac, the two must **pair** once. Pairing teaches the
daemon to trust one specific client key. After that, the client reconnects forever with no
further pairing.

**Pairing is just a dial-up/introduction layer.** It does not move any video or control. It
exists solely so the daemon learns "this client key is allowed," and the client learns "this
is the real Mac's key."

### What the daemon generates and shows

The Mac's owner triggers pairing by pressing **P** in the daemon's terminal. The daemon then:

1. Generates a short-lived **one-time pairing secret `S`** (random, ~12 characters, expires in
   a few minutes, usable once).
2. Builds a **pairing URL** and renders it as a QR code in the terminal.

The pairing URL carries everything the client needs, in the URL **fragment** (the part after
`#`, which browsers and servers never send over the network — it stays on the device):

```
https://<client-app>/#signal=<wss-broker-url>&daemon=<daemonID>&pair=<S>
```

| Parameter | Meaning |
|-----------|---------|
| `signal`  | the broker's WebSocket URL to dial |
| `daemon`  | the daemon's public key (`daemonID`) — the client **pins** this as the real Mac |
| `pair`    | the one-time secret `S` |

### What the client does with it

The client scans the QR (or opens the URL) and reads `signal`, `daemon`, `pair` from the
fragment. It then:

1. Generates **its own** permanent keypair (Ed25519) if it doesn't already have one. Its public
   key is `clientPubKey`.
2. Picks a random `nonce`.
3. Computes an **enrollment proof** that demonstrates it knows `S` *without sending `S`*:

   ```
   pair_proof = base64( HMAC-SHA256( S, clientPubKey || 0x00 || nonce ) )
   ```

   (the message is the base64 `clientPubKey` string, a single `0x00` byte, then the base64
   `nonce` string).

4. Connects to the broker and sends a **join** carrying the proof:

```json
{
  "type":  "join",
  "role":  "client",
  "daemon": "<daemonID>",
  "pubkey": "<clientPubKey>",
  "nonce":  "<nonce>",
  "pair":   "<pair_proof>"
}
```

The secret `S` never travels to the broker — only the HMAC proof does, so the untrusted broker
can't learn `S` or replay it.

### What the daemon receives and saves

The broker relays the join to the daemon as a **connect**:

```json
{ "type": "connect", "pubkey": "<clientPubKey>", "nonce": "<nonce>", "pair": "<pair_proof>" }
```

The daemon recomputes the same HMAC with its secret `S`. If it matches and the pairing window
is still open, the daemon **pins** `clientPubKey` — it saves the client's public key to its
trusted list (`~/.simcast/clients.json`). From now on, this client is recognized and the
pairing window is burned (one-time use).

After pinning, the client still completes the normal key challenge (next section) to finish the
connection. A revoked device is removed with `simcastd unpair <clientPubKey>`.

That's pairing: a one-time, secret-gated introduction that ends with **the daemon trusting the
client's public key** and **the client trusting the daemon's public key**. Neither secret nor
key ever has to be exchanged again.

---

## Connecting (a paired session)

Once paired, connecting is a **mutual key challenge** followed by the WebRTC handshake. The
broker relays the messages but learns nothing it could use to impersonate either side.

### What each side does

- **The daemon listens** (via the broker) for a paired client to show up. It already holds its
  permanent WebSocket open (it's online). When a client joins, the daemon challenges it to prove
  it owns a pinned key, then answers its WebRTC offer.
- **The client sends** a `join` (this time with no `pair` — it's already pinned), signs the
  challenge, then sends a WebRTC offer.
- **The broker relays** these messages, and *additionally* slips in its own challenge so it can
  independently confirm the client's key (this is what gates TURN — see below). It strips its own
  bits back out before forwarding, so the daemon and client only ever see what concerns them.

### The handshake, step by step

```
CLIENT                         BROKER                         DAEMON
  │  join(pubkey, daemon) ──────►│                              │
  │                              │  connect(pubkey) ───────────►│
  │                              │◄─ challenge(nonce) ──────────│
  │◄ challenge(nonce, brokerNonce)                              │
  │                                                             │
  │  proof(sig, brokerSig) ─────►│                              │
  │                              │  proof(sig) ────────────────►│   (brokerSig stripped)
  │◄ iceServers ─────────────────│◄ iceServers ─────────────────│
  │                                                             │
  │  offer(sdp) ────────────────►│  offer(sdp) ────────────────►│
  │◄ answer(sdp, sig) ───────────│◄ answer(sdp, sig) ───────────│
  │                                                             │
  │═══════ direct peer-to-peer WebRTC (video + control) ════════│
```

1. **join** — client announces itself: `{ "type":"join", "role":"client", "daemon":"<daemonID>",
   "pubkey":"<clientPubKey>", "nonce":"<nonce>" }`.
2. **challenge** — the daemon sends a random `nonce` for the client to sign. The broker adds its
   own `brokerNonce` to the same message:
   `{ "type":"challenge", "nonce":"<daemonNonce>", "brokerNonce":"<brokerNonce>" }`.
3. **proof** — the client signs **both** nonces with its private key:
   `{ "type":"proof", "sig":"<sign(daemonNonce)>", "brokerSig":"<sign(brokerNonce)>" }`.
   - The daemon verifies `sig` against the pinned `clientPubKey` → confirms it's really the paired
     client.
   - The broker verifies `brokerSig` independently → confirms the client's key for TURN gating,
     then removes `brokerSig` before forwarding.
4. **iceServers** — the broker sends each side the ICE configuration to use (STUN always; TURN if
   the client has an active subscription). See [TURN](#turn-and-subscriptions).
5. **offer / answer** — the client (offerer) sends a standard WebRTC SDP **offer**. The daemon
   (answerer) replies with an SDP **answer** *and signs it* with its private key:
   `{ "type":"answer", "sdp":"<...>", "sig":"<sign(sdp)>" }`. The client verifies `sig` against the
   pinned `daemonID` — this is the anti-MITM check that proves the answer came from the real Mac and
   not a broker substituting its own.

After the answer, ICE completes and the WebRTC connection comes up **directly between client and
Mac**. The broker's job is done.

Only one client talks to a given daemon at a time; if a second client joins, the broker drops the
first.

### The live session: control and video

Two WebRTC channels carry everything from here on, peer-to-peer:

**Control — a DataChannel labeled `control`** carries JSON commands from client to daemon and
replies back. The client sends:

| Command | Shape | Meaning |
|---------|-------|---------|
| list    | `{"type":"list"}` | enumerate the Mac's simulators |
| boot    | `{"type":"boot","udid":"<udid>"}` | power on a simulator |
| attach  | `{"type":"attach","udid":"<udid>"}` | start streaming this simulator's screen |
| detach  | `{"type":"detach"}` | stop streaming |
| tap     | `{"type":"tap","x":0.5,"y":0.5}` | tap at normalized [0,1] coordinates |
| swipe   | `{"type":"swipe","x1":..,"y1":..,"x2":..,"y2":..,"duration":<sec>}` | drag |
| home    | `{"type":"home"}` | press the Home button |
| key     | `{"type":"key","key":"<KeyboardEvent.key>"}` | a hardware key press |

Coordinates are **normalized 0–1** relative to the displayed frame; the daemon scales them to the
simulator's logical points. (Keyboard input sends physical HID key codes — the actual character is
chosen by the keyboard layout active *inside the simulator*.)

The daemon replies on the same channel:

| Reply | Shape |
|-------|-------|
| sims     | `{"type":"sims","sims":[{"udid":..,"name":..,"state":..,"os_version":..}, …]}` |
| booted   | `{"type":"booted","udid":"<udid>"}` |
| attached | `{"type":"attached","w":<points>,"h":<points>}` (the simulator's screen size) |
| detached | `{"type":"detached"}` |
| error    | `{"type":"error","msg":"<reason>"}` |

**Video — an H.264 track** flows from daemon to client. The track is negotiated up front but stays
**silent until you `attach` a simulator**. On `attach`, the daemon starts capturing that simulator
and pushing H.264; on `detach` (or a new `attach`), it stops. You don't renegotiate the WebRTC
session to switch simulators — the video track just goes quiet and resumes.

---

## TURN and subscriptions

WebRTC tries to connect the two peers directly. On the same network or with friendly routers, it
succeeds using **host candidates** or **STUN** — no relay, no cost. **STUN is always provided, to
everyone, free.**

When both peers are behind strict NATs and can't reach each other directly, WebRTC needs a **TURN**
relay — a server that forwards the encrypted media. Relays cost real bandwidth, so on the default
(public) infrastructure **TURN is gated behind a subscription**. The media stays end-to-end
encrypted even through TURN; the relay only sees ciphertext.

What this means in practice:

- **Free / no subscription:** you get STUN. You can connect on the same Wi-Fi and across many home
  networks. If both ends are on hostile NATs, the connection may fail — that's the upsell moment.
- **Active subscription:** the broker additionally hands you **short-lived TURN credentials** in the
  `iceServers` message, so the relay fallback is available.

The TURN credentials the broker issues look like this inside `iceServers`:

```json
{
  "urls": ["turn:relay.example:3478"],
  "username": "<unix-expiry>:<clientPubKey>",
  "credential": "<base64( HMAC-SHA1( turnSecret, username ) )>"
}
```

They're ephemeral (default ~1 minute TTL) and computed from a secret shared between the broker and
the TURN server (`coturn`), so no per-credential state is stored anywhere.

When you self-host, **you decide the policy** — point the broker at your own `coturn` and your own
subscription store, or skip TURN entirely and run STUN-only.

### The subscription API

A subscription is a claim — "this client key is entitled until this date" — stored at the broker and
keyed by the client's public key (the **same** `clientPubKey` used in pairing and the key challenge).
The client submits it to the broker over plain HTTPS:

```
POST /v1/subscription
Content-Type: application/json
X-App-Sig:     <base64( HMAC-SHA256( appSecret, canonical ) )>
X-Account-Sig: <base64( Ed25519-sign( clientPrivateKey, canonical ) )>

{
  "client_pubkey": "<clientPubKey>",
  "product_id":    "pro.monthly",
  "issued_at":     "2026-06-10T12:00:00Z",
  "expires_at":    "2026-07-10T12:00:00Z"
}
```

Both signatures are computed over the **same canonical byte string**, the four fields joined by a
`0x1F` (unit-separator) byte, in this exact order:

```
client_pubkey  0x1F  product_id  0x1F  expires_at  0x1F  issued_at
```

There are two signatures because they answer two different questions:

- **`X-App-Sig` — "is this our app build?"** An HMAC keyed by a shared **app secret**. This is a
  *weak* barrier on purpose: the secret is baked into the client binary, so anyone who reverse-
  engineers the app can extract it. It deters casual scripting, nothing more.
  - The app secret is configured on the broker via the **`SIMCAST_APP_SECRET`** environment variable.
    In development the value is `dev-app-secret`. (If the broker is started without it, the app-sig
    barrier is simply disabled.) The same string must be compiled into the client to produce a valid
    `X-App-Sig`.
- **`X-Account-Sig` — "does this account really authorize this?"** A real **Ed25519 signature** by the
  client's private key. This is the cryptographic boundary: it proves the holder of `clientPubKey`
  authorized the claim.

Rules and behavior:

- **`issued_at`** must be within ±48 h of the server clock (loose, since client clocks drift).
- **Idempotent, last-write-wins by `issued_at`:** re-posting is safe; only a *newer* `issued_at`
  overwrites the stored row. Apps can spam this on foreground/background without harm.
- **Response:** `200 OK` (empty body) on success; `401` on a bad signature; `400` on malformed
  timestamps. The subscription is "active" while `expires_at` is in the future — that's exactly what
  the TURN gate checks.
- **`GET /v1/subscription/me?pubkey=…&ts=…&sig=…`** returns the caller's current subscription (the
  `sig` is an Ed25519 signature over `pubkey 0x1F ts`, with a fresh `ts`). Convenience only — the
  authoritative record is the broker's store.

> Today subscriptions are **client-asserted** (the app vouches for itself). A future server-side Apple
> receipt check can replace the trust source without changing this wire shape.

---

## Putting it together: a client's lifecycle

1. **Pair once** — scan the QR from the Mac, prove knowledge of `S`, get pinned. (One time per Mac.)
2. **Watch presence** — open a `watch` socket to see which paired Macs are online. (No auth.)
3. **Subscribe (optional)** — `POST /v1/subscription` so TURN relay is available on strict networks.
4. **Connect** — `join` an online Mac, pass the key challenge, exchange offer/answer, verify the
   daemon's signature.
5. **Use it** — over the P2P link: `list` / `boot` / `attach` a simulator, watch H.264, send taps,
   swipes, Home, keys. `detach` or close to end.

---

## Glossary

| Term | Meaning |
|------|---------|
| **Daemon** | `simcastd`, the program on the Mac that owns the simulators and serves the stream. |
| **Broker** | `simcast-signal`, the rendezvous/signalling server that helps peers find each other. |
| **Client** | The iPad app (or browser) that views and controls a simulator. |
| **daemonID** | The daemon's Ed25519 **public key** (base64). Stable per Mac; serves as its address on the broker. Public, not a secret. |
| **clientPubKey** | The client's Ed25519 public key. Pinned by the daemon during pairing; identifies the account. |
| **Pinning** | Storing the other side's public key as "trusted," so future connections are authenticated by key. |
| **Pairing** | One-time introduction (secret-gated) that makes the daemon trust a client key and vice-versa. |
| **Signalling** | The exchange of small handshake messages (offer/answer/challenge) needed to set up a WebRTC link. The broker does this. |
| **Rendezvous** | Same idea as signalling — the broker is where two parties meet to start a connection. |
| **WebRTC** | The browser/native standard for real-time peer-to-peer audio/video/data. Carries the video and control channel. |
| **SDP** | Session Description Protocol — the text blob describing a WebRTC connection, exchanged as **offer** and **answer**. |
| **Offerer / Answerer** | In a WebRTC handshake, the side that proposes the session (client) vs. the side that responds (daemon). |
| **DataChannel** | A WebRTC channel for arbitrary data (here: JSON control commands), separate from the media track. |
| **ICE** | Interactive Connectivity Establishment — how WebRTC discovers a working network path between two peers. |
| **ICE candidate / host candidate** | A possible address/path for the connection; a *host* candidate is a direct local-network address. |
| **STUN** | A lightweight server that tells a peer its public address so two peers can connect **directly**. Free, always offered. |
| **TURN** | A relay server that forwards media when a direct path is impossible. Costs bandwidth; subscription-gated on the public infra. |
| **NAT** | Network Address Translation — the home/office router behavior that hides devices behind one public IP and complicates direct connections. |
| **DTLS-SRTP** | The encryption WebRTC uses for media. End-to-end between the two peers, even through a TURN relay. |
| **MITM** | Man-in-the-middle — an attacker who sits between two parties. Defeated here by signing the SDP answer with the daemon's pinned key. |
| **Ed25519** | The public-key signature scheme used for all identities and challenges. |
| **HMAC** | A keyed hash used for the pairing proof, the app-sig barrier, and TURN credentials. |
| **Nonce** | A random one-shot value sent to be signed, proving freshness and key ownership. |
| **Pairing secret `S`** | The short-lived one-time code in the QR; the client proves it knows `S` via HMAC without revealing it. |
| **Presence** | Online/offline status of a daemon, observable by `daemonID` with no authentication. |
| **App secret** | A shared HMAC key (`SIMCAST_APP_SECRET`) baked into the client; a weak "is this our build" check on the subscription API. |
| **UDID** | The unique identifier of a specific iOS simulator on the Mac. |
| **idb / idb_companion** | Meta's open-source tool the daemon drives to capture the simulator screen and inject input. |
| **GOP / keyframe** | H.264 video structure terms; simcast re-encodes frames to keep keyframes frequent for low latency. |
