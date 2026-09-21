package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// testTranslator returns a translator in English, so an assertion can name the
// text a reader would actually see.
func testTranslator(t *testing.T) *i18n.Translator {
	t.Helper()

	bundle, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	return bundle.Translator("en")
}

// fakeProber stands in for the engine so the panel can be tested without a
// registry.
//
// What it records is what the panel decided to measure, which is the part of a
// run that is policy rather than measurement — and policy is what these tests
// are about. The measurements themselves are the probe package's business.
type fakeProber struct {
	mu  sync.Mutex
	all [][]mirror.Source
	one []mirror.Source

	// allErr and oneErr make a recording failure reachable, which is how the
	// page's "measured but not saved" path gets exercised.
	allErr error
	oneErr error
}

func (f *fakeProber) RunAll(_ context.Context, sources []mirror.Source) probe.Summary {
	f.mu.Lock()
	f.all = append(f.all, sources)
	f.mu.Unlock()

	results := make([]probe.Result, len(sources))
	for i := range sources {
		results[i] = probe.Result{StartedAt: time.Now().UTC()}
	}

	var errs []error
	if f.allErr != nil {
		errs = append(errs, f.allErr)
	}
	return probe.Summary{Results: results, RecordErrors: errs}
}

func (f *fakeProber) RunOne(_ context.Context, src mirror.Source) (probe.Result, error) {
	f.mu.Lock()
	f.one = append(f.one, src)
	f.mu.Unlock()

	return probe.Result{StartedAt: time.Now().UTC()}, f.oneErr
}

// batches returns the mirror sets RunAll was given, and batchIDs the identifiers
// in the first of them.
func (f *fakeProber) batches() [][]mirror.Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.all
}

func (f *fakeProber) batchIDs(t *testing.T) []string {
	t.Helper()

	batches := f.batches()
	if len(batches) != 1 {
		t.Fatalf("RunAll was called %d times, want exactly once", len(batches))
	}

	ids := make([]string, 0, len(batches[0]))
	for _, src := range batches[0] {
		ids = append(ids, src.ID)
	}
	return ids
}

func (f *fakeProber) singleIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]string, 0, len(f.one))
	for _, src := range f.one {
		ids = append(ids, src.ID)
	}
	return ids
}

// withProbe wires a fake engine into the panel the way the binary does.
func withProbe(f *fakeProber) func(*Options) {
	return func(o *Options) {
		o.Probe = func(probe.Target) (Prober, error) { return f, nil }
	}
}

// withFailingFactory makes building the engine fail, which is a target the
// engine refuses rather than a mirror that cannot be reached.
func withFailingFactory(err error) func(*Options) {
	return func(o *Options) {
		o.Probe = func(probe.Target) (Prober, error) { return nil, err }
	}
}

// record stores a measurement, which is how the page gets something to render.
func (h *harness) record(rec store.ProbeRecord) {
	h.t.Helper()

	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now().UTC()
	}
	if err := h.store.InsertProbe(context.Background(), rec); err != nil {
		h.t.Fatalf("InsertProbe: %v", err)
	}
}

func TestProbePageWithoutAnEngineExplainsItself(t *testing.T) {
	h := newHarness(t)
	h.setup()

	resp := h.get("/probe")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /probe = %d, want 200", resp.StatusCode)
	}

	body := h.body(resp)
	if !strings.Contains(body, "no probe engine") {
		t.Error("the page does not explain that this build cannot measure")
	}
	// Offering a button that cannot work is the failure this branch exists to
	// avoid.
	if strings.Contains(body, "Measure every mirror") {
		t.Error("the page offers a run button without an engine behind it")
	}

	// And posting anyway is refused with the same explanation rather than a
	// 500 or a silent success.
	resp = h.post("/probe", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /probe = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/probe?flash=probe.error.unavailable" {
		t.Errorf("Location = %q", got)
	}
}

// clearProxies empties every proxy variable this process could pick up.
//
// Without it the egress assertions depend on whether the machine running the
// tests happens to have a proxy configured, which is a test that passes for one
// person and fails for another.
func clearProxies(t *testing.T) {
	t.Helper()

	for _, name := range proxyVars {
		t.Setenv(name, "")
	}
}

func TestProbePageNamesTheImageAndTheEgress(t *testing.T) {
	h := newHarness(t)
	h.setup()
	clearProxies(t)

	body := h.body(h.get("/probe"))

	defaults := probe.DefaultTarget()
	if !strings.Contains(body, defaults.Repository+":"+defaults.Reference) {
		t.Errorf("the page does not name the default image %s:%s",
			defaults.Repository, defaults.Reference)
	}
	// The vantage point is on the page because a speed number without it is
	// not actionable.
	if !strings.Contains(body, "Where these numbers come from") {
		t.Error("the page does not disclose where the measurements are taken")
	}
	if !strings.Contains(body, "No proxy variables are set") {
		t.Error("the page does not say the probe goes out directly")
	}
}

// A number taken through a tunnel describes the tunnel, so the page has to say
// so — and a proxy URL routinely carries a password.
func TestProbePageDisclosesAProxyWithoutItsCredentials(t *testing.T) {
	h := newHarness(t)
	h.setup()
	clearProxies(t)

	t.Setenv("HTTPS_PROXY", "http://ops:hunter2@proxy.internal:3128")

	// A GET of the page is all this needs; the environment is read per request.
	body := h.body(h.get("/probe"))

	if !strings.Contains(body, "goes through it") {
		t.Error("the page does not report that a proxy is configured")
	}
	if !strings.Contains(body, "proxy.internal:3128") {
		t.Error("the page does not name the proxy")
	}
	if strings.Contains(body, "hunter2") {
		t.Error("the proxy password reached the page")
	}
	if !strings.Contains(body, "…@proxy.internal:3128") {
		t.Error("the proxy is not shown in a redacted form")
	}
}

func TestProbeBatchCoversEnabledDockerHubMirrorsOnly(t *testing.T) {
	fake := &fakeProber{}
	h := newHarness(t, withProbe(fake))
	h.setup()
	h.seedBuiltin()

	// A mirror that proxies only ghcr.io answers 404 to every layer of a
	// Docker Hub probe, so including it would report a mirror as failing for
	// something it never claimed to do.
	h.post("/sources", userMirror(map[string]string{
		"id":    "ghcr-only",
		"name":  "GHCR only",
		"scope": "ghcr",
	}))

	resp := h.post("/probe", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /probe = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}
	if got := resp.Header.Get("Location"); got != "/probe?flash=probe.ran" {
		t.Errorf("Location = %q", got)
	}

	got := fake.batchIDs(t)

	// The order is the store's: enabled first, then by name, case-insensitively.
	// A batch that reshuffled between runs would make the table jump around.
	// The catalogue's defaults enable only the sources that serve anonymous
	// pulls from anywhere; everything else ships switched off.
	want := []string{"1ms", "1panel", "daocloud"}

	if len(got) != len(want) {
		t.Fatalf("the batch covered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the batch covered %v, want %v", got, want)
		}
	}

	for _, id := range got {
		switch id {
		case "ghcr-only":
			t.Error("a mirror that does not proxy Docker Hub was measured and would be blamed for the 404")
		case "rat-dev":
			t.Errorf("a switched-off mirror (%s) was measured", id)
		}
	}
}

// A batch run is skipped entirely when there is nothing to measure, rather than
// reporting a run that covered nothing.
func TestProbeBatchWithNothingEnabledStillSucceeds(t *testing.T) {
	fake := &fakeProber{}
	h := newHarness(t, withProbe(fake))
	h.setup()

	resp := h.post("/probe", nil)
	if got := resp.Header.Get("Location"); got != "/probe?flash=probe.ran" {
		t.Errorf("Location = %q", got)
	}

	batches := fake.batches()
	if len(batches) != 1 {
		t.Fatalf("RunAll was called %d times, want once", len(batches))
	}
	if len(batches[0]) != 0 {
		t.Errorf("the batch covered %v, want nothing", batches[0])
	}
}

// Probing one mirror by hand ignores the enabled flag: being unable to test a
// mirror you just switched off is how you end up switching it back on to find
// out whether it works.
func TestProbeOneMirrorIgnoresTheEnabledFlag(t *testing.T) {
	fake := &fakeProber{}
	h := newHarness(t, withProbe(fake))
	h.setup()

	if resp := h.post("/sources", userMirror(map[string]string{"enabled": ""})); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}

	resp := h.post("/probe", url.Values{"id": {"my-mirror"}})
	if got := resp.Header.Get("Location"); got != "/probe?flash=probe.ran" {
		t.Errorf("Location = %q", got)
	}

	got := fake.singleIDs()
	if len(got) != 1 || got[0] != "my-mirror" {
		t.Errorf("a single run measured %v, want [my-mirror]", got)
	}
	if batches := fake.batches(); len(batches) != 0 {
		t.Errorf("a single run also started %d batch(es)", len(batches))
	}
}

func TestProbeRunSurvivesARecordingFailure(t *testing.T) {
	fake := &fakeProber{allErr: errors.New("disk is full")}
	h := newHarness(t, withProbe(fake))
	h.setup()

	// The measurement still happened, so the run is not reported as a failure
	// — the history graph going flat is a bug nobody would report otherwise,
	// which is why it is logged.
	resp := h.post("/probe", nil)
	if got := resp.Header.Get("Location"); got != "/probe?flash=probe.ran" {
		t.Errorf("Location = %q, want the run to be reported as finished", got)
	}
}

func TestProbeRunRefusesAnUnusableTarget(t *testing.T) {
	h := newHarness(t, withFailingFactory(errors.New("mirror: repository is required")))
	h.setup()

	resp := h.post("/probe", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /probe = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/probe?flash=probe.error.target" {
		t.Errorf("Location = %q", got)
	}
}

// The page shows the four layers separately, because "why" is what makes it
// worth reading.
func TestProbePageShowsEachLayerAndTheResolvedDigest(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	digest := "sha256:" + strings.Repeat("ab12", 16)

	h.record(store.ProbeRecord{
		SourceID:         "daocloud",
		Connectivity:     string(probe.StatusOK),
		TokenStatus:      string(probe.StatusOK),
		ManifestStatus:   string(probe.StatusOK),
		ThroughputStatus: string(probe.StatusOK),
		ConnectMS:        42,
		TokenMS:          118,
		ManifestMS:       260,
		ThroughputBPS:    11_500_000,
		ResolvedDigest:   digest,
		Status:           string(probe.StatusOK),
	})

	// A mirror that was told to back off: up, but not talking to us. 1ms.run
	// is the one with a real history of rate limiting, so it is the honest
	// stand-in.
	h.record(store.ProbeRecord{
		SourceID:     "1ms",
		Connectivity: string(probe.StatusOK),
		TokenStatus:  string(probe.StatusRateLimited),
		Status:       string(probe.StatusRateLimited),
		ConnectMS:    91,
		Detail:       "429 after 1s",
	})

	body := h.body(h.get("/probe"))

	for _, want := range []string{
		"42 ms", "118 ms", "260 ms",
		"11.5 MB/s",
		// The digest is trimmed in the cell and carried whole in the title, so
		// nobody copies a prefix believing they have the digest.
		`title="` + digest + `"`,
		"ab12ab12ab12…",
		// Rate limiting is its own verdict, not a failure.
		"rate limited",
		// And the layer that never ran is a dash rather than a zero.
		"—",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the results table does not contain %q", want)
		}
	}

	// A mirror with no history at all says so instead of drawing a flat line.
	if !strings.Contains(body, "not measured yet") {
		t.Error("an unmeasured mirror does not say so")
	}
	if !strings.Contains(body, "not enough runs") {
		t.Error("a mirror with one sample does not explain why there is no trend line")
	}
}

// One sample is not a trend and a one-pixel line reads as a measurement nobody
// made, so the sparkline waits for two.
func TestProbeSparklineNeedsTwoSamples(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	first := store.ProbeRecord{
		SourceID:     "daocloud",
		Connectivity: string(probe.StatusOK),
		Status:       string(probe.StatusOK),
		StartedAt:    time.Now().UTC().Add(-time.Hour),
	}
	h.record(first)

	if body := h.body(h.get("/probe")); strings.Contains(body, "<polyline") {
		t.Error("a single sample drew a trend line")
	}

	second := first
	second.StartedAt = time.Now().UTC()
	h.record(second)

	body := h.body(h.get("/probe"))
	if !strings.Contains(body, "<polyline") {
		t.Error("two samples did not draw a trend line")
	}
	// The samples are laid out left to right, oldest first.
	if !strings.Contains(body, `points="0.0`) && !strings.Contains(body, `points="0.0,`) {
		t.Error("the trend line does not start at the left edge")
	}
}

func TestProbePageMarksAMirrorThatIsNotForThisImage(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	// Enabled, but it proxies only ghcr.io, so a Docker Hub probe would be
	// unfair to it.
	if resp := h.post("/sources", userMirror(map[string]string{
		"id":    "ghcr-only",
		"name":  "GHCR only",
		"scope": "ghcr",
	})); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}

	body := h.body(h.get("/probe"))

	if !strings.Contains(body, "not for this image") {
		t.Error("a mirror that does not serve the target upstream is not marked")
	}
	if !strings.Contains(body, "switched off") {
		t.Error("the switched-off built-in mirrors are not marked")
	}
}

// A POST has to be gated too, not only the GET that renders the page.
//
// The session cookie is dropped but the CSRF cookie is kept, because CSRF is
// checked before the session: sending no token would be rejected as a forgery
// and never reach the check this test is about.
func TestProbePageRedirectsRunsWhileLoggedOut(t *testing.T) {
	h := newHarness(t, withProbe(&fakeProber{}))
	h.setup()

	u, err := url.Parse(h.ts.URL)
	if err != nil {
		t.Fatalf("parse the test server URL: %v", err)
	}

	// Expire the session cookie in the jar; the CSRF cookie stays.
	h.client.Jar.SetCookies(u, []*http.Cookie{{
		Name:   auth.SessionCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	}})

	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == auth.SessionCookieName {
			t.Fatalf("the session cookie is still in the jar: %v", c)
		}
	}

	resp := h.post("/probe", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /probe while logged out = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}
}

func TestHumanRate(t *testing.T) {
	cases := map[int64]string{
		// Nothing measured is not "0 B/s": the two mean different things and
		// only one of them was observed.
		0:   "",
		-1:  "",
		1:   "1 B/s",
		999: "999 B/s",
		// Decimal units, because every figure a person compares throughput
		// against is decimal.
		1_000:         "1.0 kB/s",
		1_500:         "1.5 kB/s",
		999_999:       "1000.0 kB/s",
		1_000_000:     "1.0 MB/s",
		11_500_000:    "11.5 MB/s",
		2_500_000_000: "2.5 GB/s",
	}

	for bps, want := range cases {
		if got := humanRate(bps); got != want {
			t.Errorf("humanRate(%d) = %q, want %q", bps, got, want)
		}
	}
}

func TestShortDigest(t *testing.T) {
	long := "sha256:" + strings.Repeat("ab12", 16)

	cases := map[string]string{
		"":              "",
		"sha256:abc123": "sha256:abc123",
		long:            "ab12ab12ab12…",
		// A value with no algorithm prefix is still trimmed rather than
		// printed whole.
		strings.Repeat("f", 20): "ffffffffffff…",
	}

	for digest, want := range cases {
		if got := shortDigest(digest); got != want {
			t.Errorf("shortDigest(%q) = %q, want %q", digest, got, want)
		}
	}
}

// A proxy URL routinely carries a username and a password, and this value lands
// in HTML that gets screenshotted into bug reports.
func TestRedactProxy(t *testing.T) {
	cases := map[string]string{
		"http://proxy.local:3128":             "http://proxy.local:3128",
		"http://ops:hunter2@proxy.local:3128": "…@proxy.local:3128",
		"socks5://dc:pass@10.0.0.1:1080":      "…@10.0.0.1:1080",
		"http://ops@proxy.local:3128":         "…@proxy.local:3128",
		"nonsense:pass@host":                  "…@host",
		"":                                    "",
		// Only the last "@" matters, so a password containing one cannot
		// smuggle the earlier part through.
		"http://user:pa@ss@proxy.local:3128": "…@proxy.local:3128",
	}

	for value, want := range cases {
		if got := redactProxy(value); got != want {
			t.Errorf("redactProxy(%q) = %q, want %q", value, got, want)
		}
	}
}

// A layer that produced no number is a dash, not a zero: zero means either
// "never ran" or "finished faster than the clock could resolve", and printing
// "0 ms" asserts the second without knowing it.
func TestLayerViewsKeepAnUnmeasuredLayerApartFromAMeasuredZero(t *testing.T) {
	tr := testTranslator(t)

	measured := newTimeLayer(probe.StatusOK, 42, tr)
	if !measured.Measured || measured.Value != "42 ms" {
		t.Errorf("a measured layer = %+v, want 42 ms and Measured", measured)
	}
	if measured.Class != "pill-ok" || measured.Label != "ok" {
		t.Errorf("a measured layer = %+v, want the ok badge", measured)
	}

	unmeasured := newTimeLayer(probe.StatusOK, 0, tr)
	if unmeasured.Measured || unmeasured.Value != "" {
		t.Errorf("an unmeasured layer = %+v, want nothing measured", unmeasured)
	}
	// The status is still shown: an unmeasured layer still has a verdict, and
	// hiding it would make a failed step look like a step that never ran.
	if unmeasured.Label != "ok" {
		t.Errorf("an unmeasured layer lost its verdict: %+v", unmeasured)
	}

	rate := newRateLayer(probe.StatusRateLimited, 0, tr)
	if rate.Measured {
		t.Errorf("a rate layer with nothing measured = %+v", rate)
	}
	if rate.Class != "pill-warn" || rate.Label != "rate limited" {
		t.Errorf("a rate-limited layer = %+v, want the amber badge", rate)
	}

	if got := statusClass(probe.Status("something_new")); got != "pill-muted" {
		t.Errorf("an unknown status = %q, want the muted badge", got)
	}
	// The fallback shows the raw value rather than a blank pill: a blank pill
	// is a lie, and "rate_limited" at least says what happened.
	if got := statusLabel(probe.Status("something_new"), tr); got != "something_new" {
		t.Errorf("an unknown status = %q, want the raw value", got)
	}
}

// The series reads oldest first because a time axis does, and failed runs stay
// in as zeroes: dropping them would make an unreliable mirror look steady.
func TestThroughputSeriesIsOldestFirstAndKeepsFailures(t *testing.T) {
	history := []store.ProbeRecord{
		{ThroughputBPS: 300}, // newest
		{ThroughputBPS: 0},
		{ThroughputBPS: 100}, // oldest
	}

	got := throughputSeries(history)
	want := []int64{100, 0, 300}

	if len(got) != len(want) {
		t.Fatalf("throughputSeries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("throughputSeries = %v, want %v", got, want)
		}
	}
}

func TestSparklineGeometry(t *testing.T) {
	if got := newSparkline([]int64{5}, 120, 28); got != nil {
		t.Error("one sample produced a trend line")
	}
	if got := newSparkline(nil, 120, 28); got != nil {
		t.Error("no samples produced a trend line")
	}

	// A series of zeroes has no shape to draw, so it becomes a flat line along
	// the bottom rather than a division by zero.
	flat := newSparkline([]int64{0, 0, 0}, 120, 28)
	if flat == nil {
		t.Fatal("three samples produced no trend line")
	}
	if flat.Peak != 0 || flat.PeakLabel != "" {
		t.Errorf("a flat series reports a peak of %d (%q)", flat.Peak, flat.PeakLabel)
	}
	if strings.Contains(flat.Points, "NaN") || strings.Contains(flat.Points, "+Inf") {
		t.Errorf("a flat series produced %q", flat.Points)
	}

	rise := newSparkline([]int64{0, 1000}, 120, 28)
	if rise == nil {
		t.Fatal("two samples produced no trend line")
	}
	if rise.Peak != 1000 {
		t.Errorf("peak = %d, want 1000", rise.Peak)
	}
	if rise.PeakLabel != "1.0 kB/s" {
		t.Errorf("peak label = %q", rise.PeakLabel)
	}
	// The highest sample sits at the top of the box and the lowest at the
	// bottom; y grows downwards, which is the trap here.
	if !strings.Contains(rise.Points, "0.0,28.0 120.0,0.0") {
		t.Errorf("the geometry is %q, want the series to run bottom-left to top-right", rise.Points)
	}
	if rise.Samples != 2 {
		t.Errorf("samples = %d, want 2", rise.Samples)
	}
}
