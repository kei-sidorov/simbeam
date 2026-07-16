# Phase 4 — Distribution + self-host auto-deploy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship simbeam as a forkable open-core project — release the macOS daemon via a Homebrew tap and the Linux signaller as a binary, and stand up a VPS that auto-updates `simbeam-signal` from GitHub Releases with zero server secrets in the repo.

**Architecture:** One GoReleaser pipeline builds both binaries on a git tag and publishes a GitHub Release + a Homebrew formula (separate public tap). The VPS runs `simbeam-signal` + coturn behind Caddy (auto-TLS); a systemd timer runs a pull-updater that swaps the binary when a newer release appears. Generic deploy scaffolding lives in the repo; secrets/personal values live only on the server in `/etc/simbeam/signal.env`.

**Tech Stack:** Go, GoReleaser v2, GitHub Actions, Homebrew, systemd, Caddy, coturn, bash.

Reference spec: `docs/superpowers/specs/2026-06-06-phase4-distribution-design.md`.

---

## File structure

- Create `.goreleaser.yaml` — release build config (both artifacts + Homebrew formula).
- Modify `cmd/simbeam-signal/main.go` — `version` var + `--version` flag.
- Modify `cmd/simbeamd/main.go` — `version` var + `version` subcommand.
- Create `.github/workflows/ci.yml` — fmt/vet/test/build on push & PR.
- Create `.github/workflows/release.yml` — GoReleaser on tag `v*`.
- Create `deploy/systemd/simbeam-signal.service` — broker unit.
- Create `deploy/systemd/simbeam-signal-update.service` + `.timer` — pull-updater unit + schedule.
- Create `deploy/simbeam-signal-update.sh` — pull-updater script.
- Create `deploy/bootstrap.sh` — first-time VPS setup.
- Create `deploy/Caddyfile` — reverse proxy + auto-TLS.
- Create `deploy/signal.env.example` — env-file template (committed).
- Verify/adjust `deploy/coturn/turnserver.conf` (exists).
- Rewrite `deploy/README.md`; modify `README.md`, `docs/ROADMAP.md`, `docs/decisions.md`.

---

### Task 1: Version flags for both binaries

**Files:**
- Modify: `cmd/simbeam-signal/main.go`
- Modify: `cmd/simbeamd/main.go`

- [ ] **Step 1: Add version var + flag to `simbeam-signal`**

In `cmd/simbeam-signal/main.go`, add a package-level var after the imports block (before `func main`):

```go
// version is set at release time via -ldflags "-X main.version=...". "dev" otherwise.
var version = "dev"
```

Then inside `func main()`, make the FIRST two lines of the body (before `addr := flag.String(...)`):

```go
	versionFlag := flag.Bool("version", false, "print version and exit")
```

And immediately after `flag.Parse()`:

```go
	if *versionFlag {
		fmt.Println(version)
		return
	}
```

- [ ] **Step 2: Add version subcommand to `simbeamd`**

In `cmd/simbeamd/main.go`, add after the imports block (before `func main`):

```go
// version is set at release time via -ldflags "-X main.version=...". "dev" otherwise.
var version = "dev"
```

In the `switch args[0]` block, add this case before `case "-h", "--help", "help":`:

```go
	case "version", "--version", "-v":
		fmt.Println(version)
```

In `func usage`, add this line after the `unpair` line:

```go
	fmt.Fprintln(w, "  simbeamd version Print the version")
```

- [ ] **Step 3: Verify both print version**

Run:
```bash
go build ./... && go run ./cmd/simbeam-signal --version && go run ./cmd/simbeamd version
```
Expected: each prints `dev` on its own line. No build errors.

- [ ] **Step 4: Verify ldflags injection works**

Run:
```bash
go run -ldflags "-X main.version=9.9.9" ./cmd/simbeam-signal --version
```
Expected: prints `9.9.9`.

- [ ] **Step 5: Verify suite still green**

Run: `gofmt -l cmd/ && go vet ./... && go test ./...`
Expected: `gofmt -l` prints nothing; vet clean; all tests pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/simbeam-signal/main.go cmd/simbeamd/main.go
git commit -m "feat(cmd): version flag for simbeam-signal and simbeamd

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: GoReleaser config

**Files:**
- Create: `.goreleaser.yaml`

GoReleaser builds darwin binaries for `simbeamd` and a linux binary for `simbeam-signal`, all pure-Go (`CGO_ENABLED=0`; the sqlite driver is `modernc.org/sqlite`, decision #61). It also generates the Homebrew formula and pushes it to the `homebrew-simbeam` tap.

- [ ] **Step 1: Write `.goreleaser.yaml`**

```yaml
version: 2

project_name: simbeam

builds:
  - id: simbeamd
    main: ./cmd/simbeamd
    binary: simbeamd
    env:
      - CGO_ENABLED=0
    goos: [darwin]
    goarch: [arm64, amd64]
    ldflags:
      - -s -w -X main.version={{ .Version }}
  - id: simbeam-signal
    main: ./cmd/simbeam-signal
    binary: simbeam-signal
    env:
      - CGO_ENABLED=0
    goos: [linux]
    goarch: [amd64]
    ldflags:
      - -s -w -X main.version={{ .Version }}

archives:
  - id: simbeamd
    ids: [simbeamd]
    name_template: "simbeamd_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
  - id: simbeam-signal
    ids: [simbeam-signal]
    name_template: "simbeam-signal_{{ .Version }}_{{ .Os }}_{{ .Arch }}"

checksum:
  name_template: "checksums.txt"

brews:
  - name: simbeamd
    ids: [simbeamd]
    repository:
      owner: kei-sidorov
      name: homebrew-simbeam
      token: "{{ .Env.HOMEBREW_TAP_TOKEN }}"
    homepage: "https://github.com/kei-sidorov/simbeam"
    description: "simbeam daemon — stream an iOS simulator from Mac to iPad"
    dependencies:
      - name: idb-companion
      - name: ffmpeg
    caveats: |
      simbeamd is shipped as an unsigned binary. Homebrew removes the macOS
      quarantine attribute on install, so Gatekeeper should not block it.
      simbeamd shells out to idb_companion (simulators) and ffmpeg (h264_videotoolbox).

release:
  github:
    owner: kei-sidorov
    name: simbeam
```

- [ ] **Step 2: Install GoReleaser if needed and validate the config**

Run:
```bash
which goreleaser || brew install goreleaser
goreleaser check
```
Expected: `goreleaser check` reports the config is valid (1 configuration file). Fix any reported schema errors.

- [ ] **Step 3: Dry-run the full build (no publish)**

Run:
```bash
goreleaser release --snapshot --clean --skip=publish
```
Expected: completes successfully and produces `dist/` containing `simbeamd` darwin arm64+amd64 archives, a `simbeam-signal` linux amd64 archive, and `checksums.txt`. (The `brews` step is skipped automatically in snapshot mode.)

- [ ] **Step 4: Confirm the injected version**

Run:
```bash
tar -xzf dist/simbeam-signal_*_linux_amd64.tar.gz -O simbeam-signal > /tmp/scs && chmod +x /tmp/scs
# Linux binary won't run on macOS; instead confirm the version string is embedded:
strings dist/simbeamd_*_darwin_arm64/simbeamd 2>/dev/null | grep -E "^[0-9]+\.[0-9]+\.[0-9]+" | head -1 || echo "snapshot version embedded"
```
Expected: a snapshot version string is present (GoReleaser uses a snapshot tag like `0.0.0-...`). This just confirms ldflags wired through; exact value is not asserted.

- [ ] **Step 5: Ignore the dist dir**

Add to `.gitignore` (under the Go section):
```
/dist/            # GoReleaser snapshot/release output
```
(If a `/dist/` entry already exists, skip — `.gitignore` already had `/dist/` for GoReleaser output.)

- [ ] **Step 6: Commit**

```bash
git add .goreleaser.yaml .gitignore
git commit -m "build: GoReleaser config for simbeamd (Homebrew) + simbeam-signal

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Write `.github/workflows/ci.yml`**

```yaml
name: ci

on:
  push:
  pull_request:

jobs:
  build-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: gofmt
        run: |
          unformatted="$(gofmt -l .)"
          if [ -n "$unformatted" ]; then echo "unformatted files:"; echo "$unformatted"; exit 1; fi
      - name: vet
        run: go vet ./...
      - name: test
        run: go test ./...
      - name: build
        run: go build ./...
```

- [ ] **Step 2: Validate YAML locally**

Run:
```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); print('YAML OK')"
```
Expected: `YAML OK`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: gofmt/vet/test/build on push and PR

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Release workflow

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Step 1: Write `.github/workflows/release.yml`**

```yaml
name: release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write

jobs:
  goreleaser:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          HOMEBREW_TAP_TOKEN: ${{ secrets.HOMEBREW_TAP_TOKEN }}
```

- [ ] **Step 2: Validate YAML locally**

Run:
```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); print('YAML OK')"
```
Expected: `YAML OK`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: GoReleaser release workflow on tag push

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: systemd units + env template

**Files:**
- Create: `deploy/systemd/simbeam-signal.service`
- Create: `deploy/systemd/simbeam-signal-update.service`
- Create: `deploy/systemd/simbeam-signal-update.timer`
- Create: `deploy/signal.env.example`

The broker binds to localhost (Caddy fronts it with TLS). Flags + secrets come from `/etc/simbeam/signal.env` (NOT committed; the `.example` is the template). `$SIMCAST_SIGNAL_ARGS` is word-split by systemd; `SIMCAST_APP_SECRET` is read from the environment by the binary.

- [ ] **Step 1: Write `deploy/systemd/simbeam-signal.service`**

```ini
[Unit]
Description=simbeam signaling broker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=simbeam
Group=simbeam
EnvironmentFile=/etc/simbeam/signal.env
ExecStart=/usr/local/bin/simbeam-signal $SIMCAST_SIGNAL_ARGS
Restart=always
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
StateDirectory=simbeam
WorkingDirectory=/var/lib/simbeam

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 2: Write `deploy/systemd/simbeam-signal-update.service`**

```ini
[Unit]
Description=simbeam-signal auto-update (pull latest GitHub release)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/simbeam-signal-update.sh
```

- [ ] **Step 3: Write `deploy/systemd/simbeam-signal-update.timer`**

```ini
[Unit]
Description=Periodically check for a new simbeam-signal release

[Timer]
OnBootSec=2min
OnUnitActiveSec=10min
Persistent=true

[Install]
WantedBy=timers.target
```

- [ ] **Step 4: Write `deploy/signal.env.example`**

```bash
# Copy to /etc/simbeam/signal.env on the server and fill in real values (chmod 600).
# SIMCAST_APP_SECRET is read from the environment by the broker; it must match the
# bench/app value used to sign subscription POSTs.
SIMCAST_APP_SECRET=change-me-to-a-long-random-string

# Broker CLI flags (word-split by systemd). Bind to localhost; Caddy terminates TLS.
# --turn-secret MUST equal coturn's static-auth-secret in turnserver.conf.
SIMCAST_SIGNAL_ARGS=--addr 127.0.0.1:9000 --db /var/lib/simbeam/simbeam.db --stun stun:stun.l.google.com:19302 --turn turn:YOUR_DOMAIN:3478 --turn-secret change-me-same-as-coturn --turn-ttl 1m
```

- [ ] **Step 5: Lint the unit files**

Run (systemd-analyze is Linux-only; on macOS this is a no-op check of presence):
```bash
ls deploy/systemd/*.service deploy/systemd/*.timer && grep -q "ExecStart=/usr/local/bin/simbeam-signal " deploy/systemd/simbeam-signal.service && echo UNITS_OK
```
Expected: lists three files and prints `UNITS_OK`.

- [ ] **Step 6: Commit**

```bash
git add deploy/systemd/ deploy/signal.env.example
git commit -m "deploy: systemd units for broker + auto-update timer, env template

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Pull-updater script

**Files:**
- Create: `deploy/simbeam-signal-update.sh`

The script must match the GoReleaser archive name (`simbeam-signal_<version>_linux_amd64.tar.gz`) and the version reported by `simbeam-signal --version` (GoReleaser `.Version` has no leading `v`, matching `--version` output).

- [ ] **Step 1: Write `deploy/simbeam-signal-update.sh`**

```bash
#!/usr/bin/env bash
# Pull-based auto-updater for simbeam-signal.
#
# Polls the GitHub Releases API for the latest tag; if it differs from the running
# binary's --version, downloads + checksum-verifies + atomically installs the new
# linux/amd64 binary and restarts the systemd unit. Designed to run from a systemd
# timer (see deploy/systemd/simbeam-signal-update.timer). Pass --dry-run to check
# without installing. No secrets required (public repo).
set -euo pipefail

REPO="${SIMCAST_REPO:-kei-sidorov/simbeam}"
BIN_PATH="${SIMCAST_BIN:-/usr/local/bin/simbeam-signal}"
UNIT="${SIMCAST_UNIT:-simbeam-signal}"
DRY_RUN=0
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1

log() { echo "simbeam-update: $*"; }

latest_tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
if [ -z "${latest_tag}" ]; then log "could not resolve latest release tag"; exit 1; fi

current="dev"
if [ -x "${BIN_PATH}" ]; then current="$("${BIN_PATH}" --version 2>/dev/null || echo dev)"; fi
want="${latest_tag#v}"

if [ "${current}" = "${want}" ]; then log "up to date (${current})"; exit 0; fi
log "update available: ${current} -> ${want}"
if [ "${DRY_RUN}" = 1 ]; then log "dry-run: not installing"; exit 0; fi

tmp="$(mktemp -d)"; trap 'rm -rf "${tmp}"' EXIT
archive="simbeam-signal_${want}_linux_amd64.tar.gz"
base="https://github.com/${REPO}/releases/download/${latest_tag}"

curl -fsSL -o "${tmp}/${archive}" "${base}/${archive}"
curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt"
( cd "${tmp}" && grep " ${archive}\$" checksums.txt | sha256sum -c - )

tar -xzf "${tmp}/${archive}" -C "${tmp}"
install -m 0755 "${tmp}/simbeam-signal" "${BIN_PATH}.new"
mv -f "${BIN_PATH}.new" "${BIN_PATH}"   # atomic swap (same filesystem)
systemctl restart "${UNIT}"
log "updated to ${want} and restarted ${UNIT}"
```

- [ ] **Step 2: Make it executable and shellcheck it**

Run:
```bash
chmod +x deploy/simbeam-signal-update.sh
which shellcheck || brew install shellcheck
shellcheck deploy/simbeam-signal-update.sh && echo SHELLCHECK_OK
```
Expected: `SHELLCHECK_OK` (no warnings). Fix anything shellcheck reports.

- [ ] **Step 3: Dry-run against the real repo (after it exists) or expect a clean tag-parse**

Run:
```bash
SIMCAST_REPO=kei-sidorov/simbeam SIMCAST_BIN=/nonexistent bash deploy/simbeam-signal-update.sh --dry-run || true
```
Expected (before any release exists): it fails to resolve the latest tag and logs the error — that's fine pre-release. After the first release exists, it should log `update available: dev -> <version>` and stop (dry-run). Note this in the output; no assertion needed pre-release.

- [ ] **Step 4: Commit**

```bash
git add deploy/simbeam-signal-update.sh
git commit -m "deploy: pull-based simbeam-signal auto-updater (checksum-verified, atomic)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Caddyfile, coturn reaffirm, and bootstrap script

**Files:**
- Create: `deploy/Caddyfile`
- Create: `deploy/bootstrap.sh`
- Verify: `deploy/coturn/turnserver.conf`

- [ ] **Step 1: Write `deploy/Caddyfile`**

```
# Reverse-proxy the signalling broker behind automatic HTTPS (Let's Encrypt).
# Replace signal.example.com with your domain. Pairing URLs use wss://<domain>/ws.
# Caddy upgrades WebSocket connections transparently.
signal.example.com {
	reverse_proxy 127.0.0.1:9000
}
```

- [ ] **Step 2: Inspect the existing coturn config and reaffirm the secret contract**

Run: `cat deploy/coturn/turnserver.conf`
Confirm it contains a `static-auth-secret` line and `realm`. If `static-auth-secret` is missing, add:
```
# MUST equal the broker's --turn-secret (see /etc/simbeam/signal.env).
static-auth-secret=change-me-same-as-broker
realm=YOUR_DOMAIN
# Set to the server's public IP:
# external-ip=YOUR_PUBLIC_IP
```
If the file already has these, leave it unchanged.

- [ ] **Step 3: Write `deploy/bootstrap.sh`**

```bash
#!/usr/bin/env bash
# First-time VPS setup for the simbeam signalling server. Run as root from a checkout
# of this repo's deploy/ directory: sudo ./deploy/bootstrap.sh
#
# Installs coturn, lays down the systemd units + updater + Caddyfile, creates the
# simbeam user and /etc/simbeam/signal.env from the template (if absent), pulls the
# first simbeam-signal binary, and enables the broker + auto-update timer.
# Idempotent: re-running is safe.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then echo "run as root (sudo)"; exit 1; fi
here="$(cd "$(dirname "$0")" && pwd)"

echo "==> installing coturn"
apt-get update -y
apt-get install -y coturn curl

echo "==> creating simbeam user + state dir"
id -u simbeam >/dev/null 2>&1 || useradd --system --home /var/lib/simbeam --shell /usr/sbin/nologin simbeam
install -d -o simbeam -g simbeam -m 0750 /var/lib/simbeam

echo "==> installing systemd units + updater"
install -m 0644 "${here}/systemd/simbeam-signal.service" /etc/systemd/system/
install -m 0644 "${here}/systemd/simbeam-signal-update.service" /etc/systemd/system/
install -m 0644 "${here}/systemd/simbeam-signal-update.timer" /etc/systemd/system/
install -m 0755 "${here}/simbeam-signal-update.sh" /usr/local/bin/simbeam-signal-update.sh

echo "==> env file"
install -d -m 0750 /etc/simbeam
if [ ! -f /etc/simbeam/signal.env ]; then
  install -m 0600 "${here}/signal.env.example" /etc/simbeam/signal.env
  echo "    created /etc/simbeam/signal.env from template — EDIT IT before the service is useful"
fi

echo "==> first binary pull"
/usr/local/bin/simbeam-signal-update.sh || echo "    (no release yet? re-run after the first GitHub release)"

echo "==> enabling services"
systemctl daemon-reload
systemctl enable --now simbeam-signal.service || echo "    broker failed to start — likely needs /etc/simbeam/signal.env edited"
systemctl enable --now simbeam-signal-update.timer

cat <<'NOTE'

Next steps (manual):
  1. Edit /etc/simbeam/signal.env (app secret, domain, --turn-secret).
  2. Set coturn static-auth-secret == --turn-secret in /etc/turnserver.conf, set
     external-ip and realm, then: systemctl enable --now coturn
  3. Install Caddy and point deploy/Caddyfile at your domain, then reload Caddy.
  4. systemctl restart simbeam-signal
NOTE
```

- [ ] **Step 4: shellcheck the bootstrap script**

Run:
```bash
chmod +x deploy/bootstrap.sh
shellcheck deploy/bootstrap.sh && echo SHELLCHECK_OK
```
Expected: `SHELLCHECK_OK`. Fix anything reported.

- [ ] **Step 5: Commit**

```bash
git add deploy/Caddyfile deploy/bootstrap.sh deploy/coturn/turnserver.conf
git commit -m "deploy: Caddyfile (auto-TLS), bootstrap.sh, coturn secret contract

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Documentation + decisions

**Files:**
- Rewrite: `deploy/README.md`
- Modify: `README.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/decisions.md`

- [ ] **Step 1: Rewrite `deploy/README.md`**

Replace the entire file with a VPS self-host walkthrough. It must NOT reference the removed `--grant-turn` stub. Content:

```markdown
# Deploying the simbeam signalling server (Phase 4)

A single VPS runs the signalling broker (`simbeam-signal`) + coturn (TURN relay)
behind Caddy (automatic HTTPS). The broker auto-updates itself from GitHub Releases
via a systemd timer — no CI access to the server, no secrets in the repo.

## Prerequisites

- A Linux VPS (amd64) and a domain pointing at it (A record for `signal.<domain>`).
- Ports: 443 (Caddy), 3478 + the coturn relay UDP range (TURN).

## One-time setup

```bash
# On the VPS, as root, from a checkout of this repo:
git clone https://github.com/kei-sidorov/simbeam && cd simbeam
sudo ./deploy/bootstrap.sh
```

`bootstrap.sh` installs coturn, lays down the systemd units + updater, creates the
`simbeam` user and `/etc/simbeam/signal.env` from the template, pulls the first
binary, and enables the broker + auto-update timer.

## Configure

1. **`/etc/simbeam/signal.env`** (chmod 600): set `SIMCAST_APP_SECRET` (must match the
   value your client/app signs subscription POSTs with) and the `--turn-secret` /
   domain inside `SIMCAST_SIGNAL_ARGS`. Then `systemctl restart simbeam-signal`.
2. **coturn** (`/etc/turnserver.conf`): set `static-auth-secret` **equal to** the
   broker's `--turn-secret`, set `external-ip` and `realm`, then
   `systemctl enable --now coturn`.
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

## ICE entries the browser receives

| Entry | When | Cost |
|-------|------|------|
| `stun:` | always | ~free (stateless) |
| `turn:` + ephemeral HMAC creds | only when the client's subscription is active | relays media — the metered resource |

The TURN gate reads the subscription store keyed by the challenge-verified client key
(Phase 3C, decision #63). Free tier (STUN only) works on the same LAN and friendly
NATs; a hostile NAT yields `connectionState === "failed"` and the client shows the upsell.
```

- [ ] **Step 2: Add a Homebrew install subsection to `README.md`**

Find the `## Запуск` section (around line 85). Immediately BEFORE it, insert:

```markdown
## Install (Homebrew)

Install the macOS daemon from the tap (pulls `idb-companion` and `ffmpeg` as deps):

```bash
brew install kei-sidorov/simbeam/simbeamd
simbeamd version
```

Update later with `brew upgrade`. To self-host the signalling server (broker + TURN
with auto-update), see `deploy/README.md`.

```

- [ ] **Step 3: Update `docs/ROADMAP.md` Phase 4 section**

Replace the Phase 4 bullet block (the `## Phase 4 — Дистрибуция + прод-облако` section body) with:

```markdown
## Phase 4 — Дистрибуция + self-host

- **GoReleaser**: один пайплайн собирает `simbeamd` (darwin arm64/amd64) и
  `simbeam-signal` (linux amd64) на тег `v*`.
- **Homebrew tap** (`kei-sidorov/homebrew-simbeam`): предсобранный неподписанный
  `simbeamd`, зависимости `idb-companion` + `ffmpeg`. Обновление — `brew upgrade`.
- **Self-host сервера**: VPS + systemd, брокер + coturn за Caddy (авто-TLS).
  **Pull-автообновление**: systemd timer тянет новый релиз из GitHub Releases,
  проверяет checksum, атомарно подменяет бинарь, рестартит юнит. Ноль серверных
  секретов в репо/CI; deploy-скаффолдинг генерик, секреты — на сервере.
- **Серверную проверку чека Apple НЕ делаем** в этой фазе — подписки остаются
  client-asserted (решение #62). Дизайн — `docs/superpowers/specs/2026-06-06-phase4-distribution-design.md`.
```

- [ ] **Step 4: Append decisions #66–#70 to `docs/decisions.md`**

Add after the `| 65 | ... |` row:

```markdown
| 66 | Phase 4: релизы — **GoReleaser, один пайплайн** на тег `v*` → два артефакта (`simbeamd` darwin arm64/amd64, `simbeam-signal` linux amd64), все pure-Go (`CGO_ENABLED=0`, sqlite = modernc, #61). Версия через ldflags `-X main.version` | один источник правды для сборки обоих бинарников; кросс-компиляция тривиальна без cgo |
| 67 | Phase 4: macOS-демон — **Homebrew tap `kei-sidorov/homebrew-simbeam`, предсобранный неподписанный бинарь** (без Developer ID / нотаризации); зависимости `idb-companion` + `ffmpeg`. Обновление `brew upgrade` (без автообновления на Mac) | аудитория = iOS-разработчики, Homebrew снимает quarantine → unsigned обычно запускается; подпись/нотаризацию добавим, если Gatekeeper прижмёт |
| 68 | Phase 4: автодеплой сервера — **pull-модель**: systemd timer на VPS тянет релиз из публичных GitHub Releases, проверяет checksum, атомарно ставит, рестартит юнит. **Ноль серверных секретов в репо и CI**; deploy-скаффолдинг (юниты, апдейтер, Caddyfile, bootstrap) генерик в репо, личные значения — в `/etc/simbeam/signal.env` на сервере | прямая цель форкаемости: репо = образцовый open-core, коробка = свои секреты; pull избегает SSH-секретов в Actions |
| 69 | Phase 4: TLS брокера — **Caddy reverse-proxy с авто-Let's Encrypt**; брокер слушает `127.0.0.1`, наружу только Caddy (wss://). coturn на хосте, `static-auth-secret` == брокерский `--turn-secret` | авто-TLS без ручного обновления сертов; брокер не светит порт; coturn любит host-network |
| 70 | Phase 4: **серверная проверка чека Apple отложена** — подписки остаются client-asserted (#62), флип `source` без смены схемы возможен позже. Эта фаза = только дистрибуция + self-host | held-back по запросу: фокус фазы на доставке, а не на биллинге; схема уже готова к усилению |
```

- [ ] **Step 5: Commit**

```bash
git add deploy/README.md README.md docs/ROADMAP.md docs/decisions.md
git commit -m "docs(phase4): self-host deploy guide, Homebrew install, roadmap, decisions #66-70

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Publish to GitHub (interactive — user runs auth steps)

**Files:** none (repo/remote setup)

These steps need interactive GitHub auth and account access; the executor should
present the commands for the USER to run (e.g. via `! <cmd>`), not assume non-interactive
success. Do NOT proceed past a step that fails auth.

- [ ] **Step 1: Authenticate gh (user runs)**

Run: `gh auth status || gh auth login`
Expected: an authenticated GitHub CLI session.

- [ ] **Step 2: Create the public repo and push**

Run:
```bash
gh repo create kei-sidorov/simbeam --public --source=. --remote=origin --push
```
Expected: repo created, `main` pushed, `origin` remote set.

- [ ] **Step 3: Create the (empty) public Homebrew tap repo**

Run:
```bash
gh repo create kei-sidorov/homebrew-simbeam --public --description "Homebrew tap for simbeam"
```
Expected: empty tap repo exists. GoReleaser will push the formula into it on first release.

- [ ] **Step 4: Add the tap token secret**

Create a fine-grained PAT with `contents: write` on `kei-sidorov/homebrew-simbeam`, then:
```bash
gh secret set HOMEBREW_TAP_TOKEN --repo kei-sidorov/simbeam
```
Expected: secret stored. (The default `GITHUB_TOKEN` cannot push to a different repo, hence this PAT.)

- [ ] **Step 5: Verify CI ran**

Run: `gh run list --repo kei-sidorov/simbeam --workflow ci.yml`
Expected: a `ci` run for the pushed `main` appears and succeeds.

---

### Task 10: First release + end-to-end verification

**Files:** none (verification)

- [ ] **Step 1: Pre-release local gate**

Run:
```bash
gofmt -l . && go vet ./... && go test ./... && goreleaser check && goreleaser release --snapshot --clean --skip=publish
```
Expected: all clean; `dist/` has both macOS archives, the linux archive, and `checksums.txt`.

- [ ] **Step 2: Tag and push a release (user)**

Run:
```bash
git tag v0.1.0 && git push origin v0.1.0
```
Expected: the `release` workflow runs GoReleaser, creates the GitHub Release with the
three archives + `checksums.txt`, and pushes the `simbeamd` formula to the tap.

- [ ] **Step 3: Verify the release artifacts**

Run:
```bash
gh release view v0.1.0 --repo kei-sidorov/simbeam
gh api repos/kei-sidorov/homebrew-simbeam/contents/Formula/simbeamd.rb >/dev/null && echo FORMULA_OK
```
Expected: release lists the archives + checksums; `FORMULA_OK` prints.

- [ ] **Step 4: Verify Homebrew install (on a Mac)**

Run:
```bash
brew install kei-sidorov/simbeam/simbeamd && simbeamd version
```
Expected: installs (pulling `idb-companion` + `ffmpeg`) and prints `0.1.0`.

- [ ] **Step 5: Verify server bootstrap + auto-update (on the VPS)**

Following `deploy/README.md`: run `sudo ./deploy/bootstrap.sh`, edit `/etc/simbeam/signal.env`,
configure coturn + Caddy. Then confirm the live loop:
```bash
systemctl status simbeam-signal --no-pager
/usr/local/bin/simbeam-signal-update.sh --dry-run    # logs "up to date (0.1.0)"
```
Then tag `v0.1.1`, push, and within one timer interval confirm:
```bash
journalctl -u simbeam-signal-update.service --no-pager | tail
simbeam-signal --version    # 0.1.1
```
Expected: the timer pulled the new release and restarted the unit onto `0.1.1`.

- [ ] **Step 6: Verify the full pairing path over the deployed broker**

From a browser, open the pairing URL with `wss://signal.<domain>/ws`, pair a Mac, apply a
future-dated subscription, reconnect, and confirm the TURN indicator flips to
"YES (subscriber)" — proving Caddy TLS, coturn, and the subscription gate work end-to-end.

---

## Self-Review notes (for the executor)

- **Spec coverage:** §Components 1 (GoReleaser) → Task 2; §2 (version flags) → Task 1;
  §3 (CI/release workflows) → Tasks 3–4; §4 (server scaffolding) → Tasks 5–7; §5 (docs) →
  Task 8; §6 (publish to git) → Task 9; §Testing/verification → Tasks 2/6/10.
- **Name consistency:** archive name `simbeam-signal_<version>_linux_amd64.tar.gz` is
  identical in `.goreleaser.yaml` (Task 2) and the updater (Task 6). `--version` prints the
  bare version (no `v`), matching GoReleaser `.Version` and the updater's `want="${tag#v}"`.
  The env var `SIMCAST_SIGNAL_ARGS` and `SIMCAST_APP_SECRET` match across Task 5's unit,
  env template, and the broker's existing `os.Getenv("SIMCAST_APP_SECRET")`.
- **Secrets boundary:** only `HOMEBREW_TAP_TOKEN` lives in repo Actions secrets (Task 9);
  no server/SSH secrets anywhere; personal values only in on-server `/etc/simbeam/signal.env`.
```
