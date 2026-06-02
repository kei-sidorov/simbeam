// Package companion is a thin wrapper around the idb_companion CLI binary
// (Meta's idb, MIT). simcast does not reimplement CoreSimulator or the video
// pipeline — it shells out to idb_companion and parses its output.
//
// On this Phase, only the lifecycle CLI surface is used (--version, --list).
// The gRPC surface (describe / video_stream / hid) is wired up in Phase 1.
package companion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// DefaultBinary is the idb_companion executable name resolved against PATH.
const DefaultBinary = "idb_companion"

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

// Client wraps the idb_companion binary.
type Client struct {
	// Binary is the path or name of the idb_companion executable. Empty means
	// DefaultBinary resolved against PATH.
	Binary string
}

// New returns a Client that resolves idb_companion from PATH.
func New() *Client { return &Client{Binary: DefaultBinary} }

func (c *Client) binary() string {
	if c.Binary == "" {
		return DefaultBinary
	}
	return c.Binary
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

// List runs `idb_companion --list 1` and returns the available simulators.
//
// idb_companion interleaves JSON device lines with human-readable diagnostic
// lines on stdout, so we parse line-by-line and keep only lines that unmarshal
// into a Simulator with a UDID. Real (non-simulator) devices are filtered out —
// simcast is scoped to simulators only.
func (c *Client) List(ctx context.Context) ([]Simulator, error) {
	out, err := c.run(ctx, "--list", "1")
	if err != nil {
		return nil, err
	}

	var sims []Simulator
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s Simulator
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue // diagnostic or otherwise non-device JSON line
		}
		if s.UDID == "" {
			continue
		}
		if s.Type != "" && !strings.EqualFold(s.Type, "Simulator") {
			continue // scope: simulators only
		}
		sims = append(sims, s)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading idb_companion output: %w", err)
	}
	return sims, nil
}

// Boot boots the simulator with the given UDID via `idb_companion --boot <udid>`.
// idb_companion blocks until the simulator reaches a known-booted state
// (--verify-booted defaults to true). Booting an already-booted simulator is
// effectively a no-op.
func (c *Client) Boot(ctx context.Context, udid string) error {
	if _, err := c.run(ctx, "--boot", udid); err != nil {
		return err
	}
	return nil
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
