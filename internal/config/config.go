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
	// over-eager schedule.
	MinimumInterval = time.Minute
	maximumTimeout  = 5 * time.Minute
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
	Timeout Duration `yaml:"timeout"`

	// Interval is the spacing between automatic probe runs. Users trigger most
	// probes by hand; this only bounds a background sweep.
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

	if interval := c.Probe.Interval.Std(); interval < MinimumInterval {
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
