// Command simcast-signal is the reference simcast signaling broker: a thin WSS
// rendezvous that pairs a daemon and a browser by pairing token, relays one
// offer→answer, and issues iceServers (STUN always; TURN only when granted).
// Media never transits it. The managed/production broker is the open-core moat
// (decision #9, #47); this reference build is for local dev and self-host.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kei-sidorov/simcast/internal/signalbroker"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs (handed to everyone)")
	turn := flag.String("turn", "", "comma-separated TURN URLs (handed only to granted rooms)")
	turnSecret := flag.String("turn-secret", "", "coturn static-auth-secret for ephemeral credentials")
	turnTTL := flag.Duration("turn-ttl", time.Minute, "ephemeral TURN credential lifetime")
	grantTURN := flag.Bool("grant-turn", false, "STUB subscription gate: grant TURN to every room (Phase 4 = real billing)")
	flag.Parse()

	b := signalbroker.New(signalbroker.Config{
		STUNURLs:   splitNonEmpty(*stun),
		TURNURLs:   splitNonEmpty(*turn),
		TURNSecret: *turnSecret,
		TURNTTL:    *turnTTL,
		GrantTURN:  func(string) bool { return *grantTURN },
	})

	fmt.Printf("simcast-signal listening on %s (ws path: /ws)\n", *addr)
	if *grantTURN {
		fmt.Println("WARNING: --grant-turn is a STUB that grants TURN to every room (dev/testing only)")
	}
	if err := http.ListenAndServe(*addr, b.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func splitNonEmpty(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
