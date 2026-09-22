package probe

import (
	"strings"
	"sync"
	"time"

	"github.com/DC1024/mirrorpilot/internal/mirror"
)

// staleAfter is how long a run may go on claiming to be active.
//
// A batch has its own three-minute ceiling, so a run older than this is a
// record whose owner is gone — a handler that panicked, a process that was
// signalled mid-run. Reporting it as active would leave a page describing work
// that is not happening, which is worse than reporting nothing.
const staleAfter = 5 * time.Minute

// Activity is the live view of the probes running in one process.
//
// It exists so the panel can say what it is measuring instead of showing a
// clock and nothing else. A batch of a dozen mirrors against a mirror that
// stalls runs for minutes, and a count-up that never changes is
// indistinguishable from a panel that has hung.
//
// It keeps no history: a run is registered when it starts and dropped when it
// ends, and the numbers that matter are written to the store by the Runner.
//
// A nil *Activity is usable and does nothing. That is not defensive clutter:
// the observer is optional wiring, and a nil pointer stored in an interface is
// not nil, so the alternative is a panic in the one configuration where
// nobody is watching.
type Activity struct {
	mu   sync.Mutex
	next int
	run  *liveRun
}

// liveRun is one batch, as it happens.
type liveRun struct {
	id      int
	started time.Time
	total   int
	done    int

	// order is the steps in the order their mirrors started, which is the
	// order a reader should see them in: a table whose rows reshuffle as the
	// mirrors finish is unreadable.
	order []string
	steps map[string]*liveStep
}

// liveStep is one mirror's progress through the four layers.
type liveStep struct {
	id    string
	name  string
	layer LayerName
	since time.Time
	trace []Trace
	done  bool
}

// Trace is one finished layer, as a live view needs it.
type Trace struct {
	Layer  LayerName
	Status Status

	// Duration is how long the layer took, at full resolution: the panel
	// rounds it, and rounding here would lose the sub-millisecond difference
	// between a fast mirror and a fabricated zero.
	Duration time.Duration

	// Detail is the layer's own explanation, empty on success.
	Detail string
}

// Snapshot is the state of the current run.
type Snapshot struct {
	// Active is false when no run is registered, which is what the page shows
	// as "nothing is being measured".
	Active bool

	Elapsed time.Duration
	Total   int
	Done    int

	Steps []StepSnapshot
}

// StepSnapshot is one mirror in a run.
type StepSnapshot struct {
	SourceID string
	Name     string

	// Layer is the layer being measured right now, empty when the mirror is
	// between layers or finished.
	Layer LayerName

	// LayerElapsed is how long the current layer has been running. Zero when
	// no layer is running.
	LayerElapsed time.Duration

	// Trace is the layers that have finished, in the order they ran.
	Trace []Trace

	// Done reports whether this mirror's probe is over. Its trace is then
	// complete, and the step stays in the list so a reader can see what just
	// finished rather than watching rows vanish.
	Done bool
}

// Begin registers a run covering total mirrors and returns its identifier.
func (a *Activity) Begin(total int) int {
	if a == nil {
		return 0
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.next++
	a.run = &liveRun{
		id:      a.next,
		started: time.Now(),
		total:   total,
		steps:   make(map[string]*liveStep, total),
	}
	return a.next
}

// End retires a run.
//
// The identifier is checked because a second batch — another tab, a second
// click — supersedes the first, and the first finishing must not wipe the view
// of the one still running.
func (a *Activity) End(id int) {
	if a == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.run != nil && a.run.id == id {
		a.run = nil
	}
}

// Observe records one step of a probe.
//
// Events for a mirror that is not part of the current run are dropped. The
// background sweep measures mirrors too, and a page should not fill up with
// progress for work nobody asked for from it.
func (a *Activity) Observe(event Event) {
	if a == nil || event.Source.ID == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.run == nil {
		return
	}

	step := a.run.stepFor(event.Source)

	switch event.Phase {
	case PhaseStarted:
		step.layer = event.Layer
		step.since = time.Now()

	case PhaseFinished:
		step.layer = ""
		step.trace = append(step.trace, Trace{
			Layer:    event.Layer,
			Status:   event.Result.Status,
			Duration: event.Result.Duration,
			Detail:   event.Result.Detail,
		})

	case PhaseDone:
		step.layer = ""
		if !step.done {
			step.done = true
			a.run.done++
		}
	}
}

// stepFor returns a mirror's step, starting one the first time it is seen.
func (r *liveRun) stepFor(src mirror.Source) *liveStep {
	if step, ok := r.steps[src.ID]; ok {
		return step
	}

	// A mirror with no name is still a mirror; the identifier is a worse label
	// than a name and a better one than a blank row.
	name := strings.TrimSpace(src.Name)
	if name == "" {
		name = src.ID
	}

	step := &liveStep{id: src.ID, name: name}
	r.steps[src.ID] = step
	r.order = append(r.order, src.ID)

	return step
}

// Snapshot describes the current run as of now.
func (a *Activity) Snapshot() Snapshot {
	if a == nil {
		return Snapshot{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	run := a.run
	if run == nil {
		return Snapshot{}
	}

	now := time.Now()
	elapsed := now.Sub(run.started)
	if elapsed > staleAfter {
		return Snapshot{}
	}

	snap := Snapshot{
		Active:  true,
		Elapsed: elapsed,
		Total:   run.total,
		// Clamped because two overlapping batches can each report a finished
		// mirror into the same counter, and "finished 5 of 4" is a number no
		// reader should be shown.
		Done:  min(run.done, run.total),
		Steps: make([]StepSnapshot, 0, len(run.order)),
	}

	for _, id := range run.order {
		step := run.steps[id]

		view := StepSnapshot{
			SourceID: step.id,
			Name:     step.name,
			Layer:    step.layer,
			Trace:    step.trace,
			Done:     step.done,
		}
		if step.layer != "" {
			view.LayerElapsed = now.Sub(step.since)
		}

		snap.Steps = append(snap.Steps, view)
	}

	return snap
}
