// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/store"
)

// The roster of a game, as its game master manages it.
//
// One rule shapes every handler here: **a game master may change player seats,
// and a game master's seat is an administrator's business.** Promoting a player
// is therefore allowed and demoting anyone is not, because a demotion would be
// a change to a GM seat; deactivating a player is allowed and deactivating any
// game master — including oneself — is not, for the same reason.
//
// Stating it as one rule about the *target* rather than three rules about three
// operations is what keeps it honest. A rule per operation would have to be
// repeated correctly each time a fourth operation arrived, and the first one
// written from memory would be the one that let a game master quietly demote
// the person who invited them, or leave a game with nobody able to run it.
//
// An administrator is not bound by any of it: PUT /admin/games/{id}/memberships
// still sets any seat to any state, which is the escape hatch that makes the
// restriction here safe to be strict.

// handleListPlayers returns the human roster of a game.
//
// Inactive seats are included. They are how somebody is removed from a game —
// nothing here is deleted — so a roster that hid them would leave a game master
// unable to see, or undo, a removal they had just made.
//
// Agent seats are not here. They are the engine's players and have a listing of
// their own; mixing them in would put a bot in a list of people.
func (s *Server) handleListPlayers(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	if _, _, err := s.gameMasterSeat(c, gameID); err != nil {
		return err
	}

	players, err := s.db.ListMemberships(c.Request().Context(), gameID)
	if err != nil {
		return err
	}
	views := make([]membershipView, 0, len(players))
	for i := range players {
		views = append(views, newMembershipView(&players[i]))
	}
	return c.JSON(http.StatusOK, envelope{Data: views})
}

// addPlayerRequest is the body of POST /api/v1/games/:gameID/players.
//
// The account is named by email rather than by id, because a game master has no
// way to learn an id: listing accounts is an administrator's endpoint and must
// stay that way. Inviting someone you already know the address of needs no
// directory, and a directory is exactly what a game master should not be handed.
type addPlayerRequest struct {
	Email string `json:"email"`
	IsGM  *bool  `json:"is_gm"`
}

// handleAddPlayer seats an account in the game as a player or a game master.
//
// Every reason an account cannot be seated answers with the same message. An
// address that belongs to nobody, to an administrator, or to a deactivated
// account are all "no account here can join a game", because the alternative is
// an endpoint that reports which addresses exist to anyone who runs a game.
// What the game master needs to know — that this address will not work — is the
// same in each case.
func (s *Server) handleAddPlayer(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	game, _, err := s.gameMasterSeat(c, gameID)
	if err != nil {
		return err
	}

	var request addPlayerRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if strings.TrimSpace(request.Email) == "" {
		return unprocessable("An email address is required.",
			FieldError{Field: "email", Message: "is required"})
	}
	if !game.IsActive {
		return conflict("Players cannot be added to an inactive game.")
	}

	ctx := c.Request().Context()
	account, err := s.db.AccountByEmail(ctx, request.Email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if account == nil || !account.IsActive || account.IsAdmin() {
		return unprocessable("That account cannot join a game.",
			FieldError{Field: "email", Message: "no active player account has this address"})
	}

	isGM := request.IsGM != nil && *request.IsGM
	seat, err := s.db.CreateMembership(ctx, gameID, account.ID, isGM, store.Now())
	if err != nil {
		return s.storeError(err, "membership")
	}
	s.log.Info("player seated by game master",
		"game_id", gameID, "player_id", seat.ID, "account_id", account.ID, "is_gm", isGM,
		"actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusCreated, envelope{Data: newMembershipView(seat)})
}

// updatePlayerRequest is the body of
// PATCH /api/v1/games/:gameID/players/:playerID.
//
// is_gm accepts only true. There is no field a game master could set to undo a
// promotion, which is the point: see handleUpdatePlayer.
type updatePlayerRequest struct {
	IsGM     *bool `json:"is_gm"`
	IsActive *bool `json:"is_active"`
}

// handleUpdatePlayer promotes a player to game master, or activates and
// deactivates one.
//
// The target must be a player. A seat that is already a game master's is
// refused whatever the request asks of it, which is what makes promotion
// one-way and what stops a game master deactivating a colleague — or
// themselves, leaving a game nobody can run. Undoing either is an
// administrator's job.
//
// is_gm: false is refused rather than ignored. Silently accepting it would let
// a client believe a demotion had happened, and the next roster load would be
// the only hint that it had not.
func (s *Server) handleUpdatePlayer(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	playerID, err := pathID(c, "playerID")
	if err != nil {
		return err
	}
	if _, _, err := s.gameMasterSeat(c, gameID); err != nil {
		return err
	}

	var request updatePlayerRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.IsGM == nil && request.IsActive == nil {
		return unprocessable("Provide at least one field to update.")
	}
	if request.IsGM != nil && !*request.IsGM {
		return forbidden("A game master cannot be demoted. An administrator can change that seat.")
	}

	ctx := c.Request().Context()
	seat, err := s.db.MembershipBySeatID(ctx, gameID, playerID)
	if err != nil {
		return s.storeError(err, "player")
	}
	if seat.IsGM {
		return forbidden("A game master's seat can only be changed by an administrator.")
	}

	isGM := request.IsGM != nil && *request.IsGM
	isActive := seat.IsActive
	if request.IsActive != nil {
		isActive = *request.IsActive
	}

	updated, err := s.db.UpsertMembership(ctx, gameID, seat.AccountID, isGM, isActive, store.Now())
	if err != nil {
		return s.storeError(err, "player")
	}
	s.log.Info("player updated by game master",
		"game_id", gameID, "player_id", playerID, "is_gm", isGM, "is_active", isActive,
		"actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusOK, envelope{Data: newMembershipView(updated)})
}
