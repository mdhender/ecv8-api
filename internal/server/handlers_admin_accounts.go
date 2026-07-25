// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/password"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/tokens"
)

// handleListAccounts returns one page of accounts.
func (s *Server) handleListAccounts(c *echo.Context) error {
	page, err := pageParams(c)
	if err != nil {
		return err
	}
	active, err := boolQueryParam(c, "active")
	if err != nil {
		return err
	}
	filter := store.AccountFilter{
		Query:  c.QueryParam("q"),
		Role:   c.QueryParam("role"),
		Active: active,
	}
	if filter.Role != "" && filter.Role != store.RoleAdmin && filter.Role != store.RoleUser {
		return badRequest("role must be admin or user.")
	}

	ctx := c.Request().Context()
	accounts, page, err := s.db.ListAccounts(ctx, filter, page)
	if err != nil {
		return err
	}

	now := store.Now()
	views := make([]adminAccountView, 0, len(accounts))
	for i := range accounts {
		view, err := s.adminAccountView(ctx, &accounts[i], now)
		if err != nil {
			return err
		}
		views = append(views, view)
	}
	return c.JSON(http.StatusOK, envelope{Data: views, Meta: pageMeta(page)})
}

// handleGetAccount returns one account.
func (s *Server) handleGetAccount(c *echo.Context) error {
	id, err := pathID(c, "accountID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	account, err := s.db.AccountByID(ctx, id)
	if err != nil {
		return s.storeError(err, "account")
	}
	view, err := s.adminAccountView(ctx, account, store.Now())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, envelope{Data: view})
}

// createAccountRequest is the body of POST /api/v1/admin/accounts.
type createAccountRequest struct {
	Email       string `json:"email"`
	Role        string `json:"role"`
	DisplayName string `json:"display_name"`
	Timezone    string `json:"timezone"`
	AdminNotes  string `json:"admin_notes"`

	// Password, when present, creates an account that can sign in immediately
	// and returns no activation link. The Ember client never sends it and there
	// is no interface for it; see handleCreateAccount.
	Password string `json:"password"`
	// IsActive defaults to true, which is what inviting somebody means.
	IsActive *bool `json:"is_active"`
}

// createAccountResponse pairs the new account with its one-time magic link.
//
// ActivationLink is null when the request supplied a password, because there is
// then nothing to redeem. It is a pointer rather than an empty struct so that
// "no link" is a value a client can test rather than a URL that happens to be
// the empty string.
type createAccountResponse struct {
	Account        adminAccountView    `json:"account"`
	ActivationLink *activationLinkView `json:"activation_link"`
}

// handleCreateAccount invites an account, or creates one that can sign in
// straight away.
//
// There is no public registration; administrators create every account. The
// ordinary path invites: the response carries an activation URL exactly once,
// because only its hash is stored and the application does not send email, and
// the administrator delivers it out of band.
//
// Passing "password" skips that and produces an account that is already
// activated. It exists for **testing**, where inviting and then redeeming a
// link to reach a usable account is two requests and a token to parse, and it
// is deliberately not surfaced in the client — nothing in `app/` sends the
// field and no interface offers it.
//
// It grants an administrator nothing they did not already have. Reissuing an
// activation link lets one take over any account today, so the capability is
// the same and only the number of steps differs. What it does mean is that an
// account's first password is not always chosen by its owner, which is why the
// log line says which of the two happened.
func (s *Server) handleCreateAccount(c *echo.Context) error {
	var request createAccountRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.Role == "" {
		request.Role = store.RoleUser
	}
	if request.Role != store.RoleAdmin && request.Role != store.RoleUser {
		return unprocessable("The role is invalid.",
			FieldError{Field: "role", Message: "must be admin or user"})
	}
	if request.Timezone != "" {
		if _, err := time.LoadLocation(request.Timezone); err != nil {
			return unprocessable("The time zone is invalid.",
				FieldError{Field: "timezone", Message: "must be an IANA time zone such as America/Chicago"})
		}
	}

	// A password is hashed before anything else touches it, and the plaintext is
	// never logged, echoed, or stored — the same rule the activation path
	// follows once an invitee chooses one.
	passwordHash := ""
	if request.Password != "" {
		if err := password.Validate(request.Password); err != nil {
			return unprocessable("The password does not meet the requirements.",
				FieldError{Field: "password", Message: passwordRuleMessage})
		}
		hash, err := password.Hash(request.Password)
		if err != nil {
			return err
		}
		passwordHash = hash
	}

	// An account with a password has nothing to redeem, so no token is minted
	// for it. Minting one anyway would leave a live activation link nobody
	// asked for.
	token := ""
	if passwordHash == "" {
		minted, err := tokens.New()
		if err != nil {
			return err
		}
		token = minted
	}

	ctx := c.Request().Context()
	now := store.Now()

	tokenHash := ""
	if token != "" {
		tokenHash = tokens.Fingerprint(token)
	}
	account, expiresAt, err := s.db.CreateAccount(ctx, store.NewAccount{
		Email:        request.Email,
		Role:         request.Role,
		DisplayName:  request.DisplayName,
		Timezone:     request.Timezone,
		AdminNotes:   request.AdminNotes,
		PasswordHash: passwordHash,
		IsActive:     request.IsActive,
	}, tokenHash, now)
	if err != nil {
		return s.storeError(err, "account")
	}

	// Which of the two happened is worth a distinct line: one produces an
	// account whose first password its owner chose, and one does not.
	event := "account invited"
	if passwordHash != "" {
		event = "account created with a password"
	}
	s.log.Info(event,
		"account_id", account.ID, "role", account.Role, "is_active", account.IsActive,
		"actor_id", identityOf(c).Actor.ID)

	view, err := s.adminAccountView(ctx, account, now)
	if err != nil {
		return err
	}
	response := createAccountResponse{Account: view}
	if token != "" {
		response.ActivationLink = &activationLinkView{
			AccountID: account.ID,
			URL:       s.cfg.ActivationURL(token),
			ExpiresAt: expiresAt,
		}
	}
	return c.JSON(http.StatusCreated, envelope{Data: response})
}

// updateAccountRequest is the body of PATCH /api/v1/admin/accounts/:accountID.
// Omitted fields are unchanged.
type updateAccountRequest struct {
	Email       *string `json:"email"`
	Role        *string `json:"role"`
	DisplayName *string `json:"display_name"`
	Timezone    *string `json:"timezone"`
	AdminNotes  *string `json:"admin_notes"`
	IsActive    *bool   `json:"is_active"`
}

// handleUpdateAccount edits or deactivates an account.
//
// Deactivation is the only form of deletion: rows are never removed, so history
// and referential integrity survive. Deactivating does not end existing
// sessions; use the revoke endpoint for that.
func (s *Server) handleUpdateAccount(c *echo.Context) error {
	actor := identityOf(c).Actor

	id, err := pathID(c, "accountID")
	if err != nil {
		return err
	}
	var request updateAccountRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.Email == nil && request.Role == nil && request.DisplayName == nil &&
		request.Timezone == nil && request.AdminNotes == nil && request.IsActive == nil {
		return unprocessable("Provide at least one field to update.")
	}
	if request.Role != nil && *request.Role != store.RoleAdmin && *request.Role != store.RoleUser {
		return unprocessable("The role is invalid.",
			FieldError{Field: "role", Message: "must be admin or user"})
	}
	if request.Timezone != nil {
		if _, err := time.LoadLocation(*request.Timezone); err != nil {
			return unprocessable("The time zone is invalid.",
				FieldError{Field: "timezone", Message: "must be an IANA time zone such as America/Chicago"})
		}
	}

	ctx := c.Request().Context()
	target, err := s.db.AccountByID(ctx, id)
	if err != nil {
		return s.storeError(err, "account")
	}

	// Guard against an administrator locking everyone out. Both checks look at
	// the change actually being made, not at who is making it, so the same rule
	// applies to editing yourself and editing a colleague.
	losesAdmin := (request.Role != nil && *request.Role == store.RoleUser) ||
		(request.IsActive != nil && !*request.IsActive)
	if target.IsAdmin() && target.IsActive && losesAdmin {
		remaining, err := s.db.CountActiveAdmins(ctx)
		if err != nil {
			return err
		}
		if remaining <= 1 {
			return conflict("This is the last active administrator; promote another account first.")
		}
	}
	if target.ID == actor.ID && request.IsActive != nil && !*request.IsActive {
		return conflict("You cannot deactivate your own account.")
	}

	account, err := s.db.UpdateAccount(ctx, id, store.AccountUpdate{
		Email:       request.Email,
		Role:        request.Role,
		DisplayName: request.DisplayName,
		Timezone:    request.Timezone,
		AdminNotes:  request.AdminNotes,
		IsActive:    request.IsActive,
	}, store.Now())
	if err != nil {
		return s.storeError(err, "account")
	}

	s.log.Info("account updated", "account_id", account.ID, "actor_id", actor.ID)

	view, err := s.adminAccountView(ctx, account, store.Now())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, envelope{Data: view})
}

// handleReissueActivation mints a replacement magic link.
//
// Issuing a replacement invalidates every outstanding link for the account in
// the same transaction, so a link that was already sent — perhaps to the wrong
// address — stops working the moment a new one is created.
func (s *Server) handleReissueActivation(c *echo.Context) error {
	id, err := pathID(c, "accountID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	account, err := s.db.AccountByID(ctx, id)
	if err != nil {
		return s.storeError(err, "account")
	}
	if !account.IsActive {
		return conflict("Reactivate this account before issuing an activation link.")
	}

	token, err := tokens.New()
	if err != nil {
		return err
	}
	expiresAt, err := s.db.IssueActivationLink(ctx, account.ID, tokens.Fingerprint(token), store.Now())
	if err != nil {
		return s.storeError(err, "account")
	}

	s.log.Info("activation link reissued",
		"account_id", account.ID, "actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusCreated, envelope{Data: activationLinkView{
		AccountID: account.ID,
		URL:       s.cfg.ActivationURL(token),
		ExpiresAt: expiresAt,
	}})
}

// revokeSessionsResponse reports how many sessions were ended.
type revokeSessionsResponse struct {
	AccountID int64 `json:"account_id"`
	Revoked   int   `json:"revoked"`
}

// handleRevokeSessions ends every session belonging to an account.
//
// This is separate from deactivation on purpose. Deactivating an account blocks
// new logins but leaves existing sessions alone; ending those is an explicit,
// audited administrator action.
func (s *Server) handleRevokeSessions(c *echo.Context) error {
	actor := identityOf(c).Actor

	id, err := pathID(c, "accountID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	if _, err := s.db.AccountByID(ctx, id); err != nil {
		return s.storeError(err, "account")
	}
	// Revoking your own sessions would sign you out mid-request and looks like
	// a mistake; log out instead.
	if id == actor.ID {
		return conflict("Use logout to end your own session.")
	}

	revoked, err := s.db.DeleteSessionsForAccount(ctx, id)
	if err != nil {
		return err
	}
	s.log.Warn("sessions revoked", "account_id", id, "revoked", revoked, "actor_id", actor.ID)

	return c.JSON(http.StatusOK, envelope{Data: revokeSessionsResponse{AccountID: id, Revoked: revoked}})
}

// adminAccountView decorates an account with the activation and session
// information only administrators see.
func (s *Server) adminAccountView(ctx context.Context, account *store.Account, now time.Time) (adminAccountView, error) {
	expiresAt, pending, err := s.db.PendingActivation(ctx, account.ID, now)
	if err != nil {
		return adminAccountView{}, err
	}
	sessions, err := s.db.CountSessionsForAccount(ctx, account.ID, now)
	if err != nil {
		return adminAccountView{}, err
	}
	var pendingAt *time.Time
	if pending {
		pendingAt = &expiresAt
	}
	return newAdminAccountView(account, pendingAt, sessions), nil
}

// storeError converts a store failure into a client-safe Problem. Anything it
// does not recognise is returned unchanged so the error handler logs it and
// answers 500 without detail.
func (s *Server) storeError(err error, subject string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return notFound("No such " + subject + ".")
	case store.IsConstraintError(err):
		return conflict(constraintDetail(err))
	case errors.Is(err, store.ErrReadOnly):
		return newProblem(http.StatusServiceUnavailable,
			"The database is open read-only; this operation is unavailable.")
	}
	return err
}

// constraintDetail extracts the message from a store conflict.
//
// store.ErrConflict wraps a message written for an end user, formatted as
// "conflict: <message>". Any other constraint failure — a raw SQLite error —
// falls back to a generic string so a schema detail never reaches a client.
func constraintDetail(err error) string {
	if errors.Is(err, store.ErrConflict) {
		marker := store.ErrConflict.Error() + ": "
		if message := err.Error(); strings.Contains(message, marker) {
			_, detail, _ := strings.Cut(message, marker)
			return capitalize(detail) + "."
		}
	}
	return "The request conflicts with the current state of the data."
}

// capitalize upper-cases the first rune so a lower-case store message reads as
// a sentence.
func capitalize(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+len(string(r)):]
	}
	return s
}
