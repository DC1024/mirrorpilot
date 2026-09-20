package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// Cookie and form field names.
const (
	// csrfCookie holds the double-submit token.
	csrfCookie = "mirrorpilot_csrf"

	// csrfField is the hidden input every state-changing form carries.
	csrfField = "csrf"

	// csrfHeader lets a fetch() send the token instead.
	csrfHeader = "X-CSRF-Token"

	// csrfMaxAge is how long a rendered form stays submittable.
	csrfMaxAge = 12 * 3600
)

// ctxKey namespaces this package's context values.
type ctxKey int

const stateKey ctxKey = iota

// requestState is the per-request view of who is asking and what they may see.
//
// It is computed once per request and carried in the context, because every
// handler needs some of it and recomputing it would mean re-reading the session
// row — which is a write, since authenticating slides the expiry.
type requestState struct {
	Setup    bool
	Authed   bool
	Unlocked bool
	Username string
	Session  auth.Session

	Locale string
	Theme  string
	CSRF   string
}

// stateFrom returns the request state, or a zero value if the middleware did
// not run. A zero state denies everything, which is the safe direction.
func stateFrom(ctx context.Context) *requestState {
	if st, ok := ctx.Value(stateKey).(*requestState); ok {
		return st
	}
	return &requestState{}
}

// wrap applies the middleware chain, outermost first.
func (s *Server) wrap(next http.Handler) http.Handler {
	h := next
	h = s.withState(h)
	h = s.withCSRF(h)
	h = s.securityHeaders(h)
	h = s.withRecovery(h)
	h = s.withLogging(h)
	return h
}

// withRecovery turns a panic into a 500 instead of a dead connection.
//
// net/http already recovers per connection, but it closes the socket without a
// response, which looks to the user like the panel hung. This logs the stack
// and answers properly.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			// http.ErrAbortHandler is the documented way for a handler to say
			// "kill this connection"; re-panic so the server can do that.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			s.log.ErrorContext(r.Context(), "web: panic serving request",
				"method", r.Method, "path", r.URL.Path,
				"panic", rec, "stack", string(debug.Stack()))

			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()

		next.ServeHTTP(w, r)
	})
}

// withLogging records method, path, status and duration.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.log.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// securityHeaders sets the response headers that make the panel hard to embed,
// sniff, or exfiltrate from.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	// No inline scripts or styles, hence no 'unsafe-inline'. This is why the
	// theme lives in a data attribute and the copy buttons live in a file.
	const csp = "default-src 'none'; " +
		"script-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"form-action 'self'; " +
		"base-uri 'none'; " +
		"frame-ancestors 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), interest-cohort=()")

		// Only meaningful over TLS; sending it over plain HTTP is noise.
		if s.secure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// withCSRF enforces the double-submit token on state-changing requests.
//
// Two defences stack here. The session cookie is SameSite=Lax, so a browser
// will not attach it to a cross-site POST — the request arrives unauthenticated
// and is rejected. On top of that, every form carries a token that must match
// the CSRF cookie, which the Same-Origin Policy stops an attacker from reading.
// Either alone would do; together, neither has to be perfect.
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStateChanging(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(csrfCookie)
		if err != nil || cookie.Value == "" || !matchesCSRF(r, cookie.Value) {
			s.log.WarnContext(r.Context(), "web: rejected a request with a bad CSRF token",
				"method", r.Method, "path", r.URL.Path)

			// The template layer is not needed for a 403; a plain body keeps
			// this path from depending on anything that could itself fail.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// matchesCSRF compares the submitted token with the cookie in constant time.
func matchesCSRF(r *http.Request, want string) bool {
	got := r.PostFormValue(csrfField)
	if got == "" {
		got = r.Header.Get(csrfHeader)
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// isStateChanging reports whether a method needs CSRF protection. HEAD and
// OPTIONS are safe; everything else is checked.
func isStateChanging(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// withState resolves the session, preferences and CSRF token once per request.
func (s *Server) withState(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := &requestState{}
		ctx := r.Context()

		setup, err := s.store.HasUser(ctx)
		if err != nil {
			s.fail(w, r, "web: read setup state", err)
			return
		}
		st.Setup = setup

		if u, err := s.store.User(ctx); err == nil {
			st.Username = u.Username
		} else if !errorsIsNotFound(err) {
			s.fail(w, r, "web: read user", err)
			return
		}

		// Resolve the session. An unknown or expired cookie is not an error:
		// it just means the visitor is not logged in.
		if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
			if sess, err := s.auth.Authenticate(ctx, cookie.Value); err == nil {
				st.Authed = true
				st.Session = sess
			} else if !isExpectedAuthError(err) {
				s.fail(w, r, "web: authenticate session", err)
				return
			}
		}

		st.Unlocked = s.auth.Unlocked()

		prefs, err := s.store.Settings(ctx)
		if err != nil {
			s.fail(w, r, "web: read settings", err)
			return
		}
		st.Locale = s.resolveLocale(r, prefs)
		st.Theme = resolveTheme(prefs)

		// Mint the CSRF token here, before the handler has written anything:
		// a Set-Cookie after the body starts is silently dropped.
		token := s.ensureCSRFCookie(w, r)
		st.CSRF = token

		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, stateKey, st)))
	})
}

// resolveLocale picks the language: an explicit setting, else the browser's
// stated preference, else the default.
func (s *Server) resolveLocale(r *http.Request, prefs map[string]string) string {
	if stored := prefs[store.SettingLocale]; s.i18n.Has(stored) {
		return stored
	}
	return s.i18n.Negotiate("", r.Header.Get("Accept-Language"))
}

// resolveTheme reads the theme setting, defaulting to following the system.
func resolveTheme(prefs map[string]string) string {
	switch prefs[store.SettingTheme] {
	case themeLight, themeDark, themeAuto:
		return prefs[store.SettingTheme]
	default:
		return themeAuto
	}
}

// ensureCSRFCookie returns the token to embed in the page, minting a cookie the
// first time it is needed.
func (s *Server) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}

	token, err := randomToken()
	if err != nil {
		// Returning "" makes every subsequent form fail closed rather than
		// open, so there is nothing to recover from here but the log.
		s.log.ErrorContext(r.Context(), "web: mint CSRF token", "err", err)
		return ""
	}

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   csrfMaxAge,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})

	return token
}

// requireSession gates a handler behind a valid session.
//
// The redirect target is chosen from the state rather than fixed, so a
// half-configured or freshly restarted panel sends the user to the one page
// that can actually move them forward.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := stateFrom(r.Context())

		if !st.Setup {
			redirect(w, r, "/setup")
			return
		}
		if !st.Authed {
			redirect(w, r, "/login")
			return
		}

		next(w, r)
	}
}

// setSessionCookie writes the session cookie.
func (s *Server) setSessionCookie(w http.ResponseWriter, sess auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    sess.ID,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie removes the session cookie.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// isExpectedAuthError reports whether an authenticate failure is just "not
// logged in" rather than something worth a 500.
func isExpectedAuthError(err error) bool {
	return errors.Is(err, auth.ErrSessionExpired)
}

// errorsIsNotFound reports whether a store lookup found nothing.
func errorsIsNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
