package updatecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		tag, current string
		want         bool
	}{
		{"v0.11.0", "0.10.1", true},
		{"v0.10.1", "0.10.1", false},
		{"v0.10.0", "0.10.1", false},
		{"v1.0.0", "0.99.99", true},
		{"v0.10.2-rc1", "0.10.1", true}, // pre-release suffix ignored
		{"v0.10", "0.10.0", false},      // missing component = 0
		{"v2", "1.9.9", true},
		{"nightly", "0.10.1", false}, // unparsable tag must never nag
		{"v0.11.0", "dev", false},    // unparsable current likewise
	}
	for _, tc := range cases {
		if got := newer(tc.tag, tc.current); got != tc.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tc.tag, tc.current, got, tc.want)
		}
	}
}

// githubStub serves the releases/latest shape and counts hits.
func githubStub(t *testing.T, tag string) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/repos/kei-sidorov/simbeam/releases/latest" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"tag_name":"` + tag + `"}`))
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

// A fresh cache must answer without touching GitHub — restart churn (and the
// periodic loop) stays within one request per TTL.
func TestCheckCachesAcrossCalls(t *testing.T) {
	stub, hits := githubStub(t, "v0.11.0")
	c := &Checker{
		Repo:      "kei-sidorov/simbeam",
		Current:   "0.10.1",
		CachePath: filepath.Join(t.TempDir(), "update-check.json"),
		TTL:       time.Hour,
		apiBase:   stub.URL,
	}
	for i := 0; i < 3; i++ {
		if got := c.check(context.Background()); got != "v0.11.0" {
			t.Fatalf("check #%d = %q, want v0.11.0", i, got)
		}
	}
	if *hits != 1 {
		t.Fatalf("GitHub hit %d times, want 1 (cache must absorb the rest)", *hits)
	}

	// A second Checker over the same cache file (a daemon restart) also stays
	// off the network while the cache is fresh.
	c2 := &Checker{Repo: c.Repo, Current: c.Current, CachePath: c.CachePath, TTL: time.Hour, apiBase: stub.URL}
	if got := c2.check(context.Background()); got != "v0.11.0" {
		t.Fatalf("restarted check = %q, want v0.11.0", got)
	}
	if *hits != 1 {
		t.Fatalf("GitHub hit %d times after restart, want still 1", *hits)
	}
}

// A dead GitHub must degrade to the cached answer, and to "" without one —
// never an error surfaced to the daemon.
func TestCheckDegradesToCache(t *testing.T) {
	stub, _ := githubStub(t, "v0.11.0")
	path := filepath.Join(t.TempDir(), "update-check.json")
	c := &Checker{Repo: "kei-sidorov/simbeam", Current: "0.10.1", CachePath: path, TTL: time.Nanosecond, apiBase: stub.URL}
	if got := c.check(context.Background()); got != "v0.11.0" {
		t.Fatalf("warm-up check = %q", got)
	}
	stub.Close() // TTL is 1ns, so the next check re-fetches — and fails
	if got := c.check(context.Background()); got != "v0.11.0" {
		t.Fatalf("check with dead GitHub = %q, want cached v0.11.0", got)
	}

	fresh := &Checker{Repo: "kei-sidorov/simbeam", Current: "0.10.1", TTL: time.Nanosecond, apiBase: stub.URL}
	if got := fresh.check(context.Background()); got != "" {
		t.Fatalf("check with dead GitHub and no cache = %q, want empty", got)
	}
}

// Run must notify exactly once per distinct newer version, with the "v"
// stripped, and must not notify when up to date.
func TestRunNotifiesOncePerVersion(t *testing.T) {
	stub, _ := githubStub(t, "v0.11.0")
	c := &Checker{
		Repo: "kei-sidorov/simbeam", Current: "0.10.1",
		CachePath: filepath.Join(t.TempDir(), "update-check.json"),
		TTL:       5 * time.Millisecond, apiBase: stub.URL,
	}
	got := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx, func(latest string) { got <- latest })

	select {
	case v := <-got:
		if v != "0.11.0" {
			t.Fatalf("notify(%q), want 0.11.0 (bare semver)", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notify")
	}
	select {
	case v := <-got:
		t.Fatalf("second notify(%q) for the same version", v)
	case <-time.After(50 * time.Millisecond): // several TTLs
	}
}
