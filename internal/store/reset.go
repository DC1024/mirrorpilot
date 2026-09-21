package store

import (
	"context"
	"fmt"
)

// Reset removes everything standing between a forgotten password and a usable
// panel: the account, the vault the key is derived from, every credential
// sealed with it, and every live session.
//
// It exists because the alternative is deleting app.db by hand, and telling
// someone to rm a file that sits next to a -wal and a -shm is a good way to
// lose work that had nothing to do with the problem.
//
// What it deliberately does not touch is the mirror catalogue, the
// preferences, and the measurement history. None of that is secret, and
// throwing away months of measurements to fix a login would be a poor trade.
//
// One transaction, so a failure half-way cannot leave a database holding a
// vault but no account — a state in which setup is refused and login is
// impossible, which is strictly worse than either.
func (s *Store) Reset(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Each table on its own statement, in an order that reads as the reverse
	// of how the data came to exist: sessions, then what the key protected,
	// then the key's salt, then the account that derives it.
	for _, statement := range []string{
		`DELETE FROM sessions`,
		`DELETE FROM credentials`,
		`DELETE FROM vault`,
		`DELETE FROM users`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("store: reset: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit reset: %w", err)
	}
	return nil
}

// ResetCounts reports how much a Reset would remove, so a caller can say what
// it is about to do rather than only what it did.
//
// Read in its own transaction and before the write, because a number reported
// afterwards would be zero by construction and would tell the reader nothing.
func (s *Store) ResetCounts(ctx context.Context) (ResetSummary, error) {
	var out ResetSummary

	rows := []struct {
		query string
		into  *int
	}{
		{`SELECT COUNT(*) FROM users`, &out.Users},
		{`SELECT COUNT(*) FROM credentials`, &out.Credentials},
		{`SELECT COUNT(*) FROM sessions`, &out.Sessions},
	}

	for _, row := range rows {
		if err := s.db.QueryRowContext(ctx, row.query).Scan(row.into); err != nil {
			return ResetSummary{}, fmt.Errorf("store: count for reset: %w", err)
		}
	}
	return out, nil
}

// ResetSummary is what a Reset would remove.
type ResetSummary struct {
	Users       int
	Credentials int
	Sessions    int
}
