package signal

import (
	"net/url"
)

// PairingURL builds the browser link the daemon prints when its enrollment
// window is open. The coordinates go in the URL *fragment* so they never reach
// the client web server's request line or logs:
//
//	<clientBase>#signal=<wss-url>&daemon=<daemonPubKey>&pair=<S>
//
// daemonPubKey (== daemonID) lets the client pin the Mac (anti-MITM); S is the
// one-time enrollment secret proving the client is authorized to be pinned.
//
// signalingURL may be empty: the hosted web client knows its own default broker,
// so a distributed daemon that talks to that same broker omits the parameter to
// keep the URL (and its QR) short. An empty signalingURL drops "signal" from the
// fragment entirely; the client falls back to its baked default.
func PairingURL(clientBase, signalingURL, daemonPubKey, secret string) string {
	frag := url.Values{}
	if signalingURL != "" {
		frag.Set("signal", signalingURL)
	}
	frag.Set("daemon", daemonPubKey)
	frag.Set("pair", secret)
	return clientBase + "#" + frag.Encode()
}
