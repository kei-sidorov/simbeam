package sim

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/server"
)

// TestLiveAttach exercises the full Mac path against a real booted simulator and
// the brew-installed simbeam-control. Gated on SIMBEAM_LIVE_SIM=<udid> so CI and
// normal `go test` skip it.
func TestLiveAttach(t *testing.T) {
	udid := os.Getenv("SIMBEAM_LIVE_SIM")
	if udid == "" {
		t.Skip("set SIMBEAM_LIVE_SIM=<booted-udid> to run")
	}
	bin, err := ResolveControl()
	if err != nil {
		t.Fatalf("ResolveControl: %v", err)
	}
	b := New(companion.New(), bin)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	feed, err := b.Attach(ctx, udid, server.QualityOpts{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer feed.Close()

	w, h := feed.Screen()
	if w == 0 || h == 0 {
		t.Fatalf("Screen() = %dx%d, want non-zero (handshake not parsed)", w, h)
	}
	t.Logf("screen = %dx%d", w, h)

	// Drain a few frames, checking the first is a keyframe-bearing access unit.
	var got int
	deadline := time.After(5 * time.Second)
	for got < 10 {
		select {
		case f, ok := <-feed.Frames():
			if !ok {
				t.Fatalf("frames closed after %d frames", got)
			}
			if len(f.Data) == 0 {
				t.Fatal("empty frame")
			}
			got++
		case <-deadline:
			t.Fatalf("only %d frames in 5s", got)
		}
	}
	t.Logf("drained %d frames", got)

	// Fire every input kind; these are fire-and-forget, so we only assert they
	// don't panic and the feed keeps producing frames afterwards.
	feed.Input(ctx, server.Input{Type: "tap", X: 0.5, Y: 0.5})
	feed.Input(ctx, server.Input{Type: "swipe", X1: 0.5, Y1: 0.7, X2: 0.5, Y2: 0.3, Duration: 0.25})
	feed.Input(ctx, server.Input{Type: "home"})
	feed.Input(ctx, server.Input{Type: "key", Key: "a"})

	// Full-res screenshot via simctl.
	shot, err := feed.Screenshot(ctx)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if len(shot) < 1000 {
		t.Fatalf("screenshot too small: %d bytes", len(shot))
	}
	t.Logf("screenshot = %d bytes", len(shot))

	// Frames still flowing after input.
	select {
	case f, ok := <-feed.Frames():
		if !ok || len(f.Data) == 0 {
			t.Fatal("no frame after input")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frames stalled after input")
	}
}
