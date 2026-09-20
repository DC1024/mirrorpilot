// Package store persists MirrorPilot's state in SQLite.
//
// The driver is modernc.org/sqlite, a pure-Go implementation. That choice is
// deliberate: the CGO-based alternatives would make cross-compiling to
// linux/arm64 awkward and would rule out a minimal base image — both of which
// this project wants to keep, since it targets ARM NAS boxes as much as servers.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// FileName is the database file, created inside the configured data
	// directory.
	FileName = "app.db"

	// busyTimeoutMS makes writers wait for a lock instead of failing
	// immediately with SQLITE_BUSY.
	busyTimeoutMS = 5000

	// timeLayout is how every timestamp is written. RFC3339 with nanos sorts
	// lexicographically, which lets SQL compare them directly as strings.
	timeLayout = time.RFC3339Nano
)

// ErrNotFound is returned when a lookup finds no row. Callers should treat it
// as an expected outcome, not an internal error.
var ErrNotFound = errors.New("store: not found")

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// Open connects to the database in dataDir, creating it and applying any
// pending schema migrations.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		filepath.Join(dataDir, FileName), busyTimeoutMS,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}

	// SQLite serialises writers regardless. Capping the pool at one connection
	// makes concurrent HTTP handlers queue on the driver instead of racing into
	// SQLITE_BUSY. Correctness over throughput: this serves a single user.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// formatTime renders a timestamp for storage. Everything is stored in UTC so
// the data survives the host's timezone changing under us.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// parseTime reads a stored timestamp back.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse timestamp %q: %w", s, err)
	}
	return t, nil
}

// wrapNotFound converts sql.ErrNoRows into ErrNotFound and leaves everything
// else alone.
func wrapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
