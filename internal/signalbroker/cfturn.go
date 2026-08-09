package signalbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
)

// CloudflareTURNEndpoint builds the credential endpoint for a Cloudflare
// Realtime TURN key ID (https://developers.cloudflare.com/realtime/turn/).
func CloudflareTURNEndpoint(keyID string) string {
	return "https://rtc.live.cloudflare.com/v1/turn/keys/" + keyID + "/credentials/generate-ice-servers"
}

// usableRelayURLs drops Cloudflare's port-53 relay from the list it returns.
//
// Non-trickle ICE means neither peer sends its SDP until gathering is complete,
// so ONE dead URL delays the whole connection. Port 53 is the DNS port and
// carriers routinely intercept it: measured against the live broker, the six
// URLs Cloudflare hands back gather in 7.81s, and the same set minus :53 in
// 0.05s. On a 4G iPad :53 produced no relay candidate at all. Everything it
// could get through is already covered by turns:443, which is the URL built for
// hostile networks.
func usableRelayURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if strings.Contains(u, ":53?") || strings.HasSuffix(u, ":53") {
			continue
		}
		out = append(out, u)
	}
	return out
}

// turnFetcher issues relay credentials from Cloudflare Realtime TURN. Unlike
// coturn's REST-API mechanism there is no shared secret to HMAC locally —
// credentials exist only if Cloudflare's API mints them, so the broker fetches
// one and reuses it until it is halfway to expiry.
//
// ponytail: ONE credential shared by every subscriber. A leaked one relays on
// our bill until it expires; per-client credentials (Cloudflare's
// customIdentifier, one API call per connection) are the upgrade if that bites.
type turnFetcher struct {
	endpoint string        // .../v1/turn/keys/<id>/credentials/generate-ice-servers
	token    string        // TURN key API token
	ttl      time.Duration // credential lifetime asked of Cloudflare
	now      func() time.Time
	http     *http.Client

	mu      sync.Mutex // held across the refresh: one fetch in flight, others wait
	cached  signal.ICEServer
	refresh time.Time // fetch again at/after this
}

// get returns the shared relay entry, refreshing it at half its TTL. The
// credential must outlive the whole session: Cloudflare kills an allocation
// whose credential expired, unlike coturn, which only checks it at allocation.
func (f *turnFetcher) get(ctx context.Context) (signal.ICEServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.now().Before(f.refresh) {
		return f.cached, nil
	}

	body, err := json.Marshal(map[string]int64{"ttl": int64(f.ttl.Seconds())})
	if err != nil {
		return signal.ICEServer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.endpoint, bytes.NewReader(body))
	if err != nil {
		return signal.ICEServer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return signal.ICEServer{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return signal.ICEServer{}, fmt.Errorf("cloudflare turn: %s", resp.Status)
	}
	var out struct {
		ICEServers []signal.ICEServer `json:"iceServers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return signal.ICEServer{}, err
	}
	// The response also carries Cloudflare's STUN entry; we want only the relay,
	// which is the entry with credentials (our own STUN goes out to everyone).
	for _, s := range out.ICEServers {
		if s.Credential != "" {
			s.URLs = usableRelayURLs(s.URLs)
			f.cached, f.refresh = s, f.now().Add(f.ttl/2)
			return s, nil
		}
	}
	return signal.ICEServer{}, fmt.Errorf("cloudflare turn: no relay entry in response")
}
