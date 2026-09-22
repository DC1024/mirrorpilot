package probe

import "github.com/DC1024/mirrorpilot/internal/mirror"

// LayerName names one of the four measurement steps.
//
// A distinct type rather than a bare string so a layer cannot be confused with
// a status, and so the four values live in one place: the page, the progress
// events and the stored row all have to agree on what "throughput" is called.
type LayerName string

const (
	LayerConnect    LayerName = "connect"
	LayerToken      LayerName = "token"
	LayerManifest   LayerName = "manifest"
	LayerThroughput LayerName = "throughput"
)

// Phase says what happened to a layer.
type Phase string

const (
	// PhaseStarted means the layer's request is about to go out.
	PhaseStarted Phase = "started"

	// PhaseFinished carries the layer's outcome.
	PhaseFinished Phase = "finished"

	// PhaseDone means the whole probe of this mirror is over, whether or not
	// every layer ran. A mirror that fails connectivity never reaches the
	// later layers, and a watcher that only hears about layers would keep it
	// on screen as in-flight forever.
	PhaseDone Phase = "done"
)

// Event is one step of a probe in progress.
type Event struct {
	// Source is the mirror being measured. The whole value is carried rather
	// than just its id because a page showing progress needs something to
	// call the mirror by, and looking the name up again would be a second
	// source of truth for it.
	Source mirror.Source

	// Layer is the step this event is about, empty when Phase is PhaseDone.
	Layer LayerName

	Phase Phase

	// Result is the finished layer's outcome. Only meaningful for
	// PhaseFinished.
	Result Layer
}

// Observer is told how probes are going, while they are going.
//
// Calls arrive from several goroutines at once — a batch probes several
// mirrors in parallel — so an implementation must be safe for concurrent use.
// It is also called on the probe's own goroutine, so an implementation that
// blocks delays the measurement it is describing.
type Observer interface {
	Observe(Event)
}
