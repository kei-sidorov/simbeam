package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kei-sidorov/simcast/internal/companion"
)

type stubComp struct {
	sims        []companion.Simulator
	listErr     error
	booted      []string
	bootErr     error
	shutdown    []string
	shutdownErr error
}

func (c *stubComp) List(context.Context) ([]companion.Simulator, error) {
	return c.sims, c.listErr
}
func (c *stubComp) Boot(_ context.Context, udid string) error {
	c.booted = append(c.booted, udid)
	return c.bootErr
}
func (c *stubComp) Shutdown(_ context.Context, udid string) error {
	c.shutdown = append(c.shutdown, udid)
	return c.shutdownErr
}

// newTestDispatch returns a dispatcher whose replies are captured into *out.
func newTestDispatch(comp Companion, out *[]ctrlReply) *rtcDispatch {
	return &rtcDispatch{
		comp:    comp,
		baseCtx: context.Background(),
		send: func(b []byte) {
			var r ctrlReply
			_ = json.Unmarshal(b, &r)
			*out = append(*out, r)
		},
	}
}

func TestDoListSendsSims(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{sims: []companion.Simulator{
		{UDID: "A", Name: "iPhone", State: "Booted"},
		{UDID: "B", Name: "iPad", State: "Shutdown"},
	}}, &out)
	d.handle([]byte(`{"type":"list"}`))
	if len(out) != 1 || out[0].Type != "sims" || len(out[0].Sims) != 2 {
		t.Fatalf("want one sims reply with 2 sims, got %+v", out)
	}
}

func TestDoListErrorReply(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{listErr: errors.New("boom")}, &out)
	d.handle([]byte(`{"type":"list"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want one error reply, got %+v", out)
	}
}

func TestDoBootMissingUDID(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"boot"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want error reply for missing udid, got %+v", out)
	}
	if len(c.booted) != 0 {
		t.Fatalf("Boot should not be called, got %v", c.booted)
	}
}

func TestDoBootSuccess(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"boot","udid":"ABC"}`))
	if len(out) != 1 || out[0].Type != "booted" || out[0].UDID != "ABC" {
		t.Fatalf("want booted reply for ABC, got %+v", out)
	}
	if len(c.booted) != 1 || c.booted[0] != "ABC" {
		t.Fatalf("Boot(ABC) not called, got %v", c.booted)
	}
}

func TestDoShutdownMissingUDID(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"shutdown"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want error reply for missing udid, got %+v", out)
	}
	if len(c.shutdown) != 0 {
		t.Fatalf("Shutdown should not be called, got %v", c.shutdown)
	}
}

func TestDoShutdownSuccess(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"shutdown","udid":"ABC"}`))
	if len(out) != 1 || out[0].Type != "shutdown" || out[0].UDID != "ABC" {
		t.Fatalf("want shutdown reply for ABC, got %+v", out)
	}
	if len(c.shutdown) != 1 || c.shutdown[0] != "ABC" {
		t.Fatalf("Shutdown(ABC) not called, got %v", c.shutdown)
	}
}

func TestDoShutdownErrorReply(t *testing.T) {
	var out []ctrlReply
	c := &stubComp{shutdownErr: errors.New("boom")}
	d := newTestDispatch(c, &out)
	d.handle([]byte(`{"type":"shutdown","udid":"ABC"}`))
	if len(out) != 1 || out[0].Type != "error" {
		t.Fatalf("want one error reply, got %+v", out)
	}
}

func TestDoDetachReplies(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{}, &out)
	d.handle([]byte(`{"type":"detach"}`))
	if len(out) != 1 || out[0].Type != "detached" {
		t.Fatalf("want one detached reply, got %+v", out)
	}
}

func TestInputBeforeAttachIgnored(t *testing.T) {
	var out []ctrlReply
	d := newTestDispatch(&stubComp{}, &out)
	// No attachment yet → a tap must be a safe no-op (no panic, no reply).
	d.handle([]byte(`{"type":"tap","x":0.5,"y":0.5}`))
	if len(out) != 0 {
		t.Fatalf("tap before attach should produce no reply, got %+v", out)
	}
}
