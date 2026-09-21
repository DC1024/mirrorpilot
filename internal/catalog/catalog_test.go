package catalog

import (
	"context"
	"slices"
	"testing"

	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

func newStore(t *testing.T) *store.Store {
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
	return st
}

// builtinRows is a small, hand-made catalogue, so a filtering test states
// exactly which rows exist instead of depending on what ships in sources.json.
func builtinRows() []store.SourceRecord {
	return []store.SourceRecord{
		{
			ID: "hub-on", Name: "Hub on", URL: "https://a.example",
			Scope: []string{"dockerhub"}, Provider: "cloud", Trust: "verified",
			Builtin: true, Enabled: true,
		},
		{
			ID: "ghcr-on", Name: "GHCR on", URL: "https://b.example",
			Scope: []string{"ghcr"}, Provider: "cloud", Trust: "verified",
			Builtin: true, Enabled: true,
		},
		{
			ID: "hub-off", Name: "Hub off", URL: "https://c.example",
			Scope: []string{"dockerhub"}, Provider: "cloud", Trust: "verified",
			Builtin: true, Enabled: false,
		},
		{
			// The spelling that catches a case-sensitive scope comparison.
			ID: "mixed", Name: "Mixed", URL: "https://d.example",
			Scope: []string{"DockerHub", "ghcr"}, Provider: "community", Trust: "known",
			Builtin: true, Enabled: true,
		},
	}
}

func TestSyncSeedsTheBuiltinCatalogueOnce(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	added, err := Sync(ctx, st)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if added == 0 {
		t.Fatal("first Sync added nothing — the embedded catalogue is not reaching the database")
	}

	builtin, err := mirror.Builtin()
	if err != nil {
		t.Fatalf("mirror.Builtin: %v", err)
	}
	if added != builtin.Len() {
		t.Errorf("added %d rows, want %d", added, builtin.Len())
	}

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != builtin.Len() {
		t.Fatalf("stored %d sources, want %d", len(rows), builtin.Len())
	}
	for _, rec := range rows {
		if !rec.Builtin {
			t.Errorf("%s was seeded without the builtin flag, so it could be deleted", rec.ID)
		}
	}

	// Running again is how a new release's mirrors arrive. It must not
	// duplicate what is already there.
	again, err := Sync(ctx, st)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if again != 0 {
		t.Errorf("second Sync added %d rows, want 0", again)
	}
}

// A release that drops a source from sources.json must also drop the row from
// installs that first met it earlier — otherwise the dead mirror haunts the
// speed test page and the ranking forever.
func TestSyncRetiresSourcesTheCatalogueNoLongerCarries(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	ghost := store.SourceRecord{
		ID: "ghost", Name: "Ghost", URL: "https://ghost.example",
		Scope: []string{"dockerhub"}, Provider: "community", Trust: "known",
		Builtin: true, Enabled: true,
	}
	if _, err := st.SyncBuiltinSources(ctx, []store.SourceRecord{ghost}); err != nil {
		t.Fatalf("seeding the ghost: %v", err)
	}

	added, err := Sync(ctx, st)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = added

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	for _, rec := range rows {
		if rec.ID == "ghost" {
			t.Fatal("a source the catalogue no longer carries survived the sync")
		}
	}

	// A user-added row is not the catalogue's to delete, even when it collides
	// with the ghost's name.
	mine := store.SourceRecord{
		ID: "my-ghost", Name: "My Own", URL: "https://mine.example",
		Scope: []string{"dockerhub"}, Provider: "community", Trust: "known",
		Enabled: true,
	}
	if err := st.CreateSource(ctx, mine); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if _, err := Sync(ctx, st); err != nil {
		t.Fatalf("Sync after adding a user row: %v", err)
	}
	if _, err := st.GetSource(ctx, "my-ghost"); err != nil {
		t.Errorf("the user's own row did not survive the sync: %v", err)
	}
}

func TestSyncKeepsTheUsersOwnChoices(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	if _, err := Sync(ctx, st); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	builtin, err := mirror.Builtin()
	if err != nil {
		t.Fatalf("mirror.Builtin: %v", err)
	}
	if builtin.Len() == 0 {
		t.Fatal("nothing to sync")
	}
	victim := builtin.All()[0]

	// The user turned a mirror off. A restart must not turn it back on —
	// that is the difference between a default and a decision.
	if err := st.SetSourceEnabled(ctx, victim.ID, false); err != nil {
		t.Fatalf("SetSourceEnabled: %v", err)
	}
	if _, err := Sync(ctx, st); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	rec, err := st.GetSource(ctx, victim.ID)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if rec.Enabled {
		t.Errorf("%s came back enabled after a restart", victim.ID)
	}
}

func TestToRecordTranslatesEveryField(t *testing.T) {
	src := mirror.Source{
		ID:       "example",
		Name:     "Example",
		URL:      "https://mirror.example",
		Homepage: "https://example.com/mirror",
		Scope:    []mirror.Scope{mirror.ScopeGHCR, mirror.ScopeDockerHub},
		Provider: mirror.ProviderAcademic,
		Trust:    mirror.TrustKnown,
		Note:     "a note",
		Builtin:  false,
		Enabled:  true,
		Insecure: false,
	}

	got := ToRecord(src)

	if got.ID != src.ID || got.Name != src.Name || got.URL != src.URL || got.Homepage != src.Homepage {
		t.Errorf("identity fields did not survive: %+v", got)
	}
	if got.Provider != "academic" || got.Trust != "known" {
		t.Errorf("provider/trust = %q/%q, want academic/known", got.Provider, got.Trust)
	}
	if !got.Enabled || got.Builtin {
		t.Errorf("enabled/builtin = %v/%v, want true/false", got.Enabled, got.Builtin)
	}
	if got.Note != "a note" {
		t.Errorf("note = %q", got.Note)
	}
	// Sorted, because the stored bytes should not depend on the order a form
	// happened to submit its checkboxes in.
	if want := []string{"dockerhub", "ghcr"}; !slices.Equal(got.Scope, want) {
		t.Errorf("scope = %v, want %v", got.Scope, want)
	}
}

func TestToRecordNormalizesWhatItIsGiven(t *testing.T) {
	got := ToRecord(mirror.Source{
		ID:    "  Example ",
		Name:  " Example ",
		URL:   "https://mirror.example/",
		Scope: []mirror.Scope{" DockerHub ", "dockerhub"},
	})

	if got.ID != "example" {
		t.Errorf("id = %q, want %q", got.ID, "example")
	}
	if got.Name != "Example" {
		t.Errorf("name = %q", got.Name)
	}
	// Trailing slashes matter: "https://a/" and "https://a" would otherwise be
	// two different rows to a uniqueness check and one mirror to a human.
	if got.URL != "https://mirror.example" {
		t.Errorf("url = %q", got.URL)
	}
	if len(got.Scope) != 1 || got.Scope[0] != "dockerhub" {
		t.Errorf("scope = %v, want the duplicate collapsed", got.Scope)
	}
}

func TestToProbeSourceCarriesOnlyTheAddress(t *testing.T) {
	// Deliberately junk enums: the store keeps these as free text, and a probe
	// only needs somewhere to point. Rebuilding the full domain value here
	// would force this call to fail, or to invent values.
	rec := store.SourceRecord{
		ID:       "odd",
		Name:     "Odd",
		URL:      "https://odd.example",
		Provider: "not-a-provider",
		Trust:    "not-a-trust",
		Scope:    []string{"nonsense"},
		Insecure: true,
	}

	got := ToProbeSource(rec)

	if got.ID != rec.ID || got.Name != rec.Name || got.URL != rec.URL {
		t.Errorf("probe source = %+v, want the id, name and url", got)
	}
}

func TestEnabledForDockerHubSelectsWhatCanBeMeasured(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	if _, err := st.SyncBuiltinSources(ctx, builtinRows()); err != nil {
		t.Fatalf("SyncBuiltinSources: %v", err)
	}

	got, err := EnabledForDockerHub(ctx, st)
	if err != nil {
		t.Fatalf("EnabledForDockerHub: %v", err)
	}

	var ids []string
	for _, src := range got {
		ids = append(ids, src.ID)
	}

	// hub-on and mixed both serve Docker Hub and are enabled. ghcr-on is
	// enabled but cannot answer, and hub-off was switched off by someone.
	if want := []string{"hub-on", "mixed"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestEnabledForDockerHubIsEmptyWhenNothingQualifies(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	got, err := EnabledForDockerHub(ctx, st)
	if err != nil {
		t.Fatalf("EnabledForDockerHub: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d sources from an empty store", len(got))
	}
}

// TestProbeTargetFallsBackWhenNothingIsSet pins the default answer.
//
// The sweep depends on this: an unconfigured install starts measuring
// immediately after startup, and getting this wrong would either leave it with
// nothing to measure or make it invent an image of its own.
func TestProbeTargetFallsBackWhenNothingIsSet(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	got, err := ProbeTarget(ctx, st)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}

	want := probe.DefaultTarget()
	if got != want {
		t.Errorf("ProbeTarget = %+v, want the default %+v", got, want)
	}
}

// TestProbeTargetReadsTheSettings is the other half: the setting wins when it
// is there, so changing what is measured changes what the next run measures.
func TestProbeTargetReadsTheSettings(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	if err := st.SetSetting(ctx, store.SettingProbeRepository, "library/busybox"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.SetSetting(ctx, store.SettingProbeReference, "1.37"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	got, err := ProbeTarget(ctx, st)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}

	if got.Repository != "library/busybox" {
		t.Errorf("repository = %q", got.Repository)
	}
	if got.Reference != "1.37" {
		t.Errorf("reference = %q", got.Reference)
	}
}

// TestProbeTargetTrimsWhatTheSettingsSay covers values with stray whitespace.
//
// A form can save a padded value, and " library/alpine " resolved to a
// different image than the one shown on the page would be a quiet lie about
// what the numbers mean.
func TestProbeTargetTrimsWhatTheSettingsSay(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	if err := st.SetSetting(ctx, store.SettingProbeRepository, "  library/alpine  "); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	got, err := ProbeTarget(ctx, st)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	if got.Repository != "library/alpine" {
		t.Errorf("repository = %q, want the trimmed value", got.Repository)
	}

	// The reference was never set, so it is still the default rather than
	// empty: a target with an image and no tag is not a target.
	if got.Reference == "" {
		t.Error("reference is empty, want the default when the setting is absent")
	}
}
