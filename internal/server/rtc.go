package server

import (
	"context"

	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simcast/internal/rtc"
)

// rtcFPS is the screenshot/encode frame rate for the WebRTC path.
const rtcFPS = 15

// startSession builds a session-scoped control plane: a pre-negotiated
// (initially silent) H.264 peer + a bidirectional "control" DataChannel wired
// to a fresh dispatcher. iceServers comes from the broker for remote rendezvous.
// The caller owns ctx and must call sess.Close()/d.stopAttachment() on teardown.
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
