// Command simbeam-signal is the reference simbeam signaling broker: a thin WSS
// rendezvous that keeps a daemon present by daemonID, relays the mutual
// challenge-response + one offer→answer, issues iceServers (STUN always; TURN
// only when the client's subscription is active), and serves the subscription
// API. Media never transits it. The managed/production broker is the open-core
// moat (decisions #9, #47); this build is for local dev and self-host.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signalbroker"
	"github.com/kei-sidorov/simbeam/internal/store"
)

// version is set at release time via -ldflags "-X main.version=...". "dev" otherwise.
var version = "dev"

func main() {
	versionFlag := flag.Bool("version", false, "print version and exit")
	addr := flag.String("addr", ":9000", "listen address")
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs (handed to everyone)")
	turn := flag.String("turn", "", "comma-separated TURN URLs (handed only to active subscribers)")
	turnSecret := flag.String("turn-secret", "", "coturn static-auth-secret for ephemeral credentials")
	turnTTL := flag.Duration("turn-ttl", time.Minute, "ephemeral TURN credential lifetime")
	turnOpen := flag.Bool("turn-open", false, "grant TURN to ALL authenticated clients, bypassing the subscription gate (temporary — use while there are no subscriptions)")
	db := flag.String("db", "simbeam.db", "SQLite path for the subscriptions store")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return
	}

	st, err := store.OpenSQLite(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer st.Close()

	appSecret := os.Getenv("SIMCAST_APP_SECRET")
	if appSecret == "" {
		fmt.Fprintln(os.Stderr, "WARNING: SIMCAST_APP_SECRET is empty — the subscription API app-sig barrier is disabled")
	}
	if *turnOpen {
		fmt.Fprintln(os.Stderr, "WARNING: --turn-open set — TURN relay handed to ALL authenticated clients (subscription gate bypassed)")
	}

	b := signalbroker.New(signalbroker.Config{
		STUNURLs:   splitNonEmpty(*stun),
		TURNURLs:   splitNonEmpty(*turn),
		TURNSecret: *turnSecret,
		TURNTTL:    *turnTTL,
		Store:      st,
		AppSecret:  appSecret,
		TURNOpen:   *turnOpen,
	})

	fmt.Printf("simbeam-signal listening on %s (ws: /ws, api: /v1/subscription, db: %s)\n", *addr, *db)
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
