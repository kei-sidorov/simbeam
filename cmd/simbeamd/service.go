package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// serviceLabel is the launchd job label; the plist file is named after it.
const serviceLabel = "dev.simbeam.simbeamd"

// runService manages simbeamd as a launchd LaunchAgent so `serve` survives the
// terminal and reboots. A LaunchAgent (not LaunchDaemon) on purpose: simulators
// live in the user's GUI session and are invisible to system daemons.
func runService(argv []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service is macOS-only (launchd); on Linux use a systemd unit (see deploy/)")
	}
	if len(argv) == 0 {
		return fmt.Errorf("usage: simbeamd service install|uninstall|start|stop|status")
	}
	switch argv[0] {
	case "install":
		return serviceInstall()
	case "uninstall":
		return serviceUninstall()
	case "start":
		return serviceStart()
	case "stop":
		return serviceStop()
	case "status":
		return serviceStatus()
	default:
		return fmt.Errorf("unknown service command %q (want install|uninstall|start|stop|status)", argv[0])
	}
}

func servicePaths() (plist, log string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"),
		filepath.Join(home, "Library", "Logs", "simbeamd.log"), nil
}

// servicePlist renders the LaunchAgent. PATH must carry the daemon's own
// directory (install.sh puts simbeam-control next to simbeamd) plus the
// Homebrew and system paths — launchd agents get a bare PATH otherwise and
// LookPath would miss simbeam-control and xcrun.
func servicePlist(exe, logPath string) string {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	path := strings.Join([]string{filepath.Dir(exe), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}, ":")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + serviceLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(exe) + `</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>` + esc(path) + `</string>
	</dict>
	<key>StandardOutPath</key><string>` + esc(logPath) + `</string>
	<key>StandardErrorPath</key><string>` + esc(logPath) + `</string>
</dict>
</plist>
`
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func serviceTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel) }

func serviceInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	plist, logPath, err := servicePaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(servicePlist(exe, logPath)), 0o644); err != nil {
		return err
	}
	// Re-installing over a running agent: bootout first, ignore "not loaded".
	_ = launchctl("bootout", serviceTarget())
	if err := launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist); err != nil {
		return err
	}
	fmt.Printf("service installed and started (%s)\nlogs: %s\n", serviceLabel, logPath)
	fmt.Println("pair a device any time with: simbeamd pair")
	return nil
}

// serviceStart re-bootstraps an installed agent; serviceStop boots it out but
// keeps the plist, so launchd will pick it up again at next login (RunAtLoad)
// or via `service start` — the pair covers "pause to pair a new device" without
// the uninstall/install dance.
func serviceStart() error {
	plist, _, err := servicePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (run: simbeamd service install)")
	}
	if err := launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist); err != nil {
		return err
	}
	fmt.Println("service started")
	return nil
}

func serviceStop() error {
	plist, _, err := servicePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (run: simbeamd service install)")
	}
	if err := launchctl("bootout", serviceTarget()); err != nil {
		return err
	}
	fmt.Println("service stopped (starts again at next login or 'simbeamd service start')")
	return nil
}

func serviceUninstall() error {
	plist, _, err := servicePaths()
	if err != nil {
		return err
	}
	_ = launchctl("bootout", serviceTarget()) // ignore "not loaded"
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("service stopped and removed")
	return nil
}

func serviceStatus() error {
	plist, logPath, err := servicePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		fmt.Println("service not installed")
		return nil
	}
	out, err := exec.Command("launchctl", "print", serviceTarget()).Output()
	if err != nil {
		fmt.Println("service installed but not loaded (try: simbeamd service install)")
		return nil
	}
	state := "unknown"
	for _, line := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "state = "); ok {
			state = s
			break
		}
	}
	fmt.Printf("service %s: %s\nlogs: %s\n", serviceLabel, state, logPath)
	return nil
}
