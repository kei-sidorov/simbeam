// Package server exposes the simcast daemon HTTP API: REST list/boot plus the
// authenticated WebRTC rendezvous (via the signaling broker) that streams H.264
// and carries control over a DataChannel. There is no unauthenticated local
// streaming path — all video/input requires a paired client.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/kei-sidorov/simcast/internal/companion"
)

// Companion is the lifecycle surface the server needs (satisfied by *companion.Client).
type Companion interface {
	List(ctx context.Context) ([]companion.Simulator, error)
	Boot(ctx context.Context, udid string) error
}

// Server wires HTTP handlers over a Companion plus the idb_companion binary path.
type Server struct {
	comp     Companion
	binary   string                    // path to idb_companion for sidecars; "" → "idb_companion"
	webDir   string                    // static debug client dir; "" → not served
	onEnroll func(clientPubKey string) // fired when a new client enrolls via the pairing window; nil → no-op
}

// New creates a Server. webDir is served at / when non-empty.
func New(comp Companion, webDir string) *Server {
	return &Server{comp: comp, webDir: webDir, binary: "idb_companion"}
}

// WithBinary overrides the idb_companion path used for sidecars.
func (s *Server) WithBinary(bin string) *Server { s.binary = bin; return s }

// OnEnroll registers a callback fired (with the client's public key) the moment a
// not-yet-pinned client completes pairing — i.e. the single-use window was just
// consumed. Used by the daemon to print confirmation to the terminal.
func (s *Server) OnEnroll(fn func(clientPubKey string)) *Server { s.onEnroll = fn; return s }

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/simulators", s.handleSimulators)
	mux.HandleFunc("/api/boot", s.handleBoot)
	if s.webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	}
	return mux
}

func (s *Server) handleSimulators(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	sims, err := s.comp.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sims)
}

func (s *Server) handleBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		UDID string `json:"udid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UDID == "" {
		writeErr(w, http.StatusBadRequest, "missing udid")
		return
	}
	if err := s.comp.Boot(r.Context(), body.UDID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "Booted"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
