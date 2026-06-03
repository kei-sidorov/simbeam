package signal

import (
	"crypto/rand"
	"encoding/base64"
	"net/url"
)

// NewToken returns a one-time pairing token: 16 random bytes, URL-safe base64
// (no padding). The broker treats it as an opaque room key.
func NewToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// PairingURL builds the browser link the daemon prints/renders. The signaling
// coordinates (signalingURL, token, daemonPubKey) go in the URL *fragment* so
// they never reach the client web server's request line or logs:
//
//	<clientBase>#signal=<wss-url>&token=<token>&pubkey=<base64>
//
// clientBase is where the debug client is served (e.g. http://localhost:8080/).
func PairingURL(clientBase, signalingURL, token, pubKeyB64 string) string {
	frag := url.Values{}
	frag.Set("signal", signalingURL)
	frag.Set("token", token)
	frag.Set("pubkey", pubKeyB64)
	return clientBase + "#" + frag.Encode()
}
