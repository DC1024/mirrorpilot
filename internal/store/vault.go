package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Vault holds the material needed to derive the master key.
//
// Note what is absent: the key itself. The KDF salt is not secret, but it must
// remain stable, or the same master password would derive a different key on
// the next login and every stored credential would become unreadable.
type Vault struct {
	KDFSalt        []byte
	KeyFingerprint string
	CreatedAt      time.Time
	RotatedAt      *time.Time
}

// Vault returns the vault row. ErrNotFound means setup has not completed.
func (s *Store) Vault(ctx context.Context) (Vault, error) {
	var (
		v         Vault
		createdAt string
		rotatedAt sql.NullString
		err       error
	)

	err = s.db.QueryRowContext(ctx,
		`SELECT kdf_salt, key_fingerprint, created_at, rotated_at FROM vault WHERE id = 1`,
	).Scan(&v.KDFSalt, &v.KeyFingerprint, &createdAt, &rotatedAt)
	if err != nil {
		return Vault{}, wrapNotFound(fmt.Errorf("store: load vault: %w", err))
	}

	if v.CreatedAt, err = parseTime(createdAt); err != nil {
		return Vault{}, err
	}
	if rotatedAt.Valid {
		t, err := parseTime(rotatedAt.String)
		if err != nil {
			return Vault{}, err
		}
		v.RotatedAt = &t
	}
	return v, nil
}

// CreateVault writes the vault row. It does not upsert: a second setup request
// must fail rather than replace the salt out from under existing credentials.
func (s *Store) CreateVault(ctx context.Context, salt []byte, fingerprint string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO vault (id, kdf_salt, key_fingerprint, created_at) VALUES (1, ?, ?, ?)`,
		salt, fingerprint, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store: create vault: %w", err)
	}
	return nil
}

// RotateVault replaces the salt and fingerprint after a master password change.
// The caller is responsible for re-encrypting credentials under the new key
// first — see ReplaceCredentials.
func (s *Store) RotateVault(ctx context.Context, salt []byte, fingerprint string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE vault SET kdf_salt = ?, key_fingerprint = ?, rotated_at = ? WHERE id = 1`,
		salt, fingerprint, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store: rotate vault: %w", err)
	}
	return requireAffected(res, "vault")
}

// SaveCredential upserts one encrypted credential.
func (s *Store) SaveCredential(ctx context.Context, name string, ciphertext []byte) error {
	now := formatTime(time.Now())

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (name, ciphertext, created_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     ciphertext = excluded.ciphertext,
		     updated_at = excluded.updated_at`,
		name, ciphertext, now, now)
	if err != nil {
		return fmt.Errorf("store: save credential %q: %w", name, err)
	}
	return nil
}

// Credential returns one encrypted credential.
func (s *Store) Credential(ctx context.Context, name string) ([]byte, error) {
	var blob []byte

	err := s.db.QueryRowContext(ctx,
		`SELECT ciphertext FROM credentials WHERE name = ?`, name).Scan(&blob)
	if err != nil {
		return nil, wrapNotFound(fmt.Errorf("store: load credential %q: %w", name, err))
	}
	return blob, nil
}

// Credentials returns every encrypted credential keyed by name.
//
// The dashboard uses the key set to show which integrations are configured
// without needing the master key to be unlocked.
func (s *Store) Credentials(ctx context.Context) (map[string][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, ciphertext FROM credentials`)
	if err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]byte)
	for rows.Next() {
		var (
			name string
			blob []byte
		)
		if err := rows.Scan(&name, &blob); err != nil {
			return nil, fmt.Errorf("store: scan credential: %w", err)
		}
		out[name] = blob
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate credentials: %w", err)
	}
	return out, nil
}

// DeleteCredential removes a credential. A missing row is not an error —
// deleting something twice should be a no-op.
func (s *Store) DeleteCredential(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM credentials WHERE name = ?`, name); err != nil {
		return fmt.Errorf("store: delete credential %q: %w", name, err)
	}
	return nil
}
