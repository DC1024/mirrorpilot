package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func userSource(id string) SourceRecord {
	return SourceRecord{
		ID:       id,
		Name:     "Example " + id,
		URL:      "https://" + id + ".example.invalid",
		Homepage: "https://example.invalid",
		Scope:    []string{"ghcr", "dockerhub"},
		Provider: "community",
		Trust:    "known",
		Note:     "a note",
		Enabled:  true,
	}
}

func builtinSource(id string) SourceRecord {
	rec := userSource(id)
	rec.Builtin = true
	return rec
}

func TestCreateSourceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in := userSource("example")
	if err := s.CreateSource(ctx, in); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	got, err := s.GetSource(ctx, "example")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}

	if got.Name != in.Name || got.URL != in.URL || got.Homepage != in.Homepage {
		t.Errorf("got %+v, want name/url/homepage from %+v", got, in)
	}
	if got.Provider != in.Provider || got.Trust != in.Trust || got.Note != in.Note {
		t.Errorf("got %+v, want provider/trust/note from %+v", got, in)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want true")
	}
	if got.Builtin {
		t.Error("Builtin = true for a user-created source")
	}
	if got.Insecure {
		t.Error("Insecure = true, want false")
	}

	// Scope is stored sorted, so a caller that supplied it in the other order
	// still reads back the canonical form.
	want := []string{"dockerhub", "ghcr"}
	if len(got.Scope) != len(want) || got.Scope[0] != want[0] || got.Scope[1] != want[1] {
		t.Errorf("Scope = %v, want %v", got.Scope, want)
	}

	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps were not recorded")
	}
}

func TestGetSourceNotFound(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.GetSource(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSource error = %v, want ErrNotFound", err)
	}
}

func TestCreateSourceRefusesToMintABuiltin(t *testing.T) {
	s := newTestStore(t)

	// Accepting this would let a request forge the rows the UI presents as
	// read-only and pre-vetted.
	err := s.CreateSource(context.Background(), builtinSource("forged"))
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("CreateSource error = %v, want ErrReadOnly", err)
	}
}

func TestListSourcesPutsEnabledFirstThenAlphabetical(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, rec := range []SourceRecord{
		{ID: "zulu", Name: "Zulu", Enabled: false},
		{ID: "alpha", Name: "Alpha", Enabled: false},
		{ID: "mike", Name: "Mike", Enabled: true},
	} {
		rec.URL = "https://" + rec.ID + ".example.invalid"
		rec.Scope = []string{"dockerhub"}
		rec.Provider = "community"
		rec.Trust = "known"
		if err := s.CreateSource(ctx, rec); err != nil {
			t.Fatalf("CreateSource(%s): %v", rec.ID, err)
		}
	}

	got, err := s.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}

	wantOrder := []string{"mike", "alpha", "zulu"}
	if len(got) != len(wantOrder) {
		t.Fatalf("ListSources returned %d rows, want %d", len(got), len(wantOrder))
	}
	for i, id := range wantOrder {
		if got[i].ID != id {
			t.Errorf("row %d = %q, want %q (order: %v)", i, got[i].ID, id, ids(got))
		}
	}
}

func ids(recs []SourceRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func TestUpdateSourceRewritesUserRowsOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateSource(ctx, userSource("mine")); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	edited := userSource("mine")
	edited.Name = "Renamed"
	edited.Enabled = false
	edited.Insecure = true
	edited.URL = "http://mine.example.invalid"

	if err := s.UpdateSource(ctx, edited); err != nil {
		t.Fatalf("UpdateSource: %v", err)
	}

	got, err := s.GetSource(ctx, "mine")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.Name != "Renamed" || got.Enabled || !got.Insecure || got.URL != edited.URL {
		t.Errorf("got %+v, want the edited values", got)
	}

	// A built-in row must survive an edit attempt untouched.
	if _, err := s.SyncBuiltinSources(ctx, []SourceRecord{builtinSource("vendor")}); err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}

	hijack := builtinSource("vendor")
	hijack.Name = "Hijacked"
	hijack.Trust = "verified"

	if err := s.UpdateSource(ctx, hijack); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("UpdateSource on a built-in error = %v, want ErrReadOnly", err)
	}

	after, err := s.GetSource(ctx, "vendor")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if after.Name == "Hijacked" {
		t.Error("a built-in source was rewritten")
	}
}

func TestUpdateSourceNotFound(t *testing.T) {
	s := newTestStore(t)

	if err := s.UpdateSource(context.Background(), userSource("ghost")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSource error = %v, want ErrNotFound", err)
	}
}

func TestDeleteSourceRefusesBuiltins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncBuiltinSources(ctx, []SourceRecord{builtinSource("vendor")}); err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}
	if err := s.DeleteSource(ctx, "vendor"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("DeleteSource on a built-in error = %v, want ErrReadOnly", err)
	}
	if _, err := s.GetSource(ctx, "vendor"); err != nil {
		t.Fatalf("built-in source disappeared: %v", err)
	}

	if err := s.DeleteSource(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteSource error = %v, want ErrNotFound", err)
	}

	if err := s.CreateSource(ctx, userSource("mine")); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := s.DeleteSource(ctx, "mine"); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if _, err := s.GetSource(ctx, "mine"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted source is still there: %v", err)
	}
}

func TestSetSourceEnabledWorksOnBuiltins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncBuiltinSources(ctx, []SourceRecord{builtinSource("vendor")}); err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}

	// This is the one mutation a built-in allows, precisely because a user who
	// does not want it probed must be able to say so.
	if err := s.SetSourceEnabled(ctx, "vendor", false); err != nil {
		t.Fatalf("SetSourceEnabled: %v", err)
	}

	got, err := s.GetSource(ctx, "vendor")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true after being switched off")
	}

	if err := s.SetSourceEnabled(ctx, "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetSourceEnabled error = %v, want ErrNotFound", err)
	}
}

func TestSyncBuiltinSourcesAddsAndRefreshesWithoutLosingChoices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	v1 := builtinSource("vendor")
	v1.Note = "first edition"

	added, err := s.SyncBuiltinSources(ctx, []SourceRecord{v1})
	if err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}

	// The user switches it off.
	if err := s.SetSourceEnabled(ctx, "vendor", false); err != nil {
		t.Fatalf("SetSourceEnabled: %v", err)
	}

	// A later release ships a corrected note and one new mirror.
	v2 := builtinSource("vendor")
	v2.Note = "second edition"
	added, err = s.SyncBuiltinSources(ctx, []SourceRecord{v2, builtinSource("newcomer")})
	if err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only newcomer is new)", added)
	}

	got, err := s.GetSource(ctx, "vendor")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.Note != "second edition" {
		t.Errorf("Note = %q, want the refreshed value", got.Note)
	}
	if got.Enabled {
		t.Error("Enabled = true; the sync must not undo the user's choice")
	}
}

func TestSyncBuiltinSourcesLeavesUserRowsAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A user happens to have created an id that a later release also uses.
	mine := userSource("vendor")
	mine.Name = "My Own Mirror"
	if err := s.CreateSource(ctx, mine); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	added, err := s.SyncBuiltinSources(ctx, []SourceRecord{builtinSource("vendor")})
	if err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}

	got, err := s.GetSource(ctx, "vendor")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.Builtin || got.Name != "My Own Mirror" {
		t.Errorf("got %+v, want the user's own row to win", got)
	}
	// And it stays deletable, because it is still theirs.
	if err := s.DeleteSource(ctx, "vendor"); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
}

func TestSyncBuiltinSourcesRefusesUserRows(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.SyncBuiltinSources(context.Background(), []SourceRecord{userSource("mine")}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SyncBuiltinSources error = %v, want ErrReadOnly", err)
	}
}

// A source dropped from the catalogue must not linger on installs that met it
// in an earlier release — and its history goes with it, or the ranking page
// keeps arguing about a mirror nobody can measure any more. Rows the user
// created themselves are not the catalogue's to delete, whatever their name.
func TestRetireBuiltinsDropsOnlyRetiredBuiltins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncBuiltinSources(ctx, []SourceRecord{
		builtinSource("keeper"),
		builtinSource("retired"),
	}); err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}
	// The user's own mirror happens to share the retired row's domain, but it
	// is a different row with a different id: theirs, not the catalogue's.
	mine := userSource("my-own")
	if err := s.CreateSource(ctx, mine); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := s.InsertProbe(ctx, probeRecord("retired", time.Now(), 1)); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}

	retired, err := s.RetireBuiltins(ctx, []string{"keeper", "my-own"})
	if err != nil {
		t.Fatalf("RetireBuiltins: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}

	if _, err := s.GetSource(ctx, "retired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSource after retire = %v, want ErrNotFound", err)
	}
	var runs int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM probe_runs WHERE source_id = 'retired'`).Scan(&runs); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if runs != 0 {
		t.Errorf("%d probe runs survived the source they belong to", runs)
	}

	if _, err := s.GetSource(ctx, "my-own"); err != nil {
		t.Errorf("the user's own row did not survive: %v", err)
	}

	// An empty keep list would wipe the whole catalogue on a catalogue bug;
	// it is refused rather than obeyed.
	if _, err := s.RetireBuiltins(ctx, nil); err == nil {
		t.Error("RetireBuiltins with an empty keep list succeeded")
	}

	// Idempotent: nothing left to retire, no error.
	again, err := s.RetireBuiltins(ctx, []string{"keeper", "my-own"})
	if err != nil || again != 0 {
		t.Errorf("second retire = (%d, %v), want (0, nil)", again, err)
	}
}

func TestSyncBuiltinSourcesWithNoRowsIsANoop(t *testing.T) {
	s := newTestStore(t)

	added, err := s.SyncBuiltinSources(context.Background(), nil)
	if err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}
}

func probeRecord(sourceID string, at time.Time, bps int64) ProbeRecord {
	return ProbeRecord{
		SourceID:      sourceID,
		StartedAt:     at,
		Connectivity:  "ok",
		ConnectMS:     12,
		TokenMS:       30,
		ManifestMS:    40,
		ThroughputBPS: bps,
		Status:        "ok",
	}
}

func TestProbeHistoryIsNewestFirstAndLimited(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateSource(ctx, userSource("example")); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		rec := probeRecord("example", base.Add(time.Duration(i)*time.Minute), int64(1000*(i+1)))
		if err := s.InsertProbe(ctx, rec); err != nil {
			t.Fatalf("InsertProbe: %v", err)
		}
	}

	got, err := s.RecentProbes(ctx, "example", 3)
	if err != nil {
		t.Fatalf("RecentProbes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}

	wantBPS := []int64{5000, 4000, 3000}
	for i, want := range wantBPS {
		if got[i].ThroughputBPS != want {
			t.Errorf("row %d throughput = %d, want %d", i, got[i].ThroughputBPS, want)
		}
	}
	if !got[0].StartedAt.Equal(base.Add(4 * time.Minute)) {
		t.Errorf("row 0 started at %s, want the newest", got[0].StartedAt)
	}
	if got[0].ConnectMS != 12 || got[0].ManifestMS != 40 || got[0].Connectivity != "ok" {
		t.Errorf("per-layer fields did not round-trip: %+v", got[0])
	}
}

func TestRecentProbesWithNonPositiveLimitReturnsNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateSource(ctx, userSource("example")); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := s.InsertProbe(ctx, probeRecord("example", time.Now(), 1)); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}

	for _, limit := range []int{0, -1} {
		got, err := s.RecentProbes(ctx, "example", limit)
		if err != nil {
			t.Fatalf("RecentProbes(%d): %v", limit, err)
		}
		if len(got) != 0 {
			t.Errorf("RecentProbes(%d) returned %d rows, want 0", limit, len(got))
		}
	}
}

func TestLatestProbesKeepsOneRowPerSource(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"one", "two"} {
		if err := s.CreateSource(ctx, userSource(id)); err != nil {
			t.Fatalf("CreateSource(%s): %v", id, err)
		}
	}

	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, rec := range []ProbeRecord{
		probeRecord("one", base, 100),
		probeRecord("one", base.Add(time.Minute), 200),
		probeRecord("two", base, 300),
	} {
		if err := s.InsertProbe(ctx, rec); err != nil {
			t.Fatalf("InsertProbe: %v", err)
		}
	}

	got, err := s.LatestProbes(ctx)
	if err != nil {
		t.Fatalf("LatestProbes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sources, want 2", len(got))
	}
	if got["one"].ThroughputBPS != 200 {
		t.Errorf("one = %d B/s, want the newest (200)", got["one"].ThroughputBPS)
	}
	if got["two"].ThroughputBPS != 300 {
		t.Errorf("two = %d B/s, want 300", got["two"].ThroughputBPS)
	}
}

func TestPruneProbesKeepsNewestPerSource(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"one", "two"} {
		if err := s.CreateSource(ctx, userSource(id)); err != nil {
			t.Fatalf("CreateSource(%s): %v", id, err)
		}
	}

	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"one", "two"} {
		for i := range 4 {
			rec := probeRecord(id, base.Add(time.Duration(i)*time.Minute), int64(i))
			if err := s.InsertProbe(ctx, rec); err != nil {
				t.Fatalf("InsertProbe: %v", err)
			}
		}
	}

	deleted, err := s.PruneProbes(ctx, 2)
	if err != nil {
		t.Fatalf("PruneProbes: %v", err)
	}
	if deleted != 4 {
		t.Errorf("deleted = %d, want 4", deleted)
	}

	for _, id := range []string{"one", "two"} {
		got, err := s.RecentProbes(ctx, id, 10)
		if err != nil {
			t.Fatalf("RecentProbes(%s): %v", id, err)
		}
		if len(got) != 2 {
			t.Fatalf("%s kept %d rows, want 2", id, len(got))
		}
		// The survivors must be the newest two, not an arbitrary pair.
		if got[0].ThroughputBPS != 3 || got[1].ThroughputBPS != 2 {
			t.Errorf("%s survivors = %d,%d, want 3,2", id, got[0].ThroughputBPS, got[1].ThroughputBPS)
		}
	}

	// Pruning again is a no-op rather than an error.
	deleted, err = s.PruneProbes(ctx, 2)
	if err != nil {
		t.Fatalf("PruneProbes: %v", err)
	}
	if deleted != 0 {
		t.Errorf("second prune deleted %d rows, want 0", deleted)
	}
}

func TestPruneProbesRejectsNegativeKeep(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.PruneProbes(context.Background(), -1); err == nil {
		t.Fatal("PruneProbes(-1) = nil, want an error")
	}
}

func TestDeletingASourceRemovesItsHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateSource(ctx, userSource("example")); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := s.InsertProbe(ctx, probeRecord("example", time.Now(), 1)); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}
	if err := s.DeleteSource(ctx, "example"); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}

	// The foreign key cascade has to actually fire. If the pragma were off we
	// would silently accumulate orphaned history forever.
	got, err := s.RecentProbes(ctx, "example", 10)
	if err != nil {
		t.Fatalf("RecentProbes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("%d orphaned probe rows survived", len(got))
	}
}

func TestInsertProbeRejectsUnknownSource(t *testing.T) {
	s := newTestStore(t)

	err := s.InsertProbe(context.Background(), probeRecord("ghost", time.Now(), 1))
	if err == nil {
		t.Fatal("InsertProbe for an unknown source = nil, want a foreign-key error")
	}
}
