package server

import (
	"context"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/encoder"
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

// Quality bounds for QualityOpts. Scale floors at 0.25 because simulator text
// stops being readable below it; Bitrate floors low enough to keep a picture
// alive on a bad link and ceils at twice today's default, headroom for scale 1.0
// on a retina device.
const (
	MinScale       = 0.25
	MaxScale       = 1.0
	MinBitrate     = 500_000
	MaxBitrate     = 16_000_000
	DefaultBitrate = 8_000_000
)

// QualityOpts is the video quality the client asked for. Presets deliberately do
// not exist here — the wire carries numbers and the iPad client owns the presets
// (decision №88). fps is absent on purpose: idb's Screenshot RPC costs more per
// frame than the 15fps poll interval, so capture is already the ceiling and a
// knob would promise what the pipeline cannot do.
//
// A zero field means "unset", which is what an old client's attach unmarshals
// to; Resolve then supplies the backend's default, so an old client keeps the
// stream it has always had.
//
// This is embedded into every wire struct that carries quality (controlMsg,
// bulkMsg, bulkQuality) rather than copied into each: encoding/json flattens an
// embedded struct, so the fields land at the top level of those messages exactly
// as before, and there is one place to change if a knob is ever added. Note the
// tags must NOT use omitempty — on the reply, an applied scale is meaningful and
// must be sent even in the (unreachable) zero case, and silently dropping it
// would look to a client like "the daemon ignored me".
type QualityOpts struct {
	Scale   float64 `json:"scale"`   // resolution multiplier of the source; 0 → backend default
	Bitrate int     `json:"bitrate"` // target bits/s; 0 → DefaultBitrate
}

// Resolve fills unset fields and clamps the rest into range. defScale differs per
// backend (the sim halves its retina capture, the browser already captures at
// target), so only the caller knows it. Out-of-range clamps rather than errors: a
// client asking for more than the daemon allows should get the daemon's best, not
// a failed attach.
func (o QualityOpts) Resolve(defScale float64) QualityOpts {
	if o.Scale <= 0 {
		o.Scale = defScale
	}
	o.Scale = min(max(o.Scale, MinScale), MaxScale)
	if o.Bitrate <= 0 {
		o.Bitrate = DefaultBitrate
	}
	o.Bitrate = min(max(o.Bitrate, MinBitrate), MaxBitrate)
	return o
}

// Backend abstracts what is being streamed: real iOS simulators via
// idb_companion sidecars on macOS (backend/sim), or a headless browser for the
// hosted demo (backend/browser). The session layer (rtcDispatch) only ever
// talks to this interface; main wires the concrete backend.
type Backend interface {
	Companion
	// DefaultScale is the resolution multiplier applied when the client requests
	// none. It is backend-specific (the sim halves its retina capture, the
	// browser is already at target), and the session layer needs it to tell the
	// client which quality actually took effect.
	DefaultScale() float64
	// Attach starts a live feed for udid at the requested quality (unset fields
	// take the backend's defaults). The feed stops producing frames when ctx is
	// cancelled; the caller must also Close() it to release resources.
	Attach(ctx context.Context, udid string, q QualityOpts) (Feed, error)
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
	// Screenshot captures one frame of the attached device at its native, full
	// resolution and returns the encoded image bytes (PNG). This is the source
	// for the client's "full-resolution screenshot" button — deliberately not
	// pulled from the video track, which is downscaled (decision №40). Unlike
	// Input this is request/reply, so it surfaces errors to the caller.
	Screenshot(ctx context.Context) ([]byte, error)
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
