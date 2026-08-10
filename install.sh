#!/bin/sh
# simbeam installer: fetches the latest simbeamd + simbeam-control release
# binaries from GitHub, verifies checksums, installs into ~/.local/bin.
# No sudo, no Homebrew. Usage:
#   curl -fsSL https://simbeam.dev/install.sh | sh
set -eu

REPO="kei-sidorov/simbeam"
CONTROL_REPO="kei-sidorov/simbeam-control"
INSTALL_DIR="${SIMBEAM_INSTALL_DIR:-$HOME/.local/bin}"

log()  { printf '  \033[32m>\033[0m %s\n' "$1"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; }
err()  { printf '  \033[31m✗\033[0m %s\n' "$1" >&2; exit 1; }

main() {
    echo ""
    echo "  simbeam installer — https://simbeam.dev"
    echo ""

    [ "$(uname -s)" = "Darwin" ] || err "simbeamd streams iOS Simulators and runs on macOS only"
    case "$(uname -m)" in
        arm64)  arch="arm64" ;;
        x86_64) arch="amd64" ;;
        *)      err "unsupported architecture: $(uname -m)" ;;
    esac
    log "detected macos/${arch}"

    for tool in curl tar shasum; do
        command -v "$tool" >/dev/null 2>&1 || err "requires '$tool'"
    done

    TMP="$(mktemp -d)"
    trap 'rm -rf "$TMP"' EXIT

    # simbeamd: checksums.txt of the latest release names every asset with its
    # version and hash, so one fetch resolves both the file name and the checksum.
    log "resolving latest simbeamd release..."
    SUMS="$(curl -fsSL --retry 3 --connect-timeout 10 --max-time 20 \
        "https://github.com/${REPO}/releases/latest/download/checksums.txt")" \
        || err "can't fetch the release manifest from github.com/${REPO}"
    LINE="$(printf '%s\n' "$SUMS" | grep "simbeamd_.*_darwin_${arch}\.tar\.gz" | head -1)"
    [ -n "$LINE" ] || err "latest release has no simbeamd build for darwin/${arch}"
    SHA="$(printf '%s' "$LINE" | awk '{print $1}')"
    FILE="$(printf '%s' "$LINE" | awk '{print $2}')"
    VERSION="$(printf '%s' "$FILE" | cut -d_ -f2)"

    fetch_verify "https://github.com/${REPO}/releases/download/v${VERSION}/${FILE}" "$FILE" "$SHA"
    tar -xzf "${TMP}/${FILE}" -C "$TMP" simbeamd
    log "simbeamd v${VERSION} verified"

    # simbeam-control: separate repo; latest tag comes from the /releases/latest
    # redirect, the hash from the .sha256 file published next to the archive.
    log "resolving latest simbeam-control release..."
    CTAG="$(curl -fsSL --retry 3 --connect-timeout 10 --max-time 20 -o /dev/null -w '%{url_effective}' \
        "https://github.com/${CONTROL_REPO}/releases/latest")"
    CTAG="${CTAG##*/}"
    case "$CTAG" in v*) ;; *) err "can't resolve the latest simbeam-control release" ;; esac
    CVER="${CTAG#v}"
    CFILE="simbeam-control_${CVER}_darwin_universal.tar.gz"
    CURL_BASE="https://github.com/${CONTROL_REPO}/releases/download/${CTAG}"
    CSHA="$(curl -fsSL --retry 3 --connect-timeout 10 --max-time 20 "${CURL_BASE}/${CFILE}.sha256" | awk '{print $1}')"

    fetch_verify "${CURL_BASE}/${CFILE}" "$CFILE" "$CSHA"
    # the control archive nests the binary under bin/
    tar -xzf "${TMP}/${CFILE}" -C "$TMP" bin/simbeam-control
    mv "${TMP}/bin/simbeam-control" "${TMP}/simbeam-control"
    log "simbeam-control v${CVER} verified"

    mkdir -p "$INSTALL_DIR"
    mv "${TMP}/simbeamd" "${TMP}/simbeam-control" "$INSTALL_DIR/"
    chmod +x "${INSTALL_DIR}/simbeamd" "${INSTALL_DIR}/simbeam-control"
    log "installed simbeamd + simbeam-control to ${INSTALL_DIR}"

    HAVE_XCODE=1
    if ! /usr/bin/xcode-select -p 2>/dev/null | grep -q "\.app"; then
        HAVE_XCODE=0
        warn "full Xcode not detected — simbeam-control needs Xcode (not just Command Line Tools)"
    fi

    # A Homebrew copy alongside this one means PATH order silently decides
    # which binary runs (and `brew upgrade` only updates its own).
    for dir in /opt/homebrew/bin /usr/local/bin; do
        if [ "$dir" != "$INSTALL_DIR" ] && [ -x "${dir}/simbeamd" ]; then
            warn "another simbeamd found at ${dir}/simbeamd (Homebrew?) — keep one:"
            echo "      brew uninstall --cask simbeamd && brew uninstall simbeam-control"
            echo "      (or remove ${INSTALL_DIR}/simbeamd and stay on Homebrew)"
        fi
    done

    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            echo ""
            warn "${INSTALL_DIR} is not in your PATH — add to your shell config:"
            echo ""
            echo "    export PATH=\"${INSTALL_DIR}:\$PATH\""
            ;;
    esac

    # Start the daemon as a background service right away (launchd LaunchAgent:
    # survives the terminal and reboots). Skipped without Xcode — serve would
    # just crash-loop until Xcode is installed. `simbeamd update` sets
    # SIMBEAM_NO_SERVICE=1 when no service is installed, so an update never
    # surprise-installs one.
    echo ""
    if [ "${SIMBEAM_NO_SERVICE:-0}" = 1 ]; then
        log "ready (service step skipped)."
    elif [ "$HAVE_XCODE" = 1 ]; then
        if "${INSTALL_DIR}/simbeamd" service install >/dev/null 2>&1; then
            log "background service installed and running"
            log "ready. pair your iPad with: simbeamd pair"
        else
            warn "could not start the background service — run manually: simbeamd service install"
        fi
    else
        warn "install Xcode, then run: simbeamd service install && simbeamd pair"
    fi
    echo ""
}

# fetch_verify <url> <filename> <expected-sha256>: download into $TMP and
# compare the SHA-256; a mismatch or a malformed manifest hash aborts.
fetch_verify() {
    url="$1"; name="$2"; want="$3"
    want="$(printf '%s' "$want" | tr 'A-F' 'a-f')"
    [ "${#want}" -eq 64 ] || err "release manifest has no valid SHA-256 for ${name}"
    curl -fsSL --retry 3 --connect-timeout 10 --max-time 120 "$url" -o "${TMP}/${name}" \
        || err "download failed: ${url}"
    got="$(shasum -a 256 < "${TMP}/${name}" | awk '{print $1}')"
    [ "$got" = "$want" ] || err "checksum mismatch for ${name}"
}

main "$@"
