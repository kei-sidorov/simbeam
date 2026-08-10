package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

// The daemon's local control API: a loopback-only HTTP listener a sibling
// `simbeamd pair` invocation uses to arm the pairing window of the RUNNING
// process (the service). Loopback alone is not the auth boundary — pairing
// grants persistent access and "same machine" is not "same user" — so the
// daemon writes a bearer token to a 0600 file only this user can read.

// controlFilePath is where the running daemon advertises its control endpoint.
func controlFilePath() string { return defaultStatePath("control.json") }

type controlInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

type pairReply struct {
	URL        string `json:"url"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type pairState struct {
	State  string `json:"state"` // none | open | consumed | expired
	Client string `json:"client,omitempty"`
}

// pairWindow is the slice of *server.pairingWindow the control API needs (the
// concrete type is unexported; the interface keeps it nameable here).
type pairWindow interface {
	Open(secret string, now time.Time, ttl time.Duration)
	State(now time.Time) string
}

type controlServer struct {
	win      pairWindow
	buildURL func(secret string) string
	ttl      time.Duration
	token    string

	mu         sync.Mutex
	lastClient string
}

// noteEnrolled records who consumed the window so GET /pair can report it.
func (c *controlServer) noteEnrolled(clientPubKey string) {
	c.mu.Lock()
	c.lastClient = clientPubKey
	c.mu.Unlock()
}

func (c *controlServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		secret, err := signal.NewPairingSecret()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.win.Open(secret, time.Now(), c.ttl)
		json.NewEncoder(w).Encode(pairReply{URL: c.buildURL(secret), TTLSeconds: int(c.ttl.Seconds())})
	})
	mux.HandleFunc("GET /pair", func(w http.ResponseWriter, r *http.Request) {
		st := pairState{State: c.win.State(time.Now())}
		if st.State == "consumed" {
			c.mu.Lock()
			st.Client = c.lastClient
			c.mu.Unlock()
		}
		json.NewEncoder(w).Encode(st)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+c.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// startControl listens on an ephemeral loopback port and publishes addr+token
// to the control file. Callers defer stop(); a crash leaves a stale file, which
// the pair client reads as "connection refused" and reports as daemon-not-running.
func startControl(win pairWindow, buildURL func(string) string, ttl time.Duration) (*controlServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		ln.Close()
		return nil, err
	}
	c := &controlServer{win: win, buildURL: buildURL, ttl: ttl, token: hex.EncodeToString(raw)}
	path := controlFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		ln.Close()
		return nil, err
	}
	blob, _ := json.Marshal(controlInfo{Addr: ln.Addr().String(), Token: c.token})
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	go http.Serve(ln, c.handler())
	return c, nil
}

func (c *controlServer) stop() { os.Remove(controlFilePath()) }

// runPair asks the running daemon (service or interactive serve) to open a
// pairing window, prints the URL + QR, and waits for the outcome.
func runPair(argv []string) error {
	if len(argv) > 0 {
		return fmt.Errorf("pair takes no arguments")
	}
	blob, err := os.ReadFile(controlFilePath())
	if err != nil {
		return errors.New("no running daemon found — start one with 'simbeamd service install' (or 'simbeamd serve')")
	}
	var info controlInfo
	if err := json.Unmarshal(blob, &info); err != nil {
		return fmt.Errorf("control file corrupt (%s): %w", controlFilePath(), err)
	}
	call := func(method, path string, out any) error {
		req, err := http.NewRequest(method, "http://"+info.Addr+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+info.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return errors.New("daemon not responding — is the service running? (simbeamd service status)")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("daemon control API: %s", resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	var rep pairReply
	if err := call("POST", "/pair", &rep); err != nil {
		return err
	}
	ttl := time.Duration(rep.TTLSeconds) * time.Second
	fmt.Printf("Pairing window open for %s. Scan with your iPad, or open:\n\n  %s\n\n", ttl, rep.URL)
	printPairingQR(os.Stdout, rep.URL)
	fmt.Println("\nwaiting for the device… (Ctrl-C to stop waiting; the code stays valid until it expires)")

	for {
		time.Sleep(time.Second)
		var st pairState
		if err := call("GET", "/pair", &st); err != nil {
			return err
		}
		switch st.State {
		case "consumed":
			short := st.Client
			if len(short) > 16 {
				short = short[:16]
			}
			fmt.Printf("✓ paired %s…\n", short)
			return nil
		case "expired", "none":
			return errors.New("pairing window expired — run 'simbeamd pair' again")
		}
	}
}
