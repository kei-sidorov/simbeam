package server

import (
	"context"

	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simbeam/internal/rtc"
)

// startSession builds a session-scoped control plane: a pre-negotiated
// (initially silent) H.264 peer + a bidirectional "control" DataChannel wired
// to a fresh dispatcher. iceServers comes from the broker for remote rendezvous.
// The caller owns ctx and must call sess.Close()/d.stopAttachment() on teardown.
func (s *Server) startSession(ctx context.Context, iceServers []webrtc.ICEServer) (*rtc.Session, *rtcDispatch, error) {
	d := &rtcDispatch{backend: s.backend, baseCtx: ctx, hostName: s.hostName, osVersion: s.osVersion}
	sess, err := rtc.New(d.handle, d.handleBulk, iceServers)
	if err != nil {
		return nil, nil, err
	}
	d.send = func(b []byte) { _ = sess.Send(b) }
	// Bulk sends surface their error: a dropped reply is indistinguishable from a
	// wedged daemon at the client, which would just wait out its failsafe. The
	// dispatcher turns a failed send into a text error the client can act on.
	d.sendBulk = sess.SendBulk
	d.sendBulkText = sess.SendBulkText
	// Read per transfer, not once: the cap is only known after the SCTP
	// association is up, and it decides how the image is chunked.
	d.bulkMaxMsg = sess.MaxMessageSize
	d.writeFrame = sess.WriteFrame
	// Push the hello (host info) the moment the client opens the control channel.
	sess.OnControlOpen(d.sendHello)
	return sess, d, nil
}
