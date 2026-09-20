package auth

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/secret"
	"github.com/DC1024/mirrorpilot/internal/store"
)

const (
	testUser     = "dc"
	testPassword = "correct-horse"
	testNewPass  = "battery-staple"
)

// clock is a hand-cranked replacement for time.Now, so expiry tests do not have
// to sleep.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestManager returns a Manager over a fresh database, plus the store so
// tests can poke at rows the Manager does not expose.
func newTestManager(t *testing.T, c *clock) (*Manager, *store.Store) {
	t.Helper()

	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	return New(st, Config{SessionTTL: time.Hour, Now: c.now}), st
}

// mustSetup runs setup and fails the test if it does not succeed.
func mustSetup(t *testing.T, m *Manager) Session {
	t.Helper()

	sess, err := m.Setup(context.Background(), testUser, testPassword)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return sess
}

func TestStatusOnFreshPanel(t *testing.T) {
	m, _ := newTestManager(t, newClock())

	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Setup {
		t.Error("Setup = true on a fresh panel")
	}
	if st.Unlocked {
		t.Error("Unlocked = true on a fresh panel")
	}
}

func TestSetupCreatesAccountAndUnlocks(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())

	sess := mustSetup(t, m)

	if sess.ID == "" {
		t.Error("Setup returned an empty session ID")
	}
	if !sess.ExpiresAt.After(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("ExpiresAt = %s, want in the future", sess.ExpiresAt)
	}

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Setup || !st.Unlocked {
		t.Errorf("Status = %+v, want setup and unlocked", st)
	}

	// Setup also creates the session, so it should be usable straight away.
	if _, err := m.Authenticate(ctx, sess.ID); err != nil {
		t.Errorf("Authenticate after Setup: %v", err)
	}
}

func TestSetupRejectsSecondCall(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	mustSetup(t, m)

	if _, err := m.Setup(ctx, "intruder", testPassword); !errors.Is(err, ErrAlreadySetup) {
		t.Errorf("second Setup = %v, want ErrAlreadySetup", err)
	}

	// The original credentials must still work.
	if _, err := m.Login(ctx, testUser, testPassword); err != nil {
		t.Errorf("Login with the original password: %v", err)
	}
}

func TestSetupRejectsWeakPassword(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())

	if _, err := m.Setup(ctx, testUser, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("Setup with a short password = %v, want ErrWeakPassword", err)
	}

	// A rejected setup must not half-create the panel.
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Setup {
		t.Error("a rejected setup left the panel marked as set up")
	}
}

func TestSetupRejectsEmptyUsername(t *testing.T) {
	m, _ := newTestManager(t, newClock())

	if _, err := m.Setup(context.Background(), "   ", testPassword); err == nil {
		t.Fatal("Setup with a blank username succeeded, want an error")
	}
}

func TestSetupAcceptsExactlyMinimumLength(t *testing.T) {
	m, _ := newTestManager(t, newClock())

	pw := string(bytes.Repeat([]byte("x"), MinPasswordLength))
	if _, err := m.Setup(context.Background(), testUser, pw); err != nil {
		t.Errorf("Setup with a %d-character password: %v", MinPasswordLength, err)
	}
}

func TestSetupBeforeLoginWorksAndLoginBeforeSetupDoesNot(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())

	if _, err := m.Login(ctx, testUser, testPassword); !errors.Is(err, ErrNotSetup) {
		t.Errorf("Login before Setup = %v, want ErrNotSetup", err)
	}

	mustSetup(t, m)

	if _, err := m.Login(ctx, testUser, testPassword); err != nil {
		t.Errorf("Login after Setup: %v", err)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	mustSetup(t, m)

	cases := map[string]struct{ user, pass string }{
		"wrong password": {testUser, "not-the-password"},
		"wrong username": {"someone-else", testPassword},
		"both wrong":     {"someone-else", "not-the-password"},
		"empty":          {"", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Login(ctx, c.user, c.pass); !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("Login = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

func TestAuthenticateSlidesExpiry(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	m, _ := newTestManager(t, c)
	sess := mustSetup(t, m)

	// Half way through the window: still valid, and the window moves forward.
	c.advance(30 * time.Minute)
	slid, err := m.Authenticate(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := c.now().Add(time.Hour)
	if !slid.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", slid.ExpiresAt, want)
	}

	// Past the original expiry but inside the slid window: still valid. This
	// is what makes an actively used panel stay logged in.
	c.advance(40 * time.Minute)
	if _, err := m.Authenticate(ctx, sess.ID); err != nil {
		t.Errorf("Authenticate inside the slid window: %v", err)
	}
}

func TestAuthenticateRejectsInvalidSessions(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	m, _ := newTestManager(t, c)
	sess := mustSetup(t, m)

	if _, err := m.Authenticate(ctx, ""); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("Authenticate with an empty id = %v, want ErrSessionExpired", err)
	}
	if _, err := m.Authenticate(ctx, "made-up"); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("Authenticate with an unknown id = %v, want ErrSessionExpired", err)
	}

	// Well past even a slid window, since nothing touched it in between.
	c.advance(2 * time.Hour)
	if _, err := m.Authenticate(ctx, sess.ID); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("Authenticate after expiry = %v, want ErrSessionExpired", err)
	}
}

// An expired row should be reaped rather than left for the sweeper.
func TestAuthenticateDeletesExpiredSession(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	m, st := newTestManager(t, c)
	sess := mustSetup(t, m)

	c.advance(2 * time.Hour)
	if _, err := m.Authenticate(ctx, sess.ID); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Authenticate after expiry = %v, want ErrSessionExpired", err)
	}

	if _, err := st.Session(ctx, sess.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the expired session row survived: %v", err)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	sess := mustSetup(t, m)

	if err := m.Logout(ctx, sess.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if err := m.Logout(ctx, sess.ID); err != nil {
		t.Errorf("second Logout = %v, want nil", err)
	}
	if err := m.Logout(ctx, ""); err != nil {
		t.Errorf("Logout with an empty id = %v, want nil", err)
	}
	if _, err := m.Authenticate(ctx, sess.ID); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("Authenticate after Logout = %v, want ErrSessionExpired", err)
	}
}

func TestSweepSessions(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	m, _ := newTestManager(t, c)

	// Three sessions at different ages, all with a one-hour window.
	stale := mustSetup(t, m)
	c.advance(30 * time.Minute)
	alsoStale, err := m.Login(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	c.advance(45 * time.Minute)
	fresh, err := m.Login(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// The first two expired at +1h and +1h30m; it is now +1h15m, so only the
	// first has lapsed.
	n, err := m.SweepSessions(ctx)
	if err != nil {
		t.Fatalf("SweepSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("SweepSessions removed %d sessions, want 1", n)
	}

	if _, err := m.Authenticate(ctx, stale.ID); !errors.Is(err, ErrSessionExpired) {
		t.Error("the swept session is still valid")
	}
	if _, err := m.Authenticate(ctx, alsoStale.ID); err != nil {
		t.Errorf("a live session was swept: %v", err)
	}
	if _, err := m.Authenticate(ctx, fresh.ID); err != nil {
		t.Errorf("a live session was swept: %v", err)
	}
}

func TestLockAndUnlock(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	sess := mustSetup(t, m)

	if err := m.SaveCredential(ctx, store.CredentialGitHubToken, []byte("ghp_secret")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	m.Lock()

	if m.Unlocked() {
		t.Error("Unlocked = true after Lock")
	}
	if _, err := m.Sealer(); !errors.Is(err, ErrLocked) {
		t.Errorf("Sealer after Lock = %v, want ErrLocked", err)
	}
	if _, err := m.Credential(ctx, store.CredentialGitHubToken); !errors.Is(err, ErrLocked) {
		t.Errorf("Credential after Lock = %v, want ErrLocked", err)
	}

	// Locking must not touch the session: the user stays logged in, they just
	// cannot reach stored credentials.
	if _, err := m.Authenticate(ctx, sess.ID); err != nil {
		t.Errorf("Authenticate while locked: %v", err)
	}

	// The dashboard needs to show which slots are filled even when locked.
	configured, err := m.ConfiguredCredentials(ctx)
	if err != nil {
		t.Fatalf("ConfiguredCredentials while locked: %v", err)
	}
	if !configured[store.CredentialGitHubToken] {
		t.Error("ConfiguredCredentials missed a stored credential while locked")
	}

	if err := m.Unlock(ctx, "not-the-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Unlock with a wrong password = %v, want ErrInvalidCredentials", err)
	}
	if m.Unlocked() {
		t.Error("a failed Unlock left the panel unlocked")
	}

	if err := m.Unlock(ctx, testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	got, err := m.Credential(ctx, store.CredentialGitHubToken)
	if err != nil {
		t.Fatalf("Credential after Unlock: %v", err)
	}
	if string(got) != "ghp_secret" {
		t.Errorf("credential = %q, want ghp_secret", got)
	}
}

// A restart is a fresh Manager over an existing database: locked, with the
// browser's cookie still valid.
func TestRestartKeepsSessionsButLosesTheKey(t *testing.T) {
	ctx := context.Background()
	c := newClock()

	dir := t.TempDir()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	before := New(st, Config{SessionTTL: time.Hour, Now: c.now})
	sess := mustSetup(t, before)
	if err := before.SaveCredential(ctx, store.CredentialACRPassword, []byte("acr-pw")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	st2, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("store.Open after restart: %v", err)
	}
	defer func() { _ = st2.Close() }()

	after := New(st2, Config{SessionTTL: time.Hour, Now: c.now})

	if after.Unlocked() {
		t.Fatal("a restarted panel came up unlocked")
	}

	// The cookie still works, so the user is not asked to log in again...
	if _, err := after.Authenticate(ctx, sess.ID); err != nil {
		t.Errorf("Authenticate after restart: %v", err)
	}

	// ...but the credential is unreadable until they unlock once.
	if _, err := after.Credential(ctx, store.CredentialACRPassword); !errors.Is(err, ErrLocked) {
		t.Errorf("Credential after restart = %v, want ErrLocked", err)
	}
	if err := after.Unlock(ctx, testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	got, err := after.Credential(ctx, store.CredentialACRPassword)
	if err != nil {
		t.Fatalf("Credential after Unlock: %v", err)
	}
	if string(got) != "acr-pw" {
		t.Errorf("credential = %q, want acr-pw", got)
	}

	// And the panel is still set up, so the UI shows login rather than setup.
	status, err := after.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Setup {
		t.Error("Setup = false after restart")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	mustSetup(t, m)

	if _, err := m.Credential(ctx, store.CredentialGitHubToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Credential before save = %v, want store.ErrNotFound", err)
	}

	// Multi-byte and binary payloads must survive intact.
	payload := []byte("ghp_\u00e9\u4e2d\u6587\x00\xff")
	if err := m.SaveCredential(ctx, store.CredentialGitHubToken, payload); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	got, err := m.Credential(ctx, store.CredentialGitHubToken)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("credential = %q, want %q", got, payload)
	}

	// Saving again replaces rather than duplicating.
	if err := m.SaveCredential(ctx, store.CredentialGitHubToken, []byte("second")); err != nil {
		t.Fatalf("SaveCredential (update): %v", err)
	}
	got, err = m.Credential(ctx, store.CredentialGitHubToken)
	if err != nil {
		t.Fatalf("Credential after update: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("credential after update = %q, want second", got)
	}

	if err := m.DeleteCredential(ctx, store.CredentialGitHubToken); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, err := m.Credential(ctx, store.CredentialGitHubToken); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Credential after delete = %v, want store.ErrNotFound", err)
	}
}

// The ciphertext is bound to its slot name, so relocating a blob must fail to
// decrypt rather than hand the wrong token to the wrong API.
func TestCredentialCiphertextIsBoundToItsSlot(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t, newClock())
	mustSetup(t, m)

	if err := m.SaveCredential(ctx, store.CredentialGitHubToken, []byte("ghp_secret")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	blob, err := st.Credential(ctx, store.CredentialGitHubToken)
	if err != nil {
		t.Fatalf("store.Credential: %v", err)
	}
	if err := st.SaveCredential(ctx, store.CredentialACRPassword, blob); err != nil {
		t.Fatalf("SaveCredential under the wrong name: %v", err)
	}

	_, err = m.Credential(ctx, store.CredentialACRPassword)
	if err == nil {
		t.Fatal("a credential relocated to another slot decrypted successfully")
	}
	if !errors.Is(err, secret.ErrDecrypt) {
		t.Errorf("error = %v, want it to wrap secret.ErrDecrypt", err)
	}
}

func TestCredentialWriteRequiresUnlock(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	mustSetup(t, m)
	m.Lock()

	if err := m.SaveCredential(ctx, store.CredentialGitHubToken, []byte("x")); !errors.Is(err, ErrLocked) {
		t.Errorf("SaveCredential while locked = %v, want ErrLocked", err)
	}
}

func TestChangePasswordReencryptsAndKicksSessions(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	current := mustSetup(t, m)

	// A second session stands in for another browser.
	other, err := m.Login(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	secrets := map[string]string{
		store.CredentialGitHubToken: "ghp_secret",
		store.CredentialACRPassword: "acr-secret",
	}
	for name, value := range secrets {
		if err := m.SaveCredential(ctx, name, []byte(value)); err != nil {
			t.Fatalf("SaveCredential %q: %v", name, err)
		}
	}

	kicked, err := m.ChangePassword(ctx, testPassword, testNewPass)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if kicked != 2 {
		t.Errorf("kicked %d sessions, want 2", kicked)
	}

	// Both sessions are gone.
	for _, id := range []string{current.ID, other.ID} {
		if _, err := m.Authenticate(ctx, id); !errors.Is(err, ErrSessionExpired) {
			t.Errorf("session %s survived a password change: %v", id, err)
		}
	}

	// Every credential is readable under the new key, unchanged.
	for name, want := range secrets {
		got, err := m.Credential(ctx, name)
		if err != nil {
			t.Fatalf("Credential %q after rekey: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("credential %q = %q, want %q", name, got, want)
		}
	}

	// The old password is dead, the new one works.
	if _, err := m.Login(ctx, testUser, testPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login with the old password = %v, want ErrInvalidCredentials", err)
	}
	if _, err := m.Login(ctx, testUser, testNewPass); err != nil {
		t.Errorf("Login with the new password: %v", err)
	}
}

// A locked panel cannot re-encrypt anything, and must say so rather than
// rewriting the salt and destroying access to what it could not read.
func TestChangePasswordRequiresUnlock(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	mustSetup(t, m)
	if err := m.SaveCredential(ctx, store.CredentialGitHubToken, []byte("ghp_secret")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	m.Lock()

	if _, err := m.ChangePassword(ctx, testPassword, testNewPass); !errors.Is(err, ErrLocked) {
		t.Errorf("ChangePassword while locked = %v, want ErrLocked", err)
	}

	// The original password and the original ciphertext must both still work.
	if err := m.Unlock(ctx, testPassword); err != nil {
		t.Fatalf("Unlock with the original password: %v", err)
	}
	got, err := m.Credential(ctx, store.CredentialGitHubToken)
	if err != nil {
		t.Fatalf("Credential after a refused ChangePassword: %v", err)
	}
	if string(got) != "ghp_secret" {
		t.Errorf("credential = %q, want ghp_secret", got)
	}
}

func TestChangePasswordRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, newClock())
	mustSetup(t, m)

	if _, err := m.ChangePassword(ctx, "wrong-current", testNewPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("ChangePassword with a wrong current password = %v, want ErrInvalidCredentials", err)
	}
	if _, err := m.ChangePassword(ctx, testPassword, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("ChangePassword to a short password = %v, want ErrWeakPassword", err)
	}

	// Neither refusal may have changed anything.
	if _, err := m.Login(ctx, testUser, testPassword); err != nil {
		t.Errorf("Login with the original password: %v", err)
	}
}

func TestValidatePasswordBounds(t *testing.T) {
	if err := ValidatePassword(string(bytes.Repeat([]byte("x"), MinPasswordLength))); err != nil {
		t.Errorf("minimum length = %v, want nil", err)
	}
	if err := ValidatePassword(string(bytes.Repeat([]byte("x"), MinPasswordLength-1))); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("one under the minimum = %v, want ErrWeakPassword", err)
	}
	if err := ValidatePassword(string(bytes.Repeat([]byte("x"), MaxPasswordLength))); err != nil {
		t.Errorf("maximum length = %v, want nil", err)
	}
	if err := ValidatePassword(string(bytes.Repeat([]byte("x"), MaxPasswordLength+1))); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("one over the maximum = %v, want ErrWeakPassword", err)
	}
}

func TestNewSessionIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 128)
	for i := 0; i < 128; i++ {
		id, err := newSessionID()
		if err != nil {
			t.Fatalf("newSessionID: %v", err)
		}
		if seen[id] {
			t.Fatalf("newSessionID returned a duplicate after %d draws", i)
		}
		seen[id] = true
	}
}
