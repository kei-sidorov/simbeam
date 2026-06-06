# Phase 4 — Distribution + self-host auto-deploy (design)

**Date:** 2026-06-06
**Status:** approved (brainstorming) → ready for plan
**Scope:** Ship simcast as a forkable open-core project: release the macOS daemon via a
Homebrew tap, release the Linux signaller, and stand up a self-hosted server that
auto-updates itself. **Out of scope:** server-side Apple-receipt verification (explicitly
dropped — the subscription endpoint stays client-asserted as in Phase 3C).

## Goals

1. One release pipeline that builds both binaries and publishes them.
2. `brew install` the macOS daemon (`simcastd`) from a public tap.
3. A VPS running `simcast-signal` + coturn behind TLS that pulls and applies new
   releases automatically, with **zero server secrets in the repo or CI**.
4. The public repo stays clean and forkable: generic deploy scaffolding is in-repo,
   but all secrets/personal values live only on the server.

## Decisions (locked during brainstorming)

| Topic | Decision |
|-------|----------|
| Server runtime | VPS + systemd (binaries as units; coturn on host) |
| Release engine | GoReleaser, single pipeline → two artifacts |
| macOS distribution | Homebrew tap, **prebuilt unsigned** binary (no Developer ID / notarization) |
| macOS updates | manual `brew upgrade` (no Mac auto-update) |
| Server auto-deploy | **pull-based**: systemd timer on the VPS polls GitHub Releases |
| TLS | Caddy reverse proxy with automatic Let's Encrypt (requires a domain) |
| TURN | coturn installed on the VPS; `static-auth-secret` == broker `--turn-secret` |
| Repo visibility | public |
| Apple receipt check | **not done** in this phase (stays client-asserted, Phase 3C #62) |

## Architecture

Two independently-shippable artifacts from one Go module:

- **`simcastd`** (macOS daemon) — distributed via Homebrew. Runtime deps:
  `idb-companion` and `ffmpeg` (the encoder shells out to `ffmpeg` and needs the
  `h264_videotoolbox` encoder — `internal/encoder/ffmpeg.go`).
- **`simcast-signal`** (Linux broker) — distributed as a release binary the VPS pulls.

Data flow of a release:
```
git tag v1.2.3  →  GitHub Actions (release.yml)  →  goreleaser release
   ├─ GitHub Release: simcastd_darwin_{arm64,amd64}.tar.gz,
   │                  simcast-signal_linux_amd64.tar.gz, checksums.txt
   └─ push Homebrew formula → tap repo  kei-sidorov/homebrew-simcast

macOS user:   brew install kei-sidorov/simcast/simcastd ;  brew upgrade  (manual)
VPS:          systemd timer → updater script → GitHub Releases API →
              download linux binary + verify checksum → atomic swap → restart unit
```

## Components

### 1. GoReleaser config — `.goreleaser.yaml`

- `builds:` two entries:
  - `simcastd`: main `./cmd/simcastd`, GOOS=darwin, GOARCH=[arm64, amd64].
  - `simcast-signal`: main `./cmd/simcast-signal`, GOOS=linux, GOARCH=amd64.
  - Both inject version: `ldflags: -s -w -X main.version={{.Version}}`.
- `archives:` tar.gz per build, name template includes os/arch.
- `checksum:` `checksums.txt` (SHA-256).
- `brews:` one formula for `simcastd`:
  - tap repository: `kei-sidorov/homebrew-simcast`.
  - installs the prebuilt binary from the darwin archive (no build-from-source).
  - `depends_on "idb-companion"` and `depends_on "ffmpeg"`.
  - caveats note: unsigned binary; Homebrew strips quarantine on install.
- `release:` GitHub release with the archives + checksums.

`goreleaser check` must pass; `goreleaser release --snapshot --clean` must build all
artifacts locally without publishing.

### 2. Version flags

Add a `version` var (default `"dev"`) to both `cmd/simcastd` and `cmd/simcast-signal`,
set via ldflags at release. Surface it:
- `simcast-signal --version` → prints version and exits (the updater compares this
  against the latest GitHub tag).
- `simcastd version` (subcommand, consistent with its existing subcommand style) →
  prints version.

### 3. GitHub Actions — `.github/workflows/`

- `ci.yml` — on push + pull_request: `gofmt -l .` (fail if non-empty), `go vet ./...`,
  `go test ./...`, `go build ./...`. Pinned Go version matching `go.mod`.
- `release.yml` — on tag `v*`: checkout, set up Go, run `goreleaser/goreleaser-action`.
  Secrets: `GITHUB_TOKEN` (the release) + `HOMEBREW_TAP_TOKEN` (a PAT with write access
  to the tap repo). **No server/SSH secrets anywhere.**

### 4. Server deploy scaffolding — `deploy/`

Generic, secret-free, reusable. Personal values come from an on-server env file.

- `deploy/systemd/simcast-signal.service` — runs `/usr/local/bin/simcast-signal` with
  flags; reads `EnvironmentFile=/etc/simcast/signal.env` (NOT in the repo) for
  `SIMCAST_APP_SECRET`, `--turn-secret`, TURN/STUN URLs, listen addr, db path.
  Hardening: `Restart=always`, dedicated user, `ProtectSystem`, etc.
- `deploy/simcast-signal-update.sh` — the pull updater:
  1. GET GitHub Releases "latest" → tag.
  2. Compare to `simcast-signal --version`; exit 0 if equal.
  3. Download `simcast-signal_linux_amd64.tar.gz` + `checksums.txt`.
  4. Verify SHA-256 against checksums; abort on mismatch.
  5. Atomic install to `/usr/local/bin` (temp + `mv`), then `systemctl restart simcast-signal`.
  - Supports `--dry-run`; logs to journald; idempotent.
- `deploy/systemd/simcast-signal-update.service` (Type=oneshot, runs the script) +
  `deploy/systemd/simcast-signal-update.timer` (every ~10 min, `Persistent=true`).
- `deploy/Caddyfile` — `signal.<domain>` reverse-proxies WebSocket to broker `:9000`
  with automatic HTTPS (Let's Encrypt). Pairing URLs use `wss://signal.<domain>/ws`.
- `deploy/coturn/turnserver.conf` — existing; doc reaffirms `static-auth-secret` must
  equal the broker `--turn-secret`, plus `external-ip` / `realm`.
- `deploy/bootstrap.sh` — first-time VPS setup on a clean host: install coturn (apt),
  lay down systemd units + Caddyfile, create `/etc/simcast/signal.env` from a template,
  do the first `simcast-signal-update.sh` pull, `systemctl enable --now` the units and
  timer. Parameterized by env/prompts; contains no personal values itself.
- `deploy/signal.env.example` — template for the on-server env file (committed; the real
  `/etc/simcast/signal.env` is not).

### 5. Documentation

- Rewrite `deploy/README.md`: full VPS walkthrough (DNS/domain → bootstrap → env →
  systemd enable → coturn → Caddy → updater timer → verify). **Remove the stale
  `--grant-turn` references** (the stub was removed in Phase 3C).
- `README.md`: add an "Install (Homebrew)" subsection for the daemon, and a one-liner
  pointing at `deploy/README.md` for self-hosting the server.
- `docs/ROADMAP.md`: mark Phase 4 as distribution + self-host; note Apple-receipt
  verification is explicitly NOT in scope.
- `docs/decisions.md`: new decisions (numbers continue from #65) recording: GoReleaser
  single-pipeline; prebuilt-unsigned Homebrew via separate public tap; pull-based
  server auto-update (no CI/server secrets); Caddy auto-TLS; Apple-receipt check
  dropped from Phase 4.

### 6. Publish to git

The repo currently has no remote. Steps (the interactive `gh auth` / repo creation is
done by the user via `!`):
- `gh repo create kei-sidorov/simcast --public --source=. --remote=origin --push`
  (or add the remote manually and `git push -u origin main`).
- Create the empty public tap repo `kei-sidorov/homebrew-simcast`.
- Add `HOMEBREW_TAP_TOKEN` to the main repo's Actions secrets.

## Fork-friendliness (the explicit concern)

Generic deploy scaffolding in a public repo is normal, good practice and stays
fork-friendly **because it is secret-free and reusable**. The boundary:

- **In repo (generic):** GoReleaser config, workflows, systemd unit templates, the
  updater script, Caddyfile, coturn conf, bootstrap script, `signal.env.example`.
- **On the server only (personal/secret):** `/etc/simcast/signal.env` (app secret,
  turn secret, domain), TLS certs (Caddy-managed), the actual server.
- **In repo Actions secrets only:** `HOMEBREW_TAP_TOKEN`. No SSH keys, no server info —
  the pull model needs none.

A forker gets a clean build+release pipeline and reusable deploy assets, points the tap
at their own org, and supplies their own server env. Nothing personal leaks.

## Testing / verification

This phase is infrastructure; little is Go-unit-testable.

- CI (`ci.yml`) green on the changes.
- `goreleaser check` passes; `goreleaser release --snapshot --clean` builds all three
  artifacts locally (no publish) — the primary pre-merge gate.
- `shellcheck` clean on `simcast-signal-update.sh` and `bootstrap.sh`; updater exercised
  with `--dry-run`.
- Version flags: `go run ./cmd/simcast-signal --version` and `simcastd version` print
  the injected value (`dev` outside a release).
- Caddy / systemd / coturn / live auto-update: documented manual verification on the VPS
  (not reproducible in CI). Definition of done for the manual leg: tag a release, watch
  the VPS timer pull and restart onto the new version within one timer interval; a
  browser pairs over `wss://` and a subscriber receives a working `turn:` relay.

## Open risks (accepted)

- Unsigned macOS binary: relies on Homebrew stripping quarantine; a future macOS could
  tighten Gatekeeper. Mitigation deferred (sign/notarize later if it bites).
- Pull latency: up to one timer interval (~10 min) between release and server update.
- Subscriptions remain client-asserted (no Apple check) — unchanged from Phase 3C #62.
