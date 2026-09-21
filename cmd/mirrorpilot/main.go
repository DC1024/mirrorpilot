// Command mirrorpilot is the MirrorPilot server.
//
// MirrorPilot is a self-hosted panel that probes container registry mirrors from
// the local network egress and relocates images through GitHub Actions.
//
// See the project README for the full scope and, more importantly, the
// non-goals: this is not a registry proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/catalog"
	"github.com/DC1024/mirrorpilot/internal/config"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
	"github.com/DC1024/mirrorpilot/internal/web"
)

// Build metadata, injected via -ldflags at build time.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

const (
	shutdownGrace = 10 * time.Second
	sweepInterval = time.Hour
)

func main() {
	// The signal context is created here rather than inside run, so run is a
	// plain function of its context: a test can start the server, drive it over
	// real HTTP, and then cancel to observe the shutdown path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "mirrorpilot: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	listenFlag := flag.String("listen", "",
		"address to listen on (default: $MIRRORPILOT_BIND, else "+config.DefaultListen+")")
	dataFlag := flag.String("data", "",
		"persistent data directory (default: $MIRRORPILOT_DATA, else "+config.DefaultDataDir+")")
	configFlag := flag.String("config", "",
		"config file (default: <data directory>/"+config.DefaultFileName+")")
	resetFlag := flag.Bool("reset", false,
		"remove the account, the vault and every stored credential, then exit")
	flag.Parse()

	// The data directory has to be resolved before the config file can be
	// looked up, so it comes from the flag and the environment first, and is
	// then imposed on the loaded configuration afterwards. A file that also
	// sets data_dir loses to an explicit flag — the flag is what told us where
	// to look for the file in the first place.
	dataDir := *dataFlag
	if dataDir == "" {
		dataDir = envOr("MIRRORPILOT_DATA", config.DefaultDataDir)
	}

	configPath := *configFlag
	if configPath == "" {
		configPath = config.DefaultPath(dataDir)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// Command-line overrides are applied after Load, so they need the same
	// validation the file went through. A port that is not a port should fail
	// here, not as a confusing listen error later.
	cfg.DataDir = dataDir
	if *listenFlag != "" {
		cfg.Listen = *listenFlag
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("mirrorpilot starting",
		"version", Version,
		"commit", Commit,
		"built", BuildDate,
		"go", runtime.Version(),
		"config", configPath,
	)

	// Fail fast if the data directory is unusable. Degrading quietly here would
	// surface much later as a panel that cannot remember anything.
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory %q: %w", cfg.DataDir, err)
	}
	if err := checkWritable(cfg.DataDir); err != nil {
		return fmt.Errorf("data directory %q is not writable: %w", cfg.DataDir, err)
	}

	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close database", "err", err)
		}
	}()

	// Handled here rather than after the catalogue is seeded, because a reset
	// is not a step on the way to serving: it is the whole point of this run.
	if *resetFlag {
		return resetPanel(ctx, db, cfg, logger)
	}

	// Seed the built-in catalogue before serving a single page, because the
	// sources page is empty without it and an install that shows no mirrors
	// looks broken rather than unconfigured.
	//
	// Sync is idempotent and keeps the user's enable/disable choices, so
	// running it on every start is also how a later release's added mirrors
	// reach an existing install without disturbing what was already decided.
	// A failure here is fatal: it means either the embedded catalogue is
	// unreadable or the database refused a plain insert, and neither is a
	// condition to serve through and hope someone notices.
	added, err := catalog.Sync(ctx, db)
	if err != nil {
		return fmt.Errorf("seed mirror catalogue: %w", err)
	}
	if added > 0 {
		logger.Info("added new built-in mirrors", "count", added)
	}

	manager := auth.New(db, auth.Config{})

	panel, err := newPanel(cfg, db, manager, logger)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           panel.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go sweepSessions(ctx, manager, logger)

	// Started before ListenAndServe so the schedule is armed for the whole life
	// of the process, and detached from any request: this is the one piece of
	// work the panel does with nobody watching, which is exactly why it does not
	// depend on the master key being unlocked. Measuring needs no credentials.
	factory := newRunnerFactory(db)
	go sweepProbes(ctx, cfg.Probe.Interval.Std(), newProbeSweep(db, factory, logger), logger)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen, "data", cfg.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	case <-ctx.Done():
	}

	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("stopped cleanly")
	return nil
}

// newPanel assembles the panel from its dependencies.
//
// Separate from run so tests exercise the real wiring rather than a hand-built
// approximation of it, which is the kind of test that passes while the binary
// is broken.
func newPanel(cfg config.Config, db *store.Store, manager *auth.Manager, logger *slog.Logger) (*web.Server, error) {
	bundle, err := i18n.New()
	if err != nil {
		return nil, err
	}

	return web.New(web.Options{
		Store:   db,
		Auth:    manager,
		I18n:    bundle,
		Probe:   runnerProbes(newRunnerFactory(db)),
		Version: Version,
		Logger:  logger,

		// The panel has no way to know it is behind a TLS terminator, so the
		// externally visible address is the only honest signal we have. Set
		// base_url to https://... when a proxy terminates TLS for you;
		// otherwise leave it alone and cookies stay usable over plain HTTP.
		Secure: strings.HasPrefix(strings.ToLower(cfg.BaseURL), "https://"),
	})
}

// runnerFactory builds the engine that measures mirrors for one image.
type runnerFactory func(target probe.Target) (*probe.Runner, error)

// newRunnerFactory returns a factory rather than one runner built at startup,
// because the image being measured is a setting: changing it has to change what
// the next run measures, and a form field that only takes effect after a
// restart is a form field nobody trusts to do what it says.
//
// The runner is bound to the store as its recorder, so a measurement is
// persisted by the same call that produced it and there is no path that
// measures a mirror without leaving a trace of it.
//
// Shared by the speed test page and the background sweep. They have to measure
// the same way: both write into one history table, and a second engine
// configured differently would leave that table holding two datasets that look
// like one.
func newRunnerFactory(db *store.Store) runnerFactory {
	return func(target probe.Target) (*probe.Runner, error) {
		engine, err := probe.New(probe.Options{Target: target})
		if err != nil {
			return nil, err
		}
		return probe.NewRunner(engine, db, probe.RunnerOptions{})
	}
}

// runnerProbes adapts a runnerFactory to the narrower interface the panel asks
// for, which exists so the web layer can be tested without a real registry.
func runnerProbes(factory runnerFactory) web.ProbeFactory {
	return func(target probe.Target) (web.Prober, error) {
		runner, err := factory(target)
		if err != nil {
			return nil, err
		}
		return runner, nil
	}
}

// probeSweepTimeout bounds one automatic batch.
//
// Generous for the same reason the speed test page's budget is: a dozen mirrors
// at fifteen seconds each with a handful in flight is minutes of work. Finite so
// that a sweep which somehow never finishes cannot silently stop every one that
// would follow it.
const probeSweepTimeout = 5 * time.Minute

// sweepBatch is one automatic pass, reporting how many mirrors it covered.
//
// A function rather than the concrete call chain so the schedule can be tested
// without a network: what is worth testing here is the timing and the survival,
// not the measuring, which the probe package already tests on its own.
type sweepBatch func(ctx context.Context) (int, error)

// sweepProbes measures every enabled mirror on a fixed interval.
//
// The reason this exists: the config page ranks mirrors by their most recent
// measurement, so a ranking built from numbers taken once, months ago, is a
// ranking of the past. The sweep is what keeps that page honest without anyone
// remembering to press a button.
//
// A zero interval turns it off. The measurements are taken from wherever this
// process runs, and someone on a metered or shared link should not have to read
// the source to find the switch.
//
// The first pass happens one interval after startup rather than at startup. A
// container that restarts in a loop would otherwise probe every mirror on every
// restart, which is precisely the hammering of community mirrors this project
// avoids everywhere else.
func sweepProbes(ctx context.Context, interval time.Duration, batch sweepBatch, logger *slog.Logger) {
	if interval <= 0 {
		logger.Info("automatic probing is off", "enable", "probe.interval")
		return
	}

	logger.Info("automatic probing enabled", "interval", interval.String())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// One bad sweep does not end the schedule. A mirror list that is
			// briefly unreachable would otherwise turn the feature off for the
			// rest of the process's life, and nobody notices that until the
			// graph is flat.
			swept, err := batch(ctx)
			switch {
			case ctx.Err() != nil:
				// Shutting down. The batch is expected to come back with a
				// cancellation error and there is nothing to report about it.
				return
			case err != nil:
				logger.ErrorContext(ctx, "automatic probe sweep failed", "err", err)
			case swept == 0:
				logger.DebugContext(ctx, "automatic probe sweep had nothing to measure")
			default:
				logger.InfoContext(ctx, "automatic probe sweep finished", "mirrors", swept)
			}
		}
	}
}

// newProbeSweep is the sweep's actual work: measure everything that is enabled.
func newProbeSweep(db *store.Store, factory runnerFactory, logger *slog.Logger) sweepBatch {
	return func(ctx context.Context) (int, error) {
		// Re-read and rebuild on every pass: the image being measured is a
		// setting someone can change between two sweeps, and reusing one
		// engine would quietly pin the history to whatever was configured when
		// the process started.
		target, err := catalog.ProbeTarget(ctx, db)
		if err != nil {
			return 0, err
		}

		runner, err := factory(target)
		if err != nil {
			return 0, err
		}

		sources, err := catalog.EnabledForDockerHub(ctx, db)
		if err != nil {
			return 0, err
		}
		if len(sources) == 0 {
			// Nothing enabled is a configuration, not a failure. Answered
			// without a request: there is no reason to reach the network to
			// discover there is no work.
			return 0, nil
		}

		runCtx, cancel := context.WithTimeout(ctx, probeSweepTimeout)
		defer cancel()

		summary := runner.RunAll(runCtx, sources)

		// A measurement can be taken and then fail to persist. The numbers are
		// already gone by the time this returns, so this is the only place the
		// loss is visible at all.
		for _, err := range summary.RecordErrors {
			logger.ErrorContext(ctx, "automatic probe result was not recorded", "err", err)
		}

		return len(summary.Results), nil
	}
}

// resetPanel clears the account and everything encrypted under it.
//
// The one way out of a forgotten master password. It is a flag and not a route
// because a panel that can be reset over HTTP is a panel anyone who can reach
// it can take over; requiring shell access is the whole of its security model,
// and that is the right trade for a single-user tool.
//
// The mirror catalogue, the preferences and the measurement history are left
// where they are. Nothing in them is secret, and losing a month of measurements
// to fix a login would be a poor trade — the panel comes back asking to be set
// up, with everything else still there.
func resetPanel(ctx context.Context, db *store.Store, cfg config.Config, logger *slog.Logger) error {
	counts, err := db.ResetCounts(ctx)
	if err != nil {
		return err
	}

	if err := db.Reset(ctx); err != nil {
		return err
	}

	logger.Warn("panel reset: the panel is unconfigured again",
		"data", cfg.DataDir,
		"accounts_removed", counts.Users,
		"credentials_removed", counts.Credentials,
		"sessions_ended", counts.Sessions,
	)
	logger.Warn("every stored credential is gone for good: the key that encrypted them was derived from the password that was just discarded")
	logger.Info("start the panel again to set a new password", "listen", cfg.Listen)

	return nil
}

// sweepSessions prunes expired session rows.
//
// Sessions already expire on use, but a browser that simply never comes back
// leaves its row behind. This bounds that. One user means the table can only
// ever hold a handful of rows — the point is not to leave garbage accumulating
// out of habit.
func sweepSessions(ctx context.Context, manager *auth.Manager, logger *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := manager.SweepSessions(ctx)
			if err != nil {
				// A cancelled context is the expected way this ends during
				// shutdown, not something to complain about.
				if ctx.Err() == nil {
					logger.Error("sweep sessions", "err", err)
				}
				continue
			}
			if n > 0 {
				logger.Info("pruned expired sessions", "count", n)
			}
		}
	}
}

// newLogger builds the process logger at the configured level.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level

	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// checkWritable verifies we can actually create files in dir. The permission
// bits on a mounted volume can look right while the volume is still read-only.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return err
	}

	name := f.Name()
	_ = f.Close()

	return os.Remove(name)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
