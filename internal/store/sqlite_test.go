package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "subs.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLite_UpsertGetActive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	// Absent → inactive, not found.
	if active, err := s.Active(ctx, "pub", now); err != nil || active {
		t.Fatalf("absent active=%v err=%v, want false/nil", active, err)
	}
	if _, ok, err := s.Get(ctx, "pub"); err != nil || ok {
		t.Fatalf("absent get ok=%v err=%v", ok, err)
	}

	// Insert a future expiry → active.
	sub := Subscription{
		ClientPubKey: "pub", ProductID: "pro.monthly",
		ExpiresAt: "2026-12-31T00:00:00Z", IssuedAt: "2026-06-04T00:00:00Z",
		Source: "client", UpdatedAt: "2026-06-04T12:00:00Z",
	}
	if err := s.Upsert(ctx, sub); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if active, err := s.Active(ctx, "pub", now); err != nil || !active {
		t.Fatalf("active=%v err=%v, want true/nil", active, err)
	}

	// Expired expiry → inactive.
	past := Subscription{
		ClientPubKey: "pub", ProductID: "pro.monthly",
		ExpiresAt: "2026-01-01T00:00:00Z", IssuedAt: "2026-06-05T00:00:00Z",
		Source: "client", UpdatedAt: "2026-06-05T00:00:00Z",
	}
	if err := s.Upsert(ctx, past); err != nil {
		t.Fatalf("upsert past: %v", err)
	}
	if active, err := s.Active(ctx, "pub", now); err != nil || active {
		t.Fatalf("after expiry active=%v err=%v, want false", active, err)
	}
}

func TestSQLite_LastWriteWinsByIssuedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	newer := Subscription{ClientPubKey: "pub", ProductID: "p", ExpiresAt: "2026-12-31T00:00:00Z", IssuedAt: "2026-06-10T00:00:00Z", Source: "client", UpdatedAt: "2026-06-10T00:00:00Z"}
	if err := s.Upsert(ctx, newer); err != nil {
		t.Fatal(err)
	}
	// An OLDER issued_at must be ignored (out-of-order report).
	older := Subscription{ClientPubKey: "pub", ProductID: "p", ExpiresAt: "2026-07-01T00:00:00Z", IssuedAt: "2026-06-01T00:00:00Z", Source: "client", UpdatedAt: "2026-06-11T00:00:00Z"}
	if err := s.Upsert(ctx, older); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "pub")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.ExpiresAt != "2026-12-31T00:00:00Z" {
		t.Fatalf("older write clobbered newer: expires=%q", got.ExpiresAt)
	}
}

func TestSQLite_EqualIssuedAtIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	first := Subscription{ClientPubKey: "pub", ProductID: "p", ExpiresAt: "2026-12-31T00:00:00Z", IssuedAt: "2026-06-10T00:00:00Z", Source: "client", UpdatedAt: "2026-06-10T00:00:00Z"}
	if err := s.Upsert(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Same issued_at, different expiry → must NOT overwrite (idempotent re-delivery).
	dup := Subscription{ClientPubKey: "pub", ProductID: "p", ExpiresAt: "2027-01-01T00:00:00Z", IssuedAt: "2026-06-10T00:00:00Z", Source: "client", UpdatedAt: "2026-06-12T00:00:00Z"}
	if err := s.Upsert(ctx, dup); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "pub")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.ExpiresAt != "2026-12-31T00:00:00Z" {
		t.Fatalf("equal issued_at clobbered the row: expires=%q", got.ExpiresAt)
	}
}
