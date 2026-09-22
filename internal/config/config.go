// Package config loads MirrorPilot's runtime configuration.
//
// Only non-sensitive settings live here. Everything that needs protecting — the
// admin password hash, the master-password salt, encrypted third-party
// credentials — belongs in the SQLite database under the data directory. Putting
// secrets in a file people routinely paste into bug reports is a mistake we are
// not going to make.
//
// Precedence, lowest to highest: built-in defaults, then the YAML file, then
// environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultListen binds all interfaces so the panel is reachable from other
	// machines on a LAN. It is NOT intended to be exposed to the internet.
	DefaultListen = "0.0.0.0:8080"

	// DefaultDataDir matches the volume declared in the Dockerfile.
	DefaultDataDir = "/data"

	// DefaultFileName is looked up inside the data directory.
	DefaultFileName = "config.yaml"

	// Bounds on probe concurrency. See ProbeConfig.Concurrency.
	MinProbeConcurrency = 1
	MaxProbeConcurrency = 16

	// MinimumInterval guards community mirrors from being hammered by an
	// over-eager schedule. It applies to any non-zero interval; zero means the
	// sweep is off, which is never "too frequent".
	MinimumInterval = time.Minute
	maximumTimeout  = 5 * time.Minute

	// defaultBlobTimeout matches the engine's own default, and a test holds the
	// two together. Written out rather than imported because this package
	// depends on nothing but the standard library and the YAML parser: it is
	// the one place a reader can see every knob at once, and that is worth more
	// than saving a literal.
	defaultBlobTimeout = 45 * time.Second
)

// Duration is a time.Duration that unmarshals from a Go duration string such as
// "15s" or "30m". The standard library type would only accept an integer count
// of nanoseconds, which is not something a human should have to write.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("config: duration must be a string like \"15s\": %w", err)
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", raw, err)
	}

	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.Std().String(), nil }

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ProbeConfig controls how mirror probing behaves.
type ProbeConfig struct {
	// Concurrency caps how many mirrors are probed simultaneously.
	//
	// Deliberately low by default. These are community-run mirrors, and
	// hammering them concurrently is precisely the behaviour this project
	// discourages in its own users.
	Concurrency int `yaml:"concurrency"`

	// Timeout is the per-request budget. Probing a mirror that is merely slow
	// should not hang the whole run.
	//
	// This covers the three layers that are round trips — connectivity, the
	// token exchange, and reading the manifest. It deliberately does not cover
	// the transfer: see BlobTimeout.
	Timeout Duration `yaml:"timeout"`

	// BlobTimeout is the budget for layer 4 alone, where a probe stops being a
	// round trip and becomes a transfer.
	//
	// Separate from Timeout because they are different questions. Fifteen
	// seconds is generous for a request and far too little for a download, and
	// one deadline covering both means the clock — not the mirror — decides
	// whether the transfer succeeded. That is how a slow mirror gets recorded
	// as a broken one.
	//
	// It is a limit on patience, not a target: a healthy mirror finishes in a
	// couple of seconds and reports the rate it reached. Raise it on a link
	// where the mirrors really are slower than this; the cost of raising it is
	// paid only by mirrors that are already failing.
	BlobTimeout Duration `yaml:"blob_timeout"`

	// Interval is the spacing between automatic probe runs. Users trigger most
	// probes by hand; this only bounds a background sweep.
	//
	// Zero disables the sweep entirely, which is a legitimate choice: the
	// measurements are made from wherever this process runs, and someone who
	// keeps the panel on a metered or shared link should not have to discover
	// the off switch by reading the source. Any non-zero value is floored at
	// MinimumInterval — an interval of a few seconds is not a preference, it is
	// a way to get this project's users rate-limited by community mirrors.
	Interval Duration `yaml:"interval"`
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Listen   string `yaml:"listen"`
	DataDir  string `yaml:"data_dir"`
	LogLevel string `yaml:"log_level"`

	// BaseURL is the externally visible address, used when generating commands
	// for the user to paste elsewhere. Left empty, we infer it from the request.
	BaseURL string `yaml:"base_url"`

	Probe ProbeConfig `yaml:"probe"`
}

// Default returns a configuration that is valid with no file and no environment
// variables present.
func Default() Config {
	return Config{
		Listen:   DefaultListen,
		DataDir:  DefaultDataDir,
		LogLevel: "info",
		Probe: ProbeConfig{
			Concurrency: 5,
			Timeout:     Duration(15 * time.Second),
			BlobTimeout: Duration(defaultBlobTimeout),
			Interval:    Duration(30 * time.Minute),
		},
	}
}

// Load resolves configuration from the YAML file at path, then environment
// overrides, then validates the result.
//
// A missing file is not an error — every setting has a working default, and a
// first run has no file yet. An unreadable or malformed file IS an error: it
// almost always means the user meant something different from what we would
// silently fall back to.
//
// If path is empty, the file is looked for inside the default data directory.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		path = filepath.Join(cfg.DataDir, DefaultFileName)
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// Expected on first run.
	default:
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Validate rejects values that would fail later in a more confusing place.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen address must not be empty")
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen address %q is not host:port: %w", c.Listen, err)
	} else if port == "" {
		return fmt.Errorf("listen address %q has no port", c.Listen)
	}

	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("data directory must not be empty")
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log level %q is not one of debug, info, warn, error", c.LogLevel)
	}

	if c.Probe.Concurrency < MinProbeConcurrency || c.Probe.Concurrency > MaxProbeConcurrency {
		return fmt.Errorf("probe concurrency %d is outside %d..%d",
			c.Probe.Concurrency, MinProbeConcurrency, MaxProbeConcurrency)
	}

	timeout := c.Probe.Timeout.Std()
	if timeout <= 0 || timeout > maximumTimeout {
		return fmt.Errorf("probe timeout %s is outside 0..%s", timeout, maximumTimeout)
	}

	blobTimeout := c.Probe.BlobTimeout.Std()
	if blobTimeout <= 0 || blobTimeout > maximumTimeout {
		return fmt.Errorf("probe blob_timeout %s is outside 0..%s", blobTimeout, maximumTimeout)
	}

	// A negative interval is a mistake, not a way to ask for less than nothing.
	// Zero is the documented way to turn the sweep off.
	if interval := c.Probe.Interval.Std(); interval < 0 {
		return fmt.Errorf("probe interval %s cannot be negative; use 0 to disable the sweep", interval)
	} else if interval > 0 && interval < MinimumInterval {
		return fmt.Errorf("probe interval %s is below the %s minimum; these are community mirrors",
			interval, MinimumInterval)
	}

	return nil
}

// DefaultPath returns the config file location implied by a data directory.
func DefaultPath(dataDir string) string {
	return filepath.Join(dataDir, DefaultFileName)
}

// applyEnv layers MIRRORPILOT_* environment variables over whatever the file
// said. Only the settings that are genuinely useful to vary per-deployment are
// exposed this way.
func applyEnv(c *Config) {
	if v := os.Getenv("MIRRORPILOT_BIND"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("MIRRORPILOT_DATA"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("MIRRORPILOT_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("MIRRORPILOT_BASE_URL"); v != "" {
		c.BaseURL = v
	}
}
