package store

import (
	"context"
	"fmt"
	"time"
)

// ProbeRecord is one four-layer measurement of one mirror.
//
// This is a trend sample, not an audit trail. Rows are pruned per source, and
// losing them costs a graph rather than anything someone would miss. That is
// why the columns are flat and the statuses are plain text: nothing here is
// worth a schema migration every time the probe vocabulary grows.
type ProbeRecord struct {
	SourceID  string
	StartedAt time.Time

	// Connectivity is the outcome of layer 1 — whether the registry answered at
	// all. The remaining layers are only meaningful when it did.
	Connectivity string

	// TokenStatus, ManifestStatus and ThroughputStatus are the outcomes of
	// layers 2 to 4, so a page can say which step went wrong instead of only
	// that something did. Empty means the row predates schemaV3, or the layer
	// never ran.
	TokenStatus      string
	ManifestStatus   string
	ThroughputStatus string

	// ConnectMS, TokenMS and ManifestMS are per-layer timings. Zero means the
	// layer did not run or produced nothing, which the UI renders as a dash
	// rather than as "instant".
	ConnectMS  int64
	TokenMS    int64
	ManifestMS int64

	// ThroughputBPS is the layer-4 result: bytes per second pulled from a known
	// blob. Zero means it was not measured, which is not the same as a measured
	// zero.
	ThroughputBPS int64

	// ResolvedDigest is the digest of the manifest the probe resolved. It is
	// what makes a row self-describing: without it, comparing two mirrors
	// measured in the same batch assumes they served the same bytes, which a
	// cached tag can quietly make untrue.
	ResolvedDigest string

	// Status is the overall verdict, and Detail is a short human-readable
	// explanation, usually empty on success.
	Status string
	Detail string
}

const probeColumns = `source_id, started_at, connectivity,
	token_status, manifest_status, throughput_status,
	connect_ms, token_ms, manifest_ms, throughput_bps,
	resolved_digest, status, detail`

// InsertProbe appends one measurement.
func (s *Store) InsertProbe(ctx context.Context, rec ProbeRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO probe_runs (`+probeColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.SourceID, formatTime(rec.StartedAt), rec.Connectivity,
		rec.TokenStatus, rec.ManifestStatus, rec.ThroughputStatus,
		rec.ConnectMS, rec.TokenMS, rec.ManifestMS, rec.ThroughputBPS,
		rec.ResolvedDigest, rec.Status, rec.Detail,
	)
	if err != nil {
		return fmt.Errorf("store: insert probe for %q: %w", rec.SourceID, err)
	}
	return nil
}

// RecentProbes returns a source's history, newest first.
func (s *Store) RecentProbes(ctx context.Context, sourceID string, limit int) ([]ProbeRecord, error) {
	if limit <= 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+probeColumns+` FROM probe_runs
		  WHERE source_id = ?
		  ORDER BY started_at DESC, id DESC
		  LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent probes for %q: %w", sourceID, err)
	}
	defer rows.Close()

	var out []ProbeRecord
	for rows.Next() {
		rec, err := scanProbe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate probes: %w", err)
	}
	return out, nil
}

// LatestProbes returns the most recent measurement per source, keyed by source
// id. Sources never probed are simply absent.
func (s *Store) LatestProbes(ctx context.Context) (map[string]ProbeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+probeColumns+` FROM probe_runs p
		 WHERE p.id = (
		     SELECT q.id FROM probe_runs q
		      WHERE q.source_id = p.source_id
		      ORDER BY q.started_at DESC, q.id DESC
		      LIMIT 1
		 )`)
	if err != nil {
		return nil, fmt.Errorf("store: latest probes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]ProbeRecord)
	for rows.Next() {
		rec, err := scanProbe(rows)
		if err != nil {
			return nil, err
		}
		out[rec.SourceID] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate latest probes: %w", err)
	}
	return out, nil
}

// Histories returns the newest limit rows per source, keyed by source id and
// ordered newest first within each source.
//
// One query rather than a loop over RecentProbes: the page draws a sparkline
// per mirror, and a page that issues one query per row is a page whose cost
// grows every time someone adds a mirror.
func (s *Store) Histories(ctx context.Context, limit int) (map[string][]ProbeRecord, error) {
	if limit <= 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+probeColumns+`
		  FROM (
		      SELECT *, ROW_NUMBER() OVER (
		                 PARTITION BY source_id
		                 ORDER BY started_at DESC, id DESC
		             ) AS rn
		        FROM probe_runs
		  )
		 WHERE rn <= ?
		 ORDER BY source_id, started_at DESC, id DESC`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: probe histories: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]ProbeRecord)
	for rows.Next() {
		rec, err := scanProbe(rows)
		if err != nil {
			return nil, err
		}
		out[rec.SourceID] = append(out[rec.SourceID], rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate probe histories: %w", err)
	}
	return out, nil
}

// PruneProbes keeps the newest keepPerSource rows for each source and reports
// how many rows it deleted.
//
// Without this the history grows without bound, one row per source per probe,
// forever. Pruning on write rather than on a timer means there is no background
// job whose failure would be invisible until the disk filled.
func (s *Store) PruneProbes(ctx context.Context, keepPerSource int) (int, error) {
	if keepPerSource < 0 {
		return 0, fmt.Errorf("store: prune probes: keepPerSource must not be negative")
	}

	res, err := s.db.ExecContext(ctx, `
		DELETE FROM probe_runs
		 WHERE id IN (
		     SELECT id FROM (
		         SELECT id, ROW_NUMBER() OVER (
		                    PARTITION BY source_id
		                    ORDER BY started_at DESC, id DESC
		                ) AS rn
		           FROM probe_runs
		     ) WHERE rn > ?
		 )`, keepPerSource)
	if err != nil {
		return 0, fmt.Errorf("store: prune probes: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune probes: %w", err)
	}
	return int(n), nil
}

func scanProbe(src scanner) (ProbeRecord, error) {
	var (
		rec     ProbeRecord
		started string
	)

	if err := src.Scan(
		&rec.SourceID, &started, &rec.Connectivity,
		&rec.TokenStatus, &rec.ManifestStatus, &rec.ThroughputStatus,
		&rec.ConnectMS, &rec.TokenMS, &rec.ManifestMS, &rec.ThroughputBPS,
		&rec.ResolvedDigest, &rec.Status, &rec.Detail,
	); err != nil {
		return ProbeRecord{}, wrapNotFound(fmt.Errorf("store: scan probe: %w", err))
	}

	t, err := parseTime(started)
	if err != nil {
		return ProbeRecord{}, err
	}
	rec.StartedAt = t

	return rec, nil
}
