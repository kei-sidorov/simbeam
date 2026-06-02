package server

import (
	"context"
	"sync"
)

// frameBuffer holds only the most recent frame. set() overwrites any
// un-consumed frame, so a slow WebSocket writer never accumulates lag —
// stale frames are dropped, latency stays low.
type frameBuffer struct {
	mu     sync.Mutex
	latest []byte
	notify chan struct{}
}

func newFrameBuffer() *frameBuffer {
	return &frameBuffer{notify: make(chan struct{}, 1)}
}

func (b *frameBuffer) set(frame []byte) {
	b.mu.Lock()
	b.latest = frame
	b.mu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default: // signal already pending
	}
}

// next blocks until a frame is available or ctx is done. It never returns a
// nil frame: a notify signal can outlive its frame (set drains into latest,
// a consumer reads and clears latest, leaving a stale pending signal), so a
// nil read is treated as a spurious wakeup and next keeps waiting.
func (b *frameBuffer) next(ctx context.Context) ([]byte, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-b.notify:
			b.mu.Lock()
			f := b.latest
			b.latest = nil
			b.mu.Unlock()
			if f == nil {
				continue // stale signal, frame already consumed
			}
			return f, nil
		}
	}
}
