package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kei-sidorov/simcast/internal/companion"
)

type fakeCompanion struct {
	sims     []companion.Simulator
	bootErr  error
	bootUDID string
}

func (f *fakeCompanion) List(ctx context.Context) ([]companion.Simulator, error) {
	return f.sims, nil
}
func (f *fakeCompanion) Boot(ctx context.Context, udid string) error {
	f.bootUDID = udid
	return f.bootErr
}

func TestSimulatorsEndpoint(t *testing.T) {
	f := &fakeCompanion{sims: []companion.Simulator{{UDID: "U1", Name: "iPhone", OSVersion: "iOS 26.4", State: "Booted"}}}
	srv := New(f, "")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/simulators", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	var got []companion.Simulator
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UDID != "U1" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestBootEndpoint(t *testing.T) {
	f := &fakeCompanion{}
	srv := New(f, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/boot", strings.NewReader(`{"udid":"U9"}`))
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if f.bootUDID != "U9" {
		t.Fatalf("Boot got udid %q", f.bootUDID)
	}
}

func TestBootEndpointMissingUDID(t *testing.T) {
	srv := New(&fakeCompanion{}, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/boot", strings.NewReader(`{}`))
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
