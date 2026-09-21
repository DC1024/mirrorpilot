// Package catalog is the mirror catalogue as the application uses it.
//
// internal/mirror owns the data and its validation; internal/store owns the
// rows. Neither imports the other, which keeps the persistence layer reviewable
// on its own and keeps the domain type free of database vocabulary. Something
// has to sit between them, and this is that something: seeding the built-in
// list into the database, and converting at that seam in both directions.
//
// Keeping the bridge in one package means the two representations cannot drift
// apart quietly. Adding a field to either side is a compile error here until
// the other side learns about it.
package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// Sync writes the built-in catalogue into the store and reports how many rows
// were newly added.
//
// Called on every start. SyncBuiltinSources is idempotent and deliberately
// preserves the user's enabled/disabled choices, so running it repeatedly is
// how a new release's added mirrors reach an existing install without
// disturbing what the user already decided.
func Sync(ctx context.Context, s *store.Store) (int, error) {
	builtin, err := mirror.Builtin()
	if err != nil {
		return 0, err
	}

	rows := make([]store.SourceRecord, 0, builtin.Len())
	keep := make([]string, 0, builtin.Len())
	for _, src := range builtin.All() {
		rows = append(rows, ToRecord(src))
		keep = append(keep, src.ID)
	}

	added, err := s.SyncBuiltinSources(ctx, rows)
	if err != nil {
		return 0, fmt.Errorf("catalog: sync built-in sources: %w", err)
	}

	// A source the catalogue has dropped must not haunt installs that met it
	// in an earlier release: the row, and the history hanging off it, would
	// keep a dead mirror on the page and in the ranking. Idempotent — on an
	// install that never saw the removed entries, this deletes nothing.
	if _, err := s.RetireBuiltins(ctx, keep); err != nil {
		return 0, fmt.Errorf("catalog: retire removed built-ins: %w", err)
	}

	return added, nil
}

// ToRecord converts a domain source into its stored form.
//
// The caller is expected to have validated it first — mirror.Source.Validate is
// what turns a form submission into something worth storing. This function only
// translates.
func ToRecord(src mirror.Source) store.SourceRecord {
	src = src.Normalize()

	scope := make([]string, 0, len(src.Scope))
	for _, sc := range src.Scope {
		scope = append(scope, string(sc))
	}

	return store.SourceRecord{
		ID:       src.ID,
		Name:     src.Name,
		URL:      src.URL,
		Homepage: src.Homepage,
		Scope:    scope,
		Provider: string(src.Provider),
		Trust:    string(src.Trust),
		Note:     src.Note,
		Builtin:  src.Builtin,
		Enabled:  src.Enabled,
		Insecure: src.Insecure,
	}
}

// ToProbeSource converts a stored row into the shape the probe engine needs.
//
// Only the identity and the address are carried over. The probe measures
// whatever URL it is handed and never validates a source, so rebuilding the
// full domain value would mean supplying enum values a row might not actually
// hold — and a single unrecognised value would then have to either break the
// conversion or be papered over with an invented one.
func ToProbeSource(rec store.SourceRecord) mirror.Source {
	return mirror.Source{
		ID:   rec.ID,
		Name: rec.Name,
		URL:  rec.URL,
	}
}

// EnabledForDockerHub returns the enabled mirrors that actually proxy Docker
// Hub, which is the only upstream the probe can measure today.
//
// The scope filter is not tidiness. A mirror that proxies only ghcr.io will
// answer 404 to every layer of a Docker Hub probe, and reporting that as the
// mirror failing would be exactly the kind of confident-but-wrong verdict this
// project exists to avoid. A mirror that does not serve the upstream being
// measured is excluded, not blamed.
func EnabledForDockerHub(ctx context.Context, s *store.Store) ([]mirror.Source, error) {
	rows, err := s.ListSources(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]mirror.Source, 0, len(rows))
	for _, rec := range rows {
		if !rec.Enabled || !hasScope(rec, mirror.ScopeDockerHub) {
			continue
		}
		out = append(out, ToProbeSource(rec))
	}
	return out, nil
}

// hasScope reports whether a stored row declares a scope.
//
// The comparison is case-insensitive because the value comes from a database
// column of free text rather than from the validated enum, so "DockerHub" and
// "dockerhub" have to mean the same thing here.
func hasScope(rec store.SourceRecord, want mirror.Scope) bool {
	for _, sc := range rec.Scope {
		if strings.EqualFold(strings.TrimSpace(sc), string(want)) {
			return true
		}
	}
	return false
}

// ProbeTarget reads the image that probes measure, falling back to the built-in
// default when nothing has been chosen.
//
// It lives here rather than in the web layer because two callers now need it:
// the speed test page, which runs when someone clicks a button, and the
// background sweep, which runs on a timer with nobody watching. Two copies of
// "what are we measuring" would eventually answer differently, and the symptom
// would be a history graph whose points were not all measured the same way —
// the sort of thing that looks like noise rather than a bug.
//
// An absent setting is not an error: a fresh install should have something to
// measure before it has been configured. Whether the result is usable is
// probe.New's business, since it validates; repeating the rules here would be a
// second implementation to keep in step with the first, and the second one
// would win.
func ProbeTarget(ctx context.Context, s *store.Store) (probe.Target, error) {
	repository, err := s.SettingOrDefault(ctx, store.SettingProbeRepository, "")
	if err != nil {
		return probe.Target{}, err
	}
	reference, err := s.SettingOrDefault(ctx, store.SettingProbeReference, "")
	if err != nil {
		return probe.Target{}, err
	}

	fallback := probe.DefaultTarget()
	if strings.TrimSpace(repository) == "" {
		repository = fallback.Repository
	}
	if strings.TrimSpace(reference) == "" {
		reference = fallback.Reference
	}

	return probe.Target{
		Repository: strings.TrimSpace(repository),
		Reference:  strings.TrimSpace(reference),
	}, nil
}
