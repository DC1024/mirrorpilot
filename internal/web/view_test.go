package web

import (
	"strings"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/ghsync"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// The three functions here turn a record into a row. Their helpers — the
// translator lookups, humanRate, formatWhen, formatDuration — already have
// tests of their own; what had none is the wiring, and wiring is where the
// mistakes are: seven fields in a struct literal, all strings, so swapping
// Title and Branch compiles, renders, and puts the branch name where the run
// title belongs.
//
// Every field below is therefore given a value that cannot be confused with any
// other field's value.

func TestNewProbeRunMapsAVerdict(t *testing.T) {
	tr := testTranslator(t)

	run := newProbeRun(store.ProbeRecord{
		Status:        string(probe.StatusRateLimited),
		ThroughputBPS: 2_000_000,
	}, tr)

	// Rate limiting is amber, not red: the mirror is up and would not talk to
	// us, which is a different statement from a mirror that answered wrongly.
	if run.Class != "pill-warn" {
		t.Errorf("Class = %q, want pill-warn: rate limiting is not a fault", run.Class)
	}
	if want := tr.T("probe.status.rate_limited"); run.Label != want {
		t.Errorf("Label = %q, want %q", run.Label, want)
	}
	if run.BPSLabel != "2.0 MB/s" {
		t.Errorf("BPSLabel = %q, want 2.0 MB/s", run.BPSLabel)
	}
}

// A status this build has never heard of — written by a newer version, read by
// an older one — falls back to the raw value and the muted badge. A blank pill
// would be a lie, and a wrong colour worse.
func TestNewProbeRunFallsBackOnAnUnknownStatus(t *testing.T) {
	run := newProbeRun(store.ProbeRecord{Status: "something_new"}, testTranslator(t))

	if run.Label != "something_new" {
		t.Errorf("Label = %q, want the raw status", run.Label)
	}
	if run.Class != "pill-muted" {
		t.Errorf("Class = %q, want pill-muted", run.Class)
	}
	// Nothing was measured, and that is not the same as zero bytes per second.
	if run.BPSLabel != "" {
		t.Errorf("BPSLabel = %q, want empty when nothing was measured", run.BPSLabel)
	}
}

func TestNewSyncRunViewMapsARun(t *testing.T) {
	created := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	run := ghsync.Run{
		ID:         4242,
		Status:     "completed",
		Conclusion: "success",
		Title:      "Deploy the panel",
		HeadBranch: "release/1.9",
		URL:        "https://github.com/o/r/actions/runs/4242",
		CreatedAt:  created,
	}

	view := newSyncRunView(run, testTranslator(t))

	if view.ID != 4242 {
		t.Errorf("ID = %d, want 4242", view.ID)
	}
	if view.Title != "Deploy the panel" {
		t.Errorf("Title = %q, want the run title", view.Title)
	}
	if view.Branch != "release/1.9" {
		t.Errorf("Branch = %q, want the head branch", view.Branch)
	}
	if view.URL != run.URL {
		t.Errorf("URL = %q, want %q", view.URL, run.URL)
	}
	if want := testTranslator(t).T("sync.state.succeeded"); view.Label != want {
		t.Errorf("Label = %q, want %q", view.Label, want)
	}
	if view.Class != "pill-ok" {
		t.Errorf("Class = %q, want pill-ok", view.Class)
	}

	// The timestamp arrives already rendered, in local time, to the minute.
	// Parsing it back is what proves that: comparing against a literal would
	// just be the implementation written twice, and would break in any
	// timezone but this one.
	parsed, err := time.ParseInLocation("2006-01-02 15:04", view.Created, time.Local)
	if err != nil {
		t.Fatalf("Created = %q, which is not a rendered timestamp: %v", view.Created, err)
	}
	if !parsed.Equal(created) {
		t.Errorf("Created = %q, which is not %s", view.Created, created)
	}
}

func TestNewSyncRunViewWithoutATimestamp(t *testing.T) {
	// A queued run can arrive with no creation time. An empty cell is honest;
	// "0001-01-01 00:00" is not.
	view := newSyncRunView(ghsync.Run{ID: 1, Status: "queued"}, testTranslator(t))

	if view.Created != "" {
		t.Errorf("Created = %q, want empty", view.Created)
	}
	if view.Label != testTranslator(t).T("sync.state.queued") {
		t.Errorf("Label = %q, want the queued label", view.Label)
	}
}

func TestNewSyncJobViewMapsAJob(t *testing.T) {
	started := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	job := ghsync.Job{
		ID:          7,
		Name:        "Test & vet",
		Status:      "completed",
		Conclusion:  "failure",
		URL:         "https://github.com/o/r/actions/runs/4242/job/7",
		StartedAt:   started,
		CompletedAt: started.Add(90 * time.Second),
	}

	view := newSyncJobView(job, testTranslator(t))

	if view.Name != "Test & vet" {
		t.Errorf("Name = %q, want the job name", view.Name)
	}
	if view.URL != job.URL {
		t.Errorf("URL = %q, want %q", view.URL, job.URL)
	}
	if view.Duration != "1m30s" {
		t.Errorf("Duration = %q, want 1m30s", view.Duration)
	}
	if view.Label != testTranslator(t).T("sync.state.failed") {
		t.Errorf("Label = %q, want the failed label", view.Label)
	}
	if view.Class != "pill-bad" {
		t.Errorf("Class = %q, want pill-bad", view.Class)
	}
}

// A job that has not finished has no duration. That is not a duration of zero,
// and rendering "0s" would say it finished instantly.
func TestNewSyncJobViewHasNoDurationUntilItFinishes(t *testing.T) {
	job := ghsync.Job{
		Name:      "Build",
		Status:    "in_progress",
		StartedAt: time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC),
	}

	view := newSyncJobView(job, testTranslator(t))

	if view.Duration != "" {
		t.Errorf("Duration = %q, want empty while the job is still running", view.Duration)
	}
	if view.Label != testTranslator(t).T("sync.state.running") {
		t.Errorf("Label = %q, want the running label", view.Label)
	}
	if strings.TrimSpace(view.Class) == "" {
		t.Error("Class is empty: every state must get a badge colour")
	}
}
