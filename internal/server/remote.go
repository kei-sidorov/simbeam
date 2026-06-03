package server

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/kei-sidorov/simcast/internal/signal"
)

// toWebRTC converts broker iceServers to pion's type (kept here so
// internal/signal stays webrtc-free, preserving the decision #30 boundary).
func toWebRTC(in []signal.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return out
}

// signedAnswer wraps an answer SDP into a signaling Msg whose Sig authenticates
// the SDP under the daemon's session key (anti-MITM: the browser verifies it
// against the pubkey carried in the pairing URL).
func signedAnswer(sdp string, priv ed25519.PrivateKey) signal.Msg {
	return signal.Msg{Type: signal.TypeAnswer, SDP: sdp, Sig: signal.Sign(priv, []byte(sdp))}
}

// DialSignal connects to the signaling broker, registers a room under token,
// and serves exactly one client: it relays the client's offer into a fresh
// session-scoped control-plane peer (reusing startSession) and sends back a
// signed answer. The signaling socket is handshake-only — once the answer is
// sent it closes, and the live P2P peer carries everything else (decision #50;
// disconnect detection stays on pion via sess.OnClose). Blocks until the peer
// closes or ctx is cancelled.
func (s *Server) DialSignal(ctx context.Context, signalURL, token, pubKeyB64 string, priv ed25519.PrivateKey) error {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, signalURL, nil)
	if err != nil {
		return fmt.Errorf("dial signaling: %w", err)
	}
	defer ws.Close()

	if err := ws.WriteJSON(signal.Msg{
		Type: signal.TypeRegister, Room: token, Role: signal.RoleDaemon, PubKey: pubKeyB64,
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Wait for the offer; capture iceServers if they arrive first so the daemon
	// side gathers with matching servers.
	var iceServers []webrtc.ICEServer
	var offerSDP string
	for offerSDP == "" {
		var m signal.Msg
		if err := ws.ReadJSON(&m); err != nil {
			return fmt.Errorf("read signaling: %w", err)
		}
		switch m.Type {
		case signal.TypeICEServers:
			iceServers = toWebRTC(m.ICEServers)
		case signal.TypeOffer:
			offerSDP = m.SDP
		case signal.TypeError:
			return fmt.Errorf("signaling: %s", m.Msg)
		case signal.TypePeerLeft:
			return fmt.Errorf("signaling: client left before offer")
		}
	}

	sessCtx, cancel := context.WithCancel(ctx)
	sess, d, err := s.startSession(sessCtx, iceServers)
	if err != nil {
		cancel()
		_ = ws.WriteJSON(signal.Msg{Type: signal.TypeError, Msg: err.Error()})
		return err
	}
	defer sess.Close()
	defer d.stopAttachment()
	defer cancel()
	sess.OnClose(cancel)

	answerSDP, err := sess.Answer(offerSDP)
	if err != nil {
		_ = ws.WriteJSON(signal.Msg{Type: signal.TypeError, Msg: err.Error()})
		return err
	}
	if err := ws.WriteJSON(signedAnswer(answerSDP, priv)); err != nil {
		return fmt.Errorf("send answer: %w", err)
	}

	// Handshake complete. Close the signaling socket and hold the live peer
	// until it drops or ctx is cancelled.
	_ = ws.Close()
	<-sessCtx.Done()
	return nil
}
