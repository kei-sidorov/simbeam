// Package updatecheck asks GitHub Releases whether a newer simbeamd release
// exists. It is deliberately polite: at most one request per TTL with the
// last answer cached on disk across restarts, best-effort throughout (any
// network/parse failure is silence, never a startup problem), and it is not
// wired up at all for dev builds or when -no-update-check is set. It informs —
// the actual upgrade stays a user-run brew command.
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Checker polls one GitHub repo's latest release.
type Checker struct {
	Repo      string        // GitHub "owner/name" whose releases to poll
	Current   string        // running version without the "v" (from -ldflags)
	CachePath string        // JSON cache surviving restarts; "" → no cache
	TTL       time.Duration // minimum age between GitHub requests; 0 → 24h
	Client    *http.Client  // nil → 10s-timeout default

	apiBase string // test seam; "" → https://api.github.com
}

// cache is the on-disk memo of the last successful GitHub answer, so restart
// churn does not turn into API requests.
type cache struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest"` // tag_name, e.g. "v0.11.0"
}

// Run loops forever (until ctx is cancelled), waking every TTL. notify fires
// at most once per distinct newer version, with the bare semver (no "v").
// Run blocks — call it in a goroutine.
func (c *Checker) Run(ctx context.Context, notify func(latest string)) {
	ttl := c.ttl()
	notified := ""
	for {
		if latest := c.check(ctx); latest != "" && latest != notified && newer(latest, c.Current) {
			notified = latest
			notify(strings.TrimPrefix(latest, "v"))
		}
		select {
		case <-time.After(ttl):
		case <-ctx.Done():
			return
		}
	}
}

// check returns the latest release tag: the cached one while it is fresh,
// otherwise GitHub's answer (which then refreshes the cache). Every failure
// path degrades to whatever the cache holds — possibly "".
func (c *Checker) check(ctx context.Context) string {
	cached, ok := c.readCache()
	if ok && time.Since(cached.CheckedAt) < c.ttl() {
		return cached.Latest
	}
	latest, err := c.fetch(ctx)
	if err != nil {
		return cached.Latest
	}
	c.writeCache(cache{CheckedAt: time.Now(), Latest: latest})
	return latest
}

// fetch asks the GitHub API for the repo's latest release tag.
func (c *Checker) fetch(ctx context.Context) (string, error) {
	base := c.apiBase
	if base == "" {
		base = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/repos/"+c.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "simbeamd/"+c.Current)
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", fmt.Errorf("github: empty tag_name")
	}
	return body.TagName, nil
}

func (c *Checker) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return 24 * time.Hour
}

func (c *Checker) readCache() (cache, bool) {
	if c.CachePath == "" {
		return cache{}, false
	}
	b, err := os.ReadFile(c.CachePath)
	if err != nil {
		return cache{}, false
	}
	var v cache
	if err := json.Unmarshal(b, &v); err != nil {
		return cache{}, false
	}
	return v, true
}

func (c *Checker) writeCache(v cache) {
	if c.CachePath == "" {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.CachePath), 0o700)
	_ = os.WriteFile(c.CachePath, b, 0o600)
}

// newer reports whether tag a (e.g. "v0.11.0") is a strictly newer release
// than version b (e.g. "0.10.1"). Comparison is numeric per dotted component,
// missing components count as 0, and anything unparsable makes the answer
// false — an odd tag must never nag the user.
func newer(a, b string) bool {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

// parseVersion reads up to three leading numeric components of a version like
// "v1.2.3" or "0.10", ignoring any pre-release/build suffix ("-rc1", "+meta").
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	var v [3]int
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
