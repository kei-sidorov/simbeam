package signal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// CanonicalSubscription is the fixed-separator byte string both subscription
// signatures cover. Field order and the 0x1f unit separator are a frozen wire
// contract (the browser builds the identical bytes). It is fed to the weak
// app-secret HMAC AND the real Ed25519 account signature.
//
// NOTE: parameter order is clientPubKey, productID, expiresAt, issuedAt — that
// is, expiresAt comes BEFORE issuedAt. This order is part of the frozen wire
// contract and must not be swapped. Both fields are ISO8601 strings so the
// compiler cannot catch a transposition.
func CanonicalSubscription(clientPubKey, productID, expiresAt, issuedAt string) []byte {
	const sep = "\x1f"
	return []byte(clientPubKey + sep + productID + sep + expiresAt + sep + issuedAt)
}

// AppSig = base64(HMAC-SHA256(appSecret, canonical)). HONESTLY this is
// obfuscation, not a crypto boundary — appSecret is extractable by reversing the
// client binary. The real "is this the account" auth is the Ed25519 account
// signature (signal.Verify). The strong "did they pay" boundary is a future
// Apple-receipt check that drops into the same endpoint by flipping source.
func AppSig(appSecret string, canonical []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(canonical)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyAppSig recomputes AppSig and compares in constant time.
func VerifyAppSig(appSecret string, canonical []byte, sigB64 string) bool {
	want := AppSig(appSecret, canonical)
	return hmac.Equal([]byte(want), []byte(sigB64))
}
