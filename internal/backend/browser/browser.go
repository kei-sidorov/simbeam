// Package browser is the demo backend: instead of an iOS simulator it streams
// a headless Chromium tab with mobile emulation (video = screenshot poll →
// ffmpeg H.264, exactly the sim pipeline; input = CDP touch/keyboard events).
// It exists so the client can be exercised end-to-end from a machine with no
// macOS — a Linux VPS hosting an interactive demo "device" (App Review, try
// before you buy). One Chromium instance is launched per attach and torn down
// with the feed.
package browser

import (
	"context"
	"fmt"
	"log"
	"time"

	cdpinput "github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/device"
	"github.com/chromedp/chromedp/kb"

	"github.com/kei-sidorov/simcast/internal/companion"
	"github.com/kei-sidorov/simcast/internal/encoder"
	"github.com/kei-sidorov/simcast/internal/server"
)

// fps is the screenshot/encode frame rate, matching the sim backend.
const fps = 15

// UDID is the fixed identifier of the single demo device this backend exposes.
const UDID = "demo"

// Options configure the demo device.
type Options struct {
	URL       string  // page loaded as the demo "device" screen (required)
	ExecPath  string  // chromium/chrome binary; "" → chromedp's default lookup
	Width     int     // viewport width in CSS px; 0 → 390 (iPhone-ish portrait)
	Height    int     // viewport height in CSS px; 0 → 844
	Scale     float64 // deviceScaleFactor; 0 → 2 (frame = CSS size × Scale px)
	Name      string  // device name shown in the client's simulator list; "" → "Demo device"
	NoSandbox bool    // pass --no-sandbox (required when Chromium runs as root)
}

// Backend serves one always-"Booted" demo device rendered by headless Chromium.
type Backend struct {
	opts Options
}

// New creates the browser demo backend, filling option defaults.
func New(opts Options) *Backend {
	if opts.Width <= 0 {
		opts.Width = 390
	}
	if opts.Height <= 0 {
		opts.Height = 844
	}
	if opts.Scale <= 0 {
		opts.Scale = 2
	}
	if opts.Name == "" {
		opts.Name = "Demo device"
	}
	return &Backend{opts: opts}
}

// List exposes the single demo device, always Booted (no lifecycle to manage).
func (b *Backend) List(context.Context) ([]companion.Simulator, error) {
	return []companion.Simulator{{
		UDID:      UDID,
		Name:      b.opts.Name,
		OSVersion: "demo",
		State:     "Booted",
		Type:      "Simulator",
	}}, nil
}

// Boot/Shutdown/Shake are no-ops: the demo device is always on, and shake has
// no meaning in a browser. All return success so the client UI stays calm.
func (b *Backend) Boot(context.Context, string) error     { return nil }
func (b *Backend) Shutdown(context.Context, string) error { return nil }
func (b *Backend) Shake(context.Context, string) error    { return nil }

// Attach launches a headless Chromium with mobile emulation, navigates to the
// demo URL, and starts the screenshot→H.264 pipeline. The feed stops when ctx
// is cancelled; the caller must also Close() it to kill the browser.
func (b *Backend) Attach(ctx context.Context, udid string) (server.Feed, error) {
	if udid != UDID {
		return nil, fmt.Errorf("attach: unknown device %q (demo backend serves only %q)", udid, UDID)
	}
	if err := encoder.Available(); err != nil {
		return nil, err
	}

	allocOpts := chromedp.DefaultExecAllocatorOptions[:]
	if b.opts.ExecPath != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(b.opts.ExecPath))
	}
	if b.opts.NoSandbox {
		allocOpts = append(allocOpts, chromedp.NoSandbox)
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, allocOpts...)
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	cancelAll := func() { tabCancel(); allocCancel() }

	if err := chromedp.Run(tabCtx,
		chromedp.Emulate(device.Info{
			Name:      b.opts.Name,
			UserAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			Width:     int64(b.opts.Width),
			Height:    int64(b.opts.Height),
			Scale:     b.opts.Scale,
			Mobile:    true,
			Touch:     true,
		}),
		chromedp.Navigate(b.opts.URL),
	); err != nil {
		cancelAll()
		return nil, fmt.Errorf("launch demo browser: %w", err)
	}

	// Poll screenshots into the shared PNG→ffmpeg pipeline. Chromium captures at
	// device resolution (CSS × Scale), which is what Screen() reports.
	png := make(chan []byte)
	go func() {
		defer close(png)
		ticker := time.NewTicker(time.Second / fps)
		defer ticker.Stop()
		for {
			select {
			case <-tabCtx.Done():
				return
			case <-ticker.C:
				var buf []byte
				if err := chromedp.Run(tabCtx, chromedp.CaptureScreenshot(&buf)); err != nil {
					if tabCtx.Err() != nil {
						return // shutting down
					}
					log.Printf("demo screenshot: %v", err)
					continue
				}
				select {
				case png <- buf:
				case <-tabCtx.Done():
					return
				}
			}
		}
	}()

	frames, err := encoder.Encode(ctx, png, fps)
	if err != nil {
		cancelAll()
		return nil, err
	}
	return &feed{opts: b.opts, tabCtx: tabCtx, cancel: cancelAll, frames: frames}, nil
}

// feed is one live demo attachment: a Chromium tab plus its encode pipeline.
type feed struct {
	opts   Options
	tabCtx context.Context // chromedp tab context; also the input target
	cancel context.CancelFunc
	frames <-chan encoder.Frame
}

func (f *feed) Screen() (w, h uint64) {
	return uint64(float64(f.opts.Width) * f.opts.Scale), uint64(float64(f.opts.Height) * f.opts.Scale)
}
func (f *feed) Frames() <-chan encoder.Frame { return f.frames }
func (f *feed) Close() error                 { f.cancel(); return nil }

// namedKeys maps the KeyboardEvent.key names the client sends to chromedp's kb
// runes; every other key arrives as its literal character.
var namedKeys = map[string]string{
	"Enter":      kb.Enter,
	"Backspace":  kb.Backspace,
	"Tab":        kb.Tab,
	"Escape":     kb.Escape,
	"Delete":     kb.Delete,
	"ArrowRight": kb.ArrowRight,
	"ArrowLeft":  kb.ArrowLeft,
	"ArrowUp":    kb.ArrowUp,
	"ArrowDown":  kb.ArrowDown,
}

// Input dispatches one gesture as CDP events, scaling normalized coordinates
// into CSS pixels (what dispatchTouchEvent expects). Failures are logged and
// dropped — input is fire-and-forget.
func (f *feed) Input(_ context.Context, in server.Input) {
	switch in.Type {
	case "tap":
		x, y := f.css(in.X, in.Y)
		if err := f.touch(
			touchStep{typ: cdpinput.TouchStart, x: x, y: y},
			touchStep{typ: cdpinput.TouchEnd},
		); err != nil {
			log.Printf("demo tap: %v", err)
		}
	case "home":
		// The demo "Home button" returns the device to its start page.
		if err := chromedp.Run(f.tabCtx, chromedp.Navigate(f.opts.URL)); err != nil {
			log.Printf("demo home: %v", err)
		}
	case "swipe":
		go f.swipe(in) // paced with sleeps; keep the control channel responsive
	case "key":
		keys, ok := namedKeys[in.Key]
		if !ok {
			if len([]rune(in.Key)) != 1 {
				return // unsupported named key (F-keys, IME, ...)
			}
			keys = in.Key
		}
		if err := chromedp.Run(f.tabCtx, chromedp.KeyEvent(keys)); err != nil {
			log.Printf("demo key: %v", err)
		}
	}
}

// swipe plays a drag as touchStart → paced touchMoves → touchEnd. Chromium
// turns the move velocity into scroll/fling, so pacing must follow the
// requested duration rather than firing all moves at once.
func (f *feed) swipe(in server.Input) {
	dur := in.Duration
	if dur <= 0 {
		dur = 0.3
	}
	const steps = 12
	x1, y1 := f.css(in.X1, in.Y1)
	x2, y2 := f.css(in.X2, in.Y2)

	if err := f.touch(touchStep{typ: cdpinput.TouchStart, x: x1, y: y1}); err != nil {
		log.Printf("demo swipe: %v", err)
		return
	}
	pace := time.NewTicker(time.Duration(dur*float64(time.Second)) / steps)
	defer pace.Stop()
	for i := 1; i <= steps; i++ {
		select {
		case <-f.tabCtx.Done():
			return
		case <-pace.C:
		}
		t := float64(i) / steps
		if err := f.touch(touchStep{typ: cdpinput.TouchMove, x: x1 + (x2-x1)*t, y: y1 + (y2-y1)*t}); err != nil {
			log.Printf("demo swipe: %v", err)
			return
		}
	}
	if err := f.touch(touchStep{typ: cdpinput.TouchEnd}); err != nil {
		log.Printf("demo swipe: %v", err)
	}
}

// css maps normalized [0,1] coordinates to CSS pixels, clamped to the viewport.
func (f *feed) css(xNorm, yNorm float64) (x, y float64) {
	return clamp01(xNorm) * float64(f.opts.Width), clamp01(yNorm) * float64(f.opts.Height)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// touchStep is one dispatchTouchEvent call; TouchEnd carries no points.
type touchStep struct {
	typ  cdpinput.TouchType
	x, y float64
}

func (f *feed) touch(steps ...touchStep) error {
	for _, s := range steps {
		var points []*cdpinput.TouchPoint
		if s.typ != cdpinput.TouchEnd && s.typ != cdpinput.TouchCancel {
			points = []*cdpinput.TouchPoint{{X: s.x, Y: s.y}}
		}
		if err := chromedp.Run(f.tabCtx, cdpinput.DispatchTouchEvent(s.typ, points)); err != nil {
			return err
		}
	}
	return nil
}
