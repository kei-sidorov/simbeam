package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedStore_AddContainsRemovePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "clients.json")
	ps, err := LoadPinnedStore(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if ps.Contains("k1") {
		t.Fatalf("empty store should not contain k1")
	}
	if err := ps.Add("k1", "iPad"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !ps.Contains("k1") {
		t.Fatalf("k1 missing after add")
	}

	// Persisted with 0600 and reloads.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
	reloaded, err := LoadPinnedStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Contains("k1") {
		t.Fatalf("k1 missing after reload")
	}

	// Revocation removes and persists.
	if err := reloaded.Remove("k1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	again, err := LoadPinnedStore(path)
	if err != nil {
		t.Fatalf("reload after remove: %v", err)
	}
	if again.Contains("k1") {
		t.Fatalf("k1 present after remove+reload")
	}
}
