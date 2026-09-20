package store

import (
	"context"
	"fmt"
	"time"
)

// Bootstrap creates the account and its vault together, in one transaction.
//
// They are useless apart: an account without a vault has no salt to derive a
// key from, and a vault without an account has no password to derive from.
// Writing them as two statements would risk leaving the panel permanently
// half-configured, because neither table accepts a second row — CreateUser and
// CreateVault deliberately refuse to upsert, so a retry after a partial failure
// could not repair the damage.
func (s *Store) Bootstrap(ctx context.Context, username, passwordHash string, salt []byte, fingerprint string) (User, error) {
	now := time.Now()
	stamp := formatTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("store: begin bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?)`,
		username, passwordHash, stamp, stamp)
	if err != nil {
		return User{}, fmt.Errorf("store: bootstrap user: %w", err)
	}

	// Read the id before committing: LastInsertId is only meaningful on the
	// statement that produced it.
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("store: read new user id: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vault (id, kdf_salt, key_fingerprint, created_at) VALUES (1, ?, ?, ?)`,
		salt, fingerprint, stamp); err != nil {
		return User{}, fmt.Errorf("store: bootstrap vault: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("store: commit bootstrap: %w", err)
	}

	return User{
		ID:           id,
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// Rekey atomically installs a new master key.
//
// Four writes have to agree: every re-encrypted credential, the new salt, the
// new key fingerprint, and the new password hash. Spread across separate
// transactions, a failure in the middle would leave credentials sealed under a
// key the panel can no longer derive — unrecoverable, because the old key is
// gone and the new one does not match the surviving ciphertext. One transaction
// means any failure leaves the previous state fully intact and retryable.
func (s *Store) Rekey(ctx context.Context, passwordHash string, salt []byte, fingerprint string, creds map[string][]byte) error {
	stamp := formatTime(time.Now())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin rekey: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = 1`,
		passwordHash, stamp); err != nil {
		return fmt.Errorf("store: rekey user: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE vault SET kdf_salt = ?, key_fingerprint = ?, rotated_at = ? WHERE id = 1`,
		salt, fingerprint, stamp); err != nil {
		return fmt.Errorf("store: rekey vault: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM credentials`); err != nil {
		return fmt.Errorf("store: rekey clear credentials: %w", err)
	}
	for name, blob := range creds {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO credentials (name, ciphertext, created_at, updated_at)
			 VALUES (?, ?, ?, ?)`,
			name, blob, stamp, stamp); err != nil {
			return fmt.Errorf("store: rekey credential %q: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit rekey: %w", err)
	}
	return nil
}
