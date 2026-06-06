package signal

import "testing"

func TestEnrollProof_RoundTrip(t *testing.T) {
	const secret = "s3cr3t"
	const pub = "CLIENTPUBKEYBASE64=="
	nonce, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	proof := EnrollProof(secret, pub, nonce)
	if !VerifyEnrollProof(secret, pub, nonce, proof) {
		t.Fatalf("valid proof rejected")
	}
	// Wrong secret, pubkey, or nonce must all fail.
	if VerifyEnrollProof("wrong", pub, nonce, proof) {
		t.Fatalf("accepted wrong secret")
	}
	if VerifyEnrollProof(secret, "other", nonce, proof) {
		t.Fatalf("accepted wrong pubkey")
	}
	other, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	if VerifyEnrollProof(secret, pub, other, proof) {
		t.Fatalf("accepted wrong nonce")
	}
}

// TestEnrollProof_GoldenVector locks the exact proof encoding (base64 StdEncoding
// of HMAC-SHA256 over clientPubKey‖0x00‖nonce). This is the authoritative
// cross-language reference: the browser's WebCrypto implementation must produce
// this same string for these inputs, and this test catches any base64-variant or
// byte-layout regression that a round-trip test would miss.
func TestEnrollProof_GoldenVector(t *testing.T) {
	const secret = "test-secret"
	const pub = "CLIENTPUBKEYBASE64=="
	const nonce = "fixed-nonce-value"
	const want = "/8tpxwZ71oc1JM64FeJbugbuY3H/81o9+FMPTLv97A4="
	if got := EnrollProof(secret, pub, nonce); got != want {
		t.Fatalf("EnrollProof = %q, want %q (wire-contract regression?)", got, want)
	}
}

func TestNoncesAndSecretsAreRandom(t *testing.T) {
	n1, _ := NewNonce()
	n2, _ := NewNonce()
	if n1 == n2 || n1 == "" {
		t.Fatalf("nonces not random/unique: %q %q", n1, n2)
	}
	s1, _ := NewPairingSecret()
	s2, _ := NewPairingSecret()
	if s1 == s2 || s1 == "" {
		t.Fatalf("secrets not random/unique: %q %q", s1, s2)
	}
}
