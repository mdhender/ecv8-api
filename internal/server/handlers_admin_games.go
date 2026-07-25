// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/store"
)

// handleListGames returns one page of games.
func (s *Server) handleListGames(c *echo.Context) error {
	page, err := pageParams(c)
	if err != nil {
		return err
	}
	active, err := boolQueryParam(c, "active")
	if err != nil {
		return err
	}

	games, page, err := s.db.ListGames(c.Request().Context(),
		store.GameFilter{Query: c.QueryParam("q"), Active: active}, page)
	if err != nil {
		return err
	}

	views := make([]gameView, 0, len(games))
	for i := range games {
		views = append(views, newGameView(&games[i]))
	}
	return c.JSON(http.StatusOK, envelope{Data: views, Meta: pageMeta(page)})
}

// handleGetGame returns one game.
func (s *Server) handleGetGame(c *echo.Context) error {
	id, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	game, err := s.db.GameByID(c.Request().Context(), id)
	if err != nil {
		return s.storeError(err, "game")
	}
	return c.JSON(http.StatusOK, envelope{Data: newGameView(game)})
}

// createGameRequest is the body of POST /api/v1/admin/games.
type createGameRequest struct {
	Name string `json:"name"`
}

// handleCreateGame adds a game.
func (s *Server) handleCreateGame(c *echo.Context) error {
	var request createGameRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}

	game, err := s.db.CreateGame(c.Request().Context(), request.Name, store.Now())
	if err != nil {
		return s.storeError(err, "game")
	}
	s.log.Info("game created", "game_id", game.ID, "actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusCreated, envelope{Data: newGameView(game)})
}

// updateGameRequest is the body of PATCH /api/v1/admin/games/:gameID.
type updateGameRequest struct {
	Name     *string `json:"name"`
	IsActive *bool   `json:"is_active"`
}

// handleUpdateGame renames or deactivates a game. Games are never deleted;
// deactivating one hides it from players while preserving its memberships.
func (s *Server) handleUpdateGame(c *echo.Context) error {
	id, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	var request updateGameRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.Name == nil && request.IsActive == nil {
		return unprocessable("Provide at least one field to update.")
	}

	game, err := s.db.UpdateGame(c.Request().Context(), id, store.GameUpdate{
		Name:     request.Name,
		IsActive: request.IsActive,
	}, store.Now())
	if err != nil {
		return s.storeError(err, "game")
	}
	s.log.Info("game updated", "game_id", game.ID, "actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusOK, envelope{Data: newGameView(game)})
}

// handleListMemberships returns every membership of a game.
func (s *Server) handleListMemberships(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	if _, err := s.db.GameByID(ctx, gameID); err != nil {
		return s.storeError(err, "game")
	}
	memberships, err := s.db.ListMemberships(ctx, gameID)
	if err != nil {
		return err
	}

	views := make([]membershipView, 0, len(memberships))
	for i := range memberships {
		views = append(views, newMembershipView(&memberships[i]))
	}
	return c.JSON(http.StatusOK, envelope{Data: views})
}

// membershipRequest is the body of
// PUT /api/v1/admin/games/:gameID/memberships/:accountID.
type membershipRequest struct {
	IsGM     *bool `json:"is_gm"`
	IsActive *bool `json:"is_active"`
}

// handleSaveMembership adds or updates an account's place in a game.
//
// PUT is the right verb because the membership is identified entirely by its
// path: the same request creates it or brings it to the requested state.
// Memberships are deactivated rather than removed, which keeps a game's history
// intact.
//
// Only user accounts may be members. The check below produces a clear message,
// but the real guarantee is the composite foreign key in the schema, which
// rejects an admin account no matter which code path writes the row.
func (s *Server) handleSaveMembership(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	accountID, err := pathID(c, "accountID")
	if err != nil {
		return err
	}

	var request membershipRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	isGM := request.IsGM != nil && *request.IsGM
	isActive := request.IsActive == nil || *request.IsActive

	ctx := c.Request().Context()
	if _, err := s.db.GameByID(ctx, gameID); err != nil {
		return s.storeError(err, "game")
	}
	account, err := s.db.AccountByID(ctx, accountID)
	if err != nil {
		return s.storeError(err, "account")
	}
	if account.IsAdmin() {
		return conflict("Administrator accounts cannot be assigned to a game.")
	}

	membership, err := s.db.UpsertMembership(ctx, gameID, accountID, isGM, isActive, store.Now())
	if err != nil {
		return s.storeError(err, "membership")
	}
	s.log.Info("membership saved",
		"game_id", gameID, "account_id", accountID, "is_gm", isGM, "is_active", isActive,
		"actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusOK, envelope{Data: newMembershipView(membership)})
}
