// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/password"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/tokens"
)

// handleGetProfile returns the authenticated account. While impersonating, this
// is the impersonated account, which is what makes the impersonated view
// faithful.
func (s *Server) handleGetProfile(c *echo.Context) error {
	id := identityOf(c)
	return c.JSON(http.StatusOK, envelope{Data: newAccountView(id.Effective)})
}

// profileRequest is the body of PATCH /api/v1/me. A field left out of the JSON
// object stays unchanged; an explicit null is rejected as a wrong type.
type profileRequest struct {
	DisplayName *string `json:"display_name"`
	Timezone    *string `json:"timezone"`
}

// handleUpdateProfile applies the fields an account may change about itself.
// Email, role, and active state are administrator-only and are not accepted
// here.
func (s *Server) handleUpdateProfile(c *echo.Context) error {
	id := identityOf(c)

	var request profileRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.DisplayName == nil && request.Timezone == nil {
		return unprocessable("Provide at least one field to update.")
	}

	var fields []FieldError
	if request.DisplayName != nil {
		name := strings.TrimSpace(*request.DisplayName)
		if name == "" || len(name) > 100 {
			fields = append(fields, FieldError{Field: "display_name", Message: "must be 1 to 100 bytes"})
		}
		request.DisplayName = &name
	}
	if request.Timezone != nil {
		zone := strings.TrimSpace(*request.Timezone)
		if _, err := time.LoadLocation(zone); err != nil {
			fields = append(fields, FieldError{Field: "timezone", Message: "must be an IANA time zone such as America/Chicago"})
		}
		request.Timezone = &zone
	}
	if len(fields) > 0 {
		return unprocessable("Some fields are invalid.", fields...)
	}

	account, err := s.db.UpdateProfile(c.Request().Context(), id.Effective.ID,
		request.DisplayName, request.Timezone, store.Now())
	if err != nil {
		return s.storeError(err, "account")
	}
	return c.JSON(http.StatusOK, envelope{Data: newAccountView(account)})
}

// passwordRequest is the body of PUT /api/v1/me/password.
type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword replaces the authenticated account's password.
//
// The current password is required so a borrowed session cannot silently take
// over an account, and every other session for the account is revoked so a
// session created with the old credential does not outlive it. The session
// making the change is kept, so the user is not signed out of their own browser.
//
// An impersonating administrator may not do this: changing someone's password
// is a credential change, not an action taken on their behalf.
func (s *Server) handleChangePassword(c *echo.Context) error {
	id := identityOf(c)
	if id.IsImpersonating() {
		return forbidden("Passwords cannot be changed while impersonating.")
	}

	var request passwordRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if err := id.Actor.VerifyPassword(request.CurrentPassword); err != nil {
		return unprocessable("The current password is incorrect.",
			FieldError{Field: "current_password", Message: "is incorrect"})
	}
	if err := password.Validate(request.NewPassword); err != nil {
		return unprocessable("The new password does not meet the requirements.",
			FieldError{Field: "new_password", Message: passwordRuleMessage})
	}

	hash, err := password.Hash(request.NewPassword)
	if err != nil {
		return err
	}

	ctx := c.Request().Context()
	if err := s.db.SetPassword(ctx, id.Actor.ID, hash, store.Now()); err != nil {
		return s.storeError(err, "account")
	}

	revoked, err := s.db.DeleteOtherSessionsForAccount(ctx, id.Actor.ID, tokens.Fingerprint(id.token))
	if err != nil {
		return err
	}
	s.log.Info("password changed", "account_id", id.Actor.ID, "sessions_revoked", revoked)

	return c.NoContent(http.StatusNoContent)
}

// handleMyGames lists the games the authenticated account plays, newest
// membership rules applied by the database.
func (s *Server) handleMyGames(c *echo.Context) error {
	id := identityOf(c)

	// An admin account can never hold a membership, so this is always empty for
	// one; the dashboard still works and simply shows no games.
	memberships, err := s.db.ListMembershipsForAccount(c.Request().Context(), id.Effective.ID, true)
	if err != nil {
		return err
	}

	views := make([]membershipView, 0, len(memberships))
	for i := range memberships {
		views = append(views, newMembershipView(&memberships[i]))
	}
	return c.JSON(http.StatusOK, envelope{Data: views})
}
