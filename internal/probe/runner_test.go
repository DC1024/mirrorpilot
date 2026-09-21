package probe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// fakeRecorder stands in for the database. It keeps the two things a runner
// test needs to see — what was written, and how often pruning ran — and can be
// told to fail, because a panel that silently loses its history is a bug worth
// having a test for.
type fakeRecorder struct {
	mu       sync.Mutex
	records  []store.ProbeRecord
	prunes   []int
	insertEr error
	pruneEr  error
}

func (f *fakeRecorder) InsertProbe(_ context.Context, rec store.ProbeRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertEr != nil {
		return f.insertEr
	}
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeRecorder) PruneProbes(_ context.Context, keep int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes = append(f.prunes, keep)
	return 0, f.pruneEr
}

func (f *fakeRecorder) inserted() []store.ProbeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ProbeRecord(nil), f.records...)
}

func (f *fakeRecorder) pruneCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.prunes...)
}

// workingProbe returns a stubbed mirror that passes all four layers, plus a
// Prober pointing at it.
func workingProbe(t *testing.T) (*Prober, mirror.Source) {
	t.Helper()

	blob := []byte("some layer content, long enough to time")
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	srv, src, _ := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {body: child}},
		blobs:     map[string][]byte{digestOf(blob): blob},
	})

	return newProber(t, srv, nil), src
}

func TestNewRunnerRejectsMissingPieces(t *testing.T) {
	prober, _ := workingProbe(t)

	if _, err := NewRunner(nil, &fakeRecorder{}, RunnerOptions{}); err == nil {
		t.Error("want an error for a nil prober")
	}
	if _, err := NewRunner(prober, nil, RunnerOptions{}); err == nil {
		t.Error("want an error for a nil recorder")
	}
}

func TestNewRunnerAppliesDefaults(t *testing.T) {
	prober, _ := workingProbe(t)

	r, err := NewRunner(prober, &fakeRecorder{}, RunnerOptions{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	if r.concurrency != DefaultConcurrency {
		t.Errorf("concurrency = %d, want %d", r.concurrency, DefaultConcurrency)
	}
	if r.keep != DefaultKeepPerSource {
		t.Errorf("keep = %d, want %d", r.keep, DefaultKeepPerSource)
	}
	if r.Target() != defaultTarget() {
		t.Errorf("target = %+v", r.Target())
	}
}

func TestRunAllRecordsEveryMirror(t *testing.T) {
	prober, src := workingProbe(t)
	rec := &fakeRecorder{}

	r, err := NewRunner(prober, rec, RunnerOptions{Concurrency: 2, KeepPerSource: 5})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	sources := []mirror.Source{
		{ID: "a", Name: "A", URL: src.URL},
		{ID: "b", Name: "B", URL: src.URL},
		{ID: "c", Name: "C", URL: src.URL},
	}

	got := r.RunAll(t.Context(), sources)

	if len(got.RecordErrors) != 0 {
		t.Fatalf("record errors = %v", got.RecordErrors)
	}
	if len(got.Results) != len(sources) {
		t.Fatalf("results = %d, want %d", len(got.Results), len(sources))
	}
	for i, res := range got.Results {
		if res.SourceID != sources[i].ID {
			t.Errorf("results[%d].SourceID = %q, want %q — order must follow the input",
				i, res.SourceID, sources[i].ID)
		}
		if res.Status != StatusOK {
			t.Errorf("results[%d].Status = %q, want %q", i, res.Status, StatusOK)
		}
	}

	if n := len(rec.inserted()); n != len(sources) {
		t.Errorf("inserted %d rows, want %d", n, len(sources))
	}
	if prunes := rec.pruneCalls(); len(prunes) != 1 || prunes[0] != 5 {
		t.Errorf("prune calls = %v, want a single call with keep=5", prunes)
	}
}

func TestRunAllKeepsInputOrderWhenFinishingOrderDiffers(t *testing.T) {
	// One slow mirror and one fast one. The slow one is first, so if results
	// were appended as they finished the table would come back reversed — and
	// a dashboard whose rows reshuffle between refreshes is unusable.
	srv, _, _ := newStub(t, stub{})
	slowSrv, _, _ := newStub(t, stub{delay: 150 * time.Millisecond})

	prober := newProber(t, srv, nil)
	r, err := NewRunner(prober, &fakeRecorder{}, RunnerOptions{Concurrency: 2})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	sources := []mirror.Source{
		{ID: "slow", URL: slowSrv.URL},
		{ID: "fast", URL: srv.URL},
	}

	got := r.RunAll(t.Context(), sources)

	if got.Results[0].SourceID != "slow" {
		t.Errorf("results[0] = %q, want the first input mirror", got.Results[0].SourceID)
	}
	if got.Results[1].SourceID != "fast" {
		t.Errorf("results[1] = %q, want the second input mirror", got.Results[1].SourceID)
	}
}

func TestRunAllRespectsConcurrencyLimit(t *testing.T) {
	// Every request sleeps, so overlapping probes show up as overlapping
	// requests. Probing is not allowed to become a burst: these are mirrors
	// other people pay for, and getting rate limited would then be recorded as
	// if it were a property of the mirror.
	srv, src, log := newStub(t, stub{delay: 40 * time.Millisecond})
	prober := newProber(t, srv, nil)

	const concurrency = 2
	r, err := NewRunner(prober, &fakeRecorder{}, RunnerOptions{Concurrency: concurrency})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	sources := make([]mirror.Source, 6)
	for i := range sources {
		sources[i] = mirror.Source{ID: fmt.Sprintf("m%d", i), URL: src.URL}
	}

	r.RunAll(t.Context(), sources)

	if peak := log.peakConcurrent(); peak > concurrency {
		t.Errorf("peak concurrent requests = %d, want at most %d", peak, concurrency)
	}
	if peak := log.peakConcurrent(); peak < 2 {
		t.Errorf("peak concurrent requests = %d — nothing overlapped, so the limit is not being used", peak)
	}
}

func TestRunAllSurfacesRecordingFailuresWithoutLosingResults(t *testing.T) {
	prober, src := workingProbe(t)
	boom := errors.New("disk on fire")
	rec := &fakeRecorder{insertEr: boom}

	r, err := NewRunner(prober, rec, RunnerOptions{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	got := r.RunAll(t.Context(), []mirror.Source{{ID: "a", URL: src.URL}})

	if len(got.RecordErrors) != 1 || !errors.Is(got.RecordErrors[0], boom) {
		t.Fatalf("record errors = %v, want the insert failure", got.RecordErrors)
	}
	// The measurement is still reported: a dashboard showing live numbers
	// while failing to trend them beats one showing nothing.
	if len(got.Results) != 1 || got.Results[0].Status != StatusOK {
		t.Errorf("results = %+v, want the measurement intact", got.Results)
	}
}

func TestRunAllReportsPruneFailures(t *testing.T) {
	prober, src := workingProbe(t)
	boom := errors.New("no space left")
	rec := &fakeRecorder{pruneEr: boom}

	r, err := NewRunner(prober, rec, RunnerOptions{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	got := r.RunAll(t.Context(), []mirror.Source{{ID: "a", URL: src.URL}})

	if len(got.RecordErrors) != 1 || !errors.Is(got.RecordErrors[0], boom) {
		t.Errorf("record errors = %v, want the prune failure", got.RecordErrors)
	}
}

func TestRunAllWithNothingToDo(t *testing.T) {
	prober, _ := workingProbe(t)
	rec := &fakeRecorder{}

	r, err := NewRunner(prober, rec, RunnerOptions{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	got := r.RunAll(t.Context(), nil)

	if len(got.Results) != 0 || len(got.RecordErrors) != 0 {
		t.Errorf("summary = %+v, want empty", got)
	}
	if len(rec.inserted()) != 0 {
		t.Errorf("inserted %d rows for an empty run", len(rec.inserted()))
	}
}

func TestRunOneRecordsAndPrunes(t *testing.T) {
	prober, src := workingProbe(t)
	rec := &fakeRecorder{}

	r, err := NewRunner(prober, rec, RunnerOptions{KeepPerSource: 42})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	res, err := r.RunOne(t.Context(), mirror.Source{ID: "solo", URL: src.URL})
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != StatusOK {
		t.Errorf("status = %q, want %q", res.Status, StatusOK)
	}

	rows := rec.inserted()
	if len(rows) != 1 {
		t.Fatalf("inserted %d rows, want 1", len(rows))
	}
	if rows[0].SourceID != "solo" {
		t.Errorf("row source = %q, want %q", rows[0].SourceID, "solo")
	}
	if prunes := rec.pruneCalls(); len(prunes) != 1 || prunes[0] != 42 {
		t.Errorf("prune calls = %v, want one call with keep=42", prunes)
	}
}

func TestRunOneReportsARecordingFailure(t *testing.T) {
	prober, src := workingProbe(t)
	boom := errors.New("read-only database")
	rec := &fakeRecorder{insertEr: boom}

	r, err := NewRunner(prober, rec, RunnerOptions{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	res, err := r.RunOne(t.Context(), mirror.Source{ID: "solo", URL: src.URL})

	// Unlike RunAll, RunOne was asked for exactly one thing and can say the
	// record did not stick. The measurement still comes back.
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the insert failure", err)
	}
	if res.Status != StatusOK {
		t.Errorf("status = %q, want the measurement to survive", res.Status)
	}
}

func TestRecordFromFlattensAResult(t *testing.T) {
	started := time.Date(2026, 9, 21, 1, 30, 0, 0, time.UTC)
	res := Result{
		SourceID:       "ignored-in-favour-of-the-argument",
		StartedAt:      started,
		Connect:        Layer{Status: StatusOK, Duration: 12 * time.Millisecond},
		Token:          Layer{Status: StatusSkipped},
		Manifest:       Layer{Status: StatusOK, Duration: 34 * time.Millisecond},
		Throughput:     Layer{Status: StatusOK, Duration: 500 * time.Millisecond},
		Bytes:          1000,
		BlobDigestOK:   true,
		ResolvedDigest: "sha256:abc",
		Status:         StatusOK,
		Detail:         "all good",
	}

	got := recordFrom("src-1", res)

	if got.SourceID != "src-1" {
		t.Errorf("source = %q, want the argument, not the result's own field", got.SourceID)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("started = %v, want %v", got.StartedAt, started)
	}
	if got.Connectivity != string(StatusOK) {
		t.Errorf("connectivity = %q", got.Connectivity)
	}
	if got.ConnectMS != 12 || got.ManifestMS != 34 {
		t.Errorf("timings = %d/%d, want 12/34", got.ConnectMS, got.ManifestMS)
	}
	// Stored as a rate, not as a byte count and a duration: a stored pair
	// invites a reader to divide them wrongly.
	if got.ThroughputBPS != 2000 {
		t.Errorf("throughput = %d, want 2000", got.ThroughputBPS)
	}
	if got.Status != string(StatusOK) || got.Detail != "all good" {
		t.Errorf("status/detail = %q/%q", got.Status, got.Detail)
	}
}

// Requests made by a probe that fails early still reach the server; a test
// that only inspected the Result would not notice the extra traffic.
func TestRunAllProbesUnreachableMirrorsWithoutFailing(t *testing.T) {
	prober, _ := workingProbe(t)

	srv := httptest.NewServer(http.NotFoundHandler())
	deadURL := srv.URL
	srv.Close()

	rec := &fakeRecorder{}
	r, err := NewRunner(prober, rec, RunnerOptions{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	got := r.RunAll(t.Context(), []mirror.Source{{ID: "dead", URL: deadURL}})

	if len(got.RecordErrors) != 0 {
		t.Fatalf("record errors = %v", got.RecordErrors)
	}
	if len(got.Results) != 1 || got.Results[0].Status != StatusUnreachable {
		t.Fatalf("results = %+v, want one unreachable verdict", got.Results)
	}
	// An unreachable mirror is still recorded: "it is down" is exactly the
	// history worth having.
	if rows := rec.inserted(); len(rows) != 1 || rows[0].Status != string(StatusUnreachable) {
		t.Errorf("rows = %+v, want the failure recorded", rows)
	}
}
