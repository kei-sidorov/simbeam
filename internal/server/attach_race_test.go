package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kei-sidorov/simbeam/internal/companion"
	"github.com/kei-sidorov/simbeam/internal/encoder"
)

// liveFeed is a Feed that actually streams, unlike stubFeed's nil channel: it
// emits frames until its context is cancelled. That distinction is the whole
// point here — an orphaned attachment is invisible to a feed that never
// produces, and it is exactly what the pump's teardown relies on.
type liveFeed struct {
	name   string
	ch     chan encoder.Frame
	closed atomic.Bool
}

func newLiveFeed(ctx context.Context, name string) *liveFeed {
	f := &liveFeed{name: name, ch: make(chan encoder.Frame)}
	go func() {
		defer close(f.ch)
		for {
			select {
			case <-ctx.Done():
				return
			case f.ch <- encoder.Frame{Data: []byte(name), Duration: time.Millisecond}:
			}
		}
	}()
	return f
}

func (f *liveFeed) Screen() (uint64, uint64)                   { return 100, 200 }
func (f *liveFeed) Frames() <-chan encoder.Frame               { return f.ch }
func (f *liveFeed) Input(context.Context, Input)               {}
func (f *liveFeed) Screenshot(context.Context) ([]byte, error) { return nil, nil }
func (f *liveFeed) Close() error                               { f.closed.Store(true); return nil }

// slowBackend models the real cost of Attach — an idb sidecar spawn measures
// ~1.2s — so two attach intents overlap the way they do in production rather
// than by a lucky scheduler interleaving.
type slowBackend struct {
	mu    sync.Mutex
	feeds []*liveFeed
}

func (s *slowBackend) DefaultScale() float64                               { return 0.5 }
func (s *slowBackend) List(context.Context) ([]companion.Simulator, error) { return nil, nil }
func (s *slowBackend) Boot(context.Context, string) error                  { return nil }
func (s *slowBackend) Shutdown(context.Context, string) error              { return nil }
func (s *slowBackend) Shake(context.Context, string) error                 { return nil }

func (s *slowBackend) Attach(ctx context.Context, udid string, _ QualityOpts) (Feed, error) {
	time.Sleep(120 * time.Millisecond)
	f := newLiveFeed(ctx, udid)
	s.mu.Lock()
	s.feeds = append(s.feeds, f)
	s.mu.Unlock()
	return f, nil
}

// openFeeds names the feeds that are still live — each one holds a real sidecar.
func (s *slowBackend) openFeeds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var open []string
	for _, f := range s.feeds {
		if !f.closed.Load() {
			open = append(open, f.name)
		}
	}
	return open
}

// raceDispatch wires a dispatch that records which feed's bytes reach the video
// track, since a track fed by two encoders at once decodes to garbage.
func raceDispatch(be Backend) (*rtcDispatch, *sync.Map) {
	var writers sync.Map
	return &rtcDispatch{
		backend:      be,
		baseCtx:      context.Background(),
		send:         func([]byte) {},
		sendBulkText: func(string) error { return nil },
		writeFrame: func(b []byte, _ time.Duration) error {
			writers.Store(string(b), true)
			return nil
		},
	}, &writers
}

func names(m *sync.Map) []string {
	var out []string
	m.Range(func(k, _ any) bool { out = append(out, k.(string)); return true })
	return out
}

// A quality change racing a device switch: the user retunes sim A, then picks
// sim B before the rebuild lands. Only B may survive — the superseded attach of
// A must drop its own feed, because nothing else can. Its pump waits on Frames()
// and Frames() closes only on the ctx cancel that an orphan never receives.
func TestAttachSupersededByConcurrentAttachDropsItsFeed(t *testing.T) {
	be := &slowBackend{}
	d, writers := raceDispatch(be)

	d.doAttach("A", QualityOpts{})
	d.handleBulk([]byte(`{"type":"quality","scale":0.25}`)) // spawns a re-attach of A
	d.handle([]byte(`{"type":"attach","udid":"B"}`))        // user switches, concurrently

	time.Sleep(500 * time.Millisecond)

	if open := be.openFeeds(); len(open) != 1 || open[0] != "B" {
		t.Fatalf("open feeds = %v, want exactly [B]: a leaked feed holds an idb_companion sidecar forever", open)
	}
	d.mu.Lock()
	udid := d.att.udid
	d.mu.Unlock()
	if udid != "B" {
		t.Fatalf("attached udid = %q, want B — the client's last pick must win", udid)
	}

	// A legitimately streamed before the switch, so its bytes are expected in the
	// history. What must not happen is A still writing NOW — that is the orphan.
	writers.Range(func(k, _ any) bool { writers.Delete(k); return true })
	time.Sleep(150 * time.Millisecond)
	if w := names(writers); len(w) != 1 || w[0] != "B" {
		t.Fatalf("feeds writing to the track = %v, want only [B]: two encoders on one track decode to garbage", w)
	}
}

// Detaching during a quality rebuild must stay detached. The client has already
// torn its UI down on the "detached" reply; a feed resurrected behind its back
// streams to nobody and burns a sidecar.
func TestDetachDuringQualityRebuildStaysDetached(t *testing.T) {
	be := &slowBackend{}
	d, _ := raceDispatch(be)

	d.doAttach("A", QualityOpts{})
	d.handleBulk([]byte(`{"type":"quality","scale":0.25}`))
	d.handle([]byte(`{"type":"detach"}`))

	time.Sleep(500 * time.Millisecond)

	if open := be.openFeeds(); len(open) != 0 {
		t.Fatalf("open feeds = %v, want none after detach", open)
	}
	d.mu.Lock()
	att := d.att
	d.mu.Unlock()
	if att != nil {
		t.Fatalf("attachment = %+v, want nil: detach must win over an in-flight rebuild", att)
	}
}

// Shutting down the streaming sim during a quality rebuild must not respawn a
// sidecar against a simulator that is powering off.
func TestShutdownDuringQualityRebuildStaysDetached(t *testing.T) {
	be := &slowBackend{}
	d, _ := raceDispatch(be)

	d.doAttach("A", QualityOpts{})
	d.handleBulk([]byte(`{"type":"quality","scale":0.25}`))
	d.handle([]byte(`{"type":"shutdown","udid":"A"}`))

	time.Sleep(500 * time.Millisecond)

	if open := be.openFeeds(); len(open) != 0 {
		t.Fatalf("open feeds = %v, want none after shutdown", open)
	}
}

// Two quality changes in a row (a dragged slider) must converge on one feed.
func TestBackToBackQualityChangesLeaveOneFeed(t *testing.T) {
	be := &slowBackend{}
	d, _ := raceDispatch(be)

	d.doAttach("A", QualityOpts{})
	d.handleBulk([]byte(`{"type":"quality","scale":0.25}`))
	d.handleBulk([]byte(`{"type":"quality","scale":0.75}`))
	d.handleBulk([]byte(`{"type":"quality","scale":1.0}`))

	time.Sleep(600 * time.Millisecond)

	if open := be.openFeeds(); len(open) != 1 {
		t.Fatalf("open feeds = %v, want exactly one to survive", open)
	}
}
