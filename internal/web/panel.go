package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// handleDashboard shows where things stand.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	summary, err := s.catalogSummary(ctx)
	if err != nil {
		s.fail(w, r, "web: load catalogue summary", err)
		return
	}

	data := s.newPageData(r, stateFrom(ctx), "dashboard.title", "nav.dashboard")
	data.Summary = summary

	s.render(w, r, "dashboard", http.StatusOK, data)
}

// catalogSummary counts the catalogue and finds the newest measurement.
//
// The newest measurement is computed from the rows rather than with a MAX()
// query so the dashboard does not need a second store method for one number.
// The list and the latest-per-mirror map are both already loaded at this point.
func (s *Server) catalogSummary(ctx context.Context) (catalogSummary, error) {
	records, err := s.store.ListSources(ctx)
	if err != nil {
		return catalogSummary{}, err
	}

	latest, err := s.store.LatestProbes(ctx)
	if err != nil {
		return catalogSummary{}, err
	}

	summary := catalogSummary{Total: len(records), Probed: len(latest)}

	for _, rec := range records {
		if rec.Enabled {
			summary.Enabled++
		}
		if probe := latest[rec.ID]; probe.StartedAt.After(summary.LastProbe) {
			summary.LastProbe = probe.StartedAt
		}
	}

	return summary, nil
}

// settingsPage backs the preferences page.
type settingsPage struct {
	ProbeRepository string
	ProbeReference  string

	// ProbeDefault is the target used when the fields are left blank, shown as
	// the placeholder so an empty field explains what it will actually do
	// instead of looking unset.
	ProbeDefault probe.Target
}

// handleSettingsForm shows the preferences.
func (s *Server) handleSettingsForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	target, err := s.probeTarget(ctx)
	if err != nil {
		s.fail(w, r, "web: read probe target", err)
		return
	}

	data := s.newPageData(r, stateFrom(ctx), "settings.title", "nav.settings")
	data.SettingsPage = &settingsPage{
		ProbeRepository: target.Repository,
		ProbeReference:  target.Reference,
		ProbeDefault:    probe.DefaultTarget(),
	}

	s.render(w, r, "settings", http.StatusOK, data)
}

// handleSettings saves language, theme and the probe target.
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

	// An empty field clears the setting rather than storing "", which is what
	// makes the placeholder's promise true: blank means the default.
	if err := s.saveProbeTarget(ctx, r); err != nil {
		s.fail(w, r, "web: save probe target", err)
		return
	}

	// Post/redirect/get, so a refresh does not resubmit the form.
	redirect(w, r, "/settings?flash=settings.saved")
}

// saveProbeTarget stores the image probes measure.
func (s *Server) saveProbeTarget(ctx context.Context, r *http.Request) error {
	repository := strings.TrimSpace(r.PostFormValue("probe_repository"))
	reference := strings.TrimSpace(r.PostFormValue("probe_reference"))

	if err := s.storeSettingOrClear(ctx, store.SettingProbeRepository, repository); err != nil {
		return err
	}
	return s.storeSettingOrClear(ctx, store.SettingProbeReference, reference)
}

// storeSettingOrClear writes a setting, or removes it when the value is blank.
func (s *Server) storeSettingOrClear(ctx context.Context, key, value string) error {
	if value == "" {
		return s.store.DeleteSetting(ctx, key)
	}
	return s.store.SetSetting(ctx, key, value)
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
