package store

import (
	"context"
	"fmt"
	"time"
)

// User is the single panel account.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// HasUser reports whether the panel has been set up. It selects between the
// setup screen and the login screen.
func (s *Store) HasUser(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: count users: %w", err)
	}
	return n > 0, nil
}

// CreateUser inserts the account.
//
// It does not upsert. A second setup request must fail loudly rather than
// silently reset someone's credentials.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (User, error) {
	now := time.Now()

	// id is pinned to 1 rather than auto-assigned: the CHECK (id = 1) in the
	// schema is what makes a second setup request fail instead of quietly
	// creating a second account, and that only fires if we name the column.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?)`,
		username, passwordHash, formatTime(now), formatTime(now))
	if err != nil {
		return User{}, fmt.Errorf("store: create user: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("store: read new user id: %w", err)
	}

	return User{
		ID:           id,
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// User returns the account. ErrNotFound means setup has not happened yet.
func (s *Store) User(ctx context.Context) (User, error) {
	var (
		u         User
		createdAt string
		updatedAt string
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, updated_at
		 FROM users ORDER BY id LIMIT 1`,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAt, &updatedAt)
	if err != nil {
		return User{}, wrapNotFound(fmt.Errorf("store: load user: %w", err))
	}

	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return User{}, err
	}
	if u.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return User{}, err
	}
	return u, nil
}

// UpdatePasswordHash replaces the stored password hash.
func (s *Store) UpdatePasswordHash(ctx context.Context, userID int64, passwordHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, formatTime(time.Now()), userID)
	if err != nil {
		return fmt.Errorf("store: update password hash: %w", err)
	}
	return requireAffected(res, "user")
}
