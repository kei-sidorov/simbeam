package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleRTCMissingUDID(t *testing.T) {
	s := New(nil, "")
	req := httptest.NewRequest(http.MethodGet, "/rtc", nil) // no ?udid=
	rec := httptest.NewRecorder()
	s.handleRTC(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing udid, got %d", rec.Code)
	}
}
