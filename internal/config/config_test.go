package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/probe"
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

func TestBlobTimeoutIsLoadedFromTheFile(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, "probe:\n  blob_timeout: 90s\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Probe.BlobTimeout.Std(); got != 90*time.Second {
		t.Errorf("BlobTimeout = %s, want 90s", got)
	}
}

// The two budgets are one decision made in two packages: this one owns the
// knobs and the probe engine owns the fallback. A test rather than an import,
// because this package deliberately depends on nothing but the standard
// library, and a silent divergence would mean the shipped default and the
// documented default are different numbers.
func TestBlobTimeoutDefaultMatchesTheEngine(t *testing.T) {
	if got := Default().Probe.BlobTimeout.Std(); got != probe.DefaultBlobTimeout {
		t.Errorf("default blob_timeout = %s, but the engine falls back to %s",
			got, probe.DefaultBlobTimeout)
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
		"blob timeout zero":   func(c *Config) { c.Probe.BlobTimeout = 0 },
		"blob timeout huge":   func(c *Config) { c.Probe.BlobTimeout = Duration(10 * time.Minute) },
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

var (
	// Any mention of an environment variable, in a README or in Go source.
	envMention = regexp.MustCompile(`MIRRORPILOT_[A-Z0-9_]+`)
	// A *complete* string literal naming one. This is what a read looks like,
	// whether it goes through os.Getenv("X") directly or through a helper like
	// envOr("X", fallback): in both the name is the whole literal, so prose
	// that merely mentions a variable cannot pass for a read.
	envLiteral = regexp.MustCompile(`"MIRRORPILOT_[A-Z0-9_]+"`)
)

// TestDocumentedEnvVarsMatchTheCode pins the flag table in both READMEs to the
// code that reads the variables.
//
// MIRRORPILOT_CONFIG sat in both files for a long time as the documented way to
// point the panel at a config file, while nothing read it. Anyone who set it got
// the default path, no error, and nothing to search the logs for. Documenting a
// variable that does not exist is the dangerous direction — the reverse is only
// untidy — but both are checked here because the fix is the same.
func TestDocumentedEnvVarsMatchTheCode(t *testing.T) {
	t.Parallel()

	read, err := envVarsReadByCode()
	if err != nil {
		t.Fatalf("scan the source tree: %v", err)
	}
	// Without this the test passes vacuously if the walk ever looks in the wrong
	// place, which is the failure mode that makes a guard worthless.
	if len(read) == 0 {
		t.Fatal("found no MIRRORPILOT_* literal in the source tree")
	}

	for _, doc := range []string{"../../README.md", "../../README.zh-CN.md"} {
		body, readErr := os.ReadFile(doc)
		if readErr != nil {
			t.Fatalf("read %s: %v", doc, readErr)
		}

		documented := map[string]bool{}
		for _, name := range envMention.FindAllString(string(body), -1) {
			documented[name] = true
		}

		for _, name := range sortedKeys(documented) {
			if !read[name] {
				t.Errorf("%s documents %s, but no production code reads it: setting it would do nothing, silently", doc, name)
			}
		}
		for _, name := range sortedKeys(read) {
			if !documented[name] {
				t.Errorf("%s does not document %s, which the code reads", doc, name)
			}
		}
	}
}

// envVarsReadByCode collects every MIRRORPILOT_* name the non-test source
// spells out.
func envVarsReadByCode() (map[string]bool, error) {
	found := map[string]bool{}

	// Tests run with the package as the working directory.
	err := filepath.WalkDir("../..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "../.." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, literal := range envLiteral.FindAllString(string(body), -1) {
			found[strings.Trim(literal, `"`)] = true
		}

		return nil
	})

	return found, err
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}
