package server

import (
	"encoding/json"
	"fmt"
)

// controlMsg is an inbound message from the client — over the WebRTC "control"
// DataChannel.
type controlMsg struct {
	Type     string  `json:"type"` // tap|home|swipe|key|shake|list|boot|attach|detach|shutdown
	UDID     string  `json:"udid"` // boot, attach, shutdown
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	X1       float64 `json:"x1"`
	Y1       float64 `json:"y1"`
	X2       float64 `json:"x2"`
	Y2       float64 `json:"y2"`
	Duration float64 `json:"duration"`
	Key      string  `json:"key"`
	// QualityOpts carries attach's optional "scale"/"bitrate" (embedded, so they
	// sit at the top level of the message). An old client omits them and gets the
	// backend's defaults, which are today's hardcoded values. Changing quality
	// mid-session goes over "bulk" instead (decision №88).
	QualityOpts
}

// input converts the wire message into the backend-facing Input (the gesture
// fields verbatim; management fields like UDID are handled by rtcDispatch).
func (m controlMsg) input() Input {
	return Input{
		Type: m.Type,
		X:    m.X, Y: m.Y,
		X1: m.X1, Y1: m.Y1, X2: m.X2, Y2: m.Y2,
		Duration: m.Duration,
		Key:      m.Key,
	}
}

func parseControl(data []byte) (controlMsg, error) {
	var m controlMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("bad control json: %w", err)
	}
	switch m.Type {
	case "tap", "home", "swipe", "key", "shake", "list", "boot", "attach", "detach", "shutdown":
		return m, nil
	default:
		return m, fmt.Errorf("unknown control type %q", m.Type)
	}
}
