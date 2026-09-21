package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/DC1024/mirrorpilot/internal/catalog"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// Prober is the probe engine as the panel uses it.
//
// An interface rather than the concrete runner so the panel can be tested
// without a fake registry, and so the engine stays free of HTTP-handler
// concerns.
type Prober interface {
	RunAll(ctx context.Context, sources []mirror.Source) probe.Summary
	RunOne(ctx context.Context, src mirror.Source) (probe.Result, error)
}

// ProbeFactory builds a Prober for an image.
type ProbeFactory func(target probe.Target) (Prober, error)

// statusKeys names the translation key for each verdict.
//
// Written out rather than built from the status string. A constructed key is
// invisible to the check that proves every shipped string is actually shown, so
// the translations would quietly become dead weight; and a key that goes
// missing renders on the page as its own name, which is the sort of thing
// nobody notices in review.
var statusKeys = map[probe.Status]string{
	probe.StatusOK:           "probe.status.ok",
	probe.StatusUnauthorized: "probe.status.unauthorized",
	probe.StatusRateLimited:  "probe.status.rate_limited",
	probe.StatusUnreachable:  "probe.status.unreachable",
	probe.StatusFailed:       "probe.status.failed",
	probe.StatusSkipped:      "probe.status.skipped",
}

// statusClasses colour a verdict.
//
// Rate limiting and an authentication challenge share the amber badge even
// though neither is a fault: both mean the mirror is up and would not talk to
// us, which is a different statement from the red of a mirror that answered
// wrongly or not at all.
var statusClasses = map[probe.Status]string{
	probe.StatusOK:           "pill-ok",
	probe.StatusUnauthorized: "pill-warn",
	probe.StatusRateLimited:  "pill-warn",
	probe.StatusUnreachable:  "pill-bad",
	probe.StatusFailed:       "pill-bad",
	probe.StatusSkipped:      "pill-muted",
}

const (
	// sparklineSamples is how much history each sparkline draws. Enough to
	// show a trend, few enough that one bad afternoon does not dominate it.
	sparklineSamples = 24

	sparklineWidth  = 120
	sparklineHeight = 28

	// probeRunTimeout bounds a whole batch, detached from the request.
	//
	// Generous, because a dozen mirrors at fifteen seconds each with four in
	// flight is a minute of work in the worst case — but finite, because a
	// request that can never finish is a request that holds a connection open
	// and a browser waiting with no explanation.
	probeRunTimeout = 3 * time.Minute
)

// layerView is one of the four measurement steps, as the page shows it.
type layerView struct {
	Label string
	Class string

	// Value is the layer's number, already formatted: milliseconds for the
	// first three layers, a rate for the fourth.
	//
	// Formatted here rather than in the template because the two kinds of
	// quantity are not interchangeable. Handing the template a bare number
	// alongside a separate unit would invite someone to print a throughput in
	// milliseconds, which would look entirely plausible and be wrong by three
	// orders of magnitude.
	Value string

	// Measured is false when the layer produced nothing, which the template
	// renders as a dash.
	//
	// Zero is ambiguous — it means either "never ran" or "finished faster than
	// the clock could resolve" — and printing "0 ms" asserts the second.
	Measured bool
}

// probeRun is a mirror's newest measurement as the mirror list shows it — a
// badge and a rate.
//
// Deliberately smaller than the speed test page's row: a list of every mirror
// answers "is this one working", and the two-line summary is all that answer
// needs. Spreading the four layers across the list would make the columns that
// matter harder to compare.
type probeRun struct {
	Label string
	Class string

	// BPSLabel is the measured throughput for a person to read, empty when
	// nothing was measured.
	BPSLabel string
}

// verdict is a mirror's newest overall result.
type verdict struct {
	Status probe.Status
	Label  string
	Class  string

	// BPSLabel is the measured throughput for a person to read, empty when
	// nothing was measured.
	BPSLabel string

	Detail    string
	StartedAt time.Time
}

// probeRow is one mirror on the speed test page.
type probeRow struct {
	SourceID string
	Name     string
	Enabled  bool

	// Servable marks a mirror a batch run would include: enabled, and proxying
	// the upstream registry the target image lives in.
	Servable bool

	// Verdict is nil when this mirror has never been probed. A pointer so the
	// template can keep "not tried yet" apart from "tried and failed" — two
	// states that look identical when both render as an empty badge.
	Verdict *verdict

	// The four layers, from the newest row. Zero-valued when there is none.
	Connect    layerView
	Token      layerView
	Manifest   layerView
	Throughput layerView

	// Digest is the manifest digest that was resolved, and ShortDigest is the
	// same value trimmed for a table cell.
	Digest      string
	ShortDigest string

	// Sparkline is nil below two samples: one point is not a trend, and a
	// one-pixel line reads as a measurement nobody made.
	Sparkline *sparkline
}

// sparkline is a throughput history, as geometry.
//
// Only numbers live here; the template writes the <svg> element around them.
// Handing markup back would need template.HTML, and turning a rendering
// convenience into the one construct that bypasses escaping is not a trade
// worth making for a list of coordinates.
type sparkline struct {
	Width  int
	Height int

	// Points is the polyline's points attribute.
	Points string

	// Peak is the highest sample and PeakLabel its rendering, for the caption.
	Peak      int64
	PeakLabel string

	Samples int
}

// egressView describes where the measurements are taken from.
//
// It is on the page because a speed number without its vantage point is not
// actionable. The same mirror can be fastest from one network and slowest from
// another, and a probe that goes through a proxy is measuring the proxy as much
// as the mirror.
type egressView struct {
	// Proxies lists the proxy variables set in this process's environment, with
	// any credentials removed.
	Proxies []string

	// Proxied reports whether any of them is set at all.
	Proxied bool
}

// probePage backs the speed test page.
type probePage struct {
	Repository string
	Reference  string

	// Unavailable is set when this build has no probe engine wired up, so the
	// page can explain itself instead of offering a button that cannot work.
	Unavailable bool

	Rows []probeRow

	// Servable is how many mirrors a batch run would cover, so the button can
	// say what it is about to do.
	Servable int

	Egress egressView
}

// handleProbe shows the newest measurement of every mirror.
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	page, err := s.loadProbePage(r)
	if err != nil {
		s.fail(w, r, "web: load probe results", err)
		return
	}

	s.renderProbe(w, r, page, http.StatusOK)
}

// renderProbe writes the speed test page.
func (s *Server) renderProbe(w http.ResponseWriter, r *http.Request, page *probePage, status int) {
	data := s.newPageData(r, stateFrom(r.Context()), "probe.title", "nav.probe")
	data.ProbePage = page

	s.render(w, r, "probe", status, data)
}

// loadProbePage assembles everything the speed test page shows.
func (s *Server) loadProbePage(r *http.Request) (*probePage, error) {
	ctx := r.Context()

	target, err := s.probeTarget(ctx)
	if err != nil {
		return nil, err
	}

	records, err := s.store.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := s.store.LatestProbes(ctx)
	if err != nil {
		return nil, err
	}
	history, err := s.store.Histories(ctx, sparklineSamples)
	if err != nil {
		return nil, err
	}

	tr := s.i18n.Translator(stateFrom(ctx).Locale)

	page := &probePage{
		Repository:  target.Repository,
		Reference:   target.Reference,
		Unavailable: s.probe == nil,
		Egress:      readEgress(),
		Rows:        make([]probeRow, 0, len(records)),
	}

	for _, rec := range records {
		row := newProbeRow(rec, latest[rec.ID], history[rec.ID], tr)
		if row.Servable {
			page.Servable++
		}
		page.Rows = append(page.Rows, row)
	}

	return page, nil
}

// newProbeRow renders one stored mirror for the speed test page.
func newProbeRow(rec store.SourceRecord, newest store.ProbeRecord,
	history []store.ProbeRecord, tr *i18n.Translator) probeRow {

	row := probeRow{
		SourceID: rec.ID,
		Name:     rec.Name,
		Enabled:  rec.Enabled,
		Servable: rec.Enabled && servesDockerHub(rec),
		Sparkline: newSparkline(throughputSeries(history),
			sparklineWidth, sparklineHeight),
	}

	// A zero timestamp is how "never probed" arrives: the store finds no row,
	// and the caller passes the zero value through.
	if newest.StartedAt.IsZero() {
		return row
	}

	row.Connect = newTimeLayer(probe.Status(newest.Connectivity), newest.ConnectMS, tr)
	row.Token = newTimeLayer(probe.Status(newest.TokenStatus), newest.TokenMS, tr)
	row.Manifest = newTimeLayer(probe.Status(newest.ManifestStatus), newest.ManifestMS, tr)
	row.Throughput = newRateLayer(
		probe.Status(newest.ThroughputStatus), newest.ThroughputBPS, tr)

	row.Digest = newest.ResolvedDigest
	row.ShortDigest = shortDigest(newest.ResolvedDigest)

	row.Verdict = &verdict{
		Status:    probe.Status(newest.Status),
		Label:     statusLabel(probe.Status(newest.Status), tr),
		Class:     statusClass(probe.Status(newest.Status)),
		BPSLabel:  humanRate(newest.ThroughputBPS),
		Detail:    newest.Detail,
		StartedAt: newest.StartedAt,
	}

	return row
}

// newTimeLayer renders a layer that measured a duration.
func newTimeLayer(status probe.Status, millis int64, tr *i18n.Translator) layerView {
	// Whole milliseconds only above one: a sub-millisecond round trip shown as
	// "0 ms" would read as instant, and the more useful statement is that the
	// clock could not resolve it.
	measured := millis > 0

	value := ""
	if measured {
		value = fmt.Sprintf("%d ms", millis)
	}

	return layerView{
		Label:    statusLabel(status, tr),
		Class:    statusClass(status),
		Value:    value,
		Measured: measured,
	}
}

// newRateLayer renders layer 4, whose number is a rate rather than a duration.
func newRateLayer(status probe.Status, bps int64, tr *i18n.Translator) layerView {
	value := humanRate(bps)

	return layerView{
		Label:    statusLabel(status, tr),
		Class:    statusClass(status),
		Value:    value,
		Measured: value != "",
	}
}

// newProbeRun summarises a stored measurement for the mirror list.
func newProbeRun(rec store.ProbeRecord, tr *i18n.Translator) probeRun {
	status := probe.Status(rec.Status)

	return probeRun{
		Label:    statusLabel(status, tr),
		Class:    statusClass(status),
		BPSLabel: humanRate(rec.ThroughputBPS),
	}
}

// statusLabel translates a verdict, falling back to the raw value.
//
// The fallback is for a status written by a newer version and read by an older
// one. Showing "rate_limited" is ugly; showing a blank pill is a lie.
func statusLabel(status probe.Status, tr *i18n.Translator) string {
	key, ok := statusKeys[status]
	if !ok {
		return string(status)
	}
	return tr.T(key)
}

func statusClass(status probe.Status) string {
	if class, ok := statusClasses[status]; ok {
		return class
	}
	return "pill-muted"
}

// probeTarget reads the image to measure.
//
// An absent setting falls back to the default rather than an error, so a fresh
// install has something to measure without being configured first.
func (s *Server) probeTarget(ctx context.Context) (probe.Target, error) {
	repository, err := s.store.SettingOrDefault(ctx, store.SettingProbeRepository, "")
	if err != nil {
		return probe.Target{}, err
	}
	reference, err := s.store.SettingOrDefault(ctx, store.SettingProbeReference, "")
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

	// Whether this target is usable is New's business: it validates. Repeating
	// the rules here would be a second implementation to keep in step with the
	// first, and the second one would win.
	return probe.Target{
		Repository: strings.TrimSpace(repository),
		Reference:  strings.TrimSpace(reference),
	}, nil
}

// handleProbeRun measures mirrors and returns the visitor to the results.
//
// Synchronous, and that is a deliberate limit of this version: a batch takes
// seconds, not minutes, and doing it in the background would need a queue, a
// progress channel and a way to describe a run that is still going. When that
// becomes worth building, this is the function that changes.
func (s *Server) handleProbeRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.probe == nil {
		redirect(w, r, "/probe?flash=probe.error.unavailable")
		return
	}

	target, err := s.probeTarget(ctx)
	if err != nil {
		s.fail(w, r, "web: read probe target", err)
		return
	}

	runner, err := s.probe(target)
	if err != nil {
		// A rejected target is a setting the user can fix, so it belongs on the
		// page rather than in a stack trace — but it is also worth a log line,
		// because it means a form accepted something the engine refuses.
		s.log.WarnContext(ctx, "web: probe target was rejected", "err", err)
		redirect(w, r, "/probe?flash=probe.error.target")
		return
	}

	// Detached from the request on purpose. A browser cancels in-flight
	// requests when the user navigates away, and discarding a batch of
	// measurements because someone clicked a link would be a strange way to
	// treat work already paid for in bandwidth.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeRunTimeout)
	defer cancel()

	if id := r.PostFormValue("id"); id != "" {
		s.runOneMirror(runCtx, w, r, runner, id)
		return
	}

	sources, err := catalog.EnabledForDockerHub(ctx, s.store)
	if err != nil {
		s.fail(w, r, "web: list mirrors to probe", err)
		return
	}

	s.recordProblems(ctx, runner.RunAll(runCtx, sources))

	redirect(w, r, "/probe?flash=probe.ran")
}

// runOneMirror measures a single mirror.
//
// Probing by hand ignores the enabled flag on purpose. The flag decides what a
// batch covers, and being unable to test a mirror you just switched off is how
// you end up switching it back on to find out whether it works.
func (s *Server) runOneMirror(ctx context.Context, w http.ResponseWriter, r *http.Request,
	runner Prober, id string) {

	rec, err := s.store.GetSource(ctx, id)
	if err != nil {
		s.redirectSourceError(w, r, err)
		return
	}

	if _, err := runner.RunOne(ctx, catalog.ToProbeSource(rec)); err != nil {
		s.log.ErrorContext(ctx, "web: probe result was not recorded", "mirror", id, "err", err)
	}

	redirect(w, r, "/probe?flash=probe.ran")
}

// recordProblems logs measurements that could not be saved.
//
// A failure to persist is not a failure to measure: the numbers are still
// rendered from what the run returned. It is logged rather than surfaced
// because the user cannot act on it, and logged at all because a history that
// silently stops growing is a bug nobody reports until the graph flatlines.
func (s *Server) recordProblems(ctx context.Context, summary probe.Summary) {
	if len(summary.RecordErrors) == 0 {
		return
	}

	s.log.ErrorContext(ctx, "web: some probe results were not recorded",
		"failed", len(summary.RecordErrors),
		"measured", len(summary.Results),
		"first", summary.RecordErrors[0],
	)
}

// proxyVars are the environment variables that route this process's outbound
// HTTP somewhere else. The probe's client follows them.
var proxyVars = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
	"ALL_PROXY", "all_proxy",
}

// readEgress reports the proxy configuration this process would actually use.
//
// Shown because it changes what the numbers mean: a measurement taken through a
// tunnel describes the tunnel, and someone comparing these figures against a
// direct pull from another machine deserves to know that before concluding the
// mirror is slow.
func readEgress() egressView {
	var found []string

	for _, name := range proxyVars {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			found = append(found, name+"="+redactProxy(value))
		}
	}

	return egressView{Proxies: found, Proxied: len(found) > 0}
}

// redactProxy removes credentials from a proxy URL before it is shown.
//
// Proxy variables routinely carry a username and a password, and this value
// lands in HTML that gets screenshotted into bug reports. Everything before the
// last "@" goes, which covers user:pass@host whether or not the value happens
// to parse as a URL.
func redactProxy(value string) string {
	if at := strings.LastIndex(value, "@"); at >= 0 {
		return "…@" + value[at+1:]
	}
	return value
}

// throughputSeries extracts sparkline samples, oldest first.
//
// The store returns history newest first and a time axis reads oldest first.
// Reversing here rather than in the drawing code keeps the geometry free of a
// direction flag that someone would eventually pass the wrong way.
//
// Failed runs stay in as zeroes rather than being dropped. Filtering them out
// would make an unreliable mirror look steady, and a dip to zero is something
// that genuinely happened.
func throughputSeries(history []store.ProbeRecord) []int64 {
	series := make([]int64, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		series = append(series, history[i].ThroughputBPS)
	}
	return series
}

// newSparkline lays out a throughput series as a polyline.
//
// X is the sample index rather than the timestamp. Runs happen irregularly and
// the gaps between them are unbounded, so spacing by wall-clock time would
// squash a week of measurements into a single pixel column.
func newSparkline(samples []int64, width, height int) *sparkline {
	if len(samples) < 2 {
		return nil
	}

	peak := samples[0]
	for _, v := range samples[1:] {
		if v > peak {
			peak = v
		}
	}

	step := float64(width) / float64(len(samples)-1)

	var points strings.Builder
	points.Grow(len(samples) * 12)

	for i, v := range samples {
		// A series of zeroes — a mirror that has answered nothing but failures
		// — has no shape to draw, so it becomes a flat line along the bottom
		// rather than a division by zero.
		y := float64(height)
		if peak > 0 {
			y = float64(height) - (float64(v)/float64(peak))*float64(height)
		}

		if i > 0 {
			points.WriteByte(' ')
		}
		fmt.Fprintf(&points, "%.1f,%.1f", float64(i)*step, y)
	}

	return &sparkline{
		Width:     width,
		Height:    height,
		Points:    points.String(),
		Peak:      peak,
		PeakLabel: humanRate(peak),
		Samples:   len(samples),
	}
}

// humanRate renders a throughput for a person to read, or "" when nothing was
// measured.
//
// Decimal units, because every figure a person compares throughput against is
// decimal: a "100 Mbit" link carries 10^6 bits per second, and a value quoted
// in MiB/s would be about five percent off against all of them.
func humanRate(bps int64) string {
	if bps <= 0 {
		return ""
	}

	const unit = 1000
	units := []string{"B/s", "kB/s", "MB/s", "GB/s"}

	value := float64(bps)
	i := 0
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}

	if i == 0 {
		return fmt.Sprintf("%.0f %s", value, units[i])
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

// shortDigest trims a digest for a table cell.
//
// A digest is seventy-one characters and mostly identical between rows. Twelve
// hex digits is enough to see whether two mirrors served the same manifest,
// which is the only question the column answers — and the ellipsis is there so
// nobody copies a prefix and believes they have the digest. The full value
// travels in the cell's title attribute.
func shortDigest(digest string) string {
	if digest == "" {
		return ""
	}

	hash := digest
	if _, after, found := strings.Cut(digest, ":"); found {
		hash = after
	}
	if len(hash) <= 12 {
		return digest
	}
	return hash[:12] + "…"
}

// servesDockerHub reports whether a stored row proxies Docker Hub.
//
// Case-insensitive because the value comes from a free-text column rather than
// from the validated enum, so "DockerHub" and "dockerhub" have to agree.
func servesDockerHub(rec store.SourceRecord) bool {
	for _, sc := range rec.Scope {
		if strings.EqualFold(strings.TrimSpace(sc), string(mirror.ScopeDockerHub)) {
			return true
		}
	}
	return false
}
