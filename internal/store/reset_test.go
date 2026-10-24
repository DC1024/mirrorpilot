package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedPanel fills the four tables Reset promises to empty, plus one row in each
// of the three it promises to keep. Both halves matter: a Reset that emptied
// everything would satisfy "the account is gone" while quietly throwing away
// the measurement history.
func seedPanel(t *testing.T, s *Store) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := s.CreateUser(ctx, "dc", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateVault(ctx, []byte("salt"), "fingerprint"); err != nil {
		t.Fatalf("CreateVault: %v", err)
	}
	if err := s.SaveCredential(ctx, "github", []byte("ciphertext")); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if err := s.CreateSession(ctx, Session{
		ID:        "sid",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
		LastSeen:  now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSource(ctx, userSource("example")); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := s.InsertProbe(ctx, ProbeRecord{
		SourceID:     "example",
		StartedAt:    now,
		Connectivity: "ok",
		Status:       "works",
	}); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}
	if err := s.SetSetting(ctx, "theme", "dark"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
}

// TestResetCountsDescribesWhatResetWouldRemove covers the "would", which is the
// whole point of counting before writing: a number read afterwards is zero by
// construction.
func TestResetCountsDescribesWhatResetWouldRemove(t *testing.T) {
	s := newTestStore(t)
	seedPanel(t, s)

	got, err := s.ResetCounts(context.Background())
	if err != nil {
		t.Fatalf("ResetCounts: %v", err)
	}

	want := ResetSummary{Users: 1, Credentials: 1, Sessions: 1}
	if got != want {
		t.Errorf("ResetCounts = %+v, want %+v", got, want)
	}
}

// TestResetClearsTheAccountAndKeepsTheCatalogue is the behaviour the -reset flag
// promises: the way back from a forgotten master password.
//
// The kept half is the part worth asserting. The mirrors, their measurements and
// the preferences are not secret, and losing months of history to fix a login
// would be a poor trade — but nothing in the code says so except a comment, and
// a comment does not fail when someone adds a DELETE.
func TestResetClearsTheAccountAndKeepsTheCatalogue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedPanel(t, s)

	if err := s.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Gone: the account, the key that protects the credentials, the credentials
	// themselves, and the sessions that could reach any of it.
	if has, err := s.HasUser(ctx); err != nil || has {
		t.Errorf("HasUser after reset = %v, %v; want false, nil", has, err)
	}
	if _, err := s.Vault(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("Vault after reset = %v, want ErrNotFound", err)
	}
	if creds, err := s.Credentials(ctx); err != nil || len(creds) != 0 {
		t.Errorf("Credentials after reset = %v, %v; want an empty map and no error", creds, err)
	}
	if _, err := s.Session(ctx, "sid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Session after reset = %v, want ErrNotFound", err)
	}

	// Kept: the catalogue, its history, and the preferences.
	sources, err := s.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 1 || sources[0].ID != "example" {
		t.Errorf("sources after reset = %v, want the example mirror kept", sources)
	}

	probes, err := s.RecentProbes(ctx, "example", 10)
	if err != nil {
		t.Fatalf("RecentProbes: %v", err)
	}
	if len(probes) != 1 {
		t.Errorf("kept %d measurements after reset, want 1", len(probes))
	}

	if theme, err := s.Setting(ctx, "theme"); err != nil || theme != "dark" {
		t.Errorf("Setting(theme) after reset = %q, %v; want dark", theme, err)
	}
}

// TestResetOnAFreshStoreSucceeds pins the failure that would be worst timed: the
// flag is reached for when someone is already locked out, so "nothing to reset"
// must not be an error they have to interpret.
func TestResetOnAFreshStoreSucceeds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Reset(ctx); err != nil {
		t.Fatalf("Reset on an empty store: %v", err)
	}
	if has, err := s.HasUser(ctx); err != nil || has {
		t.Errorf("HasUser = %v, %v; want false, nil", has, err)
	}
}
