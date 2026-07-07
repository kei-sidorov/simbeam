// Package server drives the simcast daemon's authenticated WebRTC rendezvous
// (via the signaling broker): it streams H.264 and carries control (list/boot/
// attach/detach + input) over a DataChannel, all behind an Ed25519 pairing gate.
// The only HTTP surface is the optional static debug client (--web); there is no
// unauthenticated streaming or management endpoint — all video/input/lifecycle
// requires a paired client.
//
// The device being streamed is abstracted behind Backend (see backend.go): real
// simulators on macOS, a headless browser for the hosted demo.
package server

import "net/http"

// Server drives the WebRTC rendezvous over a Backend.
type Server struct {
	backend   Backend
	webDir    string                    // static debug client dir; "" → not served
	onEnroll  func(clientPubKey string) // fired when a new client enrolls via the pairing window; nil → no-op
	hostName  string                    // host display name, pushed in the hello (e.g. "Kirill's MacBook Pro"); "" → omitted
	osVersion string                    // host OS version, pushed in the hello (e.g. "26.5"); "" → omitted
}

// New creates a Server. webDir is served at / when non-empty.
func New(backend Backend, webDir string) *Server {
	return &Server{backend: backend, webDir: webDir}
}

// WithHost sets the host display name and OS version the daemon pushes in the
// per-session hello (so a paired client can show "Kirill's MacBook Pro" /
// "macOS 26.5"). Either may be empty, in which case the client omits it.
func (s *Server) WithHost(name, osVersion string) *Server {
	s.hostName, s.osVersion = name, osVersion
	return s
}

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
