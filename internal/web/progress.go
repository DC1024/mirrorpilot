package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/probe"
)

// layerLabels names each layer for a finished trace line.
//
// The same keys the results table's columns use: the live view and the table
// describe the same four steps, and two sets of words for one measurement is
// how a reader ends up wondering whether "Token" and "Token 认证" are the same
// thing. Written out rather than built from the layer name, so the check that
// proves every shipped string is shown can see them.
var layerLabels = map[probe.LayerName]string{
	probe.LayerConnect:    "probe.col.connect",
	probe.LayerToken:      "probe.col.token",
	probe.LayerManifest:   "probe.col.manifest",
	probe.LayerThroughput: "probe.col.throughput",
}

// progress is the live picture of a speed test, as the page's script reads it.
//
// Served as JSON and polled while a batch is in flight, because a form submit
// takes the page away: the browser's own progress bar says nothing, and from
// the outside "measuring the throughput layer" and "hung" look exactly alike.
//
// Numbers arrive already formatted. Handing the script raw values and a unit
// would mean a second implementation of every formatting rule in the panel,
// in a language where a mistake is invisible until someone reads the page.
type progress struct {
	// Active is false when nothing is being measured, which is what the page
	// shows as "waiting" rather than as a stalled layer.
	Active bool `json:"active"`

	ElapsedMS int64 `json:"elapsed_ms"`
	Total     int   `json:"total"`
	Done      int   `json:"done"`

	Steps []progressStep `json:"steps"`
}

// progressStep is one mirror in a run.
type progressStep struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Layer is the layer being measured right now, empty when the mirror is
	// between layers or finished. The script turns it into a label, because
	// the label is in the template along with every other translation.
	Layer string `json:"layer"`

	// LayerMS is how long that layer has been running so far. The script adds
	// its own elapsed time to this between polls, so the number keeps moving
	// without the panel having to ask again every second.
	LayerMS int64 `json:"layer_ms"`

	Trace []progressTrace `json:"trace"`

	// Done reports whether this mirror's probe is over.
	Done bool `json:"done"`
}

// progressTrace is one finished layer, worded.
type progressTrace struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`

	// Class is the badge class for the verdict, so the script can colour a
	// chip without knowing the status vocabulary.
	Class string `json:"class,omitempty"`
}

// handleProbeProgress reports what the current run is doing.
//
// A GET, and it changes nothing, so a poll is free to arrive whenever the page
// feels like asking.
func (s *Server) handleProbeProgress(w http.ResponseWriter, r *http.Request) {
	tr := s.i18n.Translator(stateFrom(r.Context()).Locale)
	view := progressView(s.activity.Snapshot(), tr)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Never cached: the whole point of this response is that it is already
	// out of date by the time it is read.
	w.Header().Set("Cache-Control", "no-store")

	if err := json.NewEncoder(w).Encode(view); err != nil {
		// Part of the response is already on the wire, so a status code is no
		// longer available; the log is the only place left to say so.
		s.log.ErrorContext(r.Context(), "web: write probe progress", "err", err)
	}
}

// progressView renders a snapshot for the page.
func progressView(snap probe.Snapshot, tr *i18n.Translator) progress {
	out := progress{
		Active:    snap.Active,
		ElapsedMS: snap.Elapsed.Milliseconds(),
		Total:     snap.Total,
		Done:      snap.Done,
		Steps:     make([]progressStep, 0, len(snap.Steps)),
	}

	for _, step := range snap.Steps {
		view := progressStep{
			ID:      step.SourceID,
			Name:    step.Name,
			Layer:   string(step.Layer),
			LayerMS: step.LayerElapsed.Milliseconds(),
			Done:    step.Done,
			Trace:   make([]progressTrace, 0, len(step.Trace)),
		}

		for _, trace := range step.Trace {
			view.Trace = append(view.Trace, progressTrace{
				Label:  layerLabel(trace.Layer, tr),
				Value:  traceValue(trace, tr),
				Detail: trace.Detail,
				Class:  statusClass(trace.Status),
			})
		}

		out.Steps = append(out.Steps, view)
	}

	return out
}

// layerLabel translates a layer's name.
func layerLabel(layer probe.LayerName, tr *i18n.Translator) string {
	key, ok := layerLabels[layer]
	if !ok {
		// A layer from a newer engine than this page knows about. Showing the
		// raw name beats showing an empty cell.
		return string(layer)
	}
	return tr.T(key)
}

// traceValue renders what a finished layer measured.
//
// A duration only when the layer produced one. A failed layer's verdict is the
// more useful of the two: "0 ms" would claim a measurement nobody made, and
// the verdict is the thing that explains why there is none.
func traceValue(trace probe.Trace, tr *i18n.Translator) string {
	if trace.Status == probe.StatusOK && trace.Duration > 0 {
		return humanDuration(trace.Duration)
	}
	return statusLabel(trace.Status, tr)
}

// humanDuration renders a layer's duration for the live view.
//
// Milliseconds below a second, seconds above it. The first three layers
// normally finish in tens or hundreds of milliseconds, while the fourth is a
// transfer that can legitimately take a minute — and "45000 ms" is hard to
// read at exactly the moment somebody is watching the number climb.
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}
