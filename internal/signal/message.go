// Package signal holds the simcast signaling wire types and the crypto
// primitives shared by the signaling broker (cmd/simcast-signal) and the
// daemon (internal/server). It imports neither webrtc nor the broker so both
// sides depend on the same definitions without a dependency cycle.
package signal

// Message types carried by Msg.Type.
const (
	TypeRegister   = "register"   // daemon → broker: claim a room by token
	TypeJoin       = "join"       // client → broker: enter a room by token
	TypeICEServers = "iceServers" // broker → peer: ICE configuration
	TypeOffer      = "offer"      // client → broker → daemon
	TypeAnswer     = "answer"     // daemon → broker → client (carries Sig)
	TypePeerLeft   = "peerLeft"   // broker → peer: the other side dropped
	TypeError      = "error"      // broker/peer → peer: fatal, text in Msg
)

// Roles carried by Msg.Role on register/join.
const (
	RoleDaemon = "daemon"
	RoleClient = "client"
)

// Msg is the single JSON envelope for every signaling message in both
// directions; unused fields stay zero. Non-trickle ICE: all candidates ride
// inside SDP, so there is no separate candidate message.
type Msg struct {
	Type       string      `json:"type"`
	Room       string      `json:"room,omitempty"`       // register/join: the pairing token
	Role       string      `json:"role,omitempty"`       // register/join: daemon|client
	SDP        string      `json:"sdp,omitempty"`        // offer/answer
	PubKey     string      `json:"pubkey,omitempty"`     // register: daemon Ed25519 public key (base64)
	Sig        string      `json:"sig,omitempty"`        // answer: Ed25519 signature of SDP (base64)
	ICEServers []ICEServer `json:"iceServers,omitempty"` // broker → peer
	Msg        string      `json:"msg,omitempty"`        // error text
}

// ICEServer is the subset of the WebRTC RTCIceServer JSON shape we transmit.
// The browser consumes it directly as an RTCIceServer; the daemon converts it
// to webrtc.ICEServer (in internal/server, to keep this package webrtc-free).
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}
