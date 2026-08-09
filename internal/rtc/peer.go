// Package rtc holds the WebRTC mechanics: one peer connection per session
// serving an H.264 video track and receiving control over a DataChannel. It
// speaks raw SDP strings and knows nothing about idb, the encoder, HTTP, or the
// meaning of control messages — the server package wires those in.
package rtc

import (
	"errors"
	"log"
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
	mu         sync.Mutex // guards onClose, onCtrlOpen, onCand, ctrl, bulk, remoteSet and pending
	onClose    func()
	onCtrlOpen func()
	onCand     func(*webrtc.ICECandidate)
	ctrl       *webrtc.DataChannel
	bulk       *webrtc.DataChannel
	remoteSet  bool                      // the remote description has been applied
	pending    []webrtc.ICECandidateInit // trickled candidates that arrived before it
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
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		s.mu.Lock()
		fn := s.onCand
		s.mu.Unlock()
		if fn != nil {
			fn(c) // nil c = gathering complete
		}
	})
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
		case webrtc.PeerConnectionStateConnected:
			if pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
				localType, remoteType := "?", "?"
				if pair.Local != nil {
					localType = pair.Local.Typ.String()
				}
				if pair.Remote != nil {
					remoteType = pair.Remote.Typ.String()
				}
				log.Printf("rtc: connected, selected candidate pair local=%s remote=%s (%s)", localType, remoteType, pair)
			} else {
				log.Printf("rtc: connected, no selected candidate pair (err=%v)", err)
			}
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.fireClose()
		}
	})
	return s, nil
}

// Answer consumes a remote offer SDP and returns the local answer SDP. Without
// OnCandidate it blocks until ICE gathering completes, so the answer carries
// every candidate (non-trickle; instant on localhost). With OnCandidate set it
// returns as soon as the local description exists and the candidates follow
// through that callback.
func (s *Session) Answer(offerSDP string) (string, error) {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.remoteSet = true
	pending, trickle := s.pending, s.onCand != nil
	s.pending = nil
	s.mu.Unlock()
	for _, c := range pending {
		if err := s.pc.AddICECandidate(c); err != nil {
			log.Printf("rtc: buffered candidate rejected: %v", err)
		}
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if trickle {
		// No GatheringCompletePromise: don't wait, and don't hand pion a second
		// gather-complete hook when OnCandidate already reports the end.
		if err := s.pc.SetLocalDescription(answer); err != nil {
			return "", err
		}
		return s.pc.LocalDescription().SDP, nil
	}
	done := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-done
	return s.pc.LocalDescription().SDP, nil
}

// OnCandidate registers a sink for locally gathered ICE candidates, which also
// switches Answer to trickle mode. fn is called with nil once gathering
// completes. Must be set before Answer; safe to call from any goroutine.
func (s *Session) OnCandidate(fn func(*webrtc.ICECandidate)) {
	s.mu.Lock()
	s.onCand = fn
	s.mu.Unlock()
}

// AddCandidate applies one trickled remote candidate (empty Candidate =
// end-of-candidates). Candidates that arrive before the offer is applied are
// buffered, because pion rejects them until the remote description is set.
func (s *Session) AddCandidate(c webrtc.ICECandidateInit) error {
	s.mu.Lock()
	if !s.remoteSet {
		s.pending = append(s.pending, c)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	return s.pc.AddICECandidate(c)
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

// SendBulk delivers one binary frame — a chunk of a screenshot or the simulator
// list — over the reliable ordered "bulk" DataChannel. The caller keeps each
// frame tiny (server/bulk.go caps it at one ~1 KB SCTP packet so it does not
// black-hole on an IPv6 path); that is well under the peer's negotiated max
// message size (see MaxMessageSize), which is a separate, far higher hard cap
// SCTP would reject a Send for exceeding. Returns ErrNoBulkChannel if the peer
// has not opened the channel yet.
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
