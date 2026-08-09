package signalbroker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTURNFetcherPicksRelayAndCaches: the fetcher returns the credentialed entry
// (not Cloudflare's STUN one) and refetches only once the credential is halfway
// to expiry.
func TestTURNFetcherPicksRelayAndCaches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"ttl":3600}` {
			t.Errorf("body = %s, want ttl 3600", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"iceServers":[
			{"urls":["stun:stun.cloudflare.com:3478"]},
			{"urls":["turns:turn.cloudflare.com:443?transport=tcp"],"username":"U","credential":"C"}]}`)
	}))
	defer srv.Close()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	f := &turnFetcher{
		endpoint: srv.URL,
		token:    "TOKEN",
		ttl:      time.Hour,
		now:      func() time.Time { return now },
		http:     srv.Client(),
	}

	got, err := f.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential != "C" || got.Username != "U" || len(got.URLs) != 1 {
		t.Fatalf("want the relay entry, got %+v", got)
	}

	// Still fresh (29 min in, half-life is 30) → cached, no second call.
	now = now.Add(29 * time.Minute)
	if _, err := f.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("within half-life: %d API calls, want 1", calls)
	}

	// Past the half-life → refetch.
	now = now.Add(2 * time.Minute)
	if _, err := f.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("past half-life: %d API calls, want 2", calls)
	}
}

// TestTURNFetcherAPIErrorSurfaces: a non-2xx from Cloudflare is an error, not a
// silently cached empty entry (the broker degrades to STUN only).
func TestTURNFetcherAPIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	f := &turnFetcher{endpoint: srv.URL, ttl: time.Hour, now: time.Now, http: srv.Client()}
	if _, err := f.get(context.Background()); err == nil {
		t.Fatal("want an error on 401")
	}
}
