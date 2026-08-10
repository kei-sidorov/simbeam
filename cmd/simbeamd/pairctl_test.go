package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeWindow struct {
	mu    sync.Mutex
	state string
}

func (f *fakeWindow) Open(secret string, now time.Time, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = "open"
}

func (f *fakeWindow) State(now time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == "" {
		return "none"
	}
	return f.state
}

func TestControlHandler(t *testing.T) {
	w := &fakeWindow{}
	c := &controlServer{
		win:      w,
		buildURL: func(secret string) string { return "https://x/#s=" + secret },
		ttl:      5 * time.Minute,
		token:    "tok",
	}
	ts := httptest.NewServer(c.handler())
	defer ts.Close()

	do := func(method, path, token string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := do("POST", "/pair", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", resp.StatusCode)
	}
	if resp := do("POST", "/pair", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d, want 401", resp.StatusCode)
	}

	resp := do("POST", "/pair", "tok")
	var rep pairReply
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep.URL == "https://x/#s=" || rep.TTLSeconds != 300 {
		t.Fatalf("bad reply: %+v", rep)
	}
	if w.State(time.Now()) != "open" {
		t.Fatal("POST /pair did not arm the window")
	}

	c.noteEnrolled("client-pub-key")
	w.mu.Lock()
	w.state = "consumed"
	w.mu.Unlock()
	var st pairState
	resp = do("GET", "/pair", "tok")
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State != "consumed" || st.Client != "client-pub-key" {
		t.Fatalf("bad state: %+v", st)
	}
}
