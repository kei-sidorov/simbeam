package server

import (
	"context"
	"testing"
	"time"
)

func TestFrameBufferReturnsLatest(t *testing.T) {
	b := newFrameBuffer()
	b.set([]byte("A"))
	b.set([]byte("B")) // must overwrite A
	got, err := b.next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "B" {
		t.Fatalf("got %q, want B (latest only)", got)
	}
}

func TestFrameBufferNextBlocksUntilSet(t *testing.T) {
	b := newFrameBuffer()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := b.next(ctx); err == nil {
		t.Fatal("expected timeout error when no frame set")
	}
}

// TestFrameBufferStaleSignalDoesNotYieldNil injects the race condition where a
// notify signal outlives its frame (frame already consumed, latest == nil).
// next must treat it as a spurious wakeup and keep blocking, never returning a
// nil frame.
func TestFrameBufferStaleSignalDoesNotYieldNil(t *testing.T) {
	b := newFrameBuffer()
	b.notify <- struct{}{} // stale signal with latest == nil
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if f, err := b.next(ctx); err == nil {
		t.Fatalf("expected timeout on stale signal, got frame %q", f)
	}
}
