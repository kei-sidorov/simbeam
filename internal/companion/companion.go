// Package companion drives the iOS Simulator lifecycle for simbeam.
//
// Simulator enumeration and boot use Apple's own `xcrun simctl` — there is no
// reason to route those through a third-party binary. idb_companion (Meta's idb,
// MIT) is still required for the streaming pipeline (the gRPC describe /
// screenshot / hid surface, wired up in internal/idb); Resolve/Version exist to
// confirm it is installed.
package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// DefaultBinary is the idb_companion executable name resolved against PATH.
const DefaultBinary = "idb_companion"

// DefaultSimctl is the launcher used to reach simctl. `xcrun simctl <args>`
// resolves the active toolchain's simctl, so no absolute path is needed.
const DefaultSimctl = "xcrun"

// Simulator is one iOS Simulator target as reported by `idb_companion --list 1`.
// Fields mirror the JSON keys emitted by idb_companion v1.1.8.
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

// Version is the build information reported by `idb_companion --version`.
type Version struct {
	BuildTime string `json:"build_time"`
	BuildDate string `json:"build_date"`
}

// String renders the version as "Aug 12 2022 08:41:50".
func (v Version) String() string {
	return strings.TrimSpace(v.BuildDate + " " + v.BuildTime)
}

// Client drives simulator lifecycle (via simctl) and confirms idb_companion
// is present (for the streaming pipeline).
type Client struct {
	// Binary is the path or name of the idb_companion executable. Empty means
	// DefaultBinary resolved against PATH.
	Binary string
	// Simctl is the launcher used for List/Boot. Empty means DefaultSimctl
	// ("xcrun"). Overridable in tests.
	Simctl string
}

// New returns a Client that resolves idb_companion from PATH and reaches
// simctl via xcrun.
func New() *Client { return &Client{Binary: DefaultBinary, Simctl: DefaultSimctl} }

func (c *Client) binary() string {
	if c.Binary == "" {
		return DefaultBinary
	}
	return c.Binary
}

func (c *Client) simctl() string {
	if c.Simctl == "" {
		return DefaultSimctl
	}
	return c.Simctl
}

// Resolve returns the absolute path to the idb_companion binary, or an error
// if it cannot be found.
func (c *Client) Resolve() (string, error) {
	path, err := exec.LookPath(c.binary())
	if err != nil {
		return "", fmt.Errorf("idb_companion not found (install with `brew install idb-companion`): %w", err)
	}
	return path, nil
}

// Version runs `idb_companion --version` and parses its JSON output.
func (c *Client) Version(ctx context.Context) (Version, error) {
	out, err := c.run(ctx, "--version")
	if err != nil {
		return Version{}, err
	}
	// idb_companion may print an objc class-collision warning before the JSON,
	// so scan for the first line that parses as a Version object.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var v Version
		if err := json.Unmarshal([]byte(line), &v); err == nil && (v.BuildDate != "" || v.BuildTime != "") {
			return v, nil
		}
	}
	return Version{}, fmt.Errorf("could not parse version from idb_companion output")
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

// run executes idb_companion with the given args and returns its stdout. The
// objc warning and CoreSimulator diagnostics are emitted on stderr, which is
// only surfaced when the command fails.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("idb_companion %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("idb_companion %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
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
