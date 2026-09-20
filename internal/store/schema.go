package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migration is one forward-only schema step.
//
// Steps are applied in order and recorded in schema_migrations, so adding a
// change is a matter of appending to the slice below. Migrations are never
// edited after release: someone's database has already run the old version.
type migration struct {
	name string
	sql  string
}

var migrations = []migration{
	{name: "0001_initial", sql: schemaV1},
}

// schemaV1 lays out the whole persistence model.
//
// Notes on the shape:
//   - users and vault are single-row tables (enforced with CHECK id = 1).
//     MirrorPilot is a single-user tool; modelling that in the schema means the
//     application never has to guard against a second row appearing.
//   - vault stores only the KDF salt and a key fingerprint. The key itself never
//     touches disk, and the fingerprint reveals nothing that helps derive it.
//   - credentials holds ciphertext only. Anything written here has already been
//     through the secret.Sealer.
const schemaV1 = `
CREATE TABLE users (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE vault (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    kdf_salt        BLOB NOT NULL,
    key_fingerprint TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    rotated_at      TEXT
);

CREATE TABLE credentials (
    name       TEXT PRIMARY KEY,
    ciphertext BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    last_seen  TEXT NOT NULL
);

CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
`

// migrate applies every migration that has not run yet, each in its own
// transaction so a failure leaves the database at a known step rather than
// half-way through one.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.name] {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schema_migrations: %w", err)
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
		m.name, formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("store: record migration %s: %w", m.name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", m.name, err)
	}
	return nil
}
