package signal

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMsg_NewFieldsRoundTrip(t *testing.T) {
	in := Msg{
		Type:        TypeChallenge,
		Daemon:      "DAEMONID==",
		PubKey:      "CLIENTPUB==",
		Nonce:       "n1",
		BrokerNonce: "bn1",
		Pair:        "proofB64",
		Sig:         "sigB64",
		BrokerSig:   "bsigB64",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Msg
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
	// New message-type constants exist and are distinct.
	if TypeConnect == TypeChallenge || TypeChallenge == TypeProof {
		t.Fatalf("handshake type constants collide")
	}
}
