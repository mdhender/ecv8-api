// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/tokens"
)

// identityKey is the echo context key holding the authenticated identity.
const identityKey = "ecv8.identity"

// identity is who the current request is acting as.
//
// Actor is always the account that actually logged in. Effective is who the
// request acts as, which differs only while an administrator is impersonating.
// Keeping both means an audit log always has the real operator, and stopping
// impersonation never needs a fresh login.
type identity struct {
	Session   *store.Session
	Actor     *store.Account
	Effective *store.Account
	// token is the plaintext cookie value for this request. It is used to
	// revoke exactly this session and is never logged or serialised.
	token string
}

// IsImpersonating reports whether the actor is acting as someone else.
func (i *identity) IsImpersonating() bool { return i.Actor.ID != i.Effective.ID }

// HasAdminRights reports whether the request may use admin endpoints.
//
// An impersonating administrator deliberately does not: while impersonating,
// the request should be able to do exactly what the impersonated user can do
// and no more. Regaining admin rights requires stopping impersonation.
func (i *identity) HasAdminRights() bool {
	return !i.IsImpersonating() && i.Actor.IsAdmin()
}

// identityOf returns the authenticated identity, or nil.
func identityOf(c *echo.Context) *identity {
	if v, ok := c.Get(identityKey).(*identity); ok {
		return v
	}
	return nil
}

// requestLogger emits one structured record per request.
//
// It logs identifiers and outcomes only. Cookies, bodies, tokens, and query
// values are never captured, so a log file cannot be replayed into a session.
func (s *Server) requestLogger() echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogLatency:   true,
		LogRemoteIP:  true,
		LogMethod:    true,
		LogURIPath:   true,
		LogRoutePath: true,
		LogRequestID: true,
		LogStatus:    true,
		HandleError:  true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			attrs := []any{
				"method", v.Method,
				"path", v.URIPath,
				"route", v.RoutePath,
				"status", v.Status,
				"latency_ms", v.Latency.Milliseconds(),
				"remote_ip", v.RemoteIP,
				"request_id", v.RequestID,
			}
			if id := identityOf(c); id != nil {
				attrs = append(attrs, "actor_id", id.Actor.ID)
				if id.IsImpersonating() {
					attrs = append(attrs, "impersonated_id", id.Effective.ID)
				}
			}

			level := slog.LevelInfo
			switch {
			case v.Status >= http.StatusInternalServerError:
				level = slog.LevelError
			case v.Status >= http.StatusBadRequest:
				level = slog.LevelWarn
			}
			s.log.Log(c.Request().Context(), level, "request", attrs...)
			return nil
		},
	})
}

// crossOriginProtection rejects unsafe cross-origin browser requests using the
// standard library's CrossOriginProtection.
//
// It replaces a form-token CSRF scheme: with a same-origin deployment the
// browser's own Sec-Fetch-Site header is a stronger signal than a token the
// server has to store and match. GET, HEAD, and OPTIONS are always allowed,
// which is why no endpoint below changes state on those methods.
//
// This is not a replacement for the cookie's HttpOnly, Secure, and SameSite
// attributes; those defend against script access and network interception,
// which cross-origin protection does not address.
func (s *Server) crossOriginProtection(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if err := s.crossOrigin.Check(c.Request()); err != nil {
			return forbidden("This request was blocked by cross-origin protection.")
		}
		return next(c)
	}
}

// authenticate resolves the session cookie into an identity when one is
// present. It never rejects a request: requireAuth and requireAdmin decide
// that, so public endpoints still work with a stale cookie.
func (s *Server) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		cookie, err := c.Cookie(s.cfg.CookieName)
		if err != nil || cookie.Value == "" {
			return next(c)
		}

		ctx := c.Request().Context()
		now := store.Now()

		session, actor, impersonated, err := s.db.SessionByTokenHash(ctx, tokens.Fingerprint(cookie.Value), now)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The cookie names a session that no longer exists, so clear it
				// rather than letting the browser resend it forever.
				s.clearSessionCookie(c)
				return next(c)
			}
			return err
		}

		effective := actor
		if impersonated != nil {
			effective = impersonated
		}
		id := &identity{Session: session, Actor: actor, Effective: effective, token: cookie.Value}
		c.Set(identityKey, id)

		// Slide the idle deadline, bounded by the absolute lifetime. The write
		// is throttled so a burst of requests does not serialise on the write
		// lock for no benefit.
		s.slideSession(c, id, now)
		return next(c)
	}
}

// slideRefreshInterval is the minimum gap between idle-deadline writes.
const slideRefreshInterval = time.Minute

// slideSession extends a session's idle window without exceeding its absolute
// lifetime. A failure here is logged but never fails the request: the session
// is still valid, it just keeps its previous deadline.
func (s *Server) slideSession(c *echo.Context, id *identity, now time.Time) {
	if s.db.ReadOnly() {
		return
	}
	if now.Sub(id.Session.LastSeenAt) < slideRefreshInterval {
		return
	}
	absolute := id.Session.CreatedAt.Add(s.cfg.SessionTTL)
	expiresAt := now.Add(s.cfg.SessionIdleTTL)
	if expiresAt.After(absolute) {
		expiresAt = absolute
	}
	if !expiresAt.After(id.Session.ExpiresAt) {
		return
	}
	if err := s.db.TouchSession(c.Request().Context(), id.Session.ID, now, expiresAt); err != nil {
		s.log.Warn("extend session", "error", err)
		return
	}
	id.Session.LastSeenAt = now
	id.Session.ExpiresAt = expiresAt
}

// requireAuth rejects unauthenticated requests.
func (s *Server) requireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		id := identityOf(c)
		if id == nil {
			return unauthorized("Sign in to continue.")
		}
		// A deactivated account keeps its existing session until it expires or
		// an administrator revokes it; only new logins are refused. See the
		// README for why that is a deliberate choice.
		return next(c)
	}
}

// requireAdmin rejects anyone without admin rights. Frontend route guards are a
// convenience; this is the actual boundary.
func (s *Server) requireAdmin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		id := identityOf(c)
		if id == nil {
			return unauthorized("Sign in to continue.")
		}
		if !id.HasAdminRights() {
			if id.IsImpersonating() {
				return forbidden("Stop impersonating to use administrator endpoints.")
			}
			return forbidden("This endpoint requires an administrator account.")
		}
		return next(c)
	}
}

// bindJSON decodes a JSON request body into dst.
//
// The body is bounded twice — by BodyLimit middleware and by MaxBytesReader —
// and unknown fields are rejected, so a client typo becomes a clear 400 instead
// of a silently ignored field. Trailing content is rejected too, so a body
// cannot smuggle a second document.
func (s *Server) bindJSON(c *echo.Context, dst any) error {
	request := c.Request()
	contentType := request.Header.Get(echo.HeaderContentType)
	if contentType != "" {
		mediaType, _, _ := strings.Cut(contentType, ";")
		if strings.TrimSpace(mediaType) != echo.MIMEApplicationJSON {
			return badRequest("Request body must be application/json.")
		}
	}

	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), request.Body, s.cfg.MaxBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		var maxBytes *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			return badRequest("A JSON request body is required.")
		case errors.As(err, &maxBytes):
			return newProblem(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("Request body must be at most %d bytes.", s.cfg.MaxBodyBytes))
		}
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return badRequest(fmt.Sprintf("Request body is not valid JSON (byte %d).", syntaxErr.Offset))
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return unprocessable("A field has the wrong type.", FieldError{
				Field:   typeErr.Field,
				Message: fmt.Sprintf("expected %s", typeErr.Type),
			})
		}
		// DisallowUnknownFields reports its own message, which names the field.
		return badRequest(strings.TrimPrefix(err.Error(), "json: "))
	}

	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return badRequest("Request body must contain exactly one JSON object.")
	}
	return nil
}

// pathID reads a positive integer path parameter.
func pathID(c *echo.Context, name string) (int64, error) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest(fmt.Sprintf("%s must be a positive integer.", name))
	}
	return id, nil
}

// Pagination bounds. The maximum keeps one request from reading the whole
// table; the default keeps a normal page small.
const (
	defaultPageSize = 25
	maxPageSize     = 100
)

// pageParams reads ?page= and ?per_page=, clamping both to sane bounds.
func pageParams(c *echo.Context) (store.Page, error) {
	page := store.Page{Number: 1, Size: defaultPageSize}

	if raw := c.QueryParam("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return page, badRequest("page must be a positive integer.")
		}
		page.Number = n
	}
	if raw := c.QueryParam("per_page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return page, badRequest("per_page must be a positive integer.")
		}
		if n > maxPageSize {
			return page, badRequest(fmt.Sprintf("per_page must be at most %d.", maxPageSize))
		}
		page.Size = n
	}
	return page, nil
}

// boolQueryParam reads an optional true/false query parameter.
func boolQueryParam(c *echo.Context, name string) (*bool, error) {
	raw := c.QueryParam(name)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, badRequest(fmt.Sprintf("%s must be true or false.", name))
	}
	return &value, nil
}
