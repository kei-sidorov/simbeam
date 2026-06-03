// Package rtc holds the WebRTC mechanics: one peer connection per session
// serving an H.264 video track and receiving control over a DataChannel. It
// speaks raw SDP strings and knows nothing about idb, the encoder, HTTP, or the
// meaning of control messages — the server package wires those in.
package rtc

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Session is one WebRTC peer connection: H.264 video out, control DataChannel in.
type Session struct {
	pc        *webrtc.PeerConnection
	track     *webrtc.TrackLocalStaticSample
	mu        sync.Mutex // guards onClose (set by caller, read on pion's goroutine)
	onClose   func()
	closeOnce sync.Once
}

// New creates a peer with one H.264 video track and routes inbound "control"
// DataChannel messages to onControl (nil to ignore).
func New(onControl func([]byte)) (*Session, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "simcast")
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
		if dc.Label() != "control" {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if onControl != nil {
				onControl(msg.Data)
			}
		})
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

// OnClose registers a callback fired exactly once when the peer
// fails/disconnects/closes. Safe to call from any goroutine.
func (s *Session) OnClose(fn func()) {
	s.mu.Lock()
	s.onClose = fn
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
