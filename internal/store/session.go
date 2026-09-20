package store

import (
	"context"
	"fmt"
	"time"
)

// Session is an authenticated browser session.
//
// The ID is the only thing the client holds. It is generated from a CSPRNG and
// stored verbatim; because it is high-entropy random, hashing it on disk would
// buy little.
type Session struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, expires_at, last_seen) VALUES (?, ?, ?, ?)`,
		sess.ID, formatTime(sess.CreatedAt), formatTime(sess.ExpiresAt), formatTime(sess.LastSeen))
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// Session looks up a session by ID. ErrNotFound covers both "never existed" and
// "already cleaned up"; callers treat both as unauthenticated.
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	var (
		sess      Session
		createdAt string
		expiresAt string
		lastSeen  string
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, expires_at, last_seen FROM sessions WHERE id = ?`,
		id).Scan(&sess.ID, &createdAt, &expiresAt, &lastSeen)
	if err != nil {
		return Session{}, wrapNotFound(fmt.Errorf("store: load session: %w", err))
	}

	if sess.CreatedAt, err = parseTime(createdAt); err != nil {
		return Session{}, err
	}
	if sess.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return Session{}, err
	}
	if sess.LastSeen, err = parseTime(lastSeen); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// TouchSession slides the expiry window forward.
func (s *Store) TouchSession(ctx context.Context, id string, expiresAt, lastSeen time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET expires_at = ?, last_seen = ? WHERE id = ?`,
		formatTime(expiresAt), formatTime(lastSeen), id)
	if err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	return requireAffected(res, "session")
}

// DeleteSession removes one session. Missing is fine.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// DeleteAllSessions invalidates every session and reports how many were removed.
//
// Called after a password change: changing the password should log out anyone
// else holding a session, and "how many were kicked" is worth showing the user.
func (s *Store) DeleteAllSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions`)
	if err != nil {
		return 0, fmt.Errorf("store: delete all sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count deleted sessions: %w", err)
	}
	return n, nil
}

// DeleteExpiredSessions prunes sessions past their expiry.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count expired sessions: %w", err)
	}
	return n, nil
}
