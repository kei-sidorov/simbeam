// Package store holds the simcast subscription persistence behind a thin Store
// interface. The only durable server state is the subscriptions table; daemon
// keys and the list of paired Macs live on the endpoints (decision: minimal DB).
// SQLite now, Postgres later by swapping the implementation behind Store.
package store

import (
	"context"
	"time"
)

// Subscription is one row: a subscription bound to a client public key (the same
// key used for pairing). Dates are RFC3339 strings (normalized to UTC by the
// endpoint) so string comparison and portability hold across SQLite/Postgres.
type Subscription struct {
	ClientPubKey string
	ProductID    string
	ExpiresAt    string // RFC3339 UTC; from StoreKit (currently client-asserted)
	IssuedAt     string // RFC3339 UTC; client report time (ordering / last-write-wins)
	Source       string // "client" now; "apple-verified" later
	UpdatedAt    string // RFC3339 UTC; server clock at write
}

// Store is the persistence boundary. now is always a parameter (never
// CURRENT_TIMESTAMP) so logic is testable and SQL stays portable.
type Store interface {
	Upsert(ctx context.Context, sub Subscription) error
	Get(ctx context.Context, clientPubKey string) (Subscription, bool, error)
	Active(ctx context.Context, clientPubKey string, now time.Time) (bool, error)
	Close() error
}
