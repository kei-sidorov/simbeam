package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// PinnedClient is an enrolled client allowed to reconnect. Name is optional UI sugar.
type PinnedClient struct {
	PubKey string `json:"pubkey"`
	Name   string `json:"name,omitempty"`
}

// PinnedStore is the daemon's set of enrolled client public keys, persisted to a
// JSON file (0600). Safe for concurrent use. Revocation = Remove (local, no server).
type PinnedStore struct {
	path string
	mu   sync.Mutex
	set  map[string]PinnedClient
}

// LoadPinnedStore reads the set from path; a missing file yields an empty store.
func LoadPinnedStore(path string) (*PinnedStore, error) {
	ps := &PinnedStore{path: path, set: map[string]PinnedClient{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ps, nil
		}
		return nil, err
	}
	// An empty file (fresh touch, or an interrupted save) means "no pins" — treat
	// it as an empty store rather than a JSON parse error that bricks startup.
	if len(bytes.TrimSpace(data)) == 0 {
		return ps, nil
	}
	var list []PinnedClient
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, c := range list {
		ps.set[c.PubKey] = c
	}
	return ps, nil
}

// Contains reports whether pubKey is enrolled.
func (ps *PinnedStore) Contains(pubKey string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	_, ok := ps.set[pubKey]
	return ok
}

// Add enrolls pubKey (idempotent) and persists.
func (ps *PinnedStore) Add(pubKey, name string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.set[pubKey] = PinnedClient{PubKey: pubKey, Name: name}
	return ps.save()
}

// Remove revokes pubKey and persists.
func (ps *PinnedStore) Remove(pubKey string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.set, pubKey)
	return ps.save()
}

// save writes the set atomically: a temp file in the same dir is fsync'd-by-OS
// on rename, so a crash mid-write can never leave a truncated/empty clients.json
// (which would otherwise lose every pin). Caller holds mu.
func (ps *PinnedStore) save() error {
	list := make([]PinnedClient, 0, len(ps.set))
	for _, c := range ps.set {
		list = append(list, c)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(ps.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".clients-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, ps.path)
}
