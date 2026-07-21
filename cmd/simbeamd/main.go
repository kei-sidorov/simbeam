// Command simbeamd is the simbeam daemon. Subcommands:
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
	ossignal "os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/kei-sidorov/simbeam/internal/backend/browser"
	"github.com/kei-sidorov/simbeam/internal/backend/sim"
	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/server"
	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// version is set at release time via -ldflags "-X main.version=...". "dev" otherwise.
var version = "dev"

// defaultSignalURL is the broker WS URL baked in at build time via -ldflags (see
// .goreleaser.yaml / the Makefile `run-remote` target) so a distributed binary
// runs `simbeamd serve` with no flags. Empty in the open-core repo → the unbaked
// build stays local-only. Pairing carries everything else in the URL itself, so
// nothing server-side is needed beyond this broker.
var defaultSignalURL = ""

// defaultClientURL is the hosted web client's base URL, baked in at build time
// alongside defaultSignalURL. Pairing links point here so a distributed binary
// prints a URL the client (iPad/browser) can actually open. Empty in the
// open-core repo → pairing falls back to http://localhost<addr>/ (local dev).
var defaultClientURL = ""

// omitSignalInPairURL, when baked truthy (-X main.omitSignalInPairURL=1 in the
// release build), tells the daemon to leave the broker WS URL out of the pairing
// link: the hosted client at defaultClientURL already knows its default broker,
// so repeating it just lengthens the URL and its QR. -X can only set strings, so
// this is a string treated as a boolean ("" = keep signal, anything else = omit).
// Empty in the open-core / dev build → the full URL is printed (localhost and
// custom brokers still pair).
var omitSignalInPairURL = ""

// pairSignalArg returns the value to pass as PairingURL's signalingURL: "" to omit
// the broker from the pairing link, or signalURL to keep it. It omits only when the
// build opted in AND the daemon is actually talking to the baked default broker —
// so a release binary launched with --signal pointing at a *different* broker still
// prints it, rather than silently sending the client to the default one.
func pairSignalArg(signalURL string) string {
	omit := omitSignalInPairURL != "" && signalURL == defaultSignalURL
	if omit {
		return ""
	}
	return signalURL
}

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
	fmt.Fprintln(w, "simbeamd — simbeam daemon")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  simbeamd list    List available iOS simulators via xcrun simctl")
	fmt.Fprintln(w, "  simbeamd serve   Serve REST API + WebSocket stream (flags: --addr, --web, --signal, --client-url, --identity, --clients, --pair-ttl)")
	fmt.Fprintln(w, "  simbeamd demo    Serve a headless-browser demo device instead of a simulator (flags: --signal, --url, --chrome, --pair-secret, ...)")
	fmt.Fprintln(w, "  simbeamd unpair  Revoke a paired client: simbeamd unpair <clientPubKey>")
	fmt.Fprintln(w, "  simbeamd version Print the version")
	fmt.Fprintln(w, "  simbeamd help    Show this help")
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

// defaultStatePath returns ~/.simbeam/<name>, falling back to ./.simbeam/<name>.
func defaultStatePath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".simbeam", name)
	}
	return filepath.Join(home, ".simbeam", name)
}

func runServe(argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	webDir := fs.String("web", "", "directory with debug client (served at /); empty = API only")
	signalURL := fs.String("signal", defaultSignalURL, "remote rendezvous: signaling broker WS URL (e.g. wss://host/ws); empty = local-only")
	clientURL := fs.String("client-url", defaultClientURL, "base URL of the web client for the pairing link; empty = http://localhost<addr>/")
	identityPath := fs.String("identity", defaultStatePath("identity.key"), "path to the daemon's persistent Ed25519 identity")
	clientsPath := fs.String("clients", defaultStatePath("clients.json"), "path to the pinned-clients store")
	pairTTL := fs.Duration("pair-ttl", 5*time.Minute, "how long an enrollment window stays open after pressing P")
	verbose := fs.Bool("v", false, "verbose logging: log every broker reconnect attempt, not just offline/online transitions")
	_ = fs.Parse(argv)

	c := companion.New()
	path, err := sim.ResolveControl()
	if err != nil {
		return err
	}
	hostName, osVersion := macHostInfo()
	srv := server.New(sim.New(c, path), *webDir).WithHost(hostName, osVersion).WithVerbose(*verbose)

	if *signalURL != "" {
		return runRemote(srv, *signalURL, *clientURL, *addr, *webDir, *identityPath, *clientsPath, *pairTTL)
	}

	fmt.Printf("simbeamd serving on %s (simbeam-control: %s)\n", *addr, path)
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
	name := fs.String("name", "simbeam demo", "device/host name shown in the client")
	addr := fs.String("addr", ":8080", "listen address for the local debug client (only with --web)")
	webDir := fs.String("web", "", "directory with the browser debug client (served at /); empty = none")
	clientURL := fs.String("client-url", defaultClientURL, "base URL of the client for the pairing link; empty = http://localhost<addr>/")
	identityPath := fs.String("identity", defaultStatePath("demo-identity.key"), "path to the demo daemon's persistent Ed25519 identity")
	clientsPath := fs.String("clients", defaultStatePath("demo-clients.json"), "path to the pinned-clients store")
	pairSecret := fs.String("pair-secret", "", "fixed pairing secret; empty = $SIMCAST_PAIR_SECRET, else generated per run")
	verbose := fs.Bool("v", false, "verbose logging: log every broker reconnect attempt, not just offline/online transitions")
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
	srv := server.New(backend, *webDir).WithHost(*name, "demo").WithVerbose(*verbose)

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

	pairURL := signal.PairingURL(base, pairSignalArg(*signalURL), id.PubB64, secret)
	fmt.Printf("simbeamd demo mode — broker: %s\n", *signalURL)
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

	// Take the terminal into a KNOWN state before printing anything, then print the
	// banner with explicit CRLF. A tty's line discipline is a property of the device,
	// not of us: a prior process that went raw (term.MakeRaw disables OPOST) and died
	// without restoring — kill -9, log.Fatal/os.Exit, a crash — leaves OPOST off on the
	// device we inherit. Printing the banner while *assuming* a cooked tty is exactly
	// the bug: a bare "\n" then moves the cursor down but not to column 0, so every line
	// staircases rightward regardless of who broke it. So force a known raw state, emit
	// the banner through a CRLF writer ourselves, and on the way out restore a *sane
	// cooked* state — derived from what we inherited but with the cooked flags forced
	// back on — so the shell we hand back to is never left wedged even if the tty
	// reached us already-raw. A signal watcher runs the same restore on kill /
	// terminal-close. watchKeys now only reads the keystrokes this raw mode enables.
	// Decision #99 (supersedes #98).
	bannerW := io.Writer(os.Stdout)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		if before, gerr := unix.IoctlGetTermios(fd, ioctlGetTermios); gerr == nil {
			if _, merr := term.MakeRaw(fd); merr == nil {
				restoreLog := installRawModeLogWriter()
				bannerW = &crlfWriter{w: os.Stdout}
				sane := saneRestoreTermios(*before)
				var once sync.Once
				restore := func() {
					once.Do(func() {
						restoreLog()
						unix.IoctlSetTermios(fd, ioctlSetTermios, &sane)
					})
				}
				defer restore()

				sigCh := make(chan os.Signal, 1)
				ossignal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
				go func() {
					<-sigCh
					restore()
					os.Exit(130)
				}()
			}
		}
	}

	fmt.Fprintf(bannerW, "simbeamd remote mode — broker: %s\n", signalURL)
	fmt.Fprintf(bannerW, "daemonID: %s\n", id.PubB64)
	fmt.Fprintln(bannerW, "Press P to pair a new device, C to cancel an open window, Q to quit.")

	ui := &pairUI{}

	onPair := func() {
		secret, serr := signal.NewPairingSecret()
		if serr != nil {
			fmt.Printf("\rpairing error: %v\r\n", serr)
			return
		}
		win.Open(secret, time.Now(), pairTTL)
		pairURL := signal.PairingURL(base, pairSignalArg(signalURL), id.PubB64, secret)
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

// saneRestoreTermios derives a cooked terminal state from whatever we inherited,
// forcing back the flags a shell needs — output post-processing (OPOST/ONLCR so
// "\n" returns the cursor to column 0), canonical input with echo (ICANON/ECHO),
// signal keys (ISIG), and CR→NL on input (ICRNL, so Enter works). Restoring this
// instead of the raw snapshot we may have inherited guarantees the terminal we
// hand back is usable even if a prior process left it raw.
func saneRestoreTermios(before unix.Termios) unix.Termios {
	t := before
	t.Oflag |= unix.OPOST | unix.ONLCR
	t.Lflag |= unix.ICANON | unix.ECHO | unix.ISIG | unix.IEXTEN
	t.Iflag |= unix.ICRNL
	return t
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
// just blocks on ctx. Raw mode (and its restore) is owned by runRemote, so a
// non-clean exit can't leave the terminal wedged; here we only read.
func watchKeys(ctx context.Context, cancel context.CancelFunc, onPair, onCancel func()) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		<-ctx.Done()
		return
	}

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
		return fmt.Errorf("usage: simbeamd unpair [--clients path] <clientPubKey>")
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
