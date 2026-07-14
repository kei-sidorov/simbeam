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

// TestLiveInputTapAfterDelay is the regression test for the halved-input bug:
// with deviceScaleFactor > 1, a few seconds after page load Chrome started
// dividing CDP input coordinates by the scale, so taps landed at css/Scale and
// missed. At Scale 1 (the enforced default) the transform is the identity. The
// tap here is fired only after the pump has streamed for several seconds —
// exactly the window in which the transform used to flip.
func TestLiveInputTapAfterDelay(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b := New(Options{URL: page})
	fd, err := b.Attach(ctx, UDID, server.QualityOpts{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer fd.Close()
	f := fd.(*feed)
	go func() {
		for range f.Frames() {
		}
	}()

	time.Sleep(6 * time.Second) // let the pump run past the transform-flip window

	f.Input(ctx, server.Input{Type: "tap", X: 0.5, Y: 0.5}) // dead center = middle cell

	if err := chromedp.Run(f.tabCtx,
		chromedp.Poll(`document.querySelectorAll('.cell.x').length === 1`,
			nil, chromedp.WithPollingTimeout(8*time.Second)),
	); err != nil {
		t.Fatalf("tap after streaming delay did not land in the game: %v", err)
	}
}
