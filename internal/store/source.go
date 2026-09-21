package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrReadOnly means the row exists but the caller may not change it.
//
// Built-in mirrors are the case this exists for: a user may switch one off, but
// may not repoint it. Letting them rewrite the URL of an entry that still
// carries a "verified" badge would make the badge meaningless, and the badge is
// the whole reason the trust grading is worth having.
var ErrReadOnly = errors.New("store: row is read-only")

// SourceRecord is one mirror as persisted.
//
// It holds primitives only and does not mention internal/mirror, so the store
// keeps its position at the bottom of the dependency graph and stays reviewable
// on its own. Code that understands mirror.Source converts at the boundary.
type SourceRecord struct {
	ID       string
	Name     string
	URL      string
	Homepage string
	// Scope is a set of short tokens, stored sorted and comma-joined.
	Scope    []string
	Provider string
	Trust    string
	Note     string

	// Builtin rows come from the embedded catalogue. They may be disabled, and
	// may not be edited or deleted.
	Builtin bool

	Enabled bool

	// Insecure marks a plain-HTTP mirror.
	Insecure bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

const sourceColumns = `id, name, url, homepage, scope, provider, trust, note,
	builtin, enabled, insecure, created_at, updated_at`

// ListSources returns every mirror. Enabled rows come first, then alphabetical:
// the list is a working view, and the ones being probed are the ones worth
// seeing at the top.
func (s *Store) ListSources(ctx context.Context) ([]SourceRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources ORDER BY enabled DESC, name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list sources: %w", err)
	}
	defer rows.Close()

	var out []SourceRecord
	for rows.Next() {
		rec, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate sources: %w", err)
	}
	return out, nil
}

// GetSource loads one mirror by id.
func (s *Store) GetSource(ctx context.Context, id string) (SourceRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE id = ?`, id)

	rec, err := scanSource(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return SourceRecord{}, err
		}
		return SourceRecord{}, err
	}
	return rec, nil
}

// CreateSource inserts a user-added mirror.
//
// Built-in rows are refused rather than silently accepted: seeding them is
// SyncBuiltinSources' job, and a create path that could mint a row claiming to
// be built-in would let a request forge the read-only ones.
func (s *Store) CreateSource(ctx context.Context, rec SourceRecord) error {
	if rec.Builtin {
		return fmt.Errorf("store: create source %q: %w", rec.ID, ErrReadOnly)
	}

	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sources (`+sourceColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		rec.ID, rec.Name, rec.URL, rec.Homepage, joinScope(rec.Scope),
		rec.Provider, rec.Trust, rec.Note,
		boolToInt(rec.Enabled), boolToInt(rec.Insecure),
		formatTime(now), formatTime(now),
	)
	if err != nil {
		return fmt.Errorf("store: create source %q: %w", rec.ID, err)
	}
	return nil
}

// UpdateSource rewrites a user-added mirror. Built-in rows are left alone.
func (s *Store) UpdateSource(ctx context.Context, rec SourceRecord) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sources
		    SET name = ?, url = ?, homepage = ?, scope = ?, provider = ?, trust = ?,
		        note = ?, enabled = ?, insecure = ?, updated_at = ?
		  WHERE id = ? AND builtin = 0`,
		rec.Name, rec.URL, rec.Homepage, joinScope(rec.Scope),
		rec.Provider, rec.Trust, rec.Note,
		boolToInt(rec.Enabled), boolToInt(rec.Insecure), formatTime(time.Now()),
		rec.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update source %q: %w", rec.ID, err)
	}

	return s.explainNoRows(ctx, res, rec.ID, "update")
}

// DeleteSource removes a user-added mirror. Built-in rows survive.
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sources WHERE id = ? AND builtin = 0`, id)
	if err != nil {
		return fmt.Errorf("store: delete source %q: %w", id, err)
	}

	return s.explainNoRows(ctx, res, id, "delete")
}

// SetSourceEnabled is the one mutation a built-in mirror allows.
func (s *Store) SetSourceEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sources SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), formatTime(time.Now()), id,
	)
	if err != nil {
		return fmt.Errorf("store: set source %q enabled: %w", id, err)
	}

	return s.explainNoRows(ctx, res, id, "enable")
}

// explainNoRows turns "nothing was affected" into the right error: a missing row
// or a row that exists but refuses to change. The distinction is the difference
// between a 404 and a 403 in the panel, so it is worth a lookup.
func (s *Store) explainNoRows(ctx context.Context, res sql.Result, id, verb string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s source %q: %w", verb, id, err)
	}
	if n > 0 {
		return nil
	}

	var builtin int
	err = s.db.QueryRowContext(ctx,
		`SELECT builtin FROM sources WHERE id = ?`, id).Scan(&builtin)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("store: %s source %q: %w", verb, id, ErrNotFound)
	case err != nil:
		return fmt.Errorf("store: %s source %q: %w", verb, id, err)
	case builtin != 0:
		return fmt.Errorf("store: %s source %q: %w", verb, id, ErrReadOnly)
	default:
		return fmt.Errorf("store: %s source %q: no rows affected", verb, id)
	}
}

// SyncBuiltinSources brings the database in line with the catalogue embedded in
// the binary, and reports how many rows were newly added.
//
// New entries appear. Existing built-in rows are refreshed, so a corrected note
// or homepage in a later release reaches installs that already ran an older
// one. Two things are deliberately preserved:
//
//   - enabled, because a user who turned a mirror off meant it; and
//   - any row the user created themselves, even if it collides with a built-in
//     id — their data wins, and the built-in simply does not take that slot.
func (s *Store) SyncBuiltinSources(ctx context.Context, rows []SourceRecord) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	before, err := s.countSources(ctx)
	if err != nil {
		return 0, err
	}

	for _, rec := range rows {
		if !rec.Builtin {
			return 0, fmt.Errorf("store: sync built-in source %q: %w", rec.ID, ErrReadOnly)
		}

		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO sources (`+sourceColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name       = excluded.name,
				url        = excluded.url,
				homepage   = excluded.homepage,
				scope      = excluded.scope,
				provider   = excluded.provider,
				trust      = excluded.trust,
				note       = excluded.note,
				insecure   = excluded.insecure,
				updated_at = excluded.updated_at
			WHERE sources.builtin = 1`,
			rec.ID, rec.Name, rec.URL, rec.Homepage, joinScope(rec.Scope),
			rec.Provider, rec.Trust, rec.Note,
			boolToInt(rec.Enabled), boolToInt(rec.Insecure),
			formatTime(time.Now()), formatTime(time.Now()),
		); err != nil {
			return 0, fmt.Errorf("store: sync built-in source %q: %w", rec.ID, err)
		}
	}

	after, err := s.countSources(ctx)
	if err != nil {
		return 0, err
	}
	return after - before, nil
}

// RetireBuiltins deletes built-in rows whose ids the catalogue no longer
// carries, and reports how many went away. It runs after SyncBuiltinSources on
// every start, so a source removed from the catalogue in a later release does
// not linger in installs that first met it in an earlier one.
//
// The probe history goes with them (probe_runs cascades on source delete).
// Keeping measurements of a source the catalogue has written off would mean a
// ranking page that still argues about a mirror nobody can measure any more;
// the honest state is for the row, and its history, to be gone.
//
// User-added rows are never touched, whatever their id — the catalogue does
// not get a vote on data the user typed in themselves. And an empty keep list
// is refused: wiping every built-in row because the catalogue failed to load
// would be a bug compounding itself.
func (s *Store) RetireBuiltins(ctx context.Context, keep []string) (int, error) {
	if len(keep) == 0 {
		return 0, fmt.Errorf("store: retire built-in sources: empty keep list")
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, len(keep))
	for i, id := range keep {
		args[i] = id
	}

	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sources WHERE builtin = 1 AND id NOT IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return 0, fmt.Errorf("store: retire built-in sources: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: retire built-in sources: %w", err)
	}
	return int(n), nil
}

func (s *Store) countSources(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sources: %w", err)
	}
	return n, nil
}

// scanner is the subset of *sql.Row and *sql.Rows that scanning a source needs,
// so both callers can share one implementation.
type scanner interface {
	Scan(dest ...any) error
}

func scanSource(src scanner) (SourceRecord, error) {
	var (
		rec                   SourceRecord
		scope                 string
		builtin, enabled, ins int
		created, updated      string
	)

	if err := src.Scan(
		&rec.ID, &rec.Name, &rec.URL, &rec.Homepage, &scope,
		&rec.Provider, &rec.Trust, &rec.Note,
		&builtin, &enabled, &ins, &created, &updated,
	); err != nil {
		return SourceRecord{}, wrapNotFound(fmt.Errorf("store: scan source: %w", err))
	}

	rec.Scope = splitScope(scope)
	rec.Builtin = builtin != 0
	rec.Enabled = enabled != 0
	rec.Insecure = ins != 0

	var err error
	if rec.CreatedAt, err = parseTime(created); err != nil {
		return SourceRecord{}, err
	}
	if rec.UpdatedAt, err = parseTime(updated); err != nil {
		return SourceRecord{}, err
	}

	return rec, nil
}

// joinScope renders a scope set as it is stored: sorted, comma-joined, no
// spaces. Sorted because the stored bytes should not depend on the order the
// caller happened to build the slice in.
func joinScope(scope []string) string {
	cleaned := make([]string, 0, len(scope))
	for _, s := range scope {
		if s = strings.TrimSpace(s); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	slices.Sort(cleaned)
	return strings.Join(cleaned, ",")
}

func splitScope(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ",")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
