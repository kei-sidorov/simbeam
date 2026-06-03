package signal

import "testing"

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("v=0\r\no=- 42 2 IN IP4 127.0.0.1\r\n")
	sig := Sign(priv, msg)
	if sig == "" {
		t.Fatal("empty signature")
	}
	if !Verify(pub, msg, sig) {
		t.Fatal("Verify rejected a valid signature")
	}
}

func TestVerifyRejectsTamper(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sig := Sign(priv, []byte("real answer sdp"))
	if Verify(pub, []byte("forged answer sdp"), sig) {
		t.Fatal("Verify accepted a signature over different bytes")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv, _ := GenerateKeyPair()
	otherPub, _, _ := GenerateKeyPair()
	msg := []byte("answer")
	if Verify(otherPub, msg, Sign(priv, msg)) {
		t.Fatal("Verify accepted a signature under the wrong public key")
	}
}

func TestVerifyRejectsGarbageInput(t *testing.T) {
	pub, _, _ := GenerateKeyPair()
	if Verify(pub, []byte("x"), "not-base64-!!") {
		t.Fatal("Verify accepted non-base64 signature")
	}
	if Verify("not-base64-!!", []byte("x"), "AAAA") {
		t.Fatal("Verify accepted non-base64 pubkey")
	}
}
