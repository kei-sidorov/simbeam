package signal

import (
	"net/url"
	"strings"
	"testing"
)

func TestNewTokenUnique(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if a == "" || a == b {
		t.Fatalf("tokens not unique/non-empty: %q %q", a, b)
	}
}

func TestPairingURLCarriesCoordinatesInFragment(t *testing.T) {
	got := PairingURL("http://localhost:8080/", "wss://sig.example/ws", "tok123", "PUBKEYB64==")

	// Coordinates must live in the fragment (#...), not the query, so they are
	// never sent to (or logged by) the client's HTTP server.
	hash := got[strings.Index(got, "#")+1:]
	if strings.Contains(got[:strings.Index(got, "#")], "tok123") {
		t.Fatalf("token leaked into non-fragment part: %q", got)
	}
	q, err := url.ParseQuery(hash)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("signal") != "wss://sig.example/ws" {
		t.Fatalf("signal = %q", q.Get("signal"))
	}
	if q.Get("token") != "tok123" {
		t.Fatalf("token = %q", q.Get("token"))
	}
	if q.Get("pubkey") != "PUBKEYB64==" {
		t.Fatalf("pubkey = %q", q.Get("pubkey"))
	}
}
