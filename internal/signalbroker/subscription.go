package signalbroker

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/kei-sidorov/simbeam/internal/signal"
	"github.com/kei-sidorov/simbeam/internal/store"
)

// replayWindow bounds how far issued_at may drift from the server clock. Generous
// because client clocks vary; with a static APP_SECRET this is hygiene, not a
// strong replay defense (the real boundary is a future Apple-receipt check).
const replayWindow = 48 * time.Hour

type subRequest struct {
	ClientPubKey string `json:"client_pubkey"`
	ProductID    string `json:"product_id"`
	ExpiresAt    string `json:"expires_at"`
	IssuedAt     string `json:"issued_at"`
}

// cors sets permissive headers and short-circuits OPTIONS preflight (the bench
// is cross-origin: served from :8080, broker on :9000, custom auth headers).
func cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-App-Sig, X-Account-Sig")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// handleSubscription verifies BOTH signatures (weak app-secret HMAC + real
// Ed25519 account signature) over the canonical fields, then idempotently upserts
// (last-write-wins by issued_at). Returns 200 on valid signatures whether it wrote
// or no-op'd (safe to spam from foreground/background); returns 500 on a genuine
// store error.
func (b *Broker) handleSubscription(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if b.cfg.Store == nil {
		http.Error(w, "no store configured", http.StatusServiceUnavailable)
		return
	}
	var req subRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	canon := signal.CanonicalSubscription(req.ClientPubKey, req.ProductID, req.ExpiresAt, req.IssuedAt)
	if !signal.VerifyAppSig(b.cfg.AppSecret, canon, r.Header.Get("X-App-Sig")) {
		http.Error(w, "bad app signature", http.StatusUnauthorized)
		return
	}
	if !signal.Verify(req.ClientPubKey, canon, r.Header.Get("X-Account-Sig")) {
		http.Error(w, "bad account signature", http.StatusUnauthorized)
		return
	}
	issued, err := time.Parse(time.RFC3339, req.IssuedAt)
	if err != nil {
		http.Error(w, "bad issued_at", http.StatusBadRequest)
		return
	}
	now := b.cfg.Now()
	if issued.Before(now.Add(-replayWindow)) || issued.After(now.Add(replayWindow)) {
		http.Error(w, "issued_at outside accepted window", http.StatusBadRequest)
		return
	}
	exp, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		http.Error(w, "bad expires_at", http.StatusBadRequest)
		return
	}
	if err := b.cfg.Store.Upsert(r.Context(), store.Subscription{
		ClientPubKey: req.ClientPubKey,
		ProductID:    req.ProductID,
		ExpiresAt:    exp.UTC().Format(time.RFC3339),
		IssuedAt:     issued.UTC().Format(time.RFC3339),
		Source:       "client",
		UpdatedAt:    now.UTC().Format(time.RFC3339),
	}); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleSubscriptionMe returns the caller's current subscription so the bench can
// show state. Auth: an Ed25519 signature over "<pubkey>\x1f<ts>" proves key
// ownership; ts must be fresh. Optional convenience (the canonical inspection is
// the SQLite file itself).
func (b *Broker) handleSubscriptionMe(w http.ResponseWriter, r *http.Request) {
	if cors(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if b.cfg.Store == nil {
		http.Error(w, "no store configured", http.StatusServiceUnavailable)
		return
	}
	pub := r.URL.Query().Get("pubkey")
	ts := r.URL.Query().Get("ts")
	sig := r.URL.Query().Get("sig")
	canon := []byte(pub + "\x1f" + ts)
	if pub == "" || !signal.Verify(pub, canon, sig) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if when, err := time.Parse(time.RFC3339, ts); err != nil || when.Before(b.cfg.Now().Add(-replayWindow)) || when.After(b.cfg.Now().Add(replayWindow)) {
		http.Error(w, "stale ts", http.StatusUnauthorized)
		return
	}
	sub, ok, err := b.cfg.Store.Get(r.Context(), pub)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no subscription", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"client_pubkey": sub.ClientPubKey, "product_id": sub.ProductID,
		"expires_at": sub.ExpiresAt, "issued_at": sub.IssuedAt,
		"source": sub.Source, "updated_at": sub.UpdatedAt,
	})
}
