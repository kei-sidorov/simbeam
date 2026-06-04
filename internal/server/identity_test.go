package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateIdentity_CreatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "identity.key")
	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id.PubB64 == "" || id.Priv == nil {
		t.Fatalf("empty identity: %+v", id)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
	id2, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id2.PubB64 != id.PubB64 {
		t.Fatalf("pubkey changed across loads: %q != %q", id2.PubB64, id.PubB64)
	}
}

func TestLoadOrCreateIdentity_RejectsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(path, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(path); err == nil {
		t.Fatalf("want error on malformed key file, got nil")
	}
}
