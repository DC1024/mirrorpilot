package web

import (
	"net/http"

	"github.com/DC1024/mirrorpilot/internal/store"
)

// handleDashboard shows where things stand.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	s.render(w, r, "dashboard", http.StatusOK,
		s.newPageData(r, st, "dashboard.title", "nav.dashboard"))
}

// handleSettingsForm shows the preferences.
func (s *Server) handleSettingsForm(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	s.render(w, r, "settings", http.StatusOK,
		s.newPageData(r, st, "settings.title", "nav.settings"))
}

// handleSettings saves language and theme.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Only accept a locale we actually ship. Otherwise a stray value would be
	// stored and every page would fall back to the default until someone
	// noticed.
	if locale := r.PostFormValue("locale"); s.i18n.Has(locale) {
		if err := s.store.SetSetting(ctx, store.SettingLocale, locale); err != nil {
			s.fail(w, r, "web: save locale", err)
			return
		}
	}

	if theme := r.PostFormValue("theme"); isTheme(theme) {
		if err := s.store.SetSetting(ctx, store.SettingTheme, theme); err != nil {
			s.fail(w, r, "web: save theme", err)
			return
		}
	}

	// Post/redirect/get, so a refresh does not resubmit the form.
	redirect(w, r, "/settings?flash=settings.saved")
}

// handleLanguage switches language from the navigation bar.
func (s *Server) handleLanguage(w http.ResponseWriter, r *http.Request) {
	locale := r.PostFormValue("locale")
	if !s.i18n.Has(locale) {
		redirect(w, r, safeNext(r.PostFormValue("next"), "/settings"))
		return
	}

	if err := s.store.SetSetting(r.Context(), store.SettingLocale, locale); err != nil {
		s.fail(w, r, "web: save locale", err)
		return
	}

	redirect(w, r, safeNext(r.PostFormValue("next"), "/settings"))
}

// handleTheme switches theme from the navigation bar.
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	theme := r.PostFormValue("theme")
	if !isTheme(theme) {
		redirect(w, r, safeNext(r.PostFormValue("next"), "/settings"))
		return
	}

	if err := s.store.SetSetting(r.Context(), store.SettingTheme, theme); err != nil {
		s.fail(w, r, "web: save theme", err)
		return
	}

	redirect(w, r, safeNext(r.PostFormValue("next"), "/settings"))
}

// handlePasswordForm shows the change-password form.
func (s *Server) handlePasswordForm(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	// Rotating the master key needs the current one in memory, so this page is
	// meaningless while locked.
	if !st.Unlocked {
		redirect(w, r, "/unlock")
		return
	}

	s.render(w, r, "password", http.StatusOK,
		s.newPageData(r, st, "password.title", "nav.settings"))
}

// handlePassword rotates the master key and re-encrypts stored credentials.
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	current := r.PostFormValue("current")
	next := r.PostFormValue("new")
	confirm := r.PostFormValue("confirm")

	if next != confirm {
		s.render(w, r, "password", http.StatusBadRequest,
			s.errorPage(r, "password.title", "nav.settings", "error.password_mismatch"))
		return
	}

	kicked, err := s.auth.ChangePassword(ctx, current, next)
	if err != nil {
		s.renderAuthError(w, r, "password", "password.title", "nav.settings", err)
		return
	}

	// Every session was invalidated, including this one, so the cookie has to
	// go too — otherwise the browser keeps presenting a dead session ID and
	// every subsequent page bounces to the login form anyway.
	s.clearSessionCookie(w)

	s.log.InfoContext(ctx, "web: master password changed", "sessions_ended", kicked)
	redirect(w, r, "/login?flash=password.changed")
}

// isTheme reports whether a value names a theme we support.
func isTheme(theme string) bool {
	switch theme {
	case themeLight, themeDark, themeAuto:
		return true
	default:
		return false
	}
}
