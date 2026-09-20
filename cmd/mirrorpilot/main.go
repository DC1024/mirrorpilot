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
	"github.com/DC1024/mirrorpilot/internal/config"
	"github.com/DC1024/mirrorpilot/internal/i18n"
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
		Version: Version,
		Logger:  logger,

		// The panel has no way to know it is behind a TLS terminator, so the
		// externally visible address is the only honest signal we have. Set
		// base_url to https://... when a proxy terminates TLS for you;
		// otherwise leave it alone and cookies stay usable over plain HTTP.
		Secure: strings.HasPrefix(strings.ToLower(cfg.BaseURL), "https://"),
	})
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
