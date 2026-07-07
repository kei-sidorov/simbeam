package server

import (
	"context"

	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/encoder"
)

// Companion is the device lifecycle surface the server needs (satisfied by
// *companion.Client for real simulators). list/boot run over the authenticated
// control DataChannel (see rtcDispatch), not over HTTP.
type Companion interface {
	List(ctx context.Context) ([]companion.Simulator, error)
	Boot(ctx context.Context, udid string) error
	Shutdown(ctx context.Context, udid string) error
	Shake(ctx context.Context, udid string) error
}

// Backend abstracts what is being streamed: real iOS simulators via
// idb_companion sidecars on macOS (backend/sim), or a headless browser for the
// hosted demo (backend/browser). The session layer (rtcDispatch) only ever
// talks to this interface; main wires the concrete backend.
type Backend interface {
	Companion
	// Attach starts a live feed for udid. The feed stops producing frames when
	// ctx is cancelled; the caller must also Close() it to release resources.
	Attach(ctx context.Context, udid string) (Feed, error)
}

// Feed is one live video attachment: pre-encoded H.264 access units plus input
// routing to whatever renders them.
type Feed interface {
	// Screen returns the frame dimensions in pixels, reported to the client in
	// the "attached" reply (for aspect/coordinate mapping).
	Screen() (w, h uint64)
	// Frames emits one H.264 access unit per video frame. The channel closes
	// when the feed dies (ctx cancelled or the pipeline fails).
	Frames() <-chan encoder.Frame
	// Input applies one gesture. Input is fire-and-forget: implementations
	// log-and-drop failures rather than surface them (an error reply would
	// wrongly drop the client's UI to "disconnected").
	Input(ctx context.Context, in Input)
	// Close releases everything the feed holds (processes, connections).
	Close() error
}

// Input is one user gesture routed to the live Feed. Tap/swipe coordinates are
// normalized to [0,1] of the displayed frame; each backend scales them into its
// own coordinate space. Key is a browser KeyboardEvent.key string.
type Input struct {
	Type           string // tap|home|swipe|key
	X, Y           float64
	X1, Y1, X2, Y2 float64
	Duration       float64 // swipe duration in seconds; <=0 → backend default
	Key            string
}
