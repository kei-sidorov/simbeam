package companion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinary writes an executable shell script that mimics idb_companion.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "idb_companion")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBootSuccess(t *testing.T) {
	c := &Client{Binary: fakeBinary(t, "exit 0\n")}
	if err := c.Boot(context.Background(), "UDID-123"); err != nil {
		t.Fatalf("Boot returned error: %v", err)
	}
}

func TestBootFailureIncludesStderr(t *testing.T) {
	c := &Client{Binary: fakeBinary(t, "echo 'boot failed: no such device' 1>&2\nexit 1\n")}
	err := c.Boot(context.Background(), "BAD")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "boot failed") {
		t.Fatalf("error %q does not include stderr", err)
	}
}
