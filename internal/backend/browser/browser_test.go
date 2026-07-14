package browser

import (
	"context"
	"testing"

	"github.com/kei-sidorov/simcast/internal/server"
)

func TestListExposesOneBootedDemoDevice(t *testing.T) {
	b := New(Options{URL: "https://example.com", Name: "Review iPhone"})
	sims, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sims) != 1 {
		t.Fatalf("want exactly one demo device, got %+v", sims)
	}
	s := sims[0]
	if s.UDID != UDID || s.State != "Booted" || s.Name != "Review iPhone" {
		t.Fatalf("want Booted %q named Review iPhone, got %+v", UDID, s)
	}
}

func TestAttachRejectsUnknownUDID(t *testing.T) {
	b := New(Options{URL: "https://example.com"})
	if _, err := b.Attach(context.Background(), "not-demo", server.QualityOpts{}); err == nil {
		t.Fatalf("Attach of unknown udid must fail before launching a browser")
	}
}

func TestOptionDefaultsAndScreen(t *testing.T) {
	b := New(Options{URL: "https://example.com"})
	if b.opts.Width != 390 || b.opts.Height != 844 || b.opts.Scale != 1 {
		t.Fatalf("defaults not applied: %+v", b.opts)
	}
	f := &feed{opts: b.opts}
	if w, h := f.Screen(); w != 390 || h != 844 {
		t.Fatalf("Screen() = %dx%d, want 390x844 (CSS × Scale)", w, h)
	}
}

// Coordinate mapping feeds CDP in CSS pixels, clamped to the viewport.
func TestCSSCoordinateMapping(t *testing.T) {
	f := &feed{opts: Options{Width: 400, Height: 800, Scale: 1}}
	if x, y := f.css(0.5, 0.25); x != 200 || y != 200 {
		t.Fatalf("css(0.5,0.25) = (%v,%v), want (200,200)", x, y)
	}
	if x, y := f.css(-1, 2); x != 0 || y != 800 {
		t.Fatalf("css must clamp to viewport, got (%v,%v)", x, y)
	}
}
