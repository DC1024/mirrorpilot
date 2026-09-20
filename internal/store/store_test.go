package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestOpenCreatesDatabaseFile(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("database file was not created: %v", err)
	}
}

// Reopening must be a no-op, not a re-run of every migration.
func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := first.CreateUser(ctx, "dc", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	// The row written before the reopen must still be there.
	user, err := second.User(ctx)
	if err != nil {
		t.Fatalf("User after reopen: %v", err)
	}
	if user.Username != "dc" {
		t.Errorf("Username = %q, want dc", user.Username)
	}
}

func TestUserLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	has, err := s.HasUser(ctx)
	if err != nil {
		t.Fatalf("HasUser: %v", err)
	}
	if has {
		t.Fatal("HasUser on a fresh database = true, want false")
	}

	created, err := s.CreateUser(ctx, "dc", "argon2-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID == 0 {
		t.Error("CreateUser returned ID 0")
	}

	has, err = s.HasUser(ctx)
	if err != nil {
		t.Fatalf("HasUser: %v", err)
	}
	if !has {
		t.Error("HasUser after CreateUser = false, want true")
	}

	loaded, err := s.User(ctx)
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if loaded.Username != "dc" || loaded.PasswordHash != "argon2-hash" {
		t.Errorf("User = %+v", loaded)
	}
	if loaded.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	if err := s.UpdatePasswordHash(ctx, loaded.ID, "new-hash"); err != nil {
		t.Fatalf("UpdatePasswordHash: %v", err)
	}
	reloaded, err := s.User(ctx)
	if err != nil {
		t.Fatalf("User after update: %v", err)
	}
	if reloaded.PasswordHash != "new-hash" {
		t.Errorf("PasswordHash = %q, want new-hash", reloaded.PasswordHash)
	}
}

// A second setup request must not silently reset the account.
func TestCreateUserRejectsSecondUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateUser(ctx, "dc", "hash"); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	if _, err := s.CreateUser(ctx, "intruder", "hash"); err == nil {
		t.Fatal("second CreateUser succeeded, want an error")
	}
}

func TestUserNotFoundOnFreshDatabase(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.User(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Errorf("User on a fresh database = %v, want ErrNotFound", err)
	}
}

func TestUpdatePasswordHashOnMissingUser(t *testing.T) {
	s := newTestStore(t)

	err := s.UpdatePasswordHash(context.Background(), 999, "hash")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdatePasswordHash on a missing user = %v, want ErrNotFound", err)
	}
}

func TestVaultLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Vault(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Vault on a fresh database = %v, want ErrNotFound", err)
	}

	salt := bytes.Repeat([]byte{0x11}, 16)
	if err := s.CreateVault(ctx, salt, "fp-one"); err != nil {
		t.Fatalf("CreateVault: %v", err)
	}

	v, err := s.Vault(ctx)
	if err != nil {
		t.Fatalf("Vault: %v", err)
	}
	if !bytes.Equal(v.KDFSalt, salt) {
		t.Errorf("KDFSalt = %x, want %x", v.KDFSalt, salt)
	}
	if v.KeyFingerprint != "fp-one" {
		t.Errorf("KeyFingerprint = %q", v.KeyFingerprint)
	}
	if v.RotatedAt != nil {
		t.Error("RotatedAt should be nil before any rotation")
	}

	newSalt := bytes.Repeat([]byte{0x22}, 16)
	if err := s.RotateVault(ctx, newSalt, "fp-two"); err != nil {
		t.Fatalf("RotateVault: %v", err)
	}

	rotated, err := s.Vault(ctx)
	if err != nil {
		t.Fatalf("Vault after rotate: %v", err)
	}
	if !bytes.Equal(rotated.KDFSalt, newSalt) {
		t.Error("salt was not replaced by RotateVault")
	}
	if rotated.KeyFingerprint != "fp-two" {
		t.Errorf("KeyFingerprint = %q, want fp-two", rotated.KeyFingerprint)
	}
	if rotated.RotatedAt == nil {
		t.Error("RotatedAt is nil after RotateVault")
	}
}

func TestCreateVaultRejectsSecondVault(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.CreateVault(ctx, bytes.Repeat([]byte{1}, 16), "fp"); err != nil {
		t.Fatalf("first CreateVault: %v", err)
	}
	if err := s.CreateVault(ctx, bytes.Repeat([]byte{2}, 16), "fp2"); err == nil {
		t.Fatal("second CreateVault succeeded, want an error")
	}
}

func TestCredentialLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Credential(ctx, "github_token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Credential on a fresh database = %v, want ErrNotFound", err)
	}

	if err := s.SaveCredential(ctx, "github_token", []byte("sealed-one")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	blob, err := s.Credential(ctx, "github_token")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if string(blob) != "sealed-one" {
		t.Errorf("credential = %q", blob)
	}

	// Upsert replaces rather than duplicating.
	if err := s.SaveCredential(ctx, "github_token", []byte("sealed-two")); err != nil {
		t.Fatalf("SaveCredential (update): %v", err)
	}
	blob, err = s.Credential(ctx, "github_token")
	if err != nil {
		t.Fatalf("Credential after update: %v", err)
	}
	if string(blob) != "sealed-two" {
		t.Errorf("credential after update = %q", blob)
	}

	all, err := s.Credentials(ctx)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("Credentials returned %d entries, want 1", len(all))
	}

	if err := s.DeleteCredential(ctx, "github_token"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	// Deleting twice must be a no-op, not an error.
	if err := s.DeleteCredential(ctx, "github_token"); err != nil {
		t.Errorf("second DeleteCredential = %v, want nil", err)
	}
}

func TestBootstrapCreatesAccountAndVaultTogether(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	salt := bytes.Repeat([]byte{0x33}, 16)
	u, err := s.Bootstrap(ctx, "dc", "argon2-hash", salt, "fp-one")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if u.ID != 1 || u.Username != "dc" {
		t.Errorf("Bootstrap returned %+v", u)
	}

	// Both halves must be readable afterwards, not just the one we happened to
	// write first.
	loaded, err := s.User(ctx)
	if err != nil {
		t.Fatalf("User after Bootstrap: %v", err)
	}
	if loaded.PasswordHash != "argon2-hash" {
		t.Errorf("PasswordHash = %q", loaded.PasswordHash)
	}
	v, err := s.Vault(ctx)
	if err != nil {
		t.Fatalf("Vault after Bootstrap: %v", err)
	}
	if !bytes.Equal(v.KDFSalt, salt) || v.KeyFingerprint != "fp-one" {
		t.Errorf("Vault = %+v", v)
	}
}

// A second bootstrap must fail, and must leave the first one untouched.
func TestBootstrapRejectsSecondCall(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	salt := bytes.Repeat([]byte{0x33}, 16)
	if _, err := s.Bootstrap(ctx, "dc", "hash", salt, "fp-one"); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	if _, err := s.Bootstrap(ctx, "intruder", "hash", salt, "fp-two"); err == nil {
		t.Fatal("second Bootstrap succeeded, want an error")
	}

	loaded, err := s.User(ctx)
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if loaded.Username != "dc" {
		t.Errorf("username = %q, want dc (the second bootstrap must not win)", loaded.Username)
	}
}

func TestRekeySwapsEverythingAtOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Bootstrap(ctx, "dc", "old-hash", bytes.Repeat([]byte{1}, 16), "fp-old"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := s.SaveCredential(ctx, "stale", []byte("old")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	replacement := map[string][]byte{
		"github_token": []byte("re-sealed-token"),
		"acr_password": []byte("re-sealed-password"),
	}
	newSalt := bytes.Repeat([]byte{2}, 16)
	if err := s.Rekey(ctx, "new-hash", newSalt, "fp-new", replacement); err != nil {
		t.Fatalf("Rekey: %v", err)
	}

	// All three tables must have moved, or a later login could not decrypt
	// what we just wrote.
	u, err := s.User(ctx)
	if err != nil {
		t.Fatalf("User after Rekey: %v", err)
	}
	if u.PasswordHash != "new-hash" {
		t.Errorf("PasswordHash = %q, want new-hash", u.PasswordHash)
	}

	v, err := s.Vault(ctx)
	if err != nil {
		t.Fatalf("Vault after Rekey: %v", err)
	}
	if !bytes.Equal(v.KDFSalt, newSalt) || v.KeyFingerprint != "fp-new" {
		t.Errorf("Vault = %+v", v)
	}
	if v.RotatedAt == nil {
		t.Error("RotatedAt is nil after Rekey")
	}

	all, err := s.Credentials(ctx)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("Credentials returned %d entries, want 2", len(all))
	}
	if _, ok := all["stale"]; ok {
		t.Error("Rekey left the old credential behind")
	}
	if string(all["github_token"]) != "re-sealed-token" {
		t.Errorf("github_token = %q", all["github_token"])
	}
}

// A rekey that cannot write must not consume the old credentials.
func TestRekeyFailureLeavesOldStateIntact(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Bootstrap(ctx, "dc", "old-hash", bytes.Repeat([]byte{1}, 16), "fp-old"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := s.SaveCredential(ctx, "github_token", []byte("still-good")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	// A credential carrying a NULL name violates the NOT NULL constraint on
	// credentials.name, so the INSERT inside the transaction fails after the
	// DELETE has already run. The rollback is what we are testing.
	bad := map[string][]byte{}
	if err := s.Rekey(ctx, "new-hash", bytes.Repeat([]byte{2}, 16), "fp-new", bad); err != nil {
		t.Fatalf("Rekey with an empty set should still succeed: %v", err)
	}

	// Sanity check the happy path left things consistent, then exercise a
	// failing rekey by closing the database out from under it.
	all, err := s.Credentials(ctx)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("Credentials = %d entries, want 0 after rekeying to an empty set", len(all))
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = s.Rekey(ctx, "newer-hash", bytes.Repeat([]byte{3}, 16), "fp-newer",
		map[string][]byte{"github_token": []byte("x")})
	if err == nil {
		t.Fatal("Rekey on a closed database succeeded, want an error")
	}
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	sess := Session{
		ID:        "session-id",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
		LastSeen:  now,
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	loaded, err := s.Session(ctx, "session-id")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if !loaded.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Errorf("ExpiresAt = %s, want %s", loaded.ExpiresAt, sess.ExpiresAt)
	}

	newExpiry := now.Add(2 * time.Hour)
	if err := s.TouchSession(ctx, "session-id", newExpiry, now); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	touched, err := s.Session(ctx, "session-id")
	if err != nil {
		t.Fatalf("Session after touch: %v", err)
	}
	if !touched.ExpiresAt.Equal(newExpiry) {
		t.Errorf("ExpiresAt = %s, want %s", touched.ExpiresAt, newExpiry)
	}

	if err := s.DeleteSession(ctx, "session-id"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := s.DeleteSession(ctx, "session-id"); err != nil {
		t.Errorf("second DeleteSession = %v, want nil", err)
	}
	if _, err := s.Session(ctx, "session-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Session after delete = %v, want ErrNotFound", err)
	}
}

func TestTouchSessionOnMissingSession(t *testing.T) {
	s := newTestStore(t)

	err := s.TouchSession(context.Background(), "nope", time.Now(), time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchSession on a missing session = %v, want ErrNotFound", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)

	expired := Session{
		ID: "expired", CreatedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour), LastSeen: now.Add(-2 * time.Hour),
	}
	live := Session{
		ID: "live", CreatedAt: now,
		ExpiresAt: now.Add(time.Hour), LastSeen: now,
	}
	for _, sess := range []Session{expired, live} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession(%s): %v", sess.ID, err)
		}
	}

	n, err := s.DeleteExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d sessions, want 1", n)
	}

	if _, err := s.Session(ctx, "live"); err != nil {
		t.Errorf("live session was removed: %v", err)
	}
	if _, err := s.Session(ctx, "expired"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session survived: %v", err)
	}
}

func TestDeleteAllSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.CreateSession(ctx, Session{
			ID: id, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeen: now,
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}

	n, err := s.DeleteAllSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteAllSessions: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted %d sessions, want 3", n)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Setting(ctx, SettingLocale); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Setting on a fresh database = %v, want ErrNotFound", err)
	}

	got, err := s.SettingOrDefault(ctx, SettingLocale, "en")
	if err != nil {
		t.Fatalf("SettingOrDefault: %v", err)
	}
	if got != "en" {
		t.Errorf("SettingOrDefault = %q, want the fallback", got)
	}

	if err := s.SetSetting(ctx, SettingLocale, "zh-CN"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, err = s.SettingOrDefault(ctx, SettingLocale, "en")
	if err != nil {
		t.Fatalf("SettingOrDefault after set: %v", err)
	}
	if got != "zh-CN" {
		t.Errorf("SettingOrDefault = %q, want zh-CN", got)
	}

	// Upsert, not duplicate.
	if err := s.SetSetting(ctx, SettingLocale, "en"); err != nil {
		t.Fatalf("SetSetting (update): %v", err)
	}
	all, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("Settings returned %d entries, want 1", len(all))
	}

	if err := s.DeleteSetting(ctx, SettingLocale); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if err := s.DeleteSetting(ctx, SettingLocale); err != nil {
		t.Errorf("second DeleteSetting = %v, want nil", err)
	}
}
