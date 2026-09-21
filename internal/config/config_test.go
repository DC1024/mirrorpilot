package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolateEnv clears the MIRRORPILOT_* variables so a developer's own shell
// environment cannot change the outcome of a test.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MIRRORPILOT_BIND",
		"MIRRORPILOT_DATA",
		"MIRRORPILOT_LOG_LEVEL",
		"MIRRORPILOT_BASE_URL",
	} {
		t.Setenv(key, "")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}

func TestLoadWithoutFileUsesDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load with no file: %v", err)
	}

	want := Default()
	if cfg.Listen != want.Listen {
		t.Errorf("Listen = %q, want %q", cfg.Listen, want.Listen)
	}
	if cfg.Probe.Concurrency != want.Probe.Concurrency {
		t.Errorf("Concurrency = %d, want %d", cfg.Probe.Concurrency, want.Probe.Concurrency)
	}
}

func TestLoadReadsFile(t *testing.T) {
	isolateEnv(t)

	path := writeConfig(t, `
listen: "127.0.0.1:9000"
data_dir: "/srv/mirrorpilot"
log_level: "debug"
base_url: "https://mirrorpilot.lan"
probe:
  concurrency: 3
  timeout: 8s
  interval: 45m
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.DataDir != "/srv/mirrorpilot" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.BaseURL != "https://mirrorpilot.lan" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Probe.Concurrency != 3 {
		t.Errorf("Concurrency = %d, want 3", cfg.Probe.Concurrency)
	}
	if got := cfg.Probe.Timeout.Std(); got != 8*time.Second {
		t.Errorf("Timeout = %s, want 8s", got)
	}
	if got := cfg.Probe.Interval.Std(); got != 45*time.Minute {
		t.Errorf("Interval = %s, want 45m", got)
	}
}

// Settings absent from the file must keep their defaults rather than zeroing.
func TestLoadPreservesDefaultsForAbsentKeys(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, "log_level: \"warn\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn", cfg.LogLevel)
	}
	if cfg.Probe.Concurrency != Default().Probe.Concurrency {
		t.Errorf("Concurrency = %d, want the default %d",
			cfg.Probe.Concurrency, Default().Probe.Concurrency)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	isolateEnv(t)

	path := writeConfig(t, "listen: \"127.0.0.1:9000\"\nlog_level: \"warn\"\n")
	t.Setenv("MIRRORPILOT_BIND", "0.0.0.0:7000")
	t.Setenv("MIRRORPILOT_LOG_LEVEL", "debug")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen != "0.0.0.0:7000" {
		t.Errorf("Listen = %q, want the environment value", cfg.Listen)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want the environment value", cfg.LogLevel)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	isolateEnv(t)

	_, err := Load(writeConfig(t, "listen: [this is not a string\n"))
	if err == nil {
		t.Fatal("Load on malformed YAML returned nil error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error = %v, want it to mention parsing", err)
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	isolateEnv(t)

	_, err := Load(writeConfig(t, "probe:\n  timeout: \"not-a-duration\"\n"))
	if err == nil {
		t.Fatal("Load with an invalid duration returned nil error")
	}
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Errorf("error = %v, want it to mention an invalid duration", err)
	}
}

func TestValidateRejects(t *testing.T) {
	base := func() Config { return Default() }

	cases := map[string]func(*Config){
		"empty listen":        func(c *Config) { c.Listen = "" },
		"listen without port": func(c *Config) { c.Listen = "127.0.0.1" },
		"empty data dir":      func(c *Config) { c.DataDir = "  " },
		"unknown log level":   func(c *Config) { c.LogLevel = "verbose" },
		"concurrency zero":    func(c *Config) { c.Probe.Concurrency = 0 },
		"concurrency huge":    func(c *Config) { c.Probe.Concurrency = 9999 },
		"timeout zero":        func(c *Config) { c.Probe.Timeout = 0 },
		"timeout too long":    func(c *Config) { c.Probe.Timeout = Duration(10 * time.Minute) },
		"interval too short": func(c *Config) {
			c.Probe.Interval = Duration(time.Second)
		},
		// Negative is a mistake, not a way to ask for less than nothing; zero
		// is the documented way to switch the sweep off, so it is accepted.
		"interval negative": func(c *Config) {
			c.Probe.Interval = Duration(-1 * time.Minute)
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate returned nil, want an error")
			}
		})
	}
}

func TestDurationUnmarshal(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, "probe:\n  timeout: 90s\n  interval: 2h\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Probe.Timeout.Std(); got != 90*time.Second {
		t.Errorf("Timeout = %s, want 90s", got)
	}
	if got := cfg.Probe.Interval.Std(); got != 2*time.Hour {
		t.Errorf("Interval = %s, want 2h", got)
	}
}

func TestDefaultPath(t *testing.T) {
	if got := DefaultPath("/data"); got != filepath.Join("/data", DefaultFileName) {
		t.Errorf("DefaultPath = %q", got)
	}
}

// TestZeroIntervalDisablesTheSweep pins the one value that means "off".
//
// It has to be spelled out because the floor works the other way: every other
// short interval is rejected as a way of hammering community mirrors, so the
// only way to ask for less probing is to ask for none.
func TestZeroIntervalDisablesTheSweep(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, "probe:\n  interval: 0s\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Probe.Interval.Std(); got != 0 {
		t.Errorf("Interval = %s, want 0 to disable the sweep", got)
	}

	// The rest of the probe settings still have to be in range: "0" is an
	// exception for the interval alone, not a blanket amnesty.
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate on a disabled sweep: %v", err)
	}
}
