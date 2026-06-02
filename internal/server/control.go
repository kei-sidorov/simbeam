package server

import (
	"encoding/json"
	"fmt"
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
