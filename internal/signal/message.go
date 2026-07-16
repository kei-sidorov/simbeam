// Package signal holds the simbeam signaling wire types and the crypto
// primitives shared by the signaling broker (cmd/simbeam-signal) and the
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

// 3C handshake additions: a mutual challenge-response runs before offer/answer.
const (
	TypeConnect   = "connect"   // broker → daemon: a client wants in (carries client pubkey + optional enrollment proof)
	TypeChallenge = "challenge" // daemon → broker → client: nonce to sign; broker adds BrokerNonce for its own gate
	TypeProof     = "proof"     // client → broker → daemon: Sig over daemon nonce (+ BrokerSig over broker nonce, stripped by broker)
)

// Live presence: a lightweight channel, independent of join, that streams
// daemon online/offline to subscribed clients.
const (
	TypeWatch    = "watch"    // client → broker (first message): observe a list of daemonIDs
	TypePresence = "presence" // broker → client: snapshot or one-key delta of online state
)

// Roles carried by Msg.Role on register/join.
const (
	RoleDaemon = "daemon"
	RoleClient = "client"
)

// Error codes carried by Msg.Code alongside the human text in Msg.Msg, so a
// client can branch on a stable machine value instead of grepping the error
// text (which is fragile — see BLIND-SPOTS #4). Msg stays human-readable.
const (
	CodeOffline     = "offline"      // join: the target daemon is not registered
	CodePairExpired = "pair_expired" // connect: the enrollment window expired or was cancelled
	CodePairUsed    = "pair_used"    // connect: the enrollment window was already consumed
	CodePairInvalid = "pair_invalid" // connect: no window armed, or the pairing secret did not match
)

// Msg is the single JSON envelope for every signaling message in both
// directions; unused fields stay zero. Non-trickle ICE: all candidates ride
// inside SDP, so there is no separate candidate message.
type Msg struct {
	Type        string          `json:"type"`
	Room        string          `json:"room,omitempty"`        // register/join: the pairing token
	Role        string          `json:"role,omitempty"`        // register/join: daemon|client
	SDP         string          `json:"sdp,omitempty"`         // offer/answer
	PubKey      string          `json:"pubkey,omitempty"`      // register: daemon Ed25519 pubkey (base64); join/connect: client Ed25519 pubkey (base64)
	Sig         string          `json:"sig,omitempty"`         // answer: daemon Ed25519 signature of SDP (base64); proof: client signature over daemon Nonce (base64)
	ICEServers  []ICEServer     `json:"iceServers,omitempty"`  // broker → peer
	Msg         string          `json:"msg,omitempty"`         // error text
	Code        string          `json:"code,omitempty"`        // error: machine-readable code (see Code* constants)
	Daemon      string          `json:"daemon,omitempty"`      // register/join: daemonID (= daemon Ed25519 pubkey, base64)
	Nonce       string          `json:"nonce,omitempty"`       // join: client nonce binding the enroll proof; challenge: daemon nonce to sign
	BrokerNonce string          `json:"brokerNonce,omitempty"` // challenge: broker nonce the client signs so the broker can gate TURN
	Pair        string          `json:"pair,omitempty"`        // join: HMAC-SHA256(S, clientPubKey‖0x00‖nonce) enrollment proof
	BrokerSig   string          `json:"brokerSig,omitempty"`   // proof: client Ed25519 signature over BrokerNonce (verified+stripped by broker)
	Daemons     []string        `json:"daemons,omitempty"`     // watch: daemonIDs to observe
	States      map[string]bool `json:"states,omitempty"`      // presence: daemonID → online (snapshot or delta)
}

// ICEServer is the subset of the WebRTC RTCIceServer JSON shape we transmit.
// The browser consumes it directly as an RTCIceServer; the daemon converts it
// to webrtc.ICEServer (in internal/server, to keep this package webrtc-free).
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}
