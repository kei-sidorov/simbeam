// Command simcastd is the simcast daemon. Subcommands:
//   - list: print the real simulators on this machine (Phase 0 bootstrap).
//   - serve: run the REST API + WebSocket stream server (Phase 1).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/server"
	"github.com/kei-sidorov/simcast/internal/signal"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch args[0] {
	case "list":
		if err := runList(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "serve":
		if err := runServe(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "simcastd — simcast daemon")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  simcastd list    List available iOS simulators via idb_companion")
	fmt.Fprintln(w, "  simcastd serve   Serve REST API + WebSocket stream (flags: --addr, --web, --signal, --client-url)")
	fmt.Fprintln(w, "  simcastd help    Show this help")
}

func runServe(argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	webDir := fs.String("web", "", "directory with debug client (served at /); empty = API only")
	signalURL := fs.String("signal", "", "remote rendezvous: signaling broker WS URL (e.g. wss://host/ws); empty = local-only")
	clientURL := fs.String("client-url", "", "base URL of the browser debug client for the pairing link; empty = http://localhost<addr>/")
	_ = fs.Parse(argv)

	c := companion.New()
	path, err := c.Resolve()
	if err != nil {
		return err
	}
	srv := server.New(c, *webDir).WithBinary(path)

	// Remote rendezvous: dial the broker, print a pairing URL, serve one client.
	if *signalURL != "" {
		return runRemote(srv, *signalURL, *clientURL, *addr, *webDir)
	}

	fmt.Printf("simcastd serving on %s (idb_companion: %s)\n", *addr, path)
	if *webDir != "" {
		fmt.Printf("debug client: http://localhost%s/\n", *addr)
	}
	return http.ListenAndServe(*addr, srv.Handler())
}

// runRemote dials the signaling broker and serves a single paired client. It
// also serves the local HTTP (debug client) so the browser has somewhere to
// load from; the pairing URL points there with the signaling coordinates in
// the fragment.
func runRemote(srv *server.Server, signalURL, clientURL, addr, webDir string) error {
	pubKey, priv, err := signal.GenerateKeyPair()
	if err != nil {
		return err
	}
	token, err := signal.NewToken()
	if err != nil {
		return err
	}
	base := clientURL
	if base == "" {
		base = "http://localhost" + addr + "/"
	}

	// Serve the debug client locally (so the browser can load it) in the
	// background; pairing coordinates travel via the URL fragment, not this server.
	// Pre-bind synchronously so a port conflict surfaces before the pairing URL is printed.
	if webDir != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("local http listen on %s: %w", addr, err)
		}
		go func() {
			if err := http.Serve(ln, srv.Handler()); err != nil {
				log.Printf("local http: %v", err)
			}
		}()
	}

	fmt.Printf("simcastd remote mode — broker: %s\n", signalURL)
	fmt.Println("Pair this device by opening:")
	fmt.Println("  " + signal.PairingURL(base, signalURL, token, pubKey))
	fmt.Println("(token is one-time; restart to pair again)")

	ctx := context.Background()
	return srv.DialSignal(ctx, signalURL, token, pubKey, priv)
}

func runList() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := companion.New()
	path, err := c.Resolve()
	if err != nil {
		return err
	}
	if v, err := c.Version(ctx); err == nil {
		fmt.Printf("idb_companion: %s (built %s)\n\n", path, v)
	} else {
		fmt.Printf("idb_companion: %s\n\n", path)
	}

	sims, err := c.List(ctx)
	if err != nil {
		return err
	}
	if len(sims) == 0 {
		fmt.Println("No simulators found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "STATE\tNAME\tOS\tUDID")
	for _, s := range sims {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.State, s.Name, s.OSVersion, s.UDID)
	}
	w.Flush()

	fmt.Printf("\n%d simulator(s).\n", len(sims))
	return nil
}
