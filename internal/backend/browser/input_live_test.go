package browser

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/kei-sidorov/simcast/internal/encoder"
	"github.com/kei-sidorov/simcast/internal/server"
)

// TestLiveInputReachesPage attaches to the real tic-tac-toe demo page and
// verifies a feed.Input tap lands as a click IN the page (X placed, AI
// answers) while the screenshot pump is running — the exact production path.
func TestLiveInputReachesPage(t *testing.T) {
	if testing.Short() {
		t.Skip("live browser test skipped in -short")
	}
	if err := encoder.Available(); err != nil {
		t.Skipf("encoder unavailable: %v", err)
	}
	if !chromeAvailable() {
		t.Skip("no Chrome/Chromium binary found")
	}

	_, thisFile, _, _ := runtime.Caller(0)
	page := "file://" + filepath.Join(filepath.Dir(thisFile), "../../../web/demo/index.html")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := New(Options{URL: page})
	fd, err := b.Attach(ctx, UDID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer fd.Close()
	f := fd.(*feed)

	// Drain frames like the session pump does.
	go func() {
		for range f.Frames() {
		}
	}()

	// Center of the middle cell, normalized to the viewport.
	var cx, cy float64
	if err := chromedp.Run(f.tabCtx,
		chromedp.WaitVisible(`#board .cell`, chromedp.ByQuery),
		chromedp.Evaluate(`(() => { const r = document.querySelectorAll('.cell')[4].getBoundingClientRect(); return (r.x + r.width/2); })()`, &cx),
		chromedp.Evaluate(`(() => { const r = document.querySelectorAll('.cell')[4].getBoundingClientRect(); return (r.y + r.height/2); })()`, &cy),
	); err != nil {
		t.Fatalf("locate cell: %v", err)
	}

	f.Input(ctx, server.Input{Type: "tap", X: cx / float64(b.opts.Width), Y: cy / float64(b.opts.Height)})

	if err := chromedp.Run(f.tabCtx,
		chromedp.Poll(`document.querySelectorAll('.cell.x').length === 1 && document.querySelectorAll('.cell.o').length === 1`,
			nil, chromedp.WithPollingTimeout(8*time.Second)),
	); err != nil {
		t.Fatalf("tap did not reach the game: %v", err)
	}
}

// TestLiveInputTapDeadCenter reproduces the remote client's exact gesture: a
// tap at normalized (0.5, 0.5) — dead center of the viewport — must land in
// the middle cell of the board.
func TestLiveInputTapDeadCenter(t *testing.T) {
	if testing.Short() {
		t.Skip("live browser test skipped in -short")
	}
	if err := encoder.Available(); err != nil {
		t.Skipf("encoder unavailable: %v", err)
	}
	if !chromeAvailable() {
		t.Skip("no Chrome/Chromium binary found")
	}

	_, thisFile, _, _ := runtime.Caller(0)
	page := "file://" + filepath.Join(filepath.Dir(thisFile), "../../../web/demo/index.html")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := New(Options{URL: page})
	fd, err := b.Attach(ctx, UDID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer fd.Close()
	f := fd.(*feed)
	go func() {
		for range f.Frames() {
		}
	}()

	f.Input(ctx, server.Input{Type: "tap", X: 0.5, Y: 0.5})

	if err := chromedp.Run(f.tabCtx,
		chromedp.Poll(`document.querySelectorAll('.cell.x').length === 1`,
			nil, chromedp.WithPollingTimeout(8*time.Second)),
	); err != nil {
		t.Fatalf("dead-center tap did not reach the game: %v", err)
	}
}
