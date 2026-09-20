// Package auth owns the panel's single account and the master key.
//
// Two pieces of state live here and they have deliberately different lifetimes:
//
//   - The session. A cookie-backed row in SQLite with a sliding expiry. It
//     answers "is this browser allowed to talk to the panel".
//
//   - The master key. Derived from the user's password, held in process memory,
//     used to decrypt stored third-party credentials. It answers "may we act on
//     the user's behalf against GitHub and the cloud registry".
//
// They are decoupled on purpose. Background work — the mirror sync, the speed
// probe — needs the master key but has no browser session, so tying the key's
// lifetime to a session would silently break syncing the moment a tab closed or
// a cookie expired. The key therefore lives as long as the process: a login
// unlocks it, and only restarting the container locks it again, at which point
// the UI asks for the password once to restore background work.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/DC1024/mirrorpilot/internal/secret"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// SessionCookieName is the cookie the panel sets to carry a session ID.
const SessionCookieName = "mirrorpilot_session"

var (
	// ErrNotSetup means the panel has no account yet; the UI should show the
	// first-run form instead of a login form.
	ErrNotSetup = errors.New("auth: panel is not set up")

	// ErrAlreadySetup means setup was attempted twice. Not an internal error:
	// the setup handler turns it into a redirect.
	ErrAlreadySetup = errors.New("auth: panel is already set up")

	// ErrInvalidCredentials covers a wrong username *and* a wrong password.
	// Distinguishing them would tell an attacker which half to keep guessing.
	ErrInvalidCredentials = errors.New("auth: invalid username or password")

	// ErrLocked means the master key is not in memory, so sealed credentials
	// cannot be read or written. Callers should prompt to unlock.
	ErrLocked = errors.New("auth: master key is locked")

	// ErrSessionExpired covers an unknown, expired, or revoked session.
	ErrSessionExpired = errors.New("auth: session is not valid")

	// ErrWeakPassword means the chosen password violates the length policy.
	ErrWeakPassword = errors.New("auth: password does not meet the policy")
)

const (
	// MinPasswordLength is a floor, not a suggestion. This password is the
	// only thing standing between a stolen /data volume and every credential
	// in it, so a short one is a real weakness rather than a nitpick.
	MinPasswordLength = 8

	// MaxPasswordLength bounds the work an unauthenticated request can ask
	// Argon2id to do. The login form is reachable before authentication, so
	// without a cap a megabyte "password" is a cheap denial of service.
	MaxPasswordLength = 200

	// DefaultSessionTTL is how long a browser stays logged in without use.
	DefaultSessionTTL = 30 * 24 * time.Hour

	// sessionIDBytes of CSPRNG output. 32 bytes is far beyond guessing range;
	// the ID is stored verbatim because hashing a value with this much entropy
	// would buy nothing.
	sessionIDBytes = 32
)

// Config tunes a Manager. The zero value is usable.
type Config struct {
	// SessionTTL overrides DefaultSessionTTL.
	SessionTTL time.Duration

	// Now overrides the clock. Tests use it; production should not.
	Now func() time.Time
}

// Manager holds the account logic and the in-memory master key.
//
// It is safe for concurrent use: HTTP handlers share one Manager, and the
// background syncer runs alongside them.
type Manager struct {
	store *store.Store
	ttl   time.Duration
	now   func() time.Time

	// sealer is non-nil exactly when the panel is unlocked. The derived key
	// itself is deliberately not retained: the only thing we ever needed it
	// for is building the AES schedule, which the cipher keeps internally.
	// Fewer copies of secret material in memory is the whole point.
	mu     sync.RWMutex
	sealer *secret.Sealer
}

// New builds a Manager over an open store.
func New(st *store.Store, cfg Config) *Manager {
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{store: st, ttl: ttl, now: now}
}

// Status is what the landing page needs to decide what to render.
type Status struct {
	// Setup is false on a brand-new install.
	Setup bool

	// Unlocked reports whether the master key is in memory. A panel can be
	// logged in but locked, which is the state right after a container
	// restart while the browser still holds a valid cookie.
	Unlocked bool
}

// Status reports the setup and unlock state.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	setup, err := m.store.HasUser(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{Setup: setup, Unlocked: m.Unlocked()}, nil
}

// Unlocked reports whether the master key is available.
func (m *Manager) Unlocked() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sealer != nil
}

// Lock forgets the master key, so sealed credentials become unreadable until
// the next unlock. Existing sessions are untouched: the user is still logged in,
// they just cannot reach stored credentials.
//
// Note that this is best-effort as a memory-hygiene measure. The expanded AES
// key schedule inside the cipher is not reachable from here and Go's GC may
// have copied the derived key on the way in. What Lock reliably gives you is
// that no further decryption happens without the password.
func (m *Manager) Lock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sealer = nil
}

// Sealer returns the cipher used for stored credentials, or ErrLocked.
func (m *Manager) Sealer() (*secret.Sealer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.sealer == nil {
		return nil, ErrLocked
	}
	return m.sealer, nil
}

// installKey promotes a derived key to the live one.
func (m *Manager) installKey(key []byte) error {
	sealer, err := secret.NewSealer(key)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sealer = sealer
	return nil
}

// unlock derives the master key from the stored salt and installs it.
//
// The caller must have verified the password already; this function does not
// look at the password hash. It does check the resulting key against the stored
// fingerprint, which is what distinguishes "the user typed the wrong password"
// from "the vault row was tampered with".
func (m *Manager) unlock(ctx context.Context, password string) error {
	v, err := m.store.Vault(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotSetup
	}
	if err != nil {
		return err
	}

	key := secret.DeriveKey(password, v.KDFSalt)
	if subtle.ConstantTimeCompare([]byte(secret.Fingerprint(key)), []byte(v.KeyFingerprint)) != 1 {
		// The password already checked out against the stored hash, so a
		// fingerprint mismatch here means the vault is inconsistent with the
		// account — not a typo.
		return fmt.Errorf("auth: vault fingerprint does not match the derived key")
	}

	return m.installKey(key)
}

// verifyHash checks a password against a stored hash, mapping the two failure
// modes onto this package's vocabulary.
//
// A mismatch is an ordinary wrong guess. A malformed hash is corruption, and
// saying so beats telling the user their correct password is wrong.
func verifyHash(hash, password string) error {
	if err := secret.VerifyPassword(hash, password); err != nil {
		if errors.Is(err, secret.ErrMismatch) {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("auth: stored password hash is unusable: %w", err)
	}
	return nil
}

// checkPassword verifies a password against the stored account hash.
func (m *Manager) checkPassword(ctx context.Context, password string) error {
	u, err := m.store.User(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotSetup
	}
	if err != nil {
		return err
	}
	return verifyHash(u.PasswordHash, password)
}

// SweepSessions prunes expired session rows. Cheap enough to run periodically;
// it keeps the table from growing on a panel that is never logged into again.
func (m *Manager) SweepSessions(ctx context.Context) (int64, error) {
	return m.store.DeleteExpiredSessions(ctx, m.now())
}
