package signal

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestMakeTURNCredentialFormat(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := MakeTURNCredential("s3cr3t", "room-abc", now, 60*time.Second)

	// username = "<expiry>:<userID>", expiry = now+ttl in unix seconds.
	wantUser := "1000060:room-abc"
	if c.Username != wantUser {
		t.Fatalf("username = %q, want %q", c.Username, wantUser)
	}

	// credential = base64(HMAC-SHA1(secret, username)) — recompute independently.
	mac := hmac.New(sha1.New, []byte("s3cr3t"))
	mac.Write([]byte(wantUser))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if c.Credential != want {
		t.Fatalf("credential = %q, want %q", c.Credential, want)
	}
}

func TestMakeTURNCredentialDeterministic(t *testing.T) {
	now := time.Unix(42, 0)
	a := MakeTURNCredential("k", "u", now, time.Minute)
	b := MakeTURNCredential("k", "u", now, time.Minute)
	if a != b {
		t.Fatalf("same inputs gave different creds: %+v vs %+v", a, b)
	}
	if strings.Contains(a.Credential, "=") && !strings.HasSuffix(a.Credential, "=") {
		t.Fatalf("credential not valid base64 padding: %q", a.Credential)
	}
}
