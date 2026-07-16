# simbeam

**Stream an iOS Simulator from your Mac to an iPad (or browser) for remote development.**

Run the iOS Simulator on a Mac, then see and control it from anywhere. Taps, swipes, the
Home button and the hardware keyboard are proxied back into the simulator over a peer-to-peer
WebRTC link. The Mac makes only outbound connections — **zero open ports**.

> **Simulators only.** Real devices are explicitly out of scope ([why](docs/ARCHITECTURE.md)).

---

## How it works

simbeam is a thin orchestration layer, not a new capture engine. All the heavy lifting —
CoreSimulator, screen capture, event injection — is done by Meta's
[`idb_companion`](https://github.com/facebook/idb) (MIT). The daemon talks gRPC to it,
re-encodes frames with hardware H.264, and ships them out over WebRTC.

```
iPad / browser                                Mac
┌──────────────┐                       ┌───────────────────────────────┐
│  <video>     │ ◄── H.264 (WebRTC) ── │ simbeamd (Go)                 │
│  gestures    │ ── control (DataCh) ─►│  ├─ idb_companion  (gRPC)     │
└──────┬───────┘                       │  │    describe/screenshot/hid │
       │                               │  ├─ ffmpeg (h264_videotoolbox)│
       │   handshake only              │  └─ pion   (WebRTC + control) │
       ▼                               └──────────────┬────────────────┘
┌──────────────┐                                      ▼
│  signalling  │  ◄── outbound WSS from both ──   iOS Simulator
│  broker      │       (rendezvous, then P2P)    (CoreSimulator, needs Xcode)
└──────────────┘
```

The signalling broker only brokers the **handshake**. Once peers find each other, video and
control flow directly P2P and are end-to-end encrypted by WebRTC (DTLS-SRTP) — even when
relayed through TURN. Pairing is authenticated with Ed25519 key pinning on both ends, so a
malicious broker can disrupt but never impersonate or eavesdrop.

**Why re-encode instead of forwarding idb's H.264?** `idb_companion` emits a fixed ~10s GOP
with no keyframe control, which produces multi-second artifacts on scene changes. By encoding
PNG screenshots through our own ffmpeg pipeline we own the GOP and emit short keyframes
(~1–2s). See [decisions #34–#40](docs/decisions.md).

## Components

| Part | What it is |
|------|------------|
| **`simbeamd`** | macOS daemon. Boots/streams a simulator, serves video over WebRTC, injects input. Open source (this repo). |
| **`simbeam-signal`** | Reference signalling broker. Rendezvous over WSS, relays the SDP handshake, issues short-lived TURN credentials. Open source (this repo). |
| **iPad client** | Native WebRTC client. Separate, paid product in its own repo — built once the server side is proven in the browser. |

simbeam is **open-core**: the server is OSS, the polished client is the commercial product.
The protocol is open; the moat is client UX and managed cloud infrastructure.

## Install

The daemon ships as a Homebrew cask (an unsigned, prebuilt binary; the cask strips the
macOS quarantine flag post-install). It pulls `idb-companion` and `ffmpeg` as dependencies:

```bash
brew install --cask kei-sidorov/simbeam/simbeamd
simbeamd version
```

Update later with `brew upgrade --cask simbeamd`.

## Quick start

List the simulators on your machine (uses `xcrun simctl` — works even before idb is installed):

```bash
simbeamd list
```

Pair a device and start streaming. By default `simbeamd serve` connects to the **public
broker** baked into the release build — no flags, no port forwarding:

```bash
simbeamd serve
```

Press **P** in the daemon terminal to open a one-time pairing window — it prints a pairing URL
and a QR code. Scan it (or open the URL) and confirm **Pair this Mac**. The client remembers the
Mac by its public key and reconnects automatically afterwards — no QR next time. Revoke a device
with `simbeamd unpair <clientPubKey>`. The daemon's identity lives in `~/.simbeam/` (0600).

> Want to drive it from a browser instead of the native client? Run with `--web ./web/debug`
> and open the printed pairing URL — a reference debug client implements the full WebRTC flow.

## Demo mode (no Mac required)

`simbeamd demo` streams a **headless Chromium tab** instead of a simulator — an
always-on interactive demo device for App Review notes and try-before-you-buy.
It runs anywhere Chromium and ffmpeg run, including a Linux VPS (H.264 via
`libx264` there, `h264_videotoolbox` on macOS):

```bash
simbeamd demo --signal wss://your-broker/ws --url https://your-demo-page \
              --pair-secret "$(openssl rand -base64 24)"
```

Pairing in demo mode is unattended and **multi-use**: the printed pairing URL
enrolls any number of clients and (with a fixed `--pair-secret`) survives
restarts. Taps, swipes and the keyboard are injected as real browser touch/key
events; Home returns to the start page. See [`deploy/README.md`](deploy/README.md)
for running it as a systemd unit next to the broker.

## The signalling server

The broker is a lightweight rendezvous point. It:

- keeps daemons reachable via a long-lived **outbound** WSS connection (live presence, auto-reconnect);
- relays exactly one offer/answer handshake per pairing, then gets out of the way;
- issues short-lived HMAC TURN credentials to subscribers (free tier gets STUN + host candidates).

It never sees your media and cannot forge either peer. **A public broker is baked into release
builds**, so most users never touch it. To run your own, point the daemon at it:

```bash
simbeamd serve --signal wss://your-broker.example/ws
```

Self-hosting is a documented, secrets-free path: VPS + systemd, broker + coturn behind Caddy
(automatic TLS), with a pull-based auto-updater that tracks GitHub Releases. See
[`deploy/README.md`](deploy/README.md).

## Requirements

- macOS with **Xcode / Command Line Tools** — required for the simulators themselves.
- **`idb_companion`** — installed automatically by the cask, or `brew install idb-companion`.
- **`ffmpeg`** with `h264_videotoolbox` — installed automatically by the cask, or `brew install ffmpeg`.

For browser playback, **Chrome** is recommended (`jitterBufferTarget=0` for lower latency;
Safari ignores the hint but still plays).

## Build from source

Requires Go (`brew install go`), plus `idb_companion` and `ffmpeg` in `PATH`.

```bash
go run ./cmd/simbeamd list                      # enumerate simulators
make run-remote                                 # daemon + baked broker + debug web client
go run ./cmd/simbeam-signal --addr :9000        # run a local broker
```

gRPC stubs for `idb.proto` are committed (`internal/idbpb`); regenerate via the `Makefile`.

## Repository layout

```
simbeam/
├── cmd/simbeamd/         # macOS daemon (boot, stream, input, pairing)
├── cmd/simbeam-signal/   # reference signalling broker
├── internal/             # companion (CLI lifecycle), idb (gRPC), rtc (pion), ...
├── web/debug/            # browser reference client (served with --web)
├── deploy/               # self-host scaffolding (systemd, Caddy, coturn, updater)
├── proto/idb.proto       # idb gRPC contract
└── docs/                 # architecture, roadmap, decision log
```

## Scope & non-goals

Simulators only — no real-device support. Deliberately deferred: adaptive bitrate, a custom
ScreenCaptureKit pipeline, bundling/notarizing `idb_companion`. See
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the reasoning.

## Documentation

- [`docs/HOW-IT-WORKS.md`](docs/HOW-IT-WORKS.md) — plain-language tour of the protocol: actors, pairing, sessions, TURN & subscriptions (with a glossary). Good to feed an LLM building a client.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — what we build and **why** (full context).
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — phases and their definition of done.
- [`docs/decisions.md`](docs/decisions.md) — chronological decision log (ADR-lite).
- [`deploy/README.md`](deploy/README.md) — self-hosting the broker + TURN.
