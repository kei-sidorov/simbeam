package signalbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kei-sidorov/simcast/internal/signal"
	"github.com/kei-sidorov/simcast/internal/store"
)

func TestSubscriptionEndpoint_TwoSigUpsertAndGate(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenSQLite(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const appSecret = "dev-app-secret"
	b := New(Config{Store: st, AppSecret: appSecret, Now: func() time.Time { return now }})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	pub, priv, _ := signal.GenerateKeyPair()
	product := "pro.monthly"
	expires := "2026-12-31T00:00:00Z"
	issued := now.UTC().Format(time.RFC3339)
	canon := signal.CanonicalSubscription(pub, product, expires, issued)

	post := func(appSig, accSig string) int {
		body, _ := json.Marshal(map[string]string{
			"client_pubkey": pub, "product_id": product, "expires_at": expires, "issued_at": issued,
		})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/subscription", bytes.NewReader(body))
		req.Header.Set("X-App-Sig", appSig)
		req.Header.Set("X-Account-Sig", accSig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	goodApp := signal.AppSig(appSecret, canon)
	goodAcc := signal.Sign(priv, canon)

	// Bad app sig → 401, nothing stored.
	if code := post("bad", goodAcc); code != http.StatusUnauthorized {
		t.Fatalf("bad app sig: code=%d want 401", code)
	}
	// Bad account sig → 401.
	if code := post(goodApp, "bad"); code != http.StatusUnauthorized {
		t.Fatalf("bad account sig: code=%d want 401", code)
	}
	// Both good → 200 and subscription becomes active.
	if code := post(goodApp, goodAcc); code != http.StatusOK {
		t.Fatalf("good: code=%d want 200", code)
	}
	if active, _ := st.Active(context.Background(), pub, now); !active {
		t.Fatalf("subscription not active after valid POST")
	}
}

func TestSubscriptionEndpoint_CORSPreflight(t *testing.T) {
	b := New(Config{Store: nil})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/subscription", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight code=%d want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("missing CORS allow-origin on preflight")
	}
}
