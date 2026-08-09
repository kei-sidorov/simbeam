# Deploying the simbeam signalling server (Phase 4)

A single VPS runs the signalling broker (`simbeam-signal`) behind Caddy (automatic
HTTPS). The TURN relay is Cloudflare Realtime TURN — managed, anycast, nothing to
run on the VPS. The broker auto-updates itself from GitHub Releases via a systemd
timer — no CI access to the server, no secrets in the repo.

## Prerequisites

- A Linux VPS (amd64) and a domain pointing at it (A record for `signal.<domain>`).
- Port 443 (Caddy). No relay ports — the relay is not on this host.
- A Cloudflare account with a Realtime TURN key (dashboard → Realtime → TURN Keys).

## One-time setup

```bash
# On the VPS, as root, from a checkout of this repo:
git clone https://github.com/kei-sidorov/simbeam && cd simbeam
sudo ./deploy/bootstrap.sh
```

`bootstrap.sh` lays down the systemd units + updater, creates the `simbeam` user and
`/etc/simbeam/signal.env` from the template, pulls the first binary, and enables the
broker + auto-update timer.

## Configure

1. **Cloudflare TURN key**: dashboard → Realtime → TURN Keys → create. Keep the key
   ID and the API token (the token is displayed once).
2. **`/etc/simbeam/signal.env`** (chmod 600): set `SIMCAST_APP_SECRET` (must match the
   value your client/app signs subscription POSTs with), `SIMBEAM_TURN_API_TOKEN`, and
   `--turn-key-id` inside `SIMCAST_SIGNAL_ARGS`. Then `systemctl restart simbeam-signal`.
3. **Caddy**: install Caddy, put `deploy/Caddyfile` at `/etc/caddy/Caddyfile` with your
   domain, then `systemctl reload caddy`. Pairing URLs now use `wss://signal.<domain>/ws`.

## Auto-update

`simbeam-signal-update.timer` runs every ~10 min: it compares the running
`simbeam-signal --version` to the latest GitHub release, and on a new version
downloads the linux binary, verifies its SHA-256 against `checksums.txt`, atomically
swaps `/usr/local/bin/simbeam-signal`, and restarts the unit. Check it:

```bash
systemctl list-timers simbeam-signal-update.timer
journalctl -u simbeam-signal-update.service --no-pager | tail
/usr/local/bin/simbeam-signal-update.sh --dry-run   # manual check
```

To ship a new server version, just push a git tag `vX.Y.Z` — the timer pulls it
within ~10 min. Full operational runbook (timing, observing, failure modes,
rollback, what does *not* auto-update): see [`UPDATING.md`](UPDATING.md).

## Optional: the demo daemon (`simbeamd demo`)

The same VPS can host an interactive **demo device** — a headless Chromium tab
streamed exactly like a simulator (App Review, try-before-you-buy). No macOS
required:

```bash
apt-get install -y ffmpeg
# On Ubuntu the apt `chromium` package is a snap shim and does not run under the
# unit's sandbox — install Google's .deb instead (chromedp finds it on PATH):
curl -fsSL -o /tmp/chrome.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
apt-get install -y /tmp/chrome.deb

# grab the linux simbeamd from the same GitHub release the updater uses:
curl -fsSL -o /tmp/simbeamd.tgz \
  "https://github.com/kei-sidorov/simbeam/releases/latest/download/simbeamd_<version>_linux_amd64.tar.gz"
tar -xzf /tmp/simbeamd.tgz -C /usr/local/bin simbeamd

# the demo page itself lives on the box, under the unit's StateDirectory:
install -d -o simbeam -g simbeam /var/lib/simbeam/demo
install -o simbeam -g simbeam -m 0644 web/demo/index.html /var/lib/simbeam/demo/index.html

cp deploy/systemd/simbeamd-demo.service /etc/systemd/system/
cp deploy/demo.env.example /etc/simbeam/demo.env && chmod 600 /etc/simbeam/demo.env
# edit /etc/simbeam/demo.env: broker URL, demo page URL, a fixed SIMCAST_PAIR_SECRET
systemctl daemon-reload && systemctl enable --now simbeamd-demo
journalctl -u simbeamd-demo --no-pager | grep -A3 "Pairing URL"
```

The logged pairing URL is **multi-use** (the enrollment window re-arms after every
pairing) and stable across restarts thanks to the fixed secret — put it in App
Review notes or a "try the demo" button. It changes only if the daemon's identity
key (`/var/lib/simbeam/demo-identity.key`) is lost — a rebuilt box means new notes.
If Chrome refuses to start under the unprivileged unit user (Ubuntu ≥23.10
restricts user namespaces for non-apt binaries), `--chrome-no-sandbox` in
`SIMCAST_DEMO_ARGS` is the last resort; the Google .deb has not needed it.

**Firewall — REQUIRED for the demo (and easy to miss).** Unlike a Mac daemon
behind home NAT (which only makes outbound connections), the demo daemon runs on
the VPS's *public* IP, so the client must reach its WebRTC media directly. pion
gathers its host candidate on a random port from the OS ephemeral range
(`/proc/sys/net/ipv4/ip_local_port_range`, typically 32768–60999). If ufw is on
and that range is closed, connections fail intermittently — a client sees
`direct connect failed` / a stalled offer whenever pion happens to pick a blocked
port. Open the ephemeral UDP range:

```bash
ufw allow 32768:60999/udp comment 'pion ICE host candidates (demo daemon)'
# verify the OS range matches: sysctl net.ipv4.ip_local_port_range
```

(The broker rule — `443/tcp` — is separate. TURN needs no inbound rules at all:
the relay is Cloudflare's, not this host's.) A tighter alternative to the
wide range is to pin pion to a small dedicated range in code and open only that;
not yet done — see the roadmap.

## ICE entries the browser receives

| Entry | When | Cost |
|-------|------|------|
| `stun:` | always | ~free (stateless) |
| `turn:`/`turns:` on `turn.cloudflare.com` | only when the client's subscription is active | relays media — the metered resource ($0.05/GB after 1000 GB/mo free) |

The TURN gate reads the subscription store keyed by the challenge-verified client key
(Phase 3C, decision #63). Free tier (STUN only) works on the same LAN and friendly
NATs; a hostile NAT yields `connectionState === "failed"` and the client shows the upsell.

The broker holds **one** Cloudflare credential shared by all active subscribers,
refetched at half its `--turn-ttl` (default 24h — Cloudflare drops an allocation once
its credential expires, so the TTL has to outlive a streaming session; their max is
48h). If Cloudflare's API is down, subscribers degrade to STUN only rather than
failing the handshake — check `journalctl -u simbeam-signal | grep 'turn credential'`.
