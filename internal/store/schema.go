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
	{name: "0002_sources_and_probes", sql: schemaV2},
	{name: "0003_probe_layer_statuses", sql: schemaV3},
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

// schemaV2 adds the mirror catalogue and the probe history.
//
// Notes on the shape:
//   - sources holds both built-in and user-added mirrors in one table. Keeping
//     built-ins in the database rather than reading the embedded list at render
//     time is what lets a user disable a built-in source, and lets a later
//     release add new built-ins without disturbing anyone's choices.
//   - scope is a sorted, comma-joined list. It is a fixed set of short tokens
//     with no internal structure, so a join table would buy nothing but two
//     more queries.
//   - probe_runs is append-only history, pruned per source. It is a trend log,
//     not an audit trail: nothing here is authoritative, and losing it costs a
//     graph rather than data.
const schemaV2 = `
CREATE TABLE sources (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    url        TEXT NOT NULL,
    homepage   TEXT NOT NULL DEFAULT '',
    scope      TEXT NOT NULL DEFAULT '',
    provider   TEXT NOT NULL,
    trust      TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    builtin    INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1,
    insecure   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_sources_enabled ON sources (enabled);

CREATE TABLE probe_runs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id      TEXT NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    started_at     TEXT NOT NULL,

    -- One row per layer of the four-layer probe. Statuses are stored as text
    -- rather than an enum because SQLite has no enums and a CHECK constraint
    -- would need a migration every time the vocabulary grows.
    connectivity   TEXT NOT NULL,
    connect_ms     INTEGER NOT NULL DEFAULT 0,
    token_ms       INTEGER NOT NULL DEFAULT 0,
    manifest_ms    INTEGER NOT NULL DEFAULT 0,
    throughput_bps INTEGER NOT NULL DEFAULT 0,

    status         TEXT NOT NULL,
    detail         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_probe_runs_source_started ON probe_runs (source_id, started_at DESC);
CREATE INDEX idx_probe_runs_started_at ON probe_runs (started_at);
`

// schemaV3 records what each of the four layers concluded on its own.
//
// schemaV2 stored the connectivity status and the overall verdict, which is
// enough to colour a row and not enough to explain it: the page could say a
// mirror "failed" without being able to say whether the manifest came back 404
// or the blob never arrived. Per-layer statuses are the difference between a
// verdict and a diagnosis, and a tool whose whole purpose is helping someone
// decide which mirror to configure owes them the diagnosis.
//
// resolved_digest is here for the same reason the probe computes it rather than
// trusting a header: it is the only record of what was actually measured. A tag
// may be answered out of a mirror's cache, so without this two rows in the same
// batch could describe different bytes and look directly comparable.
const schemaV3 = `
ALTER TABLE probe_runs ADD COLUMN token_status      TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_runs ADD COLUMN manifest_status   TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_runs ADD COLUMN throughput_status TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_runs ADD COLUMN resolved_digest   TEXT NOT NULL DEFAULT '';
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
