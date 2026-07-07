// Command simcastd is the simcast daemon. Subcommands:
//   - list: print the real simulators on this machine (Phase 0 bootstrap).
//   - serve: run the REST API + WebSocket stream server (Phase 1); with --signal,
//     persistent remote rendezvous with P-keypress pairing (Phase 3C).
//   - demo: stream a headless-browser demo device instead of simulators —
//     Linux-friendly, unattended, multi-use pairing (App Review / demos).
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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/kei-sidorov/simcast/internal/backend/browser"
	"github.com/kei-sidorov/simcast/internal/backend/sim"
	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/server"
	"github.com/kei-sidorov/simcast/internal/signal"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/term"
)

// version is set at release time via -ldflags "-X main.version=...". "dev" otherwise.
var version = "dev"

// defaultSignalURL is the broker WS URL baked in at build time via -ldflags (see
// .goreleaser.yaml / the Makefile `run-remote` target) so a distributed binary
// runs `simcastd serve` with no flags. Empty in the open-core repo → the unbaked
// build stays local-only. Pairing carries everything else in the URL itself, so
// nothing server-side is needed beyond this broker.
var defaultSignalURL = ""

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
	case "demo":
		if err := runDemo(args[1:]); err != nil {
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
	fmt.Fprintln(w, "  simcastd demo    Serve a headless-browser demo device instead of a simulator (flags: --signal, --url, --chrome, --pair-secret, ...)")
	fmt.Fprintln(w, "  simcastd unpair  Revoke a paired client: simcastd unpair <clientPubKey>")
	fmt.Fprintln(w, "  simcastd version Print the version")
	fmt.Fprintln(w, "  simcastd help    Show this help")
}

// macHostInfo returns the Mac's display name (scutil ComputerName) and macOS
// version (sw_vers -productVersion) for the pairing hello. Best-effort: any
// failure yields an empty string and the client simply omits that subtitle.
func macHostInfo() (name, osVersion string) {
	// Each command gets its own short timeout so a hung scutil can't starve
	// sw_vers (and vice versa), and neither can stall daemon startup for long.
	run := func(bin string, args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return run("scutil", "--get", "ComputerName"), run("sw_vers", "-productVersion")
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
	signalURL := fs.String("signal", defaultSignalURL, "remote rendezvous: signaling broker WS URL (e.g. wss://host/ws); empty = local-only")
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
	hostName, osVersion := macHostInfo()
	srv := server.New(sim.New(c, path), *webDir).WithHost(hostName, osVersion)

	if *signalURL != "" {
		return runRemote(srv, *signalURL, *clientURL, *addr, *webDir, *identityPath, *clientsPath, *pairTTL)
	}

	fmt.Printf("simcastd serving on %s (idb_companion: %s)\n", *addr, path)
	if *webDir != "" {
		fmt.Printf("debug client: http://localhost%s/\n", *addr)
	}
	return http.ListenAndServe(*addr, srv.Handler())
}

// runDemo runs the daemon with the headless-browser demo backend instead of
// real simulators: a Linux-friendly, always-on demo device (App Review, try
// before you buy). Unlike serve, pairing is not an interactive one-time window:
// the window is armed at startup with a long-lived secret and re-armed after
// every enrollment, so the printed pairing URL stays valid for any number of
// clients (the demo box is throwaway — this trades the serve mode's anti-abuse
// single-use property for unattended operation).
func runDemo(argv []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	signalURL := fs.String("signal", defaultSignalURL, "signaling broker WS URL (required; e.g. wss://host/ws)")
	demoURL := fs.String("url", "", "page the demo device renders (required)")
	chrome := fs.String("chrome", "", "path to the Chromium/Chrome binary; empty = auto-detect")
	noSandbox := fs.Bool("chrome-no-sandbox", false, "pass --no-sandbox to Chromium (needed only when running as root)")
	name := fs.String("name", "simcast demo", "device/host name shown in the client")
	addr := fs.String("addr", ":8080", "listen address for the local debug client (only with --web)")
	webDir := fs.String("web", "", "directory with the browser debug client (served at /); empty = none")
	clientURL := fs.String("client-url", "", "base URL of the client for the pairing link; empty = http://localhost<addr>/")
	identityPath := fs.String("identity", defaultStatePath("demo-identity.key"), "path to the demo daemon's persistent Ed25519 identity")
	clientsPath := fs.String("clients", defaultStatePath("demo-clients.json"), "path to the pinned-clients store")
	pairSecret := fs.String("pair-secret", "", "fixed pairing secret; empty = $SIMCAST_PAIR_SECRET, else generated per run")
	_ = fs.Parse(argv)

	if *signalURL == "" {
		return fmt.Errorf("demo needs a broker: pass --signal wss://host/ws")
	}
	if *demoURL == "" {
		return fmt.Errorf("demo needs a page to render: pass --url https://...")
	}

	backend := browser.New(browser.Options{
		URL:       *demoURL,
		ExecPath:  *chrome,
		NoSandbox: *noSandbox,
		Name:      *name,
	})
	srv := server.New(backend, *webDir).WithHost(*name, "demo")

	id, err := server.LoadOrCreateIdentity(*identityPath)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	pinned, err := server.LoadPinnedStore(*clientsPath)
	if err != nil {
		return fmt.Errorf("pinned store: %w", err)
	}

	secret := *pairSecret
	if secret == "" {
		secret = os.Getenv("SIMCAST_PAIR_SECRET")
	}
	if secret == "" {
		if secret, err = signal.NewPairingSecret(); err != nil {
			return fmt.Errorf("pairing secret: %w", err)
		}
	}

	// Effectively-infinite TTL; verify() consumes the window on each successful
	// enrollment, so OnEnroll re-arms it with the same secret (multi-use).
	const demoTTL = 100 * 365 * 24 * time.Hour
	win := server.NewPairingWindow()
	win.Open(secret, time.Now(), demoTTL)
	srv.OnEnroll(func(clientPubKey string) {
		log.Printf("demo: paired client %.16s…", clientPubKey)
		win.Open(secret, time.Now(), demoTTL)
	})

	base := *clientURL
	if base == "" {
		base = "http://localhost" + *addr + "/"
	}
	if *webDir != "" {
		ln, lerr := net.Listen("tcp", *addr)
		if lerr != nil {
			return fmt.Errorf("local http listen on %s: %w", *addr, lerr)
		}
		go func() {
			if err := http.Serve(ln, srv.Handler()); err != nil {
				log.Printf("local http: %v", err)
			}
		}()
	}

	pairURL := signal.PairingURL(base, *signalURL, id.PubB64, secret)
	fmt.Printf("simcastd demo mode — broker: %s\n", *signalURL)
	fmt.Printf("daemonID: %s\n", id.PubB64)
	fmt.Printf("demo page: %s\n", *demoURL)
	fmt.Printf("\nPairing URL (multi-use, survives restarts only with a fixed --pair-secret):\n\n  %s\n\n", pairURL)
	if term.IsTerminal(int(os.Stdout.Fd())) {
		printPairingQR(os.Stdout, pairURL)
	}

	return srv.ServeSignal(context.Background(), *signalURL, id, pinned, win)
}

// runRemote loads the daemon's persistent identity + pinned clients, serves the
// debug client locally (so the browser can load it), connects persistently to the
// broker under daemonID, and watches the terminal: press P to open a one-time
// enrollment window (prints a QR + URL), C to cancel it, Q/Ctrl-C to quit.
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
	fmt.Println("Press P to pair a new device, C to cancel an open window, Q to quit.")

	ui := &pairUI{}

	onPair := func() {
		secret, serr := signal.NewPairingSecret()
		if serr != nil {
			fmt.Printf("\rpairing error: %v\r\n", serr)
			return
		}
		win.Open(secret, time.Now(), pairTTL)
		pairURL := signal.PairingURL(base, signalURL, id.PubB64, secret)
		block, rows := renderPairBlock(pairURL, pairTTL)
		ui.show(block, rows, pairTTL, func() {
			// TTL fired: the secret is dead — disarm and grey out the on-screen code.
			win.Close()
			ui.retire(ansiFaint + "⏲  pairing window EXPIRED — code above is dead" + ansiReset)
		})
	}

	onCancel := func() {
		win.Close()
		ui.retire(ansiRed + "✗  pairing CANCELLED — code above is dead" + ansiReset)
	}

	// When a new device finishes pairing, the single-use window is consumed: grey
	// out the QR still on screen and confirm with the client's key.
	srv.OnEnroll(func(clientPubKey string) {
		short := clientPubKey
		if len(short) > 16 {
			short = short[:16]
		}
		ui.retire(ansiGreen + "✓  paired " + short + "… — enrollment window closed" + ansiReset)
	})

	go watchKeys(ctx, cancel, onPair, onCancel)
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

// installRawModeLogWriter routes the standard logger through crlfWriter for as
// long as the terminal is in raw mode. term.MakeRaw disables the terminal's
// output post-processing (OPOST), so a bare "\n" from log.Printf (e.g. the
// signaling reconnect line) no longer returns the cursor to column 0 and lines
// staircase rightward. Returns a restore func to reinstate the previous writer
// when raw mode is released.
func installRawModeLogWriter() (restore func()) {
	prev := log.Writer()
	log.SetOutput(&crlfWriter{w: prev})
	return func() { log.SetOutput(prev) }
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

// ANSI SGR codes for greying out / colouring the retired pairing block.
const (
	ansiReset = "\x1b[0m"
	ansiFaint = "\x1b[2m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
)

// renderPairBlock builds the on-screen pairing block (header + QR + URL) as a
// single CRLF string and reports how many terminal rows it occupies, so pairUI
// can later move the cursor back over exactly this block to grey it out.
func renderPairBlock(pairURL string, ttl time.Duration) (block string, rows int) {
	var b strings.Builder
	fmt.Fprintf(&b, "\r\nPair this device (window open %s) — scan with the iPad camera,\r\nor open the URL below:\r\n\r\n", ttl)
	printPairingQR(&b, pairURL)
	fmt.Fprintf(&b, "\r\n  %s\r\n", pairURL)
	s := b.String()
	return s, strings.Count(s, "\n")
}

// pairUI tracks the pairing block currently on screen so it can be redrawn faint
// (with a coloured status line) when the window is cancelled, expires, or is
// consumed. Best-effort: retire() repositions the cursor up over the block, which
// assumes nothing else printed in between and the block did not scroll off-screen.
type pairUI struct {
	mu     sync.Mutex
	block  string // rendered block, CRLF, no colour
	rows   int    // terminal rows it occupies
	active bool
	timer  *time.Timer
}

// show prints a freshly-rendered pairing block and arms a TTL timer that calls
// onExpire. Any previously-armed window is superseded.
func (u *pairUI) show(block string, rows int, ttl time.Duration, onExpire func()) {
	u.mu.Lock()
	if u.timer != nil {
		u.timer.Stop()
	}
	u.block, u.rows, u.active = block, rows, true
	u.timer = time.AfterFunc(ttl, onExpire)
	u.mu.Unlock()
	fmt.Fprint(os.Stdout, block)
}

// retire greys out the on-screen block (if still active) and prints status below
// it. No-op if the block was already retired, so cancel/expire/enroll racing each
// other only ever redraw once.
func (u *pairUI) retire(status string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.active {
		return
	}
	u.active = false
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[%dA\r", u.rows) // up to the first line of the block, column 0
	b.WriteString(ansiFaint)
	b.WriteString(u.block) // same content, now dimmed
	b.WriteString(ansiReset)
	fmt.Fprintf(&b, "%s\x1b[K\r\n", status)
	fmt.Fprint(os.Stdout, b.String())
}

// watchKeys reads single keystrokes from a terminal: P opens a pairing window,
// C cancels an open one, Q/Ctrl-C quits. If stdin is not a TTY (piped/tests), it
// just blocks on ctx.
func watchKeys(ctx context.Context, cancel context.CancelFunc, onPair, onCancel func()) {
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
	defer installRawModeLogWriter()()

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
		case 'c', 'C':
			onCancel()
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
	// Listing uses simctl and no longer needs idb_companion; only report idb's
	// status (it is still required for streaming) instead of failing without it.
	if path, err := c.Resolve(); err != nil {
		fmt.Printf("idb_companion: not found (required for streaming, not for list)\n\n")
	} else if v, err := c.Version(ctx); err == nil {
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
