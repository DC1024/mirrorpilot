package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// handleIndex sends the visitor to whichever page can move them forward.
//
// This is the only handler that has to reason about the whole state machine,
// which is exactly why nothing else does: every other entry point assumes it
// has already been routed correctly.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	switch {
	case !st.Setup:
		redirect(w, r, "/setup")
	case !st.Authed:
		redirect(w, r, "/login")
	case !st.Unlocked:
		redirect(w, r, "/unlock")
	default:
		redirect(w, r, "/dashboard")
	}
}

// handleSetupForm shows the first-run form.
func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())
	if st.Setup {
		redirect(w, r, "/")
		return
	}

	s.render(w, r, "setup", http.StatusOK,
		s.newPageData(r, st, "setup.title", ""))
}

// handleSetup creates the account.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())
	if st.Setup {
		redirect(w, r, "/")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	// The confirmation check lives here, not in auth: it is a property of this
	// form, not an invariant of the account.
	if password != confirm {
		s.render(w, r, "setup", http.StatusBadRequest,
			s.errorPage(r, "setup.title", "", "error.password_mismatch"))
		return
	}

	sess, err := s.auth.Setup(r.Context(), username, password)
	if err != nil {
		s.renderAuthError(w, r, "setup", "setup.title", "", err)
		return
	}

	s.setSessionCookie(w, sess)
	redirect(w, r, "/dashboard")
}

// handleLoginForm shows the login form.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	if !st.Setup {
		redirect(w, r, "/setup")
		return
	}
	if st.Authed {
		redirect(w, r, "/")
		return
	}

	s.render(w, r, "login", http.StatusOK,
		s.newPageData(r, st, "login.title", ""))
}

// handleLogin verifies the credentials, unlocks the panel, and starts a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	if !st.Setup {
		redirect(w, r, "/setup")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	sess, err := s.auth.Login(r.Context(), username, password)
	if err != nil {
		s.renderAuthError(w, r, "login", "login.title", "", err)
		return
	}

	s.setSessionCookie(w, sess)
	redirect(w, r, "/dashboard")
}

// handleLogout ends the session and returns to the login form.
//
// It takes POST only, so that a link or a prefetch cannot log the user out.
// The CSRF check applies, which is the point of making it a POST.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	if st.Authed {
		if err := s.auth.Logout(r.Context(), st.Session.ID); err != nil {
			s.fail(w, r, "web: log out", err)
			return
		}
	}

	// Cleared regardless, so a stale or invalid cookie does not survive a
	// logout click.
	s.clearSessionCookie(w)
	redirect(w, r, "/login")
}

// handleUnlockForm asks for the password again after a restart.
func (s *Server) handleUnlockForm(w http.ResponseWriter, r *http.Request) {
	st := stateFrom(r.Context())

	// Nothing to do if it is already unlocked; send them where they were going.
	if st.Unlocked {
		redirect(w, r, "/dashboard")
		return
	}

	s.render(w, r, "unlock", http.StatusOK,
		s.newPageData(r, st, "unlock.title", ""))
}

// handleUnlock restores the master key without starting a new session.
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Unlock(r.Context(), r.PostFormValue("password")); err != nil {
		s.renderAuthError(w, r, "unlock", "unlock.title", "", err)
		return
	}

	redirect(w, r, "/dashboard")
}

// renderAuthError renders a form again with the reason attached, so a wrong
// password does not cost the user the rest of what they typed.
func (s *Server) renderAuthError(w http.ResponseWriter, r *http.Request, page, titleKey, nav string, err error) {
	key, status := authErrorKey(err)

	// Anything that is not an ordinary user mistake is our bug and belongs in
	// the log, where it is diagnosable, rather than only on the screen.
	if status >= http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "web: "+page, "err", err)
	}

	s.render(w, r, page, status, s.errorPage(r, titleKey, nav, key))
}

// authErrorKey maps an auth failure onto a message key and an HTTP status.
func authErrorKey(err error) (string, int) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return "error.invalid_credentials", http.StatusUnauthorized
	case errors.Is(err, auth.ErrWeakPassword):
		return "error.weak_password", http.StatusBadRequest
	case errors.Is(err, auth.ErrLocked):
		return "error.locked", http.StatusConflict
	case errors.Is(err, auth.ErrSessionExpired):
		return "error.session_expired", http.StatusUnauthorized
	case errors.Is(err, auth.ErrAlreadySetup):
		return "error.already_setup", http.StatusConflict
	case errors.Is(err, auth.ErrNotSetup):
		return "error.not_setup", http.StatusConflict
	case errors.Is(err, store.ErrNotFound):
		return "error.not_found", http.StatusNotFound
	default:
		return "error.internal", http.StatusInternalServerError
	}
}
