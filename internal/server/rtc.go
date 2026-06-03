package server

import (
	"context"
	"net/http"

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

// handleRTC negotiates one session-scoped WebRTC peer: a pre-negotiated
// (initially silent) H.264 video track plus a bidirectional "control"
// DataChannel. No simulator is bound up front — the client drives
// list/boot/attach/detach over the control channel, and the daemon starts the
// video pump on attach. The JPEG /session path is untouched (fallback).
func (s *Server) handleRTC(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())

	d := &rtcDispatch{comp: s.comp, binary: s.binary, baseCtx: ctx}

	sess, err := rtc.New(d.handle)
	if err != nil {
		cancel()
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sess.Close()
	defer d.stopAttachment()
	defer cancel() // fires first on teardown: cancels baseCtx so the pump/stream drain before sess.Close
	sess.OnClose(cancel)

	// Wire the peer into the dispatcher before negotiation completes; control
	// messages can only arrive after ICE connects (post-answer).
	d.send = func(b []byte) { _ = sess.Send(b) }
	d.writeFrame = sess.WriteFrame

	var offer sdpMsg
	if err := conn.ReadJSON(&offer); err != nil || offer.Type != "offer" {
		return
	}
	answerSDP, err := sess.Answer(offer.SDP)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	if err := conn.WriteJSON(sdpMsg{Type: "answer", SDP: answerSDP}); err != nil {
		return
	}

	// Block until the client disconnects. All control travels over the
	// DataChannel; any WS message here is unexpected, and a read error means
	// the client is gone.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			cancel()
			return
		}
	}
}
