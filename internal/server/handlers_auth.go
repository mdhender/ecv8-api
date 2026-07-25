// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/password"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/tokens"
)

// loginRequest is the body of POST /api/v1/session.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin authenticates an account and starts a session.
//
// Every failure returns the same message and status. Distinguishing "no such
// account" from "wrong password" from "deactivated account" would let anyone
// enumerate which addresses have accounts and which have been disabled.
func (s *Server) handleLogin(c *echo.Context) error {
	var request loginRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}

	ctx := c.Request().Context()
	now := store.Now()
	const rejection = "Email or password is incorrect, or the account is not active."

	account, err := s.db.AccountByEmail(ctx, request.Email)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// Hash anyway so a missing account and a wrong password take
		// comparable time and cannot be told apart by a stopwatch.
		_ = password.Verify(decoyHash, request.Password)
		return unauthorized(rejection)
	}

	if err := account.VerifyPassword(request.Password); err != nil {
		return unauthorized(rejection)
	}
	// An inactive account is refused a new session. Sessions it already holds
	// keep working until they expire or are revoked; see requireAuth.
	if !account.IsActive {
		return unauthorized(rejection)
	}

	// A fresh login supersedes whatever cookie the browser sent, so a stale or
	// fixated session value can never be reused.
	if existing := identityOf(c); existing != nil {
		if err := s.db.DeleteSessionByTokenHash(ctx, tokens.Fingerprint(existing.token)); err != nil {
			return err
		}
	}

	token, err := tokens.New()
	if err != nil {
		return err
	}
	expiresAt := now.Add(min(s.cfg.SessionIdleTTL, s.cfg.SessionTTL))

	session, err := s.db.CreateSession(ctx, store.NewSession{
		AccountID: account.ID,
		TokenHash: tokens.Fingerprint(token),
		ExpiresAt: expiresAt,
		UserAgent: c.Request().UserAgent(),
		RemoteIP:  c.RealIP(),
	}, now)
	if err != nil {
		return err
	}

	s.setSessionCookie(c, token, expiresAt)
	s.log.Info("login", "account_id", account.ID, "session_id", session.ID)

	id := &identity{Session: session, Actor: account, Effective: account, token: token}
	return c.JSON(http.StatusOK, envelope{Data: newSessionView(id)})
}

// decoyHash is a valid bcrypt hash of a value no one can supply. Comparing
// against it gives a failed login for an unknown address roughly the same cost
// as one for a known address.
var decoyHash = mustDecoyHash()

func mustDecoyHash() string {
	hash, err := password.Hash("ecv8-decoy-secret")
	if err != nil {
		panic("server: build decoy hash: " + err.Error())
	}
	return hash
}

// handleLogout revokes the current session.
//
// It succeeds even with no session so a client can always reach a signed-out
// state, and it clears the cookie either way.
func (s *Server) handleLogout(c *echo.Context) error {
	id := identityOf(c)
	if id != nil {
		if err := s.db.DeleteSessionByTokenHash(c.Request().Context(), tokens.Fingerprint(id.token)); err != nil {
			return err
		}
		s.log.Info("logout", "account_id", id.Actor.ID, "session_id", id.Session.ID)
	}
	s.clearSessionCookie(c)
	return c.NoContent(http.StatusNoContent)
}

// handleCurrentSession returns the authenticated session. Ember Simple Auth's
// session store calls it on page load to restore a session from the cookie.
func (s *Server) handleCurrentSession(c *echo.Context) error {
	id := identityOf(c)
	if id == nil {
		return unauthorized("No active session.")
	}
	return c.JSON(http.StatusOK, envelope{Data: newSessionView(id)})
}

// impersonationRequest is the body of POST /api/v1/session/impersonation.
type impersonationRequest struct {
	AccountID int64 `json:"account_id"`
}

// handleStartImpersonation makes the current admin session act as another
// account.
//
// The session row keeps its own account_id and gains impersonated_account_id,
// so the real administrator stays recorded, stopping is a single update, and no
// second credential is ever issued. Admin endpoints are refused while
// impersonating (see identity.HasAdminRights).
func (s *Server) handleStartImpersonation(c *echo.Context) error {
	id := identityOf(c)

	var request impersonationRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.AccountID <= 0 {
		return unprocessable("A target account is required.",
			FieldError{Field: "account_id", Message: "must be a positive integer"})
	}
	if request.AccountID == id.Actor.ID {
		return conflict("You cannot impersonate yourself.")
	}

	ctx := c.Request().Context()
	target, err := s.db.AccountByID(ctx, request.AccountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound("No such account.")
		}
		return err
	}
	// Impersonating another administrator would let one admin act with a second
	// admin's identity, which defeats the audit trail.
	if target.IsAdmin() {
		return conflict("Administrator accounts cannot be impersonated.")
	}
	if !target.IsActive {
		return conflict("This account is deactivated and cannot be impersonated.")
	}
	if !target.Activated {
		return conflict("This account has not activated yet.")
	}

	if err := s.db.SetImpersonation(ctx, id.Session.ID, target.ID); err != nil {
		return err
	}
	s.log.Warn("impersonation started",
		"actor_id", id.Actor.ID, "target_id", target.ID, "session_id", id.Session.ID)

	id.Effective = target
	id.Session.ImpersonatedAccountID = target.ID
	return c.JSON(http.StatusOK, envelope{Data: newSessionView(id)})
}

// handleStopImpersonation returns the session to its real owner.
func (s *Server) handleStopImpersonation(c *echo.Context) error {
	id := identityOf(c)
	if !id.IsImpersonating() {
		return conflict("This session is not impersonating anyone.")
	}

	if err := s.db.SetImpersonation(c.Request().Context(), id.Session.ID, 0); err != nil {
		return err
	}
	s.log.Warn("impersonation stopped",
		"actor_id", id.Actor.ID, "target_id", id.Effective.ID, "session_id", id.Session.ID)

	id.Effective = id.Actor
	id.Session.ImpersonatedAccountID = 0
	return c.JSON(http.StatusOK, envelope{Data: newSessionView(id)})
}

// activationRequest is the body of POST /api/v1/activation.
type activationRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// handleRedeemActivation consumes a magic link and sets the account's first
// password, then signs the account in.
//
// Unknown, expired, and already-redeemed tokens all produce the same response,
// so the endpoint reveals nothing about which invitations exist or were
// accepted.
func (s *Server) handleRedeemActivation(c *echo.Context) error {
	var request activationRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.Token == "" {
		return unprocessable("An activation token is required.",
			FieldError{Field: "token", Message: "is required"})
	}
	if err := password.Validate(request.Password); err != nil {
		return unprocessable("The password does not meet the requirements.",
			FieldError{Field: "password", Message: passwordRuleMessage})
	}

	hash, err := password.Hash(request.Password)
	if err != nil {
		return err
	}

	ctx := c.Request().Context()
	now := store.Now()

	accountID, err := s.db.RedeemActivation(ctx, tokens.Fingerprint(request.Token), hash, now)
	if err != nil {
		if errors.Is(err, store.ErrActivationInvalid) {
			return newProblem(http.StatusGone,
				"This activation link is invalid or has expired. Ask an administrator for a new one.")
		}
		return err
	}

	account, err := s.db.AccountByID(ctx, accountID)
	if err != nil {
		return err
	}

	token, err := tokens.New()
	if err != nil {
		return err
	}
	expiresAt := now.Add(min(s.cfg.SessionIdleTTL, s.cfg.SessionTTL))
	session, err := s.db.CreateSession(ctx, store.NewSession{
		AccountID: account.ID,
		TokenHash: tokens.Fingerprint(token),
		ExpiresAt: expiresAt,
		UserAgent: c.Request().UserAgent(),
		RemoteIP:  c.RealIP(),
	}, now)
	if err != nil {
		return err
	}

	s.setSessionCookie(c, token, expiresAt)
	s.log.Info("account activated", "account_id", account.ID)

	id := &identity{Session: session, Actor: account, Effective: account, token: token}
	return c.JSON(http.StatusOK, envelope{Data: newSessionView(id)})
}

// passwordRuleMessage states the length policy without echoing the input.
const passwordRuleMessage = "must be 3 to 128 bytes"

// setSessionCookie writes the session cookie.
//
// HttpOnly keeps the value out of reach of any script, so no authentication
// state or bearer secret is ever stored in localStorage. SameSite plus
// CrossOriginProtection covers cross-site request forgery, and Secure keeps the
// cookie off plain-http connections in production.
func (s *Server) setSessionCookie(c *echo.Context, token string, expiresAt time.Time) {
	c.SetCookie(&http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: s.cfg.SameSite(),
	})
}

// clearSessionCookie expires the session cookie. The attributes must match
// setSessionCookie or the browser keeps the original.
func (s *Server) clearSessionCookie(c *echo.Context) {
	c.SetCookie(&http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: s.cfg.SameSite(),
	})
}
