// Package sim is the macOS backend: it streams real iOS simulators by spawning a
// simbeam-control process per feed (native IOSurface → VideoToolbox H.264, with
// input as NDJSON over the same process). Simulator lifecycle (list/boot/
// shutdown/shake) and full-resolution screenshots delegate to companion.Client
// (xcrun simctl). Nothing here spawns or resolves idb_companion.
package sim

import (
	"context"
	"log"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/encoder"
	"github.com/kei-sidorov/simbeam/internal/server"
)

// defaultScale halves the simulator's retina capture (decision №40) — the
// client's default when it asks for no particular scale. Text stays readable and
// the encoded frame is a quarter of the pixels.
const defaultScale = 0.5

// Backend streams iOS simulators via simbeam-control. It embeds companion.Client
// for the lifecycle surface (server.Companion) and full-resolution screenshots.
type Backend struct {
	*companion.Client
	controlBinary string // resolved path to simbeam-control
}

// New creates the simulator backend. controlBinary is the resolved
// simbeam-control path (see ResolveControl).
func New(c *companion.Client, controlBinary string) *Backend {
	return &Backend{Client: c, controlBinary: controlBinary}
}

// DefaultScale reports the halving applied when the client asks for no scale.
func (b *Backend) DefaultScale() float64 { return defaultScale }

// Attach spawns a simbeam-control process for udid and starts the native
// H.264 pipeline at the requested quality. The feed stops when ctx is cancelled;
// the caller must also Close() it to reap the process.
func (b *Backend) Attach(ctx context.Context, udid string, q server.QualityOpts) (server.Feed, error) {
	q = q.Resolve(defaultScale)
	ctl, err := newControl(ctx, b.controlBinary, udid, q)
	if err != nil {
		return nil, err
	}
	return &feed{ctl: ctl, udid: udid, comp: b.Client}, nil
}

// feed is one live simulator attachment: a simbeam-control process supplying
// video and receiving input, plus the companion client for full-res screenshots.
type feed struct {
	ctl  *control
	udid string
	comp *companion.Client
}

func (f *feed) Screen() (w, h uint64)        { return f.ctl.screen() }
func (f *feed) Frames() <-chan encoder.Frame { return f.ctl.frames }
func (f *feed) Close() error                 { return f.ctl.Close() }

// Screenshot returns a single full-resolution PNG via `simctl io screenshot` —
// the retina frame the video track downscales away.
func (f *feed) Screenshot(ctx context.Context) ([]byte, error) {
	return f.comp.Screenshot(ctx, f.udid)
}

// Input dispatches one gesture to simbeam-control. tap/swipe coordinates are
// normalized [0,1] and scaled into point space inside control. Input is
// fire-and-forget; unsupported keys are dropped.
func (f *feed) Input(_ context.Context, in server.Input) {
	switch in.Type {
	case "tap":
		f.ctl.Tap(in.X, in.Y)
	case "home":
		f.ctl.Home()
	case "swipe":
		dur := in.Duration
		if dur <= 0 {
			dur = 0.3
		}
		f.ctl.Swipe(in.X1, in.Y1, in.X2, in.Y2, dur)
	case "key":
		if usage, shift, ok := keyUsage(in.Key); ok {
			f.ctl.Key(usage, shift)
		} else {
			log.Printf("key: unsupported %q", in.Key)
		}
	}
}
