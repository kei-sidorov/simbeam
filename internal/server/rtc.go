package server

import (
	"context"
	"net/http"
	"time"

	"github.com/kei-sidorov/simcast/internal/encoder"
	"github.com/kei-sidorov/simcast/internal/idb"
	"github.com/kei-sidorov/simcast/internal/rtc"
)

// rtcFPS is the screenshot/encode frame rate for the WebRTC path.
const rtcFPS = 15

// sdpMsg is the signaling envelope exchanged over the /rtc WebSocket.
type sdpMsg struct {
	Type string `json:"type"` // "offer" | "answer" | "error"
	SDP  string `json:"sdp,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// handleRTC negotiates one WebRTC session: verify ffmpeg, spawn the sidecar,
// capture screenshots, encode them to H.264 (ffmpeg/h264_videotoolbox), pump
// access units into the track, and route DataChannel control through the shared
// parse/apply path. The JPEG /session path is untouched (fallback).
func (s *Server) handleRTC(w http.ResponseWriter, r *http.Request) {
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

	if err := encoder.Available(); err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sidecar, err := idb.Spawn(ctx, s.binary, udid)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sidecar.Close()
	client := sidecar.Client()

	screen, err := client.Describe(ctx)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}

	sess, err := rtc.New(func(data []byte) {
		m, perr := parseControl(data)
		if perr != nil {
			return
		}
		applyControl(ctx, client, screen, m)
	})
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sess.Close()
	sess.OnClose(cancel)

	var offer sdpMsg
	if err := conn.ReadJSON(&offer); err != nil || offer.Type != "offer" {
		return
	}
	answerSDP, err := sess.Answer(offer.SDP)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}

	png := client.ScreenshotStream(ctx, time.Second/rtcFPS)
	frames, err := encoder.Encode(ctx, png, rtcFPS)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	go func() {
		for f := range frames {
			if err := sess.WriteFrame(f.Data, f.Duration); err != nil {
				cancel()
				return
			}
		}
		cancel() // encoder/stream ended → tear down
	}()

	if err := conn.WriteJSON(sdpMsg{Type: "answer", SDP: answerSDP}); err != nil {
		return
	}

	// Block until the client disconnects or teardown cancels ctx.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			cancel()
			return
		}
	}
}
