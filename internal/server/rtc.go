package server

import (
	"context"
	"net/http"

	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simcast/internal/rtc"
)

// rtcFPS is the screenshot/encode frame rate for the WebRTC path.
const rtcFPS = 15

// sdpMsg is the signaling envelope exchanged over the local /rtc WebSocket
// (dev mode). Remote mode uses internal/signal instead.
type sdpMsg struct {
	Type string `json:"type"` // "offer" | "answer" | "error"
	SDP  string `json:"sdp,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// startSession builds a session-scoped control plane: a pre-negotiated
// (initially silent) H.264 peer + a bidirectional "control" DataChannel wired
// to a fresh dispatcher. iceServers is nil for local/dev (host candidates) and
// populated from the broker for remote rendezvous. The caller owns ctx and must
// call sess.Close()/d.stopAttachment() on teardown.
func (s *Server) startSession(ctx context.Context, iceServers []webrtc.ICEServer) (*rtc.Session, *rtcDispatch, error) {
	d := &rtcDispatch{comp: s.comp, binary: s.binary, baseCtx: ctx}
	sess, err := rtc.New(d.handle, iceServers)
	if err != nil {
		return nil, nil, err
	}
	d.send = func(b []byte) { _ = sess.Send(b) }
	d.writeFrame = sess.WriteFrame
	return sess, d, nil
}

// handleRTC negotiates one session-scoped WebRTC peer over the local /rtc
// WebSocket (dev mode): a pre-negotiated silent H.264 track plus a control
// DataChannel. No simulator is bound up front — the client drives
// list/boot/attach/detach over the control channel. The JPEG /session path is
// untouched. Remote rendezvous uses DialSignal (remote.go) instead.
func (s *Server) handleRTC(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())

	sess, d, err := s.startSession(ctx, nil) // dev: host candidates only (decision #32)
	if err != nil {
		cancel()
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sess.Close()
	defer d.stopAttachment()
	defer cancel() // cancels baseCtx so the pump/stream drain before sess.Close
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
	if err := conn.WriteJSON(sdpMsg{Type: "answer", SDP: answerSDP}); err != nil {
		return
	}

	// Block until the client disconnects; all control travels over the
	// DataChannel, so any WS read here is just the disconnect signal.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			cancel()
			return
		}
	}
}
