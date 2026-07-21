package companion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinary writes an executable shell script under the given name and returns
// its path. Used to stand in for either idb_companion or xcrun.
func fakeBinary(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeSimctl returns a Client whose List/Boot shell out to a fake `xcrun`.
func fakeSimctl(t *testing.T, script string) *Client {
	t.Helper()
	return &Client{Simctl: fakeBinary(t, "xcrun", script)}
}

func TestCheckToolchainOK(t *testing.T) {
	// `xcrun --find simctl` succeeds → no error.
	c := fakeSimctl(t, "exit 0\n")
	if err := c.CheckToolchain(context.Background()); err != nil {
		t.Fatalf("CheckToolchain = %v, want nil", err)
	}
}

func TestCheckToolchainMissingSimctl(t *testing.T) {
	// Fake xcrun that fails to find simctl, like a Command-Line-Tools-only Mac.
	c := fakeSimctl(t, `echo 'xcrun: error: unable to find utility "simctl"' >&2
exit 72
`)
	err := c.CheckToolchain(context.Background())
	if err == nil {
		t.Fatal("CheckToolchain = nil, want an actionable error")
	}
	for _, want := range []string{"full Xcode", "xcode-select", "simctl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q; got:\n%s", want, err)
		}
	}
}

func TestBootSuccess(t *testing.T) {
	c := fakeSimctl(t, "exit 0\n")
	if err := c.Boot(context.Background(), "UDID-123"); err != nil {
		t.Fatalf("Boot returned error: %v", err)
	}
}

// Booting an already-booted simulator must be a no-op, mirroring idb's
// --verify-booted behavior. simctl exits non-zero with a specific message.
func TestBootAlreadyBootedIsNoOp(t *testing.T) {
	c := fakeSimctl(t, "echo 'Unable to boot device in current state: Booted' 1>&2\nexit 149\n")
	if err := c.Boot(context.Background(), "UDID-123"); err != nil {
		t.Fatalf("booting an already-booted device should succeed, got: %v", err)
	}
}

func TestBootFailureIncludesStderr(t *testing.T) {
	c := fakeSimctl(t, "echo 'Invalid device or device pair: BAD' 1>&2\nexit 148\n")
	err := c.Boot(context.Background(), "BAD")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid device") {
		t.Fatalf("error %q does not include stderr", err)
	}
}

func TestShutdownSuccess(t *testing.T) {
	c := fakeSimctl(t, "exit 0\n")
	if err := c.Shutdown(context.Background(), "UDID-123"); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

// Shutting down an already-shut-down simulator must be a no-op: simctl exits
// non-zero with a specific message we recognize and swallow.
func TestShutdownAlreadyShutdownIsNoOp(t *testing.T) {
	c := fakeSimctl(t, "echo 'Unable to shutdown device in current state: Shutdown' 1>&2\nexit 164\n")
	if err := c.Shutdown(context.Background(), "UDID-123"); err != nil {
		t.Fatalf("shutting down an already-shut-down device should succeed, got: %v", err)
	}
}

func TestShutdownFailureIncludesStderr(t *testing.T) {
	c := fakeSimctl(t, "echo 'Invalid device or device pair: BAD' 1>&2\nexit 148\n")
	err := c.Shutdown(context.Background(), "BAD")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid device") {
		t.Fatalf("error %q does not include stderr", err)
	}
}

// Shake has no simctl subcommand of its own; it must spawn notifyutil to post
// the com.apple.UIKit.SimulatorShake Darwin notification into the sim. Lock the
// exact command so the notification name (the crux of the gesture) can't drift.
func TestShakePostsSimulatorShakeNotification(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	c := fakeSimctl(t, "echo \"$@\" > "+argsFile+"\nexit 0\n")
	if err := c.Shake(context.Background(), "UDID-123"); err != nil {
		t.Fatalf("Shake returned error: %v", err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "simctl spawn UDID-123 notifyutil -p com.apple.UIKit.SimulatorShake"
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("simctl args = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

func TestShakeFailureIncludesStderr(t *testing.T) {
	c := fakeSimctl(t, "echo 'Unable to spawn: device not booted' 1>&2\nexit 1\n")
	err := c.Shake(context.Background(), "BAD")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "device not booted") {
		t.Fatalf("error %q does not include stderr", err)
	}
}

func TestListParsesAndFilters(t *testing.T) {
	c := fakeSimctl(t, "cat <<'EOF'\n"+sampleDevicesJSON+"\nEOF\n")
	sims, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sims) != 2 {
		t.Fatalf("want 2 available simulators, got %d: %+v", len(sims), sims)
	}
	// Deterministic order regardless of simctl's runtime-keyed map.
	if sims[0].Name != "iPhone 14 Pro" || sims[1].Name != "iPhone 17 Pro" {
		t.Fatalf("unexpected order: %q, %q", sims[0].Name, sims[1].Name)
	}
}

func TestParseSimctlDevices(t *testing.T) {
	sims, err := parseSimctlDevices([]byte(sampleDevicesJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sims) != 2 {
		t.Fatalf("want 2 sims (unavailable filtered out), got %d", len(sims))
	}

	got := sims[1] // iPhone 17 Pro
	if got.UDID != "6B0C54AC-4629-42FA-B9DA-ABBC39EF2027" {
		t.Errorf("UDID = %q", got.UDID)
	}
	if got.Name != "iPhone 17 Pro" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.State != "Shutdown" {
		t.Errorf("State = %q", got.State)
	}
	if got.OSVersion != "26.4" {
		t.Errorf("OSVersion = %q, want 26.4", got.OSVersion)
	}
	if got.Model != "iPhone 17 Pro" {
		t.Errorf("Model = %q, want iPhone 17 Pro", got.Model)
	}
	if got.Type != "Simulator" {
		t.Errorf("Type = %q, want Simulator", got.Type)
	}
}

func TestSimulatorBootedFromSimctlState(t *testing.T) {
	sims, err := parseSimctlDevices([]byte(sampleDevicesJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !sims[0].Booted() { // iPhone 14 Pro is "Booted" in the fixture
		t.Errorf("expected iPhone 14 Pro to report Booted")
	}
}

// sampleDevicesJSON mimics `xcrun simctl list -j devices`: keyed by runtime,
// with one unavailable device that must be filtered out.
const sampleDevicesJSON = `{
  "devices" : {
    "com.apple.CoreSimulator.SimRuntime.iOS-26-4" : [
      {
        "udid" : "6B0C54AC-4629-42FA-B9DA-ABBC39EF2027",
        "isAvailable" : true,
        "deviceTypeIdentifier" : "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro",
        "state" : "Shutdown",
        "name" : "iPhone 17 Pro"
      }
    ],
    "com.apple.CoreSimulator.SimRuntime.iOS-16-0" : [
      {
        "udid" : "28BFA6F0-9EE5-49F2-AA10-1D9907098232",
        "isAvailable" : true,
        "deviceTypeIdentifier" : "com.apple.CoreSimulator.SimDeviceType.iPhone-14-Pro",
        "state" : "Booted",
        "name" : "iPhone 14 Pro"
      },
      {
        "udid" : "DEAD0000-0000-0000-0000-000000000000",
        "isAvailable" : false,
        "deviceTypeIdentifier" : "com.apple.CoreSimulator.SimDeviceType.iPhone-13",
        "state" : "Shutdown",
        "name" : "iPhone 13 (unavailable runtime)"
      }
    ]
  }
}`
