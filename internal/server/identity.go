package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"

	"github.com/kei-sidorov/simcast/internal/signal"
)

// Identity is the daemon's long-lived Ed25519 keypair. PubB64 (base64 std) is
// the daemonID: simultaneously the stable address on the broker and the crypto
// credential a paired client pins (anti-MITM).
type Identity struct {
	PubB64 string
	Priv   ed25519.PrivateKey
}

// LoadOrCreateIdentity loads the daemon key from path, or generates and persists
// a fresh one (0600) on first run. The file stores the 64-byte Ed25519 private
// key as base64 (std); the public key is derived from it.
func LoadOrCreateIdentity(path string) (Identity, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		raw, derr := base64.StdEncoding.DecodeString(string(data))
		if derr != nil || len(raw) != ed25519.PrivateKeySize {
			return Identity{}, errors.New("identity: malformed key file")
		}
		priv := ed25519.PrivateKey(raw)
		pub := priv.Public().(ed25519.PublicKey)
		return Identity{PubB64: base64.StdEncoding.EncodeToString(pub), Priv: priv}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	pubB64, priv, gerr := signal.GenerateKeyPair()
	if gerr != nil {
		return Identity{}, gerr
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return Identity{}, mkErr
	}
	enc := base64.StdEncoding.EncodeToString(priv)
	if werr := os.WriteFile(path, []byte(enc), 0o600); werr != nil {
		return Identity{}, werr
	}
	return Identity{PubB64: pubB64, Priv: priv}, nil
}
