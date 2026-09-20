package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/DC1024/mirrorpilot/internal/secret"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// Session is a live browser session, as handed back to the web layer so it can
// set the cookie.
type Session struct {
	ID        string
	ExpiresAt time.Time
}

// newSessionID returns a fresh CSPRNG session identifier.
func newSessionID() (string, error) {
	buf := make([]byte, sessionIDBytes)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: read session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Setup creates the account, generates the vault salt, and unlocks in one go.
//
// Because the master key is derived from this same password, finishing setup
// unlocks the panel immediately — there is no reason to make the user log in
// with a password they just typed.
func (m *Manager) Setup(ctx context.Context, username, password string) (Session, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return Session{}, fmt.Errorf("auth: username must not be empty")
	}
	if err := ValidatePassword(password); err != nil {
		return Session{}, err
	}

	// Checking first gives a clean ErrAlreadySetup. It is not what makes the
	// second call safe — that is the CHECK (id = 1) constraint, which holds
	// even if two setup requests race past this read.
	setup, err := m.store.HasUser(ctx)
	if err != nil {
		return Session{}, err
	}
	if setup {
		return Session{}, ErrAlreadySetup
	}

	hash, err := secret.HashPassword(password)
	if err != nil {
		return Session{}, err
	}

	salt, err := secret.NewSalt()
	if err != nil {
		return Session{}, err
	}

	key := secret.DeriveKey(password, salt)

	// Account and vault go in together: either half alone is unusable, and
	// neither table accepts a second row, so a partial write could not be
	// repaired by retrying.
	if _, err := m.store.Bootstrap(ctx, username, hash, salt, secret.Fingerprint(key)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Session{}, ErrNotSetup
		}
		// A constraint violation means someone else won the race.
		if strings.Contains(err.Error(), "constraint") {
			return Session{}, ErrAlreadySetup
		}
		return Session{}, err
	}

	if err := m.installKey(key); err != nil {
		return Session{}, err
	}

	return m.issue(ctx)
}

// Login verifies the credentials, unlocks the master key, and starts a session.
func (m *Manager) Login(ctx context.Context, username, password string) (Session, error) {
	if err := m.verifyCredentials(ctx, username, password); err != nil {
		return Session{}, err
	}

	// The password is already verified, so this is purely the key derivation.
	if err := m.unlock(ctx, password); err != nil {
		if errors.Is(err, ErrNotSetup) {
			// The account exists but its vault row does not. Logging in anyway
			// would present a panel that looks fine and cannot read a single
			// stored credential, so fail loudly instead.
			return Session{}, fmt.Errorf("auth: account exists but the vault row is missing")
		}
		return Session{}, err
	}

	return m.issue(ctx)
}

// Unlock installs the master key without creating a session.
//
// This is the post-restart path. The browser still holds a valid session cookie,
// so the user is not asked to log in again — they are asked for the password
// once to restore the credentials background work depends on.
func (m *Manager) Unlock(ctx context.Context, password string) error {
	if err := m.checkPassword(ctx, password); err != nil {
		return err
	}
	return m.unlock(ctx, password)
}

// Authenticate validates a session ID and slides its expiry forward.
//
// Sliding rather than fixed: an actively used panel should not log the user out
// mid-session. The cost is that a stolen cookie stays valid for as long as it
// keeps being used, which is what Lock and the password-change sweep are for.
func (m *Manager) Authenticate(ctx context.Context, id string) (Session, error) {
	if id == "" {
		return Session{}, ErrSessionExpired
	}

	sess, err := m.store.Session(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Session{}, ErrSessionExpired
	}
	if err != nil {
		return Session{}, err
	}

	now := m.now()
	if !now.Before(sess.ExpiresAt) {
		// Reap on the way past, so an abandoned browser does not leave a dead
		// row behind for the sweeper to find much later.
		if err := m.store.DeleteSession(ctx, id); err != nil {
			return Session{}, err
		}
		return Session{}, ErrSessionExpired
	}

	expires := now.Add(m.ttl)
	if err := m.store.TouchSession(ctx, id, expires, now); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Session{}, ErrSessionExpired
		}
		return Session{}, err
	}

	return Session{ID: id, ExpiresAt: expires}, nil
}

// Logout drops one session. It is idempotent: logging out twice is not an error.
func (m *Manager) Logout(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	return m.store.DeleteSession(ctx, id)
}

// issue creates and stores a session row.
func (m *Manager) issue(ctx context.Context) (Session, error) {
	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}

	now := m.now()
	sess := Session{ID: id, ExpiresAt: now.Add(m.ttl)}

	if err := m.store.CreateSession(ctx, store.Session{
		ID:        id,
		CreatedAt: now,
		ExpiresAt: sess.ExpiresAt,
		LastSeen:  now,
	}); err != nil {
		return Session{}, err
	}

	return sess, nil
}

// verifyCredentials checks the username and password pair.
func (m *Manager) verifyCredentials(ctx context.Context, username, password string) error {
	u, err := m.store.User(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotSetup
	}
	if err != nil {
		return err
	}

	if subtle.ConstantTimeCompare([]byte(u.Username), []byte(username)) != 1 {
		return ErrInvalidCredentials
	}

	return verifyHash(u.PasswordHash, password)
}
