// Package rtc holds the WebRTC mechanics: one peer connection per session
// serving an H.264 video track and receiving control over a DataChannel. It
// speaks raw SDP strings and knows nothing about idb, the encoder, HTTP, or the
// meaning of control messages — the server package wires those in.
package rtc

import (
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// ErrNoControlChannel is returned by Send before the remote peer has opened
// the "control" DataChannel.
var ErrNoControlChannel = errors.New("rtc: control channel not open")

// ErrNoBulkChannel is returned by SendBulk/SendBulkText before the remote peer
// has opened the "bulk" DataChannel.
var ErrNoBulkChannel = errors.New("rtc: bulk channel not open")

// Session is one WebRTC peer connection: H.264 video out, plus two inbound
// DataChannels — "control" (lossy, tap/swipe/management) and "bulk" (reliable
// ordered, full-resolution screenshots).
type Session struct {
	pc         *webrtc.PeerConnection
	track      *webrtc.TrackLocalStaticSample
	mu         sync.Mutex // guards onClose, onCtrlOpen, ctrl and bulk
	onClose    func()
	onCtrlOpen func()
	ctrl       *webrtc.DataChannel
	bulk       *webrtc.DataChannel
	closeOnce  sync.Once
}

// New creates a peer with one H.264 video track and routes inbound DataChannel
// messages by label: "control" → onControl, "bulk" → onBulk (either nil to
// ignore). iceServers configures ICE gathering: nil/empty yields host
// candidates only (localhost dev); STUN/TURN entries enable srflx/relay for
// remote rendezvous.
func New(onControl, onBulk func([]byte), iceServers []webrtc.ICEServer) (*Session, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "simbeam")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err := pc.AddTrack(track); err != nil {
		_ = pc.Close()
		return nil, err
	}
	s := &Session{pc: pc, track: track}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "control":
			s.mu.Lock()
			s.ctrl = dc
			s.mu.Unlock()
			dc.OnOpen(func() {
				s.mu.Lock()
				fn := s.onCtrlOpen
				s.mu.Unlock()
				if fn != nil {
					fn()
				}
			})
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				if onControl != nil {
					onControl(msg.Data)
				}
			})
		case "bulk":
			s.mu.Lock()
			s.bulk = dc
			s.mu.Unlock()
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				if onBulk != nil {
					onBulk(msg.Data)
				}
			})
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.fireClose()
		}
	})
	return s, nil
}

// Answer consumes a remote offer SDP and returns the local answer SDP, blocking
// until ICE gathering completes (non-trickle; instant on localhost).
func (s *Session) Answer(offerSDP string) (string, error) {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return "", err
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	done := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-done
	return s.pc.LocalDescription().SDP, nil
}

// WriteFrame writes one H.264 access unit to the video track.
func (s *Session) WriteFrame(data []byte, dur time.Duration) error {
	return s.track.WriteSample(media.Sample{Data: data, Duration: dur})
}

// Send delivers a control message to the remote peer over the "control"
// DataChannel. Returns ErrNoControlChannel if the peer has not opened it yet.
func (s *Session) Send(b []byte) error {
	s.mu.Lock()
	dc := s.ctrl
	s.mu.Unlock()
	if dc == nil {
		return ErrNoControlChannel
	}
	// SendText (not Send): the browser client parses dc.onmessage via
	// JSON.parse(ev.data), which requires a text frame; a binary frame would
	// arrive as a Blob/ArrayBuffer and fail to parse.
	return dc.SendText(string(b))
}

// SendBulk delivers one binary frame — a chunk of the full-resolution
// screenshot — over the reliable ordered "bulk" DataChannel. One frame must
// stay within the peer's negotiated max message size (see MaxMessageSize);
// SCTP rejects anything larger outright, which is why the image is chunked
// rather than sent whole. Returns ErrNoBulkChannel if the peer has not opened
// the channel yet.
func (s *Session) SendBulk(b []byte) error {
	s.mu.Lock()
	dc := s.bulk
	s.mu.Unlock()
	if dc == nil {
		return ErrNoBulkChannel
	}
	return dc.Send(b)
}

// MaxMessageSize reports the largest single message the remote peer has agreed
// to accept, negotiated over SCTP from its SDP "a=max-message-size". This is a
// hard cap, not a hint: pion rejects any Send whose payload exceeds it outright
// (pion/sctp compares len(payload) directly, so there is no framing overhead to
// subtract). A full-resolution screenshot is megabytes, so bulk senders must
// chunk under this number. Returns 0 before the SCTP association is up.
func (s *Session) MaxMessageSize() int {
	sctp := s.pc.SCTP()
	if sctp == nil {
		return 0
	}
	return int(sctp.GetCapabilities().MaxMessageSize)
}

// SendBulkText delivers a text frame over "bulk" — either the transfer header
// announcing an image's byte count, or the JSON error envelope. The client
// tells frames apart by the binary/text flag: text → header or error (by its
// "type"), binary → image chunk. Returns ErrNoBulkChannel if the peer has not
// opened the channel yet.
func (s *Session) SendBulkText(b string) error {
	s.mu.Lock()
	dc := s.bulk
	s.mu.Unlock()
	if dc == nil {
		return ErrNoBulkChannel
	}
	return dc.SendText(b)
}

// OnClose registers a callback fired exactly once when the peer
// fails/disconnects/closes. Safe to call from any goroutine.
func (s *Session) OnClose(fn func()) {
	s.mu.Lock()
	s.onClose = fn
	s.mu.Unlock()
}

// OnControlOpen registers a callback fired when the remote opens the "control"
// DataChannel — the first moment the daemon can push an unsolicited message
// (e.g. the hello carrying host info). Safe to call from any goroutine; set it
// before the peer connects.
func (s *Session) OnControlOpen(fn func()) {
	s.mu.Lock()
	s.onCtrlOpen = fn
	s.mu.Unlock()
}

func (s *Session) fireClose() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		fn := s.onClose
		s.mu.Unlock()
		if fn != nil {
			fn()
		}
	})
}

// Close tears down the peer connection.
func (s *Session) Close() error { return s.pc.Close() }
