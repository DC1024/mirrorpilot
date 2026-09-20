// Package web serves the panel: setup, login, unlock, and the settings pages.
//
// Everything is server-rendered. There is no build step, no framework, and the
// only JavaScript is a small file that handles copy buttons and destructive-form
// confirmation. That keeps the Content-Security-Policy strict enough to be
// worth having, and keeps the image small enough to pull over the slow links
// this project exists to help with.
package web

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// pages are the page templates, each contributing a "content" definition that
// layout.html renders into.
var pages = []string{
	"setup",
	"login",
	"unlock",
	"dashboard",
	"settings",
	"password",
	"error",
}

// theme names accepted by the theme setting.
const (
	themeLight = "light"
	themeDark  = "dark"
	themeAuto  = "auto"
)

// languageNames are the endonyms shown in the language switcher.
//
// Hardcoded rather than translated: a language picker has to be readable by
// someone who cannot read the language it is currently displayed in.
var languageNames = map[string]string{
	"en":    "English",
	"zh-CN": "简体中文",
}

// Options configures a Server.
type Options struct {
	Store *store.Store
	Auth  *auth.Manager
	I18n  *i18n.Bundle

	// Version is shown in the footer.
	Version string

	// Logger receives request and error logs. Defaults to slog.Default.
	Logger *slog.Logger

	// Secure marks cookies Secure.
	//
	// Set it when the panel is served over TLS. Leaving it on over plain HTTP
	// makes the browser drop the session cookie silently, which looks exactly
	// like "the password is wrong".
	Secure bool
}

// Server is the panel's HTTP surface.
type Server struct {
	store   *store.Store
	auth    *auth.Manager
	i18n    *i18n.Bundle
	log     *slog.Logger
	version string
	secure  bool

	tmpl map[string]*template.Template
	mux  *http.ServeMux
	root http.Handler
}

// New builds the panel handler, parsing every template up front so a broken one
// is a startup failure rather than a 500 on someone's first login.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("web: Store is required")
	case opts.Auth == nil:
		return nil, errors.New("web: Auth is required")
	case opts.I18n == nil:
		return nil, errors.New("web: I18n is required")
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	tmpl, err := loadTemplates()
	if err != nil {
		return nil, err
	}

	s := &Server{
		store:   opts.Store,
		auth:    opts.Auth,
		i18n:    opts.I18n,
		log:     log,
		version: opts.Version,
		secure:  opts.Secure,
		tmpl:    tmpl,
	}

	s.mux = s.routes()
	s.root = s.wrap(s.mux)

	return s, nil
}

// Handler returns the middleware-wrapped handler to serve.
func (s *Server) Handler() http.Handler { return s.root }

// SecureCookies reports whether cookies are marked Secure.
//
// Exposed for tests and for a startup log line: getting this wrong either drops
// the session cookie silently or sends it over plain HTTP, and both look like
// unrelated problems from the outside.
func (s *Server) SecureCookies() bool { return s.secure }

// loadTemplates parses layout and partials once per page, into one template set
// per page.
//
// Files are read and parsed explicitly rather than with ParseFS. ParseFS
// associates the receiver template with whichever file it happens to read
// first, and it reads in sorted order — so "templates/dashboard.html" would
// have become the root and the layout would have been ignored. Doing it by hand
// makes the layout the root by construction.
func loadTemplates() (map[string]*template.Template, error) {
	layout, err := assets.ReadFile("templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: read layout: %w", err)
	}
	partials, err := assets.ReadFile("templates/partials.html")
	if err != nil {
		return nil, fmt.Errorf("web: read partials: %w", err)
	}

	out := make(map[string]*template.Template, len(pages))

	for _, page := range pages {
		body, err := assets.ReadFile("templates/" + page + ".html")
		if err != nil {
			return nil, fmt.Errorf("web: read template %s: %w", page, err)
		}

		t, err := template.New("layout").Parse(string(layout))
		if err != nil {
			return nil, fmt.Errorf("web: parse layout for %s: %w", page, err)
		}
		if _, err := t.Parse(string(partials)); err != nil {
			return nil, fmt.Errorf("web: parse partials for %s: %w", page, err)
		}
		if _, err := t.Parse(string(body)); err != nil {
			return nil, fmt.Errorf("web: parse template %s: %w", page, err)
		}

		// Checked at startup: a page that forgot its content block would
		// otherwise render an empty shell or fail mid-request.
		if t.Lookup("content") == nil {
			return nil, fmt.Errorf("web: template %s does not define \"content\"", page)
		}

		out[page] = t
	}

	return out, nil
}

// routes wires the URL space.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	static, err := fs.Sub(assets, "static")
	if err != nil {
		// Only reachable if the embed directive and this path disagree, which
		// is a compile-time-shaped mistake; fail loudly rather than serving
		// nothing.
		panic(fmt.Sprintf("web: static assets are not embedded: %v", err))
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheImmutable(http.FileServer(http.FS(static)))))

	// Unauthenticated, and deliberately so: a healthcheck that needs a login is
	// useless to the thing doing the checking.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/version", s.handleVersion)

	mux.HandleFunc("GET /{$}", s.handleIndex)

	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetup)

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// Unlock needs a session but not an unlocked key: that combination is the
	// whole point of the page.
	mux.HandleFunc("GET /unlock", s.requireSession(s.handleUnlockForm))
	mux.HandleFunc("POST /unlock", s.requireSession(s.handleUnlock))

	mux.HandleFunc("GET /dashboard", s.requireSession(s.handleDashboard))

	mux.HandleFunc("GET /settings", s.requireSession(s.handleSettingsForm))
	mux.HandleFunc("POST /settings", s.requireSession(s.handleSettings))

	mux.HandleFunc("GET /password", s.requireSession(s.handlePasswordForm))
	mux.HandleFunc("POST /password", s.requireSession(s.handlePassword))

	mux.HandleFunc("POST /lang", s.requireSession(s.handleLanguage))
	mux.HandleFunc("POST /theme", s.requireSession(s.handleTheme))

	return mux
}

// render writes a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, data *pageData) {
	t, ok := s.tmpl[page]
	if !ok {
		s.log.ErrorContext(r.Context(), "web: unknown page", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	// Past this point the status is already on the wire, so a template failure
	// can only be logged. Everything that can fail structurally — a missing
	// file, a missing content block — was caught at startup.
	if err := t.Execute(w, data); err != nil {
		s.log.ErrorContext(r.Context(), "web: render", "page", page, "err", err)
	}
}

// fail renders the error page for an internal problem.
//
// It may be reached before withState has run, in which case stateFrom yields a
// zero state and the page renders in the default language without navigation.
// That is the right trade: a 500 is not the moment to depend on more machinery.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, msg string, err error) {
	s.log.ErrorContext(r.Context(), msg, "err", err)

	s.render(w, r, "error", http.StatusInternalServerError,
		s.errorPage(r, "error.title", "", "error.internal"))
}

// newPageData assembles the values every template needs.
//
// Flash and error messages are translated here rather than in the template:
// html/template cannot splat a slice into a variadic function, so a message
// needing arguments could not be formatted from the template side.
func (s *Server) newPageData(r *http.Request, st *requestState, titleKey, nav string) *pageData {
	tr := s.i18n.Translator(st.Locale)

	return &pageData{
		Lang:               st.Locale,
		Theme:              st.Theme,
		TitleKey:           titleKey,
		Nav:                nav,
		SelfPath:           r.URL.Path,
		translator:         tr,
		CSRF:               st.CSRF,
		Authed:             st.Authed,
		Unlocked:           st.Unlocked,
		User:               st.Username,
		Version:            s.version,
		Flash:              s.flash(r, tr),
		MinPasswordLength:  auth.MinPasswordLength,
		AvailableLanguages: s.languages(),
		ThemeOptions:       themeOptions(),
		LanguageNames:      languageNames,
	}
}

// themeOptions lists the selectable themes with the key that names each one.
func themeOptions() []themeOption {
	return []themeOption{
		{Tag: themeAuto, LabelKey: "settings.theme.auto"},
		{Tag: themeLight, LabelKey: "settings.theme.light"},
		{Tag: themeDark, LabelKey: "settings.theme.dark"},
	}
}

// errorPage builds a page carrying a translated reason for the failure.
func (s *Server) errorPage(r *http.Request, titleKey, nav, errKey string, args ...any) *pageData {
	st := stateFrom(r.Context())

	data := s.newPageData(r, st, titleKey, nav)
	data.Error = s.i18n.Translator(st.Locale).T(errKey, args...)

	return data
}

// languages lists the shipped locales with their endonyms.
func (s *Server) languages() []languageOption {
	tags := s.i18n.Langs()

	out := make([]languageOption, 0, len(tags))
	for _, tag := range tags {
		name, ok := languageNames[tag]
		if !ok {
			name = tag
		}
		out = append(out, languageOption{Tag: tag, Name: name})
	}
	return out
}

// languageOption is one entry in the language switcher.
type languageOption struct {
	Tag  string
	Name string
}

// themeOption is one entry in the theme picker.
type themeOption struct {
	Tag      string
	LabelKey string
}

// pageData is the template context.
type pageData struct {
	Lang     string
	Theme    string
	TitleKey string
	Nav      string
	CSRF     string
	Authed   bool
	Unlocked bool
	User     string
	Version  string

	// SelfPath is the path being rendered, so a preference change made from the
	// navigation bar can send the visitor back where they were.
	SelfPath string

	// Flash and Error are already translated.
	Flash string
	Error string

	MinPasswordLength  int
	AvailableLanguages []languageOption
	ThemeOptions       []themeOption
	LanguageNames      map[string]string

	// translator renders keys in the request's language.
	//
	// Unexported, and reached through T below, because text/template will only
	// pass arguments to a method. A func stored in an exported field can be
	// called with no arguments and nothing else, which is not much use for a
	// translation helper.
	translator *i18n.Translator
}

// T translates a key in the request's language, for the labels baked into the
// templates.
func (p *pageData) T(key string, args ...any) string {
	if p.translator == nil {
		return key
	}
	return p.translator.T(key, args...)
}

// randomToken returns a fresh 256-bit URL-safe token.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("web: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// cacheImmutable marks hashed static assets cacheable for a long time.
func cacheImmutable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// handleHealthz reports that the process is up. It deliberately does not touch
// the database: a healthcheck that fails because of a lock would restart a
// perfectly healthy container.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleVersion reports the build version as JSON.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	version := s.version
	if version == "" {
		version = "dev"
	}

	if err := json.NewEncoder(w).Encode(map[string]string{"version": version}); err != nil {
		s.log.ErrorContext(r.Context(), "web: write version", "err", err)
	}
}

// safeNext returns a local path to redirect to, or fallback.
//
// The value comes from a form field, so it is attacker-controlled. Only a
// site-relative path is accepted: an absolute URL or a protocol-relative
// "//host" would turn the panel into an open redirect.
func safeNext(next, fallback string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") {
		return fallback
	}
	if strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return fallback
	}
	// A path carrying a scheme is still not something we redirect to.
	if strings.Contains(next, ":") {
		return fallback
	}
	return next
}

// flashKeys are the messages a handler may ask the next page to show. A
// whitelist rather than a passthrough: the value arrives in the query string,
// and echoing an arbitrary one back would let a crafted link put text of the
// attacker's choosing on our own page.
var flashKeys = map[string]bool{
	"settings.saved":   true,
	"password.changed": true,
}

// flash reads the one-shot message from the query string and translates it.
func (s *Server) flash(r *http.Request, tr *i18n.Translator) string {
	key := r.URL.Query().Get("flash")
	if !flashKeys[key] {
		return ""
	}
	return tr.T(key)
}

// redirect sends the client to a local path.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}
