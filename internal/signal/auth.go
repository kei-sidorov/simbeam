package signal

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
)

// GenerateKeyPair returns a fresh Ed25519 keypair. The public key is base64
// (StdEncoding) for the wire/QR/URL; the private key stays in the daemon.
func GenerateKeyPair() (pubB64 string, priv ed25519.PrivateKey, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	return base64.StdEncoding.EncodeToString(pub), priv, nil
}

// Sign returns a base64 detached Ed25519 signature of msg.
func Sign(priv ed25519.PrivateKey, msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// Verify reports whether sigB64 is a valid Ed25519 signature of msg under the
// base64 public key pubB64. Any decoding/size error returns false (never
// panics) — a malformed signature is just an invalid one.
func Verify(pubB64 string, msg []byte, sigB64 string) bool {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}
