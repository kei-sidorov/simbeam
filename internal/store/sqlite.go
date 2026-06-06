package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers name "sqlite" (no cgo)
)

// SQLite is the database/sql-backed Store. Portable SQL (INSERT … ON CONFLICT …
// DO UPDATE) works on both SQLite and Postgres.
type SQLite struct{ db *sql.DB }

const schema = `CREATE TABLE IF NOT EXISTS subscriptions (
  client_pubkey TEXT PRIMARY KEY,
  product_id    TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  issued_at     TEXT NOT NULL,
  source        TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);`

// OpenSQLite opens (creating if needed) the database at path and ensures schema.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite is a single-writer; serialize to avoid SQLITE_BUSY when the broker goroutine and the HTTP handler share this store
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// Upsert inserts or updates the row, but ONLY when the incoming issued_at is
// strictly newer than the stored one (idempotent, last-write-wins, safe to spam
// from foreground/background). Caller must pass RFC3339 UTC strings with a 'Z'
// suffix (not a numeric offset) so they compare lexicographically by instant.
func (s *SQLite) Upsert(ctx context.Context, sub Subscription) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO subscriptions (client_pubkey, product_id, expires_at, issued_at, source, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(client_pubkey) DO UPDATE SET
  product_id = excluded.product_id,
  expires_at = excluded.expires_at,
  issued_at  = excluded.issued_at,
  source     = excluded.source,
  updated_at = excluded.updated_at
WHERE excluded.issued_at > subscriptions.issued_at`,
		sub.ClientPubKey, sub.ProductID, sub.ExpiresAt, sub.IssuedAt, sub.Source, sub.UpdatedAt)
	return err
}

// Get returns the row for clientPubKey (ok=false if absent).
func (s *SQLite) Get(ctx context.Context, clientPubKey string) (Subscription, bool, error) {
	var sub Subscription
	err := s.db.QueryRowContext(ctx,
		`SELECT client_pubkey, product_id, expires_at, issued_at, source, updated_at
		   FROM subscriptions WHERE client_pubkey = ?`, clientPubKey).
		Scan(&sub.ClientPubKey, &sub.ProductID, &sub.ExpiresAt, &sub.IssuedAt, &sub.Source, &sub.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, false, nil
	}
	if err != nil {
		return Subscription{}, false, err
	}
	return sub, true, nil
}

// Active reports whether the stored expires_at is in the future relative to now
// (server clock). Absent row → not active.
func (s *SQLite) Active(ctx context.Context, clientPubKey string, now time.Time) (bool, error) {
	var expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM subscriptions WHERE client_pubkey = ?`, clientPubKey).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	exp, perr := time.Parse(time.RFC3339, expiresAt)
	if perr != nil {
		return false, perr
	}
	return exp.After(now), nil
}

// Close releases the database handle.
func (s *SQLite) Close() error { return s.db.Close() }
