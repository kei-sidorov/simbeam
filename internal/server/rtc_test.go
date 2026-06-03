package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// /rtc no longer requires ?udid= (UDID is chosen later via the control
// DataChannel). A plain GET without WebSocket upgrade headers must be rejected
// by the upgrader (400), not panic.
func TestHandleRTCRejectsNonWebsocket(t *testing.T) {
	s := New(&stubComp{}, "")
	req := httptest.NewRequest(http.MethodGet, "/rtc", nil) // no upgrade headers
	rec := httptest.NewRecorder()
	s.handleRTC(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-websocket GET, got %d", rec.Code)
	}
}
