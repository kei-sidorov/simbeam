# simbeam

**Stream an iOS Simulator from your Mac to an iPad (or browser) for remote development.**

Run the iOS Simulator on a Mac, then see and control it from anywhere. Taps, swipes, the
Home button and the hardware keyboard are proxied back into the simulator over a peer-to-peer
WebRTC link. The Mac makes only outbound connections — **zero open ports**.

> **Simulators only.** Real devices are explicitly out of scope ([why](docs/ARCHITECTURE.md)).

---

## How it works

simbeam is a thin orchestration layer over a small native helper. Screen capture, H.264
encoding and event injection for the simulator are done by **`simbeam-control`**
([repo](https://github.com/kei-sidorov/simbeam-control)) — a tiny macOS binary that taps the
CoreSimulator framebuffer (IOSurface) and encodes on VideoToolbox with keyframe control we own.
The daemon spawns one per stream, reads framed H.264 from it, and ships that out over WebRTC.
Simulator lifecycle and full-resolution screenshots use Apple's own `xcrun simctl`. **No
`idb_companion`, no `ffmpeg`.**

```
iPad / browser                                Mac
┌──────────────┐                       ┌───────────────────────────────┐
│  <video>     │ ◄── H.264 (WebRTC) ── │ simbeamd (Go)                 │
│  gestures    │ ── control (DataCh) ─►│  ├─ simbeam-control           │
└──────┬───────┘                       │  │    IOSurface → H.264 + HID │
       │                               │  ├─ xcrun simctl (list/boot/  │
       │   handshake only              │  │    shutdown/shake/screenshot│
       ▼                               │  └─ pion   (WebRTC + control) │
┌──────────────┐                       └──────────────┬────────────────┘
│  signalling  │  ◄── outbound WSS ──                 ▼
│  broker      │    (rendezvous, P2P)            iOS Simulator
└──────────────┘                                 (CoreSimulator, needs Xcode)
```

The signalling broker only brokers the **handshake**. Once peers find each other, video and
control flow directly P2P and are end-to-end encrypted by WebRTC (DTLS-SRTP) — even when
relayed through TURN. Pairing is authenticated with Ed25519 key pinning on both ends, so a
malicious broker can disrupt but never impersonate or eavesdrop.

**Why our own capture engine?** `idb_companion` (Meta's idb, which simbeam used to depend on)
emits a fixed ~10s GOP with no keyframe control — multi-second artifacts on scene changes — and
its only Meta release dates to 2022. `simbeam-control` owns the encoder, so it emits short
keyframes (~1s) and a constant frame rate straight from the framebuffer, no re-encode step. See
[decisions #34–#40, #105](docs/decisions.md).

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
macOS quarantine flag post-install). Its only dependency is `simbeam-control`, vendored in the
same tap — one command, no extra taps:

```bash
brew trust kei-sidorov/simbeam    # once: Homebrew 6+ gates non-official taps
brew install --cask kei-sidorov/simbeam/simbeamd
simbeamd version
```

Homebrew 6+ refuses to load formulae/casks from a non-official tap until you trust it;
the interactive install also prompts for this, but `brew trust` up front avoids a mid-install
"Refusing to load formula … run brew trust" stop. Update later with `brew upgrade --cask simbeamd`.

> `simbeam-control` is an unsigned universal binary built from
> [its own repo](https://github.com/kei-sidorov/simbeam-control) and published as
> `Formula/simbeam-control.rb` in this tap; the cask depends on
> `kei-sidorov/simbeam/simbeam-control`, already present when the cask installs. It uses
> private CoreSimulator/SimulatorKit APIs, so a **full Xcode** install is required (not just
> the Command Line Tools).

## Quick start

List the simulators on your machine (uses `xcrun simctl` — needs only Xcode, not the stream helper):

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

- macOS with a **full Xcode** install — required both for the simulators themselves and for
  `simbeam-control` (private CoreSimulator/SimulatorKit APIs).
- **`simbeam-control`** in `PATH` — installed automatically by the cask (from this tap), or
  `brew install kei-sidorov/simbeam/simbeam-control`.

For browser playback, **Chrome** is recommended (`jitterBufferTarget=0` for lower latency;
Safari ignores the hint but still plays).

## Build from source

Requires Go (`brew install go`) and `simbeam-control` in `PATH`
(`brew install kei-sidorov/simbeam/simbeam-control`).

```bash
go run ./cmd/simbeamd list                      # enumerate simulators (simctl only)
make run-remote                                 # daemon + baked broker + debug web client
go run ./cmd/simbeam-signal --addr :9000        # run a local broker
```

## Repository layout

```
simbeam/
├── cmd/simbeamd/         # macOS daemon (boot, stream, input, pairing)
├── cmd/simbeam-signal/   # reference signalling broker
├── internal/             # companion (simctl lifecycle), backend/sim (simbeam-control), rtc (pion), ...
├── web/debug/            # browser reference client (served with --web)
├── deploy/               # self-host scaffolding (systemd, Caddy, coturn, updater)
└── docs/                 # architecture, roadmap, decision log
```

## Scope & non-goals

Simulators only — no real-device support. Deliberately deferred: adaptive bitrate,
notarizing the shipped binaries. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the
reasoning.

## Documentation

- [`docs/HOW-IT-WORKS.md`](docs/HOW-IT-WORKS.md) — plain-language tour of the protocol: actors, pairing, sessions, TURN & subscriptions (with a glossary). Good to feed an LLM building a client.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — what we build and **why** (full context).
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — phases and their definition of done.
- [`docs/decisions.md`](docs/decisions.md) — chronological decision log (ADR-lite).
- [`deploy/README.md`](deploy/README.md) — self-hosting the broker + TURN.
