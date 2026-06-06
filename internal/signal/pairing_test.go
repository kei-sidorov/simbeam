package signal

import (
	"net/url"
	"strings"
	"testing"
)

func TestPairingURL_FragmentCarriesSignalDaemonPair(t *testing.T) {
	got := PairingURL("http://localhost:8080/", "wss://host/ws", "DAEMONPUB==", "S3cr3t")
	if !strings.HasPrefix(got, "http://localhost:8080/#") {
		t.Fatalf("missing client base + fragment: %q", got)
	}
	frag := got[strings.Index(got, "#")+1:]
	v, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatalf("parse fragment: %v", err)
	}
	if v.Get("signal") != "wss://host/ws" {
		t.Fatalf("signal = %q", v.Get("signal"))
	}
	if v.Get("daemon") != "DAEMONPUB==" {
		t.Fatalf("daemon = %q", v.Get("daemon"))
	}
	if v.Get("pair") != "S3cr3t" {
		t.Fatalf("pair = %q", v.Get("pair"))
	}
}
