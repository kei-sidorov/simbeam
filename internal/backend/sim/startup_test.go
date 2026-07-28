package sim

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/server"
)

// fakeControl writes a stand-in for simbeam-control that replays script on
// stderr. The startup contract lives in the reader loop and its select — which
// line settles the attach, which is merely logged, what an exit means — so it
// is worth exercising against a real process rather than the parser alone.
func fakeControl(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "simbeam-control")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("writing fake helper: %v", err)
	}
	return path
}

// A cold boot: the waiting notice comes first, the handshake second. Reading
// stderr line 1 as the handshake would fail here, which is precisely the
// regression protocol 3 introduced.
func TestNewControlWaitsPastTheFramebufferNotice(t *testing.T) {
	bin := fakeControl(t, `
echo '{"waiting":"framebuffer","protocol":3,"timeout_ms":15000}' >&2
echo '{"ready":true,"width":402,"height":874,"scale":3,"protocol":3}' >&2
exec sleep 30
`)
	// exec, not a plain sleep: Close kills the process it spawned, and a shell
	// that forked its sleep would leave that child holding the stderr pipe open
	// — the reader would then block until the sleep finished on its own. The
	// real helper has no children, so this is the script's problem, not the
	// daemon's.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := newControl(ctx, bin, "ABC", server.QualityOpts{Scale: 0.5, Bitrate: 1_000_000})
	if err != nil {
		t.Fatalf("newControl = %v, want the handshake to be found after the waiting line", err)
	}
	defer c.Close()
	if w, h := c.screen(); w != 1206 || h != 2622 {
		t.Fatalf("screen = %dx%d, want the handshake geometry 1206x2622", w, h)
	}
}

// A typed startup failure must surface as a typed error, with the helper's own
// code — that is what lets the client offer "restart this simulator" instead of
// showing prose.
func TestNewControlReturnsTypedStartupFailure(t *testing.T) {
	bin := fakeControl(t, `
echo '{"waiting":"framebuffer","protocol":3,"timeout_ms":15000}' >&2
echo '{"ready":false,"protocol":3,"error":"display_not_ready","message":"the simulator display produced no framebuffer IOSurface within 15000 ms","retryable":true}' >&2
exit 1
`)
	_, err := newControl(context.Background(), bin, "ABC", server.QualityOpts{Scale: 0.5, Bitrate: 1_000_000})

	var typed *server.AttachError
	if !errors.As(err, &typed) {
		t.Fatalf("newControl = %v (%T), want a typed *server.AttachError", err, err)
	}
	if typed.Code != "display_not_ready" || !typed.Retryable {
		t.Fatalf("typed failure = %+v, want display_not_ready/retryable", typed)
	}
}

// Cancellation mid-startup is not a failure. Protocol 3 stops promptly with a
// plain-text note and exit 0, and the daemon must report the cancellation:
// whoever superseded this attach (a detach, a newer attach, a restart) owns the
// client's reply, and a spurious error here would contradict it.
func TestNewControlCancelledStartupIsNotAFailure(t *testing.T) {
	bin := fakeControl(t, `
echo '{"waiting":"framebuffer","protocol":3,"timeout_ms":15000}' >&2
trap 'echo "startup cancelled before the simulator framebuffer was available" >&2; exit 0' TERM
sleep 30
`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := newControl(ctx, bin, "ABC", server.QualityOpts{Scale: 0.5, Bitrate: 1_000_000})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("newControl = %v, want context.Canceled", err)
	}
	var typed *server.AttachError
	if errors.As(err, &typed) {
		t.Fatalf("cancellation must not be reported as a startup failure, got %+v", typed)
	}
}

// A helper that dies without an envelope (a crash, an unparsable build) still
// has to produce an error rather than hang until the handshake deadline.
func TestNewControlExitWithoutEnvelopeStillFails(t *testing.T) {
	bin := fakeControl(t, `
echo 'dyld: symbol not found' >&2
exit 1
`)
	_, err := newControl(context.Background(), bin, "ABC", server.QualityOpts{Scale: 0.5, Bitrate: 1_000_000})
	if err == nil {
		t.Fatal("want an error when the helper exits before handshaking")
	}
	var typed *server.AttachError
	if errors.As(err, &typed) {
		t.Fatalf("an exit with no envelope is untyped, got %+v", typed)
	}
}
