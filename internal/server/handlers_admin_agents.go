// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/engine"
	"github.com/mdhender/ecv8-api/internal/store"
)

// Agents are seats a game master fills with engine-played opponents. They are
// not accounts, cannot sign in, and exist only inside a game.
//
// The catalogue of what may be seated comes from internal/engine, never from
// the database. Which agents exist is a property of this binary, so a build
// that no longer has an implementation stops offering it — and a seat that
// names a missing one is reported as unplayable rather than failing later,
// when a turn is resolved.

// handleListAgents returns every agent this build can play.
//
// It reads nothing from the database. There is no per-game filtering because
// the answer does not depend on a game: it is what this binary is capable of.
func (s *Server) handleListAgents(c *echo.Context) error {
	catalogue := engine.Agents()
	views := make([]agentView, 0, len(catalogue))
	for _, descriptor := range catalogue {
		views = append(views, newAgentView(descriptor))
	}
	return c.JSON(http.StatusOK, envelope{Data: views})
}

// handleListAgentSeats returns the agents seated in one game.
func (s *Server) handleListAgentSeats(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	if _, err := s.db.GameByID(ctx, gameID); err != nil {
		return s.storeError(err, "game")
	}
	seats, err := s.db.ListAgentSeats(ctx, gameID)
	if err != nil {
		return err
	}

	views := make([]agentSeatView, 0, len(seats))
	for i := range seats {
		views = append(views, newAgentSeatView(&seats[i]))
	}
	return c.JSON(http.StatusOK, envelope{Data: views})
}

// createAgentSeatRequest is the body of
// POST /api/v1/admin/games/:gameID/agents.
type createAgentSeatRequest struct {
	AgentKey  string  `json:"agent_key"`
	AgentName *string `json:"agent_name"`
	IsActive  *bool   `json:"is_active"`
}

// handleCreateAgentSeat seats an agent in a game.
//
// POST rather than PUT, because the seat is not identified by its path: several
// agents may play one game, and seating the same implementation twice is a
// legitimate act that produces two players. A PUT would imply the opposite.
//
// The key is validated against the engine's catalogue here, which is the only
// place that can answer it — the schema constrains the *format* of a key and
// deliberately not the set of valid ones, because that set changes with every
// release and enumerating it in a CHECK would mean a migration per agent.
//
// It refuses an inactive game: seating an opponent in a game nobody is playing
// is a mistake worth reporting rather than recording.
func (s *Server) handleCreateAgentSeat(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}

	var request createAgentSeatRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}

	key := strings.ToLower(strings.TrimSpace(request.AgentKey))
	if key == "" {
		return unprocessable("An agent is required.",
			FieldError{Field: "agent_key", Message: "Choose one of: " + strings.Join(engine.AgentKeys(), ", ")})
	}
	descriptor, ok := engine.AgentByKey(key)
	if !ok {
		// The valid keys are listed because this build is the authority on
		// them, and a caller working from an older catalogue has no other way
		// to discover that one was withdrawn.
		return unprocessable("That agent is not available in this build.",
			FieldError{Field: "agent_key", Message: "Choose one of: " + strings.Join(engine.AgentKeys(), ", ")})
	}

	// The label defaults to the agent's own name, so the common case needs no
	// body field, and a game master who wants two distinguishable copies of one
	// agent can name them.
	name := descriptor.Name
	if request.AgentName != nil {
		name = strings.TrimSpace(*request.AgentName)
	}
	if name == "" || len([]rune(name)) > 100 {
		return unprocessable("The agent name is invalid.",
			FieldError{Field: "agent_name", Message: "Provide 1 to 100 characters, or omit it to use the agent's own name."})
	}
	isActive := request.IsActive == nil || *request.IsActive

	ctx := c.Request().Context()
	game, err := s.db.GameByID(ctx, gameID)
	if err != nil {
		return s.storeError(err, "game")
	}
	if !game.IsActive {
		return conflict("Agents cannot be added to an inactive game.")
	}

	seat, err := s.db.CreateAgentSeat(ctx, gameID, key, name, isActive, store.Now())
	if err != nil {
		return s.storeError(err, "agent seat")
	}
	// player_id is logged because it is what the engine will refer to, so an
	// operator reading engine output can trace a player back to the act that
	// created it.
	s.log.Info("agent seated",
		"game_id", gameID, "player_id", seat.ID, "agent_key", key, "is_active", isActive,
		"actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusCreated, envelope{Data: newAgentSeatView(seat)})
}

// updateAgentSeatRequest is the body of
// PATCH /api/v1/admin/games/:gameID/agents/:playerID.
type updateAgentSeatRequest struct {
	AgentName *string `json:"agent_name"`
	IsActive  *bool   `json:"is_active"`
}

// handleUpdateAgentSeat renames a seated agent or deactivates it.
//
// agent_key is not updatable and is not in the request body. The key is the
// identity the engine dispatches on and is written into a game's state, so
// changing it would hand a faction to different code mid-game. Retiring an
// agent means deactivating this seat and adding another.
//
// There is no DELETE, for the reason nothing else here has one: history and
// referential integrity survive deactivation and do not survive removal. A
// faction's controller must remain resolvable after the agent stops playing.
func (s *Server) handleUpdateAgentSeat(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	playerID, err := pathID(c, "playerID")
	if err != nil {
		return err
	}

	var request updateAgentSeatRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}
	if request.AgentName == nil && request.IsActive == nil {
		return unprocessable("Provide at least one field to update.")
	}

	ctx := c.Request().Context()
	seat, err := s.db.AgentSeatByID(ctx, gameID, playerID)
	if err != nil {
		return s.storeError(err, "agent seat")
	}

	name := seat.AgentName
	if request.AgentName != nil {
		name = strings.TrimSpace(*request.AgentName)
		if name == "" || len([]rune(name)) > 100 {
			return unprocessable("The agent name is invalid.",
				FieldError{Field: "agent_name", Message: "Provide 1 to 100 characters."})
		}
	}
	isActive := seat.IsActive
	if request.IsActive != nil {
		isActive = *request.IsActive
	}

	updated, err := s.db.UpdateAgentSeat(ctx, gameID, playerID, name, isActive, store.Now())
	if err != nil {
		return s.storeError(err, "agent seat")
	}
	s.log.Info("agent seat updated",
		"game_id", gameID, "player_id", playerID, "is_active", isActive,
		"actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusOK, envelope{Data: newAgentSeatView(updated)})
}
