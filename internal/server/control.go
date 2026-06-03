package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/kei-sidorov/simcast/internal/idb"
)

// controlMsg is an upstream WS message from the client.
type controlMsg struct {
	Type     string  `json:"type"` // "tap" | "home" | "swipe" | "key"
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	X1       float64 `json:"x1"`
	Y1       float64 `json:"y1"`
	X2       float64 `json:"x2"`
	Y2       float64 `json:"y2"`
	Duration float64 `json:"duration"`
	Key      string  `json:"key"`
}

// applyControl dispatches one parsed control message to the idb client, scaling
// touch coordinates against the simulator screen. Shared by the JPEG WS session
// and the WebRTC DataChannel so input handling lives in exactly one place.
func applyControl(ctx context.Context, client *idb.Client, screen idb.Screen, m controlMsg) {
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

func parseControl(data []byte) (controlMsg, error) {
	var m controlMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("bad control json: %w", err)
	}
	switch m.Type {
	case "tap", "home", "swipe", "key":
		return m, nil
	default:
		return m, fmt.Errorf("unknown control type %q", m.Type)
	}
}
