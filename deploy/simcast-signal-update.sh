#!/usr/bin/env bash
# Pull-based auto-updater for simcast-signal.
#
# Polls the GitHub Releases API for the latest tag; if it differs from the running
# binary's --version, downloads + checksum-verifies + atomically installs the new
# linux/amd64 binary and restarts the systemd unit. Designed to run from a systemd
# timer (see deploy/systemd/simcast-signal-update.timer). Pass --dry-run to check
# without installing. No secrets required (public repo).
set -euo pipefail

REPO="${SIMCAST_REPO:-kei-sidorov/simcast}"
BIN_PATH="${SIMCAST_BIN:-/usr/local/bin/simcast-signal}"
UNIT="${SIMCAST_UNIT:-simcast-signal}"
DRY_RUN=0
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1

log() { echo "simcast-update: $*"; }

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
archive="simcast-signal_${want}_linux_amd64.tar.gz"
base="https://github.com/${REPO}/releases/download/${latest_tag}"

curl -fsSL -o "${tmp}/${archive}" "${base}/${archive}"
curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt"
( cd "${tmp}" && grep " ${archive}\$" checksums.txt | sha256sum -c - )

tar -xzf "${tmp}/${archive}" -C "${tmp}"
install -m 0755 "${tmp}/simcast-signal" "${BIN_PATH}.new"
mv -f "${BIN_PATH}.new" "${BIN_PATH}"   # atomic swap (same filesystem)
systemctl restart "${UNIT}"
log "updated to ${want} and restarted ${UNIT}"
