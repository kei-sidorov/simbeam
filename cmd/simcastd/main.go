// Command simcastd is the simcast daemon. Subcommands:
//   - list: print the real simulators on this machine (Phase 0 bootstrap).
//   - serve: run the REST API + WebSocket stream server (Phase 1); with --signal,
//     persistent remote rendezvous with P-keypress pairing (Phase 3C).
//   - unpair: revoke a paired client (Phase 3C).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/server"
	"github.com/kei-sidorov/simcast/internal/signal"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/term"
)

// version is set at release time via -ldflags "-X main.version=...". "dev" otherwise.
var version = "dev"

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
	case "unpair":
		if err := runUnpair(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println(version)
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
	fmt.Fprintln(w, "  simcastd serve   Serve REST API + WebSocket stream (flags: --addr, --web, --signal, --client-url, --identity, --clients, --pair-ttl)")
	fmt.Fprintln(w, "  simcastd unpair  Revoke a paired client: simcastd unpair <clientPubKey>")
	fmt.Fprintln(w, "  simcastd version Print the version")
	fmt.Fprintln(w, "  simcastd help    Show this help")
}

// defaultStatePath returns ~/.simcast/<name>, falling back to ./.simcast/<name>.
func defaultStatePath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".simcast", name)
	}
	return filepath.Join(home, ".simcast", name)
}

func runServe(argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	webDir := fs.String("web", "", "directory with debug client (served at /); empty = API only")
	signalURL := fs.String("signal", "", "remote rendezvous: signaling broker WS URL (e.g. wss://host/ws); empty = local-only")
	clientURL := fs.String("client-url", "", "base URL of the browser debug client for the pairing link; empty = http://localhost<addr>/")
	identityPath := fs.String("identity", defaultStatePath("identity.key"), "path to the daemon's persistent Ed25519 identity")
	clientsPath := fs.String("clients", defaultStatePath("clients.json"), "path to the pinned-clients store")
	pairTTL := fs.Duration("pair-ttl", 5*time.Minute, "how long an enrollment window stays open after pressing P")
	_ = fs.Parse(argv)

	c := companion.New()
	path, err := c.Resolve()
	if err != nil {
		return err
	}
	srv := server.New(c, *webDir).WithBinary(path)

	if *signalURL != "" {
		return runRemote(srv, *signalURL, *clientURL, *addr, *webDir, *identityPath, *clientsPath, *pairTTL)
	}

	fmt.Printf("simcastd serving on %s (idb_companion: %s)\n", *addr, path)
	if *webDir != "" {
		fmt.Printf("debug client: http://localhost%s/\n", *addr)
	}
	return http.ListenAndServe(*addr, srv.Handler())
}

// runRemote loads the daemon's persistent identity + pinned clients, serves the
// debug client locally (so the browser can load it), connects persistently to the
// broker under daemonID, and watches the terminal: press P to open a one-time
// enrollment window (prints the pairing URL), Q/Ctrl-C to quit.
func runRemote(srv *server.Server, signalURL, clientURL, addr, webDir, identityPath, clientsPath string, pairTTL time.Duration) error {
	id, err := server.LoadOrCreateIdentity(identityPath)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	pinned, err := server.LoadPinnedStore(clientsPath)
	if err != nil {
		return fmt.Errorf("pinned store: %w", err)
	}
	win := server.NewPairingWindow()

	base := clientURL
	if base == "" {
		base = "http://localhost" + addr + "/"
	}
	if webDir != "" {
		ln, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return fmt.Errorf("local http listen on %s: %w", addr, lerr)
		}
		go func() {
			if err := http.Serve(ln, srv.Handler()); err != nil {
				log.Printf("local http: %v", err)
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("simcastd remote mode — broker: %s\n", signalURL)
	fmt.Printf("daemonID: %s\n", id.PubB64)
	fmt.Println("Press P to pair a new device (opens a one-time window). Press Q to quit.")

	onPair := func() {
		secret, serr := signal.NewPairingSecret()
		if serr != nil {
			fmt.Printf("\rpairing error: %v\r\n", serr)
			return
		}
		win.Open(secret, time.Now(), pairTTL)
		pairURL := signal.PairingURL(base, signalURL, id.PubB64, secret)
		fmt.Printf("\r\nPair this device (window open %s) — scan with the iPad camera,\r\nor open the URL below:\r\n\r\n", pairTTL)
		printPairingQR(os.Stdout, pairURL)
		fmt.Printf("\r\n  %s\r\n", pairURL)
	}

	go watchKeys(ctx, cancel, onPair)
	return srv.ServeSignal(ctx, signalURL, id, pinned, win)
}

// printPairingQR renders the pairing URL as a compact QR code in the terminal so
// the iPad camera can pick it up directly (Expo-style), avoiding a copy-paste of
// the long enrollment fragment. Half-blocks keep it small and level L keeps the
// module count low (the URL is ~200 chars); the secret is one-time and the window
// short-lived, so low error correction is fine. The caller may have put the TTY
// in raw mode, so newlines are rewritten to CRLF to avoid a staircase.
func printPairingQR(w io.Writer, url string) {
	qrterminal.GenerateWithConfig(url, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         &crlfWriter{w: w},
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		QuietZone:      1,
	})
}

// crlfWriter rewrites bare "\n" to "\r\n" so terminal output lines up under a TTY
// in raw mode (where "\n" moves down but not to column 0).
type crlfWriter struct{ w io.Writer }

func (c *crlfWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write([]byte(strings.ReplaceAll(string(p), "\n", "\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// watchKeys reads single keystrokes from a terminal: P opens a pairing window,
// Q/Ctrl-C cancels. If stdin is not a TTY (piped/tests), it just blocks on ctx.
func watchKeys(ctx context.Context, cancel context.CancelFunc, onPair func()) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		<-ctx.Done()
		return
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		<-ctx.Done()
		return
	}
	defer term.Restore(fd, old)

	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			cancel()
			return
		}
		switch buf[0] {
		case 'p', 'P':
			onPair()
		case 'q', 'Q', 3: // 3 = Ctrl-C (raw mode delivers it as a byte)
			cancel()
			return
		}
	}
}

// runUnpair revokes a client by removing it from the pinned store.
func runUnpair(argv []string) error {
	fs := flag.NewFlagSet("unpair", flag.ExitOnError)
	clientsPath := fs.String("clients", defaultStatePath("clients.json"), "path to the pinned-clients store")
	_ = fs.Parse(argv)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: simcastd unpair [--clients path] <clientPubKey>")
	}
	pinned, err := server.LoadPinnedStore(*clientsPath)
	if err != nil {
		return err
	}
	if err := pinned.Remove(fs.Arg(0)); err != nil {
		return err
	}
	fmt.Printf("revoked %s\n", fs.Arg(0))
	return nil
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
