package signal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// NewNonce returns 16 random bytes, base64 StdEncoding. Used for the mutual
// challenge-response and to bind an enrollment proof to a single attempt.
// StdEncoding is used because the nonce travels in JSON (not a URL).
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// NewPairingSecret returns a short one-time enrollment secret S: 9 random bytes,
// base64 RawURLEncoding (no padding) → 12 chars; RawURLEncoding is used because
// S travels in a URL fragment, where + and / are problematic.
func NewPairingSecret() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// EnrollProof = base64std(HMAC-SHA256(S, clientPubKey ‖ 0x00 ‖ nonce)). The client
// proves knowledge of the one-time secret S to the daemon WITHOUT revealing S to
// the untrusted broker; the nonce binds the proof to one attempt. The 0x00
// separator is part of the frozen wire contract (browser must match exactly).
func EnrollProof(secret, clientPubKey, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(clientPubKey))
	mac.Write([]byte{0})
	mac.Write([]byte(nonce))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyEnrollProof recomputes EnrollProof and compares in constant time.
func VerifyEnrollProof(secret, clientPubKey, nonce, proofB64 string) bool {
	want := EnrollProof(secret, clientPubKey, nonce)
	return hmac.Equal([]byte(want), []byte(proofB64))
}
