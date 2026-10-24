package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/config"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// TestResetPanelClearsTheAccountAndSaysWhatItRemoved drives the -reset path.
//
// Two things here are worth pinning down, and the deletion itself is neither of
// them: store.Reset has its own tests. What only resetPanel can get wrong is
// (a) the numbers in the log, which are the operator's only record of what they
// just gave up and must therefore be read *before* the wipe, and (b) leaving the
// catalogue alone. A reset that also emptied the mirror list would come back
// looking like a broken install rather than an unconfigured one.
func TestResetPanelClearsTheAccountAndSaysWhatItRemoved(t *testing.T) {
	ctx := context.Background()

	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	seedBeforeReset(t, ctx, db)

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Listen = "127.0.0.1:18080"

	if err := resetPanel(ctx, db, cfg, logger); err != nil {
		t.Fatalf("resetPanel: %v", err)
	}

	// The account is gone, so the panel goes back to the setup screen.
	if has, err := db.HasUser(ctx); err != nil || has {
		t.Errorf("HasUser after reset = %v, %v; want false, nil", has, err)
	}
	if _, err := db.Credential(ctx, "github"); err == nil {
		t.Error("a credential survived the reset")
	}
	if _, err := db.Session(ctx, "session-0"); err == nil {
		t.Error("a session survived the reset")
	}

	// The catalogue is not account data. It has to still be there, and so does
	// the history hanging off it.
	if rows, err := db.ListSources(ctx); err != nil || len(rows) != 1 {
		t.Errorf("ListSources after reset = %d rows, %v; want the seeded mirror to survive", len(rows), err)
	}
	if rows, err := db.RecentProbes(ctx, "mirror-1", 10); err != nil || len(rows) != 1 {
		t.Errorf("RecentProbes after reset = %d rows, %v; want the seeded probe to survive", len(rows), err)
	}

	records := decodeLog(t, logged.Bytes())
	if len(records) != 3 {
		t.Errorf("resetPanel wrote %d log lines, want 3: %s", len(records), logged.String())
	}

	summary := findLog(t, records, "unconfigured again")

	// WARN, not INFO: this is the line that tells someone their credentials are
	// gone for good, and it must not be the one an INFO filter hides.
	if level, _ := summary["level"].(string); level != "WARN" {
		t.Errorf("the reset summary is logged at %q, want WARN", level)
	}
	if got, _ := summary["data"].(string); got != cfg.DataDir {
		t.Errorf("the summary names data dir %q, want %q", got, cfg.DataDir)
	}

	// The three counts are seeded to 1, 2 and 3 on purpose: equal numbers would
	// let a swapped pair of fields through.
	for field, want := range map[string]float64{
		"accounts_removed":    1,
		"credentials_removed": 2,
		"sessions_ended":      3,
	} {
		if got, ok := summary[field].(float64); !ok || got != want {
			t.Errorf("%s = %v, want %v", field, summary[field], want)
		}
	}

	// The two follow-up lines exist so nobody has to guess why their GitHub
	// token stopped working or what to do next.
	findLog(t, records, "gone for good")
	if restart := findLog(t, records, "start the panel again"); restart["listen"] != cfg.Listen {
		t.Errorf("the restart hint names %v, want %q", restart["listen"], cfg.Listen)
	}
}

// TestResetPanelReportsFailureWithoutAnnouncingAReset covers the wipe failing.
//
// -reset runs once, by hand, on a panel someone is already nervous about. If it
// cannot read the counts it must say so and stop, rather than print the
// "start the panel again" banner over a database it never touched.
func TestResetPanelReportsFailureWithoutAnnouncingAReset(t *testing.T) {
	ctx := context.Background()

	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))

	if err := resetPanel(ctx, db, config.Default(), logger); err == nil {
		t.Fatal("resetPanel on a closed store returned nil, want an error")
	}
	if logged.Len() != 0 {
		t.Errorf("resetPanel logged %q after failing; a failed reset must not announce one", logged.String())
	}
}

// seedBeforeReset fills the database with everything a reset removes — one
// account, two credentials, three sessions — plus a mirror and a probe that it
// must not remove.
func seedBeforeReset(t *testing.T, ctx context.Context, db *store.Store) {
	t.Helper()

	if _, err := db.CreateUser(ctx, "operator", "password-hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	for _, name := range []string{"github", "harbor"} {
		if err := db.SaveCredential(ctx, name, []byte("ciphertext-for-"+name)); err != nil {
			t.Fatalf("SaveCredential(%q): %v", name, err)
		}
	}

	now := time.Now()
	for i := range 3 {
		sess := store.Session{
			ID:        fmt.Sprintf("session-%d", i),
			CreatedAt: now,
			ExpiresAt: now.Add(time.Hour),
			LastSeen:  now,
		}
		if err := db.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession(%q): %v", sess.ID, err)
		}
	}

	if err := db.CreateSource(ctx, store.SourceRecord{
		ID:       "mirror-1",
		Name:     "Example",
		URL:      "https://example.test",
		Provider: "example",
		Trust:    "community",
		Enabled:  true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	if err := db.InsertProbe(ctx, store.ProbeRecord{
		SourceID:     "mirror-1",
		StartedAt:    now,
		Connectivity: "ok",
		Status:       "ok",
	}); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}
}

// decodeLog parses the JSON lines resetPanel wrote, so the assertions can name
// fields rather than match substrings of a text log. Text output would also
// have to be un-escaped by hand on Windows, where every path separator arrives
// doubled.
func decodeLog(t *testing.T, logged []byte) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// findLog returns the single record whose message contains want.
func findLog(t *testing.T, records []map[string]any, want string) map[string]any {
	t.Helper()

	for _, rec := range records {
		if msg, _ := rec["msg"].(string); strings.Contains(msg, want) {
			return rec
		}
	}

	t.Fatalf("no log line mentions %q", want)
	return nil
}
