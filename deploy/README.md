# Deploying the simcast signalling server (Phase 4)

A single VPS runs the signalling broker (`simcast-signal`) + coturn (TURN relay)
behind Caddy (automatic HTTPS). The broker auto-updates itself from GitHub Releases
via a systemd timer — no CI access to the server, no secrets in the repo.

## Prerequisites

- A Linux VPS (amd64) and a domain pointing at it (A record for `signal.<domain>`).
- Ports: 443 (Caddy), 3478 + the coturn relay UDP range (TURN).

## One-time setup

```bash
# On the VPS, as root, from a checkout of this repo:
git clone https://github.com/kei-sidorov/simcast && cd simcast
sudo ./deploy/bootstrap.sh
```

`bootstrap.sh` installs coturn, lays down the systemd units + updater, creates the
`simcast` user and `/etc/simcast/signal.env` from the template, pulls the first
binary, and enables the broker + auto-update timer.

## Configure

1. **`/etc/simcast/signal.env`** (chmod 600): set `SIMCAST_APP_SECRET` (must match the
   value your client/app signs subscription POSTs with) and the `--turn-secret` /
   domain inside `SIMCAST_SIGNAL_ARGS`. Then `systemctl restart simcast-signal`.
2. **coturn** (`/etc/turnserver.conf`): set `static-auth-secret` **equal to** the
   broker's `--turn-secret`, set `external-ip` and `realm`, then
   `systemctl enable --now coturn`.
3. **Caddy**: install Caddy, put `deploy/Caddyfile` at `/etc/caddy/Caddyfile` with your
   domain, then `systemctl reload caddy`. Pairing URLs now use `wss://signal.<domain>/ws`.

## Auto-update

`simcast-signal-update.timer` runs every ~10 min: it compares the running
`simcast-signal --version` to the latest GitHub release, and on a new version
downloads the linux binary, verifies its SHA-256 against `checksums.txt`, atomically
swaps `/usr/local/bin/simcast-signal`, and restarts the unit. Check it:

```bash
systemctl list-timers simcast-signal-update.timer
journalctl -u simcast-signal-update.service --no-pager | tail
/usr/local/bin/simcast-signal-update.sh --dry-run   # manual check
```

To ship a new server version, just push a git tag `vX.Y.Z` — the timer pulls it
within ~10 min. Full operational runbook (timing, observing, failure modes,
rollback, what does *not* auto-update): see [`UPDATING.md`](UPDATING.md).

## Optional: the demo daemon (`simcastd demo`)

The same VPS can host an interactive **demo device** — a headless Chromium tab
streamed exactly like a simulator (App Review, try-before-you-buy). No macOS
required:

```bash
apt-get install -y chromium ffmpeg
# grab the linux simcastd from the same GitHub release the updater uses:
curl -fsSL -o /tmp/simcastd.tgz \
  "https://github.com/kei-sidorov/simcast/releases/latest/download/simcastd_<version>_linux_amd64.tar.gz"
tar -xzf /tmp/simcastd.tgz -C /usr/local/bin simcastd

cp deploy/systemd/simcastd-demo.service /etc/systemd/system/
cp deploy/demo.env.example /etc/simcast/demo.env && chmod 600 /etc/simcast/demo.env
# edit /etc/simcast/demo.env: broker URL, demo page URL, a fixed SIMCAST_PAIR_SECRET
systemctl daemon-reload && systemctl enable --now simcastd-demo
journalctl -u simcastd-demo --no-pager | grep -A3 "Pairing URL"
```

The logged pairing URL is **multi-use** (the enrollment window re-arms after every
pairing) and stable across restarts thanks to the fixed secret — put it in App
Review notes or a "try the demo" button. If Chromium refuses to start under the
unprivileged unit user (Ubuntu ≥23.10 restricts user namespaces for non-apt
binaries), prefer the distro `chromium` package; `--chrome-no-sandbox` in
`SIMCAST_DEMO_ARGS` is the last resort.

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

(The broker/coturn rules — `443/tcp`, `3478,5349/tcp,udp`, `49152:65535/udp`
relay — are separate and already needed for TURN.) A tighter alternative to the
wide range is to pin pion to a small dedicated range in code and open only that;
not yet done — see the roadmap.

## ICE entries the browser receives

| Entry | When | Cost |
|-------|------|------|
| `stun:` | always | ~free (stateless) |
| `turn:` + ephemeral HMAC creds | only when the client's subscription is active | relays media — the metered resource |

The TURN gate reads the subscription store keyed by the challenge-verified client key
(Phase 3C, decision #63). Free tier (STUN only) works on the same LAN and friendly
NATs; a hostile NAT yields `connectionState === "failed"` and the client shows the upsell.
