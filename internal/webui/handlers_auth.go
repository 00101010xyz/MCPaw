package webui

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/httpx"
	"github.com/00101010xyz/mcpaw/internal/platform/logging"
	"github.com/00101010xyz/mcpaw/internal/service"
)

// GetSetup renders the one-time first-administrator form.
func (s *Server) GetSetup(w http.ResponseWriter, r *http.Request) {
	needs, err := s.users.NeedsSetup(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !needs {
		redirect(w, r, "/login")
		return
	}
	s.render(w, r, http.StatusOK, "setup", s.page(r, "Setup", "", nil))
}

// PostSetup creates the first administrator and signs them in.
func (s *Server) PostSetup(w http.ResponseWriter, r *http.Request) {
	password := r.PostFormValue("password")
	if password != r.PostFormValue("password_confirm") {
		s.renderAuthError(w, r, "setup", "Setup", "The two passwords do not match.")
		return
	}

	actor := service.Actor{Type: service.ActorSystem, ID: "setup", IP: httpx.ClientIPFrom(r.Context())}
	user, err := s.users.Setup(r.Context(), actor, r.PostFormValue("email"), password)
	if err != nil {
		s.renderAuthError(w, r, "setup", "Setup", errorMessage(err))
		return
	}
	s.startSession(w, r, user, "/")
}

// GetLogin renders the sign-in form, or the setup form when no account exists.
func (s *Server) GetLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.UserFrom(r.Context()); ok {
		redirect(w, r, "/")
		return
	}
	needs, err := s.users.NeedsSetup(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if needs {
		redirect(w, r, "/setup")
		return
	}
	s.render(w, r, http.StatusOK, "login", s.page(r, "Sign in", "", nil))
}

// PostLogin authenticates an administrator.
func (s *Server) PostLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := httpx.ClientIPFrom(ctx)
	actor := service.Actor{Type: service.ActorUser, IP: ip}

	// The limiter is keyed on the client address rather than the submitted
	// email, so an attacker cannot exhaust a victim's budget to lock them out.
	if !s.loginLimiter.Allow(ip) {
		s.renderAuthErrorStatus(w, r, http.StatusTooManyRequests, "login", "Sign in",
			"Too many sign-in attempts. Wait a minute and try again.")
		return
	}

	user, err := s.users.Authenticate(ctx, actor, r.PostFormValue("email"), r.PostFormValue("password"))
	if err != nil {
		if errors.Is(err, domain.ErrUnauthorized) {
			s.loginLimiter.RecordFailure(ip)
			// One message for every failure: revealing which half was wrong
			// turns the form into an account-enumeration oracle.
			s.renderAuthErrorStatus(w, r, http.StatusUnauthorized, "login", "Sign in",
				"Incorrect email or password.")
			return
		}
		s.internalError(w, r, err)
		return
	}
	s.loginLimiter.RecordSuccess(ip)
	s.startSession(w, r, user, "/")
}

// startSession issues a session cookie and redirects.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user *domain.User, to string) {
	cookie, _, err := s.sessions.Issue(r.Context(), user, httpx.ClientIPFrom(r.Context()), r.UserAgent())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	httpx.SetSessionCookie(w, cookie, s.secureCookies, s.sessionMaxAge)
	redirect(w, r, to)
}

// PostLogout ends the session.
func (s *Server) PostLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Revoke(r.Context(), httpx.SessionCookieValue(r)); err != nil {
		logging.FromContext(r.Context()).Warn("could not revoke session", slog.String("error", err.Error()))
	}
	if user, ok := httpx.UserFrom(r.Context()); ok {
		s.audit.Success(r.Context(),
			service.Actor{Type: service.ActorUser, ID: user.ID, IP: httpx.ClientIPFrom(r.Context())},
			domain.ActionLogout, "user", user.ID, nil)
	}
	httpx.ClearSessionCookie(w, s.secureCookies)
	redirect(w, r, "/login")
}

// GetAccount renders the account page.
func (s *Server) GetAccount(w http.ResponseWriter, r *http.Request) {
	users, err := s.users.List(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "account",
		s.page(r, "Account", "account", map[string]any{"Users": users}))
}

// PostChangePassword updates the signed-in user's password.
func (s *Server) PostChangePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := httpx.UserFrom(r.Context())
	if !ok {
		redirect(w, r, "/login")
		return
	}
	next := r.PostFormValue("new_password")
	if next != r.PostFormValue("confirm_password") {
		s.flashError(r, "The two new passwords do not match.")
		redirect(w, r, "/account")
		return
	}

	actor := s.actor(r)
	if err := s.users.ChangePassword(r.Context(), actor, user.ID, r.PostFormValue("current_password"), next); err != nil {
		if errors.Is(err, domain.ErrUnauthorized) {
			s.flashError(r, "The current password is incorrect.")
		} else {
			s.flashError(r, "%s", errorMessage(err))
		}
		redirect(w, r, "/account")
		return
	}

	// Changing the password revoked every session, this one included, so the
	// user must sign in again — which is the point.
	httpx.ClearSessionCookie(w, s.secureCookies)
	redirect(w, r, "/login")
}

func (s *Server) actor(r *http.Request) service.Actor {
	a := service.Actor{Type: service.ActorUser, IP: httpx.ClientIPFrom(r.Context())}
	if user, ok := httpx.UserFrom(r.Context()); ok {
		a.ID = user.ID
	}
	return a
}

// renderAuthError re-renders an unauthenticated page with an inline message,
// since there is no session to carry a flash.
func (s *Server) renderAuthError(w http.ResponseWriter, r *http.Request, page, title, message string) {
	s.renderAuthErrorStatus(w, r, http.StatusBadRequest, page, title, message)
}

func (s *Server) renderAuthErrorStatus(w http.ResponseWriter, r *http.Request, status int, page, title, message string) {
	data := s.page(r, title, "", nil)
	data.Flash = &Flash{Level: FlashError, Message: message}
	s.render(w, r, status, page, data)
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	logging.FromContext(r.Context()).Error("web ui failure", slog.String("error", err.Error()))
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
