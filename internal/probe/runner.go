package probe

import (
	"context"
	"fmt"
	"sync"

	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// Recorder is the persistence a Runner needs, and nothing more.
//
// Naming the two methods rather than taking *store.Store keeps the runner
// testable without a database, and keeps the dependency honest: if the runner
// ever wants to reach for another table, it has to say so here.
type Recorder interface {
	InsertProbe(ctx context.Context, rec store.ProbeRecord) error
	PruneProbes(ctx context.Context, keepPerSource int) (int, error)
}

// RunnerOptions tunes a Runner. Zero values take the defaults.
type RunnerOptions struct {
	// Concurrency bounds how many mirrors are probed at once.
	//
	// Probing is almost entirely waiting on the network, so some overlap is
	// what turns a dozen mirrors from a twelve-timeout page into a fast one.
	// It stays small on purpose: these are mirrors someone else pays for, and
	// a burst of parallel pulls from one client is exactly the behaviour that
	// earns an IP a rate limit — which would then be recorded as if it were a
	// property of the mirror.
	Concurrency int

	// KeepPerSource is how many history rows per mirror survive a run.
	KeepPerSource int
}

const (
	// DefaultConcurrency is deliberately modest for the reason above.
	DefaultConcurrency = 4

	// DefaultKeepPerSource keeps enough rows to draw a trend line and still
	// leaves the database small enough to back up by copying one file.
	DefaultKeepPerSource = 200
)

// Runner probes mirrors and records what it found.
type Runner struct {
	prober      *Prober
	rec         Recorder
	concurrency int
	keep        int
}

// NewRunner wires a Prober to a Recorder.
func NewRunner(prober *Prober, rec Recorder, opts RunnerOptions) (*Runner, error) {
	if prober == nil {
		return nil, fmt.Errorf("probe: runner needs a prober")
	}
	if rec == nil {
		return nil, fmt.Errorf("probe: runner needs a recorder")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.KeepPerSource <= 0 {
		opts.KeepPerSource = DefaultKeepPerSource
	}

	return &Runner{
		prober:      prober,
		rec:         rec,
		concurrency: opts.Concurrency,
		keep:        opts.KeepPerSource,
	}, nil
}

// Summary is the outcome of a batch run.
type Summary struct {
	// Results is one entry per input mirror, in the order they were given.
	// A mirror that failed is still present, carrying its own verdict — an
	// absent row and a broken mirror are different things and must not be
	// confused by whoever reads this.
	Results []Result

	// RecordErrors lists failures to persist measurements. The measurements
	// themselves are still returned; only the history is incomplete. This is
	// reported rather than returned as a hard error because a dashboard that
	// shows live numbers while failing to trend them is more useful than one
	// that shows nothing.
	RecordErrors []error
}

// RunAll probes every mirror, overlapping the network waits, and records the
// results.
//
// Results come back in input order regardless of which mirror finished first,
// so a table does not reshuffle between runs.
func (r *Runner) RunAll(ctx context.Context, sources []mirror.Source) Summary {
	results := make([]Result, len(sources))
	if len(sources) == 0 {
		return Summary{Results: results}
	}

	var (
		mu     sync.Mutex
		recErr []error
		wg     sync.WaitGroup
	)

	queue := make(chan int)
	workers := min(r.concurrency, len(sources))

	record := func(err error) {
		mu.Lock()
		recErr = append(recErr, err)
		mu.Unlock()
	}

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				src := sources[i]

				res := r.prober.Probe(ctx, src)
				results[i] = res

				if err := r.rec.InsertProbe(ctx, recordFrom(src.ID, res)); err != nil {
					record(err)
				}
			}
		}()
	}

	for i := range sources {
		queue <- i
	}
	close(queue)
	wg.Wait()

	// Prune once per batch, not once per mirror: it is one table-wide delete,
	// and running it inside the workers would serialise them on the single
	// database connection for no benefit.
	if _, err := r.rec.PruneProbes(ctx, r.keep); err != nil {
		record(err)
	}

	return Summary{Results: results, RecordErrors: recErr}
}

// RunOne probes a single mirror and records the result.
//
// Unlike Probe this does return an error, because here the caller asked for
// exactly one thing and can be told that the record did not stick.
func (r *Runner) RunOne(ctx context.Context, src mirror.Source) (Result, error) {
	res := r.prober.Probe(ctx, src)

	if err := r.rec.InsertProbe(ctx, recordFrom(src.ID, res)); err != nil {
		return res, err
	}
	if _, err := r.rec.PruneProbes(ctx, r.keep); err != nil {
		return res, err
	}
	return res, nil
}

// Target returns the image this Runner measures.
func (r *Runner) Target() Target { return r.prober.Target() }

// recordFrom flattens a Result into the row the store keeps.
//
// Every layer's own verdict is carried over, not just the overall one. The
// overall verdict says whether to trust a mirror; the per-layer ones say why,
// and "why" is what makes a page worth reading.
//
// Throughput is stored as bytes per second rather than as bytes and a
// duration, because a stored pair invites a reader to divide them and get
// bytes-per-millisecond, which is wrong by three orders of magnitude and looks
// plausible.
func recordFrom(sourceID string, res Result) store.ProbeRecord {
	return store.ProbeRecord{
		SourceID:         sourceID,
		StartedAt:        res.StartedAt,
		Connectivity:     string(res.Connect.Status),
		TokenStatus:      string(res.Token.Status),
		ManifestStatus:   string(res.Manifest.Status),
		ThroughputStatus: string(res.Throughput.Status),
		ConnectMS:        res.Connect.Millis(),
		TokenMS:          res.Token.Millis(),
		ManifestMS:       res.Manifest.Millis(),
		ThroughputBPS:    res.BPS(),
		ResolvedDigest:   res.ResolvedDigest,
		Status:           string(res.Status),
		Detail:           res.Detail,
	}
}
