// Package companion drives the iOS Simulator lifecycle and screenshots for
// simbeam, entirely through Apple's own `xcrun simctl`. Enumeration, boot,
// shutdown, shake and full-resolution screenshots are all native toolchain
// work — nothing here depends on idb_companion (video and input now go through
// simbeam-control; see internal/backend/sim).
package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// DefaultSimctl is the launcher used to reach simctl. `xcrun simctl <args>`
// resolves the active toolchain's simctl, so no absolute path is needed.
const DefaultSimctl = "xcrun"

// Simulator is one iOS Simulator target as reported by `xcrun simctl list`.
type Simulator struct {
	UDID         string `json:"udid"`
	Name         string `json:"name"`
	Model        string `json:"model"`
	OSVersion    string `json:"os_version"`
	State        string `json:"state"` // "Booted" | "Shutdown" | ...
	Architecture string `json:"architecture"`
	Type         string `json:"type"` // "Simulator" | "Device"
}

// Booted reports whether the simulator is currently running.
func (s Simulator) Booted() bool { return strings.EqualFold(s.State, "Booted") }

// Client drives simulator lifecycle and screenshots via simctl.
type Client struct {
	// Simctl is the launcher used to reach simctl. Empty means DefaultSimctl
	// ("xcrun"). Overridable in tests.
	Simctl string
}

// New returns a Client that reaches simctl via xcrun.
func New() *Client { return &Client{Simctl: DefaultSimctl} }

func (c *Client) simctl() string {
	if c.Simctl == "" {
		return DefaultSimctl
	}
	return c.Simctl
}

// CheckToolchain verifies the iOS Simulator toolchain is usable before we lean on
// it: `xcrun` must be able to locate `simctl`, which ships only with a full Xcode
// (the Command Line Tools alone do not include it). On failure it returns an
// actionable error naming the fix, instead of letting a raw `xcrun: error: unable
// to find utility "simctl"` surface mid-operation.
func (c *Client) CheckToolchain(ctx context.Context) error {
	if err := exec.CommandContext(ctx, c.simctl(), "--find", "simctl").Run(); err == nil {
		return nil
	}

	// Enrich the hint when xcode-select is pointed at the Command Line Tools,
	// which is the usual cause on a machine that has never installed full Xcode.
	hint := ""
	if out, err := exec.CommandContext(ctx, "xcode-select", "-p").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); strings.Contains(p, "CommandLineTools") {
			hint = fmt.Sprintf("\n(xcode-select points at %s — the Command Line Tools, which don't include simctl.)", p)
		}
	}
	return fmt.Errorf(`iOS Simulator tools not found: xcrun can't locate simctl.%s

simbeam needs a full Xcode, not just the Command Line Tools. To fix:
  1. Install Xcode (App Store, or https://developer.apple.com/xcode/).
  2. Point the toolchain at it:
       sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
  3. Accept the license once:
       sudo xcodebuild -license accept`, hint)
}

// Screenshot captures a full-resolution PNG of the given simulator via
// `simctl io <udid> screenshot`. simctl refuses to write to stdout (it treats
// "-" as a filename on a read-only volume), so the frame is captured to a temp
// file and read back.
func (c *Client) Screenshot(ctx context.Context, udid string) ([]byte, error) {
	f, err := os.CreateTemp("", "simbeam-shot-*.png")
	if err != nil {
		return nil, fmt.Errorf("screenshot temp file: %w", err)
	}
	path := f.Name()
	_ = f.Close()
	defer os.Remove(path)

	if _, err := c.runSimctl(ctx, "io", udid, "screenshot", "--type=png", path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("screenshot read: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("screenshot %s: empty capture", udid)
	}
	return data, nil
}

// List runs `xcrun simctl list -j devices available` and returns the available
// simulators. simctl reports only simulators (never real devices), so no
// device-type filtering is needed; unavailable runtimes are excluded by the
// `available` argument and double-checked via isAvailable.
func (c *Client) List(ctx context.Context) ([]Simulator, error) {
	out, err := c.runSimctl(ctx, "list", "-j", "devices", "available")
	if err != nil {
		return nil, err
	}
	return parseSimctlDevices(out)
}

// simctlList mirrors the shape of `simctl list -j devices`: devices keyed by
// runtime identifier (e.g. "com.apple.CoreSimulator.SimRuntime.iOS-26-4").
type simctlList struct {
	Devices map[string][]simctlDevice `json:"devices"`
}

type simctlDevice struct {
	UDID                 string `json:"udid"`
	Name                 string `json:"name"`
	State                string `json:"state"`
	IsAvailable          bool   `json:"isAvailable"`
	DeviceTypeIdentifier string `json:"deviceTypeIdentifier"`
}

// parseSimctlDevices maps simctl's runtime-keyed JSON into a flat, deterministically
// ordered []Simulator. Map iteration order in Go is random, so results are sorted
// to keep the device list stable between calls.
func parseSimctlDevices(out []byte) ([]Simulator, error) {
	var parsed simctlList
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parsing simctl device list: %w", err)
	}

	var sims []Simulator
	for runtime, devices := range parsed.Devices {
		osVersion := osVersionFromRuntime(runtime)
		for _, d := range devices {
			if d.UDID == "" || !d.IsAvailable {
				continue
			}
			sims = append(sims, Simulator{
				UDID:      d.UDID,
				Name:      d.Name,
				Model:     modelFromDeviceType(d.DeviceTypeIdentifier),
				OSVersion: osVersion,
				State:     d.State,
				Type:      "Simulator",
			})
		}
	}
	sort.Slice(sims, func(i, j int) bool {
		if sims[i].Name != sims[j].Name {
			return sims[i].Name < sims[j].Name
		}
		if sims[i].OSVersion != sims[j].OSVersion {
			return sims[i].OSVersion < sims[j].OSVersion
		}
		return sims[i].UDID < sims[j].UDID
	})
	return sims, nil
}

// osVersionFromRuntime turns a runtime identifier like
// "com.apple.CoreSimulator.SimRuntime.iOS-26-4" into "26.4". The platform
// prefix is dropped to match the bare version string idb reported.
func osVersionFromRuntime(runtime string) string {
	const prefix = "com.apple.CoreSimulator.SimRuntime."
	id := strings.TrimPrefix(runtime, prefix)
	// id is e.g. "iOS-26-4"; strip the leading platform token, join the rest.
	parts := strings.Split(id, "-")
	if len(parts) <= 1 {
		return id
	}
	return strings.Join(parts[1:], ".")
}

// modelFromDeviceType turns a device-type identifier like
// "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro" into "iPhone 17 Pro".
func modelFromDeviceType(id string) string {
	const prefix = "com.apple.CoreSimulator.SimDeviceType."
	name := strings.TrimPrefix(id, prefix)
	return strings.ReplaceAll(name, "-", " ")
}

// Boot boots the simulator with the given UDID via `xcrun simctl boot <udid>`.
// Booting an already-booted simulator is treated as success (a no-op), matching
// idb's --verify-booted behavior; simctl instead exits non-zero with a
// "current state: Booted" message that we recognize and swallow.
func (c *Client) Boot(ctx context.Context, udid string) error {
	_, stderr, err := c.execSimctl(ctx, "boot", udid)
	if err != nil {
		if strings.Contains(stderr, "current state: Booted") {
			return nil // already booted — no-op
		}
		msg := strings.TrimSpace(stderr)
		if msg != "" {
			return fmt.Errorf("simctl boot %s: %w: %s", udid, err, msg)
		}
		return fmt.Errorf("simctl boot %s: %w", udid, err)
	}
	return nil
}

// Shutdown shuts down the simulator with the given UDID via
// `xcrun simctl shutdown <udid>`. Shutting down an already-shut-down simulator
// is treated as success (a no-op): simctl exits non-zero with a
// "current state: Shutdown" message that we recognize and swallow, mirroring
// Boot's already-booted handling.
func (c *Client) Shutdown(ctx context.Context, udid string) error {
	_, stderr, err := c.execSimctl(ctx, "shutdown", udid)
	if err != nil {
		if strings.Contains(stderr, "current state: Shutdown") {
			return nil // already shut down — no-op
		}
		msg := strings.TrimSpace(stderr)
		if msg != "" {
			return fmt.Errorf("simctl shutdown %s: %w: %s", udid, err, msg)
		}
		return fmt.Errorf("simctl shutdown %s: %w", udid, err)
	}
	return nil
}

// Shake triggers a shake gesture on the booted simulator with the given UDID.
// simctl has no shake subcommand and idb_companion's HID surface only models
// touches/buttons/swipes, so neither of the usual input paths reach it. Instead
// the gesture is a Darwin notification, com.apple.UIKit.SimulatorShake, that
// UIKit inside the simulated process observes — the very signal Simulator.app
// posts for Device ▸ Shake. We post it headlessly with
// `simctl spawn <udid> notifyutil -p com.apple.UIKit.SimulatorShake`, so no
// Simulator.app window (we boot sims headless) and no private CoreSimulator
// framework are needed. A spawn into a non-booted UDID fails, surfacing as err.
func (c *Client) Shake(ctx context.Context, udid string) error {
	_, err := c.runSimctl(ctx, "spawn", udid, "notifyutil", "-p", "com.apple.UIKit.SimulatorShake")
	return err
}

// runSimctl executes `<simctl-launcher> simctl <args>` and returns stdout,
// surfacing stderr only on failure.
func (c *Client) runSimctl(ctx context.Context, args ...string) ([]byte, error) {
	stdout, stderr, err := c.execSimctl(ctx, args...)
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg != "" {
			return nil, fmt.Errorf("simctl %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("simctl %s: %w", strings.Join(args, " "), err)
	}
	return []byte(stdout), nil
}

// execSimctl runs `<simctl-launcher> simctl <args>` and returns stdout, stderr
// and the run error separately, so callers can inspect stderr (e.g. Boot's
// already-booted check) before deciding whether the error is fatal.
func (c *Client) execSimctl(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, c.simctl(), append([]string{"simctl"}, args...)...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}
