package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/config"
	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// newTestServer builds the real handler over a throwaway database.
//
// This goes through newPanel rather than assembling a mux by hand, so a
// mis-wired dependency in production wiring shows up here.
func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()

	ctx := context.Background()

	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	panel, err := newPanel(config.Default(), db, auth.New(db, auth.Config{}), logger)
	if err != nil {
		t.Fatalf("newPanel: %v", err)
	}

	srv := httptest.NewServer(panel.Handler())
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	return srv, &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv, client := newTestServer(t)

	resp, err := client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); !strings.Contains(got, "ok") {
		t.Errorf("body = %q, want it to report ok", got)
	}
}

func TestVersionEndpoint(t *testing.T) {
	srv, client := newTestServer(t)

	resp, err := client.Get(srv.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["version"] != Version {
		t.Errorf("version = %q, want %q", got["version"], Version)
	}
}

// A fresh install has no account, so the panel must send a visitor to setup.
func TestFreshInstallRedirectsToSetup(t *testing.T) {
	srv, client := newTestServer(t)

	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/setup" {
		t.Errorf("Location = %q, want /setup", got)
	}
}

// Routes are registered with method-qualified patterns, so a POST must not be
// served by the GET handlers.
func TestWrongMethodRejected(t *testing.T) {
	srv, client := newTestServer(t)

	resp, err := client.Post(srv.URL+"/healthz", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /healthz returned 200, want a non-OK status")
	}
}

// Secure tells the panel to mark cookies Secure; getting it wrong either drops
// the session silently or leaks it over plain HTTP, so the mapping from
// configuration to option is worth pinning down.
func TestSecureCookiesFollowBaseURL(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for baseURL, want := range map[string]bool{
		"":                          false,
		"http://192.168.1.5:8080":   false,
		"https://panel.example.com": true,
		"HTTPS://PANEL.EXAMPLE.COM": true,
	} {
		cfg := config.Default()
		cfg.BaseURL = baseURL

		panel, err := newPanel(cfg, db, auth.New(db, auth.Config{}), logger)
		if err != nil {
			t.Fatalf("newPanel with base_url %q: %v", baseURL, err)
		}
		if got := panel.SecureCookies(); got != want {
			t.Errorf("base_url %q: SecureCookies = %v, want %v", baseURL, got, want)
		}
	}
}

// TestRunServesAndShutsDown drives the real entry point.
//
// Everything else in this file goes through newPanel, which covers the wiring
// but not run itself: flags, config resolution, the data directory checks, and
// the shutdown path. Those are exactly the parts that only fail in a container,
// so they get exercised over a real socket.
func TestRunServesAndShutsDown(t *testing.T) {
	dataDir := t.TempDir()
	addr := freeAddr(t)

	// run parses the global flag set, which can only be done once per process.
	os.Args = []string{"mirrorpilot", "-listen", addr, "-data", dataDir}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	base := "http://" + addr
	waitForHealth(t, base)

	// The database was created inside the data directory we asked for.
	if _, err := os.Stat(filepath.Join(dataDir, store.FileName)); err != nil {
		t.Errorf("the database was not created in the data directory: %v", err)
	}

	// A fresh install redirects to setup, over a real connection.
	resp, err := noRedirectClient().Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/setup" {
		t.Errorf("Location = %q, want /setup", got)
	}

	// And the security headers are on the wire, not just in a unit test.
	if got := resp.Header.Get("Content-Security-Policy"); got == "" {
		t.Error("Content-Security-Policy is missing from a real response")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return after its context was cancelled")
	}

	// Startup has to seed the built-in catalogue: the sources page is empty
	// without it, and an install that shows no mirrors looks broken rather
	// than unconfigured. Asserted from outside the process, so it covers the
	// real wiring rather than a call made in a test.
	assertBuiltinCatalogueSeeded(t, dataDir)
}

// assertBuiltinCatalogueSeeded reopens the database the run left behind and
// checks every mirror in the embedded catalogue is in it.
func assertBuiltinCatalogueSeeded(t *testing.T, dataDir string) {
	t.Helper()

	db, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("reopen the database: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.ListSources(context.Background())
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}

	builtin, err := mirror.Builtin()
	if err != nil {
		t.Fatalf("mirror.Builtin: %v", err)
	}

	if len(rows) != builtin.Len() {
		t.Fatalf("the database holds %d mirrors after startup, want %d",
			len(rows), builtin.Len())
	}

	stored := make(map[string]bool, len(rows))
	for _, rec := range rows {
		stored[rec.ID] = true
		if !rec.Builtin {
			t.Errorf("mirror %q came from the embedded catalogue but is not marked built-in", rec.ID)
		}
	}
	for _, src := range builtin.All() {
		if !stored[src.ID] {
			t.Errorf("the built-in mirror %q was not seeded", src.ID)
		}
	}
}

// freeAddr reserves a port and releases it, so run has somewhere to bind.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()

	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// waitForHealth polls until the server answers, so the test does not depend on
// how long the database takes to open.
func waitForHealth(t *testing.T, base string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	client := noRedirectClient()

	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("the server never became healthy")
}

func noRedirectClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestNewProbeFactoryValidatesTheTarget covers the seam between the panel and
// the engine.
//
// The panel deliberately does not re-implement the target's rules — it hands
// whatever the settings say to the factory and lets the engine judge. That
// makes this factory the only place the rule is applied, so it is the place to
// check it.
func TestNewProbeFactoryValidatesTheTarget(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	factory := newProbeFactory(db)

	runner, err := factory(probe.DefaultTarget())
	if err != nil {
		t.Fatalf("the factory refused the default target: %v", err)
	}
	if runner == nil {
		t.Fatal("the factory returned a nil runner and no error")
	}

	// A target the engine refuses, which the settings page can produce by
	// accepting a blank repository in a build with no default.
	for name, target := range map[string]probe.Target{
		"empty":      {},
		"no repo":    {Reference: "latest"},
		"no ref":     {Repository: "library/alpine"},
		"slash only": {Repository: "/", Reference: "latest"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := factory(target); err == nil {
				t.Errorf("the factory accepted the target %+v", target)
			}
		})
	}
}

func TestCheckWritable(t *testing.T) {
	if err := checkWritable(t.TempDir()); err != nil {
		t.Errorf("checkWritable on a writable temp dir: %v", err)
	}
}

func TestCheckWritableRejectsMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	if err := checkWritable(missing); err == nil {
		t.Error("checkWritable on a missing directory returned nil, want error")
	}
}

func TestNewLoggerLevels(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"info":     slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"error":    slog.LevelError,
		"":         slog.LevelInfo,
		"nonsense": slog.LevelInfo,
	}

	for level, want := range cases {
		if got := newLogger(level).Enabled(context.Background(), want); !got {
			t.Errorf("logger at level %q does not enable %v", level, want)
		}
	}

	// And a lower-priority record must stay off at a stricter level.
	if newLogger("error").Enabled(context.Background(), slog.LevelInfo) {
		t.Error("a logger at error level is accepting info records")
	}
}

func TestEnvOr(t *testing.T) {
	const key = "MIRRORPILOT_TEST_ENV_OR"

	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	if got := envOr(key, "fallback"); got != "fallback" {
		t.Errorf("envOr on unset key = %q, want %q", got, "fallback")
	}

	t.Setenv(key, "value")
	if got := envOr(key, "fallback"); got != "value" {
		t.Errorf("envOr on set key = %q, want %q", got, "value")
	}

	t.Setenv(key, "")
	if got := envOr(key, "fallback"); got != "fallback" {
		t.Errorf("envOr on empty key = %q, want %q", got, "fallback")
	}
}
