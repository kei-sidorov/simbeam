package signal

import "testing"

func TestCanonicalSubscription_StableAndOrdered(t *testing.T) {
	c := CanonicalSubscription("pub", "pro.monthly", "2026-12-31T00:00:00Z", "2026-06-04T00:00:00Z")
	want := "pub\x1fpro.monthly\x1f2026-12-31T00:00:00Z\x1f2026-06-04T00:00:00Z"
	if string(c) != want {
		t.Fatalf("canonical = %q, want %q", c, want)
	}
}

// TestAppSig_GoldenVector locks the exact AppSig encoding (base64 StdEncoding of
// HMAC-SHA256(appSecret, canonical)). Authoritative cross-language reference: the
// browser's WebCrypto implementation must produce this same string for these
// inputs. Catches any base64-variant or HMAC-keying regression a round-trip misses.
func TestAppSig_GoldenVector(t *testing.T) {
	const secret = "test-app-secret"
	canon := CanonicalSubscription(
		"CLIENTPUBKEYBASE64==", "pro.monthly",
		"2026-12-31T00:00:00Z", "2026-06-04T00:00:00Z",
	)
	const want = "l3CU5AiPbIwPLoeyTl/zbaixrn9IRnvnniSYYf1iF28="
	if got := AppSig(secret, canon); got != want {
		t.Fatalf("AppSig = %q, want %q (wire-contract regression?)", got, want)
	}
}

func TestAppSig_RoundTrip(t *testing.T) {
	const secret = "dev-app-secret"
	canon := CanonicalSubscription("pub", "p", "e", "i")
	sig := AppSig(secret, canon)
	if !VerifyAppSig(secret, canon, sig) {
		t.Fatalf("valid app-sig rejected")
	}
	if VerifyAppSig("other", canon, sig) {
		t.Fatalf("accepted wrong secret")
	}
	if VerifyAppSig(secret, CanonicalSubscription("pub", "p", "e", "DIFFERENT"), sig) {
		t.Fatalf("accepted tampered canonical")
	}
}
