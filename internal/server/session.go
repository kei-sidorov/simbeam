package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kei-sidorov/simcast/internal/idb"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // debug client; same machine
}

// screenshotInterval is the polling period for the frame source (~10 fps).
const screenshotInterval = 100 * time.Millisecond

// handleSession upgrades to WS, spawns an idb_companion sidecar for ?udid=,
// streams JPEG/PNG frames down (binary) and applies taps/home from up (JSON).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	udid := r.URL.Query().Get("udid")
	if udid == "" {
		http.Error(w, "missing udid", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sidecar, err := idb.Spawn(ctx, s.binary, udid)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "msg": err.Error()})
		return
	}
	defer sidecar.Close()
	client := sidecar.Client()

	screen, err := client.Describe(ctx)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "msg": err.Error()})
		return
	}
	_ = conn.WriteJSON(map[string]any{"type": "ready", "w": screen.Width, "h": screen.Height})

	frames := client.ScreenshotStream(ctx, screenshotInterval)

	// Producer: gRPC frames → single-slot buffer (drop stale).
	buf := newFrameBuffer()
	go func() {
		for f := range frames {
			buf.set(f)
		}
		cancel() // stream ended → tear down session
	}()

	// Writer: buffer → WS binary.
	go func() {
		for {
			f, err := buf.next(ctx)
			if err != nil {
				return
			}
			// Bound the write so a stalled client can't block the writer
			// (and keep the sidecar alive) forever — matters once --addr is
			// bound to a network interface, not just localhost.
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, f); err != nil {
				cancel()
				return
			}
		}
	}()

	// Reader (this goroutine): WS control → hid. Returns on disconnect.
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			cancel()
			return
		}
		m, err := parseControl(data)
		if err != nil {
			continue // ignore malformed/unknown
		}
		switch m.Type {
		case "tap":
			if err := client.Tap(ctx, idb.ScaleTap(m.X, m.Y, screen)); err != nil {
				log.Printf("tap: %v", err)
			}
		case "home":
			if err := client.Home(ctx); err != nil {
				log.Printf("home: %v", err)
			}
		case "swipe":
			dur := m.Duration
			if dur <= 0 {
				dur = 0.3
			}
			start := idb.ScaleTap(m.X1, m.Y1, screen)
			end := idb.ScaleTap(m.X2, m.Y2, screen)
			if err := client.Swipe(ctx, start, end, dur); err != nil {
				log.Printf("swipe: %v", err)
			}
		case "key":
			if usage, shift, ok := keyUsage(m.Key); ok {
				if err := client.KeyPress(ctx, usage, shift); err != nil {
					log.Printf("key: %v", err)
				}
			}
		}
	}
}
