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
	turnKeyID := flag.String("turn-key-id", "", "Cloudflare Realtime TURN key ID (relay handed only to active subscribers); the key's API token comes from SIMBEAM_TURN_API_TOKEN")
	turnTTL := flag.Duration("turn-ttl", 24*time.Hour, "TURN credential lifetime requested from Cloudflare (max 48h); must outlive a streaming session")
	turnOpen := flag.Bool("turn-open", false, "grant TURN to ALL authenticated clients, bypassing the subscription gate (temporary — use while there are no subscriptions)")
	db := flag.String("db", "simbeam.db", "SQLite path for the subscriptions store")
	statsAddr := flag.String("stats-addr", "", "if set, serve aggregate JSON stats at http://<addr>/stats — bind loopback only, it is NOT behind the reverse proxy's auth")
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

	var turnEndpoint string
	turnToken := os.Getenv("SIMBEAM_TURN_API_TOKEN")
	if *turnKeyID != "" {
		if turnToken == "" {
			fmt.Fprintln(os.Stderr, "--turn-key-id set but SIMBEAM_TURN_API_TOKEN is empty")
			os.Exit(1)
		}
		turnEndpoint = signalbroker.CloudflareTURNEndpoint(*turnKeyID)
	}

	b := signalbroker.New(signalbroker.Config{
		STUNURLs:     splitNonEmpty(*stun),
		TURNEndpoint: turnEndpoint,
		TURNAPIToken: turnToken,
		TURNTTL:      *turnTTL,
		Store:        st,
		AppSecret:    appSecret,
		TURNOpen:     *turnOpen,
	})

	// Stats live on their own listener so the public reverse proxy (which
	// forwards everything on the main mux) can never reach them.
	if *statsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/stats", b.StatsHandler())
		go func() {
			if err := http.ListenAndServe(*statsAddr, mux); err != nil {
				fmt.Fprintln(os.Stderr, "stats listener:", err)
			}
		}()
		fmt.Printf("simbeam-signal stats on http://%s/stats\n", *statsAddr)
	}

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
