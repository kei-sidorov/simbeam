// Package server drives the simcast daemon's authenticated WebRTC rendezvous
// (via the signaling broker): it streams H.264 and carries control (list/boot/
// attach/detach + input) over a DataChannel, all behind an Ed25519 pairing gate.
// The only HTTP surface is the optional static debug client (--web); there is no
// unauthenticated streaming or management endpoint — all video/input/lifecycle
// requires a paired client.
package server

import (
	"context"
	"net/http"

	"github.com/kei-sidorov/simcast/internal/companion"
)

// Companion is the simulator lifecycle surface the server needs (satisfied by
// *companion.Client). list/boot run over the authenticated control DataChannel
// (see rtcDispatch), not over HTTP.
type Companion interface {
	List(ctx context.Context) ([]companion.Simulator, error)
	Boot(ctx context.Context, udid string) error
}

// Server drives the WebRTC rendezvous over a Companion plus the idb_companion
// binary path.
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

// Handler returns the HTTP handler that serves the static debug client at / when
// webDir is set. It exposes no API: simulator list/boot and all video/input flow
// over the authenticated WebRTC DataChannel (see ServeSignal/rtcDispatch). When
// webDir is empty the handler serves nothing.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	}
	return mux
}
