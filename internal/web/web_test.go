package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/store"
)

const (
	testUser     = "dc"
	testPassword = "correct-horse"
	testNewPass  = "battery-staple"
)

// testWriter forwards slog output into the test log.
//
// Requests are handled synchronously inside the test goroutine, so calling
// t.Log from here is safe.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// harness is a running panel plus a cookie-aware client.
type harness struct {
	t      *testing.T
	ts     *httptest.Server
	client *http.Client
	store  *store.Store
	auth   *auth.Manager
	server *Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx := context.Background()

	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	bundle, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	manager := auth.New(st, auth.Config{})

	// Log output goes to the test log rather than nowhere: a template that
	// fails mid-render writes half a page and reports the reason only to the
	// logger, which is exactly the failure that is hardest to diagnose from
	// the response body alone.
	srv, err := New(Options{
		Store:   st,
		Auth:    manager,
		I18n:    bundle,
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	return &harness{
		t:      t,
		ts:     ts,
		store:  st,
		auth:   manager,
		server: srv,
		client: &http.Client{
			Jar: jar,
			// Redirects are asserted on rather than followed: most of what
			// this panel does is choose the right next page.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()

	resp, err := h.client.Get(h.ts.URL + path)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// post submits a form with the CSRF token the browser would have picked up from
// the page it last rendered.
func (h *harness) post(path string, form url.Values) *http.Response {
	h.t.Helper()

	if form == nil {
		form = url.Values{}
	}
	if form.Get(csrfField) == "" {
		form.Set(csrfField, h.csrfToken())
	}

	resp, err := h.client.PostForm(h.ts.URL+path, form)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// csrfToken reads the token straight out of the jar.
func (h *harness) csrfToken() string {
	h.t.Helper()

	u, err := url.Parse(h.ts.URL)
	if err != nil {
		h.t.Fatalf("parse test server URL: %v", err)
	}
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == csrfCookie {
			return c.Value
		}
	}
	return ""
}

// body reads and closes a response.
func (h *harness) body(resp *http.Response) string {
	h.t.Helper()

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// setup runs the first-run flow and returns the setup response.
func (h *harness) setup() *http.Response {
	h.t.Helper()

	// A GET first, so the CSRF cookie exists.
	h.body(h.get("/setup"))

	if resp := h.post("/setup", url.Values{
		"username": {testUser},
		"password": {testPassword},
		"confirm":  {testPassword},
	}); resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("POST /setup = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}

	return h.post("/login", url.Values{
		"username": {testUser},
		"password": {testPassword},
	})
}

// Every template must render against a minimal context.
//
// html/template's escaping analysis can reject a construct at execution time
// rather than at parse time, and when that happens the panel answers a request
// with a half-written page. Executing each one here turns that into a test
// failure with the actual reason attached.
func TestEveryTemplateRenders(t *testing.T) {
	h := newHarness(t)

	for _, name := range pages {
		t.Run(name, func(t *testing.T) {
			data := &pageData{
				Lang:     "en",
				Theme:    themeAuto,
				TitleKey: "app.name",
				Nav:      "nav.dashboard",
				SelfPath: "/dashboard",
				CSRF:     "token",
				User:     "dc",
				Version:  "test",
				Authed:   true,
				Unlocked: true,
				Flash:    "saved",
				Error:    "something went wrong",

				// No translator: this test is about the template, not the
				// catalogue, which TestTemplateKeysExistInTheCatalog covers.
				// T then echoes keys, which is enough to see the layout.

				MinPasswordLength:  auth.MinPasswordLength,
				AvailableLanguages: []languageOption{{Tag: "en", Name: "English"}},
				ThemeOptions:       themeOptions(),
				LanguageNames:      languageNames,
			}

			var buf bytes.Buffer
			if err := h.server.tmpl[name].Execute(&buf, data); err != nil {
				t.Fatalf("render: %v", err)
			}

			body := buf.String()
			if !strings.Contains(body, "<!DOCTYPE html>") {
				t.Errorf("the output does not look like a page: %q", body)
			}
			if !strings.Contains(body, `data-theme="auto"`) {
				t.Error("the theme attribute is missing")
			}
			// The layout's title comes from the translator, so with none it
			// should still be the key rather than an empty string.
			if !strings.Contains(body, "<title>app.name") {
				t.Error("the title was not rendered")
			}
		})
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	bundle, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	cases := map[string]Options{
		"no store": {Auth: auth.New(st, auth.Config{}), I18n: bundle},
		"no auth":  {Store: st, I18n: bundle},
		"no i18n":  {Store: st, Auth: auth.New(st, auth.Config{})},
	}

	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Error("New succeeded with a missing dependency")
			}
		})
	}
}

// Every template must parse and define its content block. New does this at
// startup, so a plain New is the assertion.
func TestTemplatesLoad(t *testing.T) {
	h := newHarness(t)

	if len(h.server.tmpl) != len(pages) {
		t.Errorf("loaded %d templates, want %d", len(h.server.tmpl), len(pages))
	}
}

func TestIndexRoutesByState(t *testing.T) {
	t.Run("fresh panel goes to setup", func(t *testing.T) {
		h := newHarness(t)

		resp := h.get("/")
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET / = %d, want 303", resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "/setup" {
			t.Errorf("Location = %q, want /setup", got)
		}
	})

	t.Run("logged out goes to login", func(t *testing.T) {
		h := newHarness(t)
		h.body(h.get("/setup"))
		h.post("/setup", url.Values{
			"username": {testUser},
			"password": {testPassword},
			"confirm":  {testPassword},
		})

		// Drop the session cookie to look like a new browser.
		h.client.Jar, _ = cookiejar.New(nil)

		resp := h.get("/")
		if got := resp.Header.Get("Location"); got != "/login" {
			t.Errorf("Location = %q, want /login", got)
		}
	})

	t.Run("logged in goes to the dashboard", func(t *testing.T) {
		h := newHarness(t)
		if resp := h.setup(); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("login = %d, want 303", resp.StatusCode)
		}

		resp := h.get("/")
		if got := resp.Header.Get("Location"); got != "/dashboard" {
			t.Errorf("Location = %q, want /dashboard", got)
		}
	})

	t.Run("locked goes to unlock", func(t *testing.T) {
		h := newHarness(t)
		h.body(h.setup())

		h.auth.Lock()

		resp := h.get("/")
		if got := resp.Header.Get("Location"); got != "/unlock" {
			t.Errorf("Location = %q, want /unlock", got)
		}
	})
}

func TestSetupFlow(t *testing.T) {
	h := newHarness(t)

	if got := h.get("/setup").StatusCode; got != http.StatusOK {
		t.Fatalf("GET /setup = %d, want 200", got)
	}

	resp := h.post("/setup", url.Values{
		"username": {testUser},
		"password": {testPassword},
		"confirm":  {testPassword},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /setup = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}
	if got := resp.Header.Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want /dashboard", got)
	}

	// Setup logs the new account straight in, so the dashboard is reachable
	// without a separate login.
	if got := h.get("/dashboard").StatusCode; got != http.StatusOK {
		t.Errorf("GET /dashboard = %d, want 200", got)
	}

	// And the panel is now closed to a second setup.
	if got := h.get("/setup").Header.Get("Location"); got != "/" {
		t.Errorf("GET /setup after setup, Location = %q, want /", got)
	}
}

func TestSetupRejectsMismatchedConfirmation(t *testing.T) {
	h := newHarness(t)
	h.body(h.get("/setup"))

	resp := h.post("/setup", url.Values{
		"username": {testUser},
		"password": {testPassword},
		"confirm":  {"something-else"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /setup = %d, want 400", resp.StatusCode)
	}

	// The reason is shown on the form, not on a separate error page.
	if body := h.body(resp); !strings.Contains(body, "do not match") {
		t.Errorf("the response does not explain the mismatch; body: %s", body)
	}

	// And nothing was created.
	setup, err := h.store.HasUser(context.Background())
	if err != nil {
		t.Fatalf("HasUser: %v", err)
	}
	if setup {
		t.Error("a rejected setup created the account anyway")
	}
}

func TestLoginFlow(t *testing.T) {
	h := newHarness(t)
	h.body(h.get("/setup"))
	h.post("/setup", url.Values{
		"username": {testUser},
		"password": {testPassword},
		"confirm":  {testPassword},
	})

	h.client.Jar, _ = cookiejar.New(nil)

	cases := []struct {
		name     string
		username string
		password string
		want     int
	}{
		{"correct", testUser, testPassword, http.StatusSeeOther},
		{"wrong password", testUser, "nope", http.StatusUnauthorized},
		{"wrong username", "someone", testPassword, http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.body(h.get("/login"))

			resp := h.post("/login", url.Values{
				"username": {tc.username},
				"password": {tc.password},
			})
			if resp.StatusCode != tc.want {
				t.Errorf("POST /login = %d, want %d; body: %s",
					resp.StatusCode, tc.want, h.body(resp))
			}
		})
	}
}

func TestCSRFIsRequiredOnEveryStateChangingRequest(t *testing.T) {
	h := newHarness(t)
	h.body(h.setup())

	// A token that does not match the cookie.
	resp, err := h.client.PostForm(h.ts.URL+"/logout", url.Values{csrfField: {"forged"}})
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST with a forged token = %d, want 403", resp.StatusCode)
	}

	// No token at all.
	resp, err = h.client.PostForm(h.ts.URL+"/logout", url.Values{})
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST with no token = %d, want 403", resp.StatusCode)
	}

	// The session must have survived both attempts.
	if got := h.get("/dashboard").StatusCode; got != http.StatusOK {
		t.Errorf("GET /dashboard = %d, want 200; a rejected CSRF request logged us out", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/login")
	header := resp.Header

	for name, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	csp := header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy is missing")
	}
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	// Inline script and style are what CSP is here to forbid.
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP allows inline content, which defeats it: %q", csp)
	}

	// HSTS only makes sense over TLS, and this harness is plain HTTP.
	if got := header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q on a plain-HTTP server", got)
	}
}

func TestHealthzAndVersionNeedNoLogin(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	if body := h.body(resp); !strings.Contains(body, "ok") {
		t.Errorf("healthz body = %q", body)
	}

	resp = h.get("/api/version")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/version = %d, want 200", resp.StatusCode)
	}
	if body := h.body(resp); !strings.Contains(body, "test") {
		t.Errorf("version body = %q, want it to contain the build version", body)
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/static/app.css")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/app.css = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	if body := h.body(resp); !strings.Contains(body, "--bg") {
		t.Error("app.css does not look like the stylesheet")
	}
}

func TestProtectedPagesRedirectWhenLoggedOut(t *testing.T) {
	h := newHarness(t)
	h.body(h.get("/setup"))
	h.post("/setup", url.Values{
		"username": {testUser},
		"password": {testPassword},
		"confirm":  {testPassword},
	})
	h.client.Jar, _ = cookiejar.New(nil)

	for _, path := range []string{"/dashboard", "/settings", "/password", "/unlock"} {
		t.Run(path, func(t *testing.T) {
			resp := h.get(path)
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("GET %s = %d, want 303", path, resp.StatusCode)
			}
			if got := resp.Header.Get("Location"); got != "/login" {
				t.Errorf("Location = %q, want /login", got)
			}
		})
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	h := newHarness(t)
	if resp := h.setup(); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}

	if resp := h.post("/logout", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /logout = %d, want 303", resp.StatusCode)
	}

	resp := h.get("/dashboard")
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Errorf("GET /dashboard after logout, Location = %q, want /login", got)
	}
}

// Logging out has to be a POST: a GET would let any image tag or prefetch link
// log the user out.
func TestLogoutIsNotReachableByGet(t *testing.T) {
	h := newHarness(t)
	h.setup()

	if got := h.get("/logout").StatusCode; got != http.StatusMethodNotAllowed {
		t.Errorf("GET /logout = %d, want 405", got)
	}
}

func TestUnlockFlow(t *testing.T) {
	h := newHarness(t)
	if resp := h.setup(); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}
	h.auth.Lock()

	if got := h.get("/unlock").StatusCode; got != http.StatusOK {
		t.Fatalf("GET /unlock = %d, want 200", got)
	}

	// A wrong password re-renders the form with a reason.
	resp := h.post("/unlock", url.Values{"password": {"nope"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /unlock with a wrong password = %d, want 401", resp.StatusCode)
	}
	if h.auth.Unlocked() {
		t.Fatal("a failed unlock left the panel unlocked")
	}

	if resp := h.post("/unlock", url.Values{"password": {testPassword}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /unlock = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}
	if !h.auth.Unlocked() {
		t.Error("the panel is still locked after a successful unlock")
	}
}

// The whole reason the key is process-scoped: an expired or restarted panel
// must ask for the password once, not on every visit.
func TestLockedBannerAppearsOnlyWhileLocked(t *testing.T) {
	h := newHarness(t)
	h.setup()

	h.auth.Lock()
	if body := h.body(h.get("/dashboard")); !strings.Contains(body, "banner-warn") {
		t.Error("the locked dashboard does not show the banner")
	}

	if err := h.auth.Unlock(context.Background(), testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if body := h.body(h.get("/dashboard")); strings.Contains(body, "banner-warn") {
		t.Error("the unlocked dashboard still shows the banner")
	}
}

func TestPreferencesAreSavedAndApplied(t *testing.T) {
	h := newHarness(t)
	h.setup()

	// Chinese, dark.
	if resp := h.post("/settings", url.Values{"locale": {"zh-CN"}, "theme": {"dark"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /settings = %d, want 303", resp.StatusCode)
	}

	body := h.body(h.get("/settings"))
	if !strings.Contains(body, `lang="zh-CN"`) {
		t.Error("the language setting was not applied to the document")
	}
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Error("the theme setting was not applied to the document")
	}
	if !strings.Contains(body, "设置") {
		t.Error("the page was not rendered in Chinese")
	}

	// An unsupported value is ignored rather than stored.
	if resp := h.post("/settings", url.Values{"locale": {"klingon"}, "theme": {"neon"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /settings = %d, want 303", resp.StatusCode)
	}
	body = h.body(h.get("/settings"))
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Error("an unsupported theme overwrote the stored one")
	}
	if strings.Contains(body, "klingon") {
		t.Error("an unsupported locale was stored")
	}
}

func TestPreferenceSwitchesFromTheNavBar(t *testing.T) {
	h := newHarness(t)
	h.setup()

	resp := h.post("/lang", url.Values{"locale": {"zh-CN"}, "next": {"/settings"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /lang = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/settings" {
		t.Errorf("Location = %q, want /settings", got)
	}
	if !strings.Contains(h.body(h.get("/dashboard")), "总览") {
		t.Error("the language switch did not take effect")
	}

	resp = h.post("/theme", url.Values{"theme": {"light"}, "next": {"/"}})
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
	if !strings.Contains(h.body(h.get("/dashboard")), `data-theme="light"`) {
		t.Error("the theme switch did not take effect")
	}
}

// The next path comes from a form field, so it must not become an open redirect.
func TestPreferenceSwitchRefusesOffsiteRedirects(t *testing.T) {
	h := newHarness(t)
	h.setup()

	for _, next := range []string{"https://evil.example/", "//evil.example", "/\\evil.example", "javascript:alert(1)"} {
		t.Run(next, func(t *testing.T) {
			resp := h.post("/theme", url.Values{"theme": {"dark"}, "next": {next}})
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("POST /theme = %d, want 303", resp.StatusCode)
			}
			if got := resp.Header.Get("Location"); got != "/settings" {
				t.Errorf("Location = %q, want the safe fallback /settings", got)
			}
		})
	}
}

func TestDashboardRendersInTheNegotiatedLanguage(t *testing.T) {
	h := newHarness(t)
	h.setup()

	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/dashboard", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.5")

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	if body := h.body(resp); !strings.Contains(body, `lang="zh-CN"`) {
		t.Error("Accept-Language was not honoured")
	}
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	h := newHarness(t)

	if got := h.get("/nope").StatusCode; got != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", got)
	}
}

func TestPasswordChangeFlow(t *testing.T) {
	h := newHarness(t)
	h.setup()

	t.Run("mismatched confirmation", func(t *testing.T) {
		resp := h.post("/password", url.Values{
			"current": {testPassword},
			"new":     {testNewPass},
			"confirm": {"different"},
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /password = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("wrong current password", func(t *testing.T) {
		resp := h.post("/password", url.Values{
			"current": {"nope"},
			"new":     {testNewPass},
			"confirm": {testNewPass},
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST /password = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("too short", func(t *testing.T) {
		resp := h.post("/password", url.Values{
			"current": {testPassword},
			"new":     {"short"},
			"confirm": {"short"},
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /password = %d, want 400", resp.StatusCode)
		}
	})

	// A refused change must leave the old password working.
	h.client.Jar, _ = cookiejar.New(nil)
	h.body(h.get("/login"))
	if resp := h.post("/login", url.Values{"username": {testUser}, "password": {testPassword}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the original password stopped working after refused changes: %d", resp.StatusCode)
	}

	t.Run("accepted", func(t *testing.T) {
		resp := h.post("/password", url.Values{
			"current": {testPassword},
			"new":     {testNewPass},
			"confirm": {testNewPass},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /password = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
		}
		// Changing the password ends every session, so it lands on the login
		// form with an explanation rather than a dashboard that would bounce.
		if got := resp.Header.Get("Location"); got != "/login?flash=password.changed" {
			t.Errorf("Location = %q", got)
		}
	})

	// The cookie was cleared, so the panel is closed again.
	if got := h.get("/dashboard").Header.Get("Location"); got != "/login" {
		t.Errorf("GET /dashboard after a password change, Location = %q, want /login", got)
	}

	// Old password out, new one in.
	h.body(h.get("/login"))
	if resp := h.post("/login", url.Values{"username": {testUser}, "password": {testPassword}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the old password still logs in: %d", resp.StatusCode)
	}
	h.body(h.get("/login"))
	if resp := h.post("/login", url.Values{"username": {testUser}, "password": {testNewPass}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the new password does not log in: %d; body: %s", resp.StatusCode, h.body(resp))
	}
}

func TestPasswordPageRedirectsWhileLocked(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.auth.Lock()

	resp := h.get("/password")
	if got := resp.Header.Get("Location"); got != "/unlock" {
		t.Errorf("GET /password while locked, Location = %q, want /unlock", got)
	}
}

func TestFlashIsWhitelisted(t *testing.T) {
	h := newHarness(t)
	h.setup()

	// A message the panel knows about is shown.
	if body := h.body(h.get("/settings?flash=settings.saved")); !strings.Contains(body, "banner-ok") {
		t.Error("a whitelisted flash message was not rendered")
	}

	// Crafted text is not echoed back just because it appeared in a link.
	body := h.body(h.get("/settings?flash=" + url.QueryEscape("<script>alert(1)</script>")))
	if strings.Contains(body, "alert(1)") {
		t.Error("arbitrary text from the query string reached the page")
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                     "/fallback",
		"/dashboard":           "/dashboard",
		"/settings?x=1":        "/settings?x=1",
		"https://evil.example": "/fallback",
		"//evil.example":       "/fallback",
		"javascript:alert(1)":  "/fallback",
		"/a\\b":                "/fallback",
		"relative/path":        "/fallback",
	}

	for input, want := range cases {
		if got := safeNext(input, "/fallback"); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", input, got, want)
		}
	}
}
