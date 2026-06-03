package signal

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"time"
)

// TURNCredential is an ephemeral coturn long-term credential, derived per the
// TURN REST API mechanism. coturn validates it by recomputing the same HMAC
// with its shared static-auth-secret, so no per-credential state is stored.
type TURNCredential struct {
	Username   string
	Credential string
}

// MakeTURNCredential derives a credential valid for ttl after now:
//
//	username   = "<unixExpiry>:<userID>"
//	credential = base64( HMAC-SHA1( secret, username ) )
//
// now is a parameter (not time.Now) so callers can test deterministically and
// the broker can inject a clock.
func MakeTURNCredential(secret, userID string, now time.Time, ttl time.Duration) TURNCredential {
	expiry := now.Add(ttl).Unix()
	username := strconv.FormatInt(expiry, 10) + ":" + userID
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return TURNCredential{
		Username:   username,
		Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}
}
