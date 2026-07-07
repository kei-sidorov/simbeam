package browser

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/kei-sidorov/simcast/internal/encoder"
	"github.com/kei-sidorov/simcast/internal/server"
)

// chromeAvailable reports whether chromedp can find a Chrome/Chromium binary,
// mirroring encoder.Available's skip-if-missing test pattern.
func chromeAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer allocCancel()
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	defer tabCancel()
	return chromedp.Run(tabCtx, chromedp.Navigate("about:blank")) == nil
}

// TestLiveAttachStreamsFrames drives the real pipeline end-to-end: headless
// Chromium renders a data: page, screenshots flow through ffmpeg, and H.264
// access units come out of Frames(). Skipped when Chrome or ffmpeg is missing.
func TestLiveAttachStreamsFrames(t *testing.T) {
	if testing.Short() {
		t.Skip("live browser test skipped in -short")
	}
	if err := encoder.Available(); err != nil {
		t.Skipf("encoder unavailable: %v", err)
	}
	if !chromeAvailable() {
		t.Skip("no Chrome/Chromium binary found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := New(Options{URL: "data:text/html,<body style='background:%23346'><h1>simcast demo</h1></body>"})
	feed, err := b.Attach(ctx, UDID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer feed.Close()

	if w, h := feed.Screen(); w != 780 || h != 1688 {
		t.Fatalf("Screen() = %dx%d, want 780x1688", w, h)
	}

	// The pipeline must produce a keyframe quickly and keep producing frames.
	var got int
	deadline := time.After(30 * time.Second)
	for got < 5 {
		select {
		case f, ok := <-feed.Frames():
			if !ok {
				t.Fatalf("frames channel closed after %d frames", got)
			}
			if len(f.Data) == 0 {
				t.Fatalf("empty H.264 access unit")
			}
			got++
		case <-deadline:
			t.Fatalf("timed out waiting for frames, got %d", got)
		}
	}

	// Input must not error the feed: fire a tap, a key, and a swipe at the page.
	feed.Input(ctx, server.Input{Type: "tap", X: 0.5, Y: 0.5})
	feed.Input(ctx, server.Input{Type: "key", Key: "a"})
	feed.Input(ctx, server.Input{Type: "swipe", X1: 0.5, Y1: 0.8, X2: 0.5, Y2: 0.2, Duration: 0.2})

	// Still streaming after input.
	select {
	case _, ok := <-feed.Frames():
		if !ok {
			t.Fatalf("frames channel closed after input")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no frames after input")
	}
}
