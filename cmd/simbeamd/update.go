package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// installScriptURL is the raw install.sh on main — the same file
// https://simbeam.dev/install.sh redirects to. `update` re-runs it instead of
// reimplementing download/verify/replace: one installer, two entry points.
const installScriptURL = "https://raw.githubusercontent.com/kei-sidorov/simbeam/main/install.sh"

// isBrewPath reports whether a resolved binary path was laid down by Homebrew.
// Callers must EvalSymlinks first: brew exposes binaries via symlinks in
// /opt/homebrew/bin (or /usr/local/bin) that resolve into Caskroom/Cellar.
func isBrewPath(p string) bool {
	return strings.Contains(p, "/Caskroom/") || strings.Contains(p, "/Cellar/")
}

// updateHint returns the channel-appropriate command for getting the newer
// release, so the daily update check never tells a curl-installed user to brew.
func updateHint() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil && isBrewPath(resolved) {
			return "brew upgrade --cask simbeamd && brew upgrade simbeam-control"
		}
	}
	return "simbeamd update"
}

// runUpdate updates a curl-installed daemon in place by re-running install.sh
// pointed at this binary's own directory; the script re-verifies checksums and
// restarts the service (skipped when no service is installed). Brew installs
// are brew's job — deferred to it.
func runUpdate(argv []string) error {
	if len(argv) > 0 {
		return fmt.Errorf("update takes no arguments")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	if isBrewPath(exe) {
		fmt.Println("installed via Homebrew — update with:")
		fmt.Println("  brew upgrade --cask simbeamd && brew upgrade simbeam-control")
		return nil
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("update is for the macOS install; Linux boxes update via their deploy timer")
	}

	// SIMBEAM_NO_PAIR: an update must never pop a pairing window.
	env := append(os.Environ(), "SIMBEAM_INSTALL_DIR="+filepath.Dir(exe), "SIMBEAM_NO_PAIR=1")
	if plist, _, perr := servicePaths(); perr == nil {
		if _, serr := os.Stat(plist); os.IsNotExist(serr) {
			// No service on this machine — updating must not surprise-install one.
			env = append(env, "SIMBEAM_NO_SERVICE=1")
		}
	}
	cmd := exec.Command("/bin/sh", "-c", "curl -fsSL "+installScriptURL+" | sh")
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
