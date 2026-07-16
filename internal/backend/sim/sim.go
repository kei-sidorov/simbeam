// Package sim is the macOS backend: it streams real iOS simulators by spawning
// an idb_companion sidecar per feed (video = screenshot poll → ffmpeg H.264,
// input = idb HID). Simulator lifecycle (list/boot/shutdown/shake) delegates to
// companion.Client (simctl).
package sim

import (
	"context"
	"log"
	"time"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/encoder"
	"github.com/kei-sidorov/simbeam/internal/idb"
	"github.com/kei-sidorov/simbeam/internal/server"
)

// fps is the screenshot/encode frame rate for the WebRTC path. Not a knob: one
// idb Screenshot RPC costs 71.5–73.9ms against this 66.67ms poll interval, so
// capture is already the ceiling (docs/research/2026-06-08-latency-pipeline.md).
const fps = 15

// defaultScale halves the simulator's retina capture (decision №40) — the
// client's default when it asks for no particular scale.
const defaultScale = 0.5

// Backend streams iOS simulators via idb_companion sidecars. It embeds
// companion.Client for the lifecycle surface (server.Companion).
type Backend struct {
	*companion.Client
	binary string // path to idb_companion for sidecars
}

// New creates the simulator backend. binary is the resolved idb_companion path.
func New(c *companion.Client, binary string) *Backend {
	return &Backend{Client: c, binary: binary}
}

// DefaultScale reports the halving applied when the client asks for no scale.
func (b *Backend) DefaultScale() float64 { return defaultScale }

// Attach spawns an idb_companion sidecar for udid and starts the PNG→H.264
// pipeline at the requested quality. The feed stops when ctx is cancelled; the
// caller must also Close() it to reap the sidecar.
func (b *Backend) Attach(ctx context.Context, udid string, q server.QualityOpts) (server.Feed, error) {
	q = q.Resolve(defaultScale)
	if err := encoder.Available(); err != nil {
		return nil, err
	}
	sidecar, err := idb.Spawn(ctx, b.binary, udid)
	if err != nil {
		return nil, err
	}
	client := sidecar.Client()

	screen, err := client.Describe(ctx)
	if err != nil {
		sidecar.Close()
		return nil, err
	}

	png := client.ScreenshotStream(ctx, time.Second/fps)
	frames, err := encoder.Encode(ctx, png, fps, q.Scale, q.Bitrate)
	if err != nil {
		sidecar.Close()
		return nil, err
	}
	return &feed{sidecar: sidecar, client: client, screen: screen, frames: frames}, nil
}

// feed is one live simulator attachment: a spawned sidecar whose screenshots
// are encoded to H.264, plus the gRPC client that routes input to it.
type feed struct {
	sidecar *idb.Sidecar
	client  *idb.Client
	screen  idb.Screen
	frames  <-chan encoder.Frame
}

func (f *feed) Screen() (w, h uint64)        { return f.screen.Width, f.screen.Height }
func (f *feed) Frames() <-chan encoder.Frame { return f.frames }
func (f *feed) Close() error                 { return f.sidecar.Close() }

// Screenshot returns a single full-resolution PNG straight from the sidecar's
// gRPC Screenshot — the retina frame the video track downscales away.
func (f *feed) Screenshot(ctx context.Context) ([]byte, error) {
	return f.client.Screenshot(ctx)
}

// Input dispatches one gesture to the idb client, scaling normalized
// coordinates into the simulator's logical-point space (which is what hid
// expects). Failures are logged and dropped — input is fire-and-forget.
func (f *feed) Input(ctx context.Context, in server.Input) {
	switch in.Type {
	case "tap":
		if err := f.client.Tap(ctx, idb.ScaleTap(in.X, in.Y, f.screen)); err != nil {
			log.Printf("tap: %v", err)
		}
	case "home":
		if err := f.client.Home(ctx); err != nil {
			log.Printf("home: %v", err)
		}
	case "swipe":
		dur := in.Duration
		if dur <= 0 {
			dur = 0.3
		}
		start := idb.ScaleTap(in.X1, in.Y1, f.screen)
		end := idb.ScaleTap(in.X2, in.Y2, f.screen)
		if err := f.client.Swipe(ctx, start, end, dur); err != nil {
			log.Printf("swipe: %v", err)
		}
	case "key":
		if usage, shift, ok := keyUsage(in.Key); ok {
			if err := f.client.KeyPress(ctx, usage, shift); err != nil {
				log.Printf("key: %v", err)
			}
		}
	}
}
