// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/engine"
	"github.com/mdhender/ecv8-api/internal/store"
)

// Games as the people seated at them see them, rather than as an administrator
// managing them does.
//
// Everything here authorises on the **seat**, not on the application role. An
// administrator can never hold a seat — the composite foreign key in migration
// 0001 forbids it — so an administrator is not a player and these endpoints are
// not open to one. That is not an oversight to patch with a role check: admin
// rights stop at impersonation everywhere else in this service, and an
// administrator who needs to see a game the way a player does impersonates
// them, which is exactly the mechanism that exists for it.

// playerSeat resolves the caller's seat at a game.
//
// A caller with no seat is told the game does not exist rather than that they
// may not see it. "There is a game here you are not in" is itself information
// about a game they are not in, and it is the only thing an unseated caller
// could learn by probing ids.
//
// An inactive seat is treated the same way: it is how a player is removed from
// a game, and a removed player should not still be able to read it. An inactive
// *game* is not — it stays visible to the people who played it, carrying
// is_active so the page can say so.
func (s *Server) playerSeat(c *echo.Context, gameID int64) (*store.Game, *store.Membership, error) {
	ctx := c.Request().Context()
	id := identityOf(c)

	seat, err := s.db.MembershipByID(ctx, gameID, id.Effective.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, notFound("No such game.")
		}
		return nil, nil, err
	}
	if !seat.IsActive {
		return nil, nil, notFound("No such game.")
	}

	game, err := s.db.GameByID(ctx, gameID)
	if err != nil {
		return nil, nil, s.storeError(err, "game")
	}
	return game, seat, nil
}

// gameMasterSeat is playerSeat, narrowed to the person who runs the game.
//
// Running a game is a property of the seat, not of the account: the same person
// is a game master in one game and a player in another, so there is nothing to
// check on the account and nothing a role could tell us.
func (s *Server) gameMasterSeat(c *echo.Context, gameID int64) (*store.Game, *store.Membership, error) {
	game, seat, err := s.playerSeat(c, gameID)
	if err != nil {
		return nil, nil, err
	}
	if !seat.IsGM {
		return nil, nil, forbidden("Only this game's game master can do that.")
	}
	return game, seat, nil
}

// handleGetPlayerGame returns one game, its state, and what this caller may do
// about it.
//
// A game with no state is not an error: a game exists from the moment an
// administrator creates it and is set up separately by its game master. The
// response says so with a null state, and carries the seed the setup form
// starts from when — and only when — this caller is the one who can submit it.
//
// The seat also decides whether the state's seed travels at all. Every other
// field here is the same for everybody at the table; the seed is the game
// master's alone, because the engine is reproducible from it and a player
// holding it could resolve a turn before the game does.
func (s *Server) handleGetPlayerGame(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	game, seat, err := s.playerSeat(c, gameID)
	if err != nil {
		return err
	}

	view := playerGameView{
		ID:       game.ID,
		Name:     game.Name,
		IsActive: game.IsActive,
		IsGM:     seat.IsGM,
	}

	state, err := s.db.GameStateByGameID(c.Request().Context(), gameID)
	switch {
	case err == nil:
		rendered := newGameStateView(state, seat.IsGM)
		view.State = &rendered
	case errors.Is(err, store.ErrNotFound):
		if seat.IsGM {
			seed := newSeedView(engine.DefaultSeed())
			view.DefaultSeed = &seed
		}
	default:
		return err
	}

	return c.JSON(http.StatusOK, envelope{Data: view})
}

// seedRequest is one seed inside a request body. Its field names match
// seedView's, so a client can send back what it was given without translating.
type seedRequest struct {
	Hi string `json:"hi"`
	Lo string `json:"lo"`
}

// createGameStateRequest is the body of POST /api/v1/games/:gameID/state.
//
// seed is optional, and omitting it is the ordinary case: a game master with no
// reason to choose a stream should not have to invent two numbers to begin.
// The default comes from engine.DefaultSeed rather than from a constant here,
// so the value this endpoint writes and the value the GET offers as a starting
// point can never drift apart.
type createGameStateRequest struct {
	Seed *seedRequest `json:"seed"`
}

// seedRuleMessage is the whole rule a seed word has to satisfy, written for
// whoever is filling in the form.
const seedRuleMessage = "must be a whole number from 0 to 18446744073709551615"

// parseSeedWord reads one decimal seed word from the wire.
//
// It is a string rather than a number for the reason seedView explains: the
// upper half of the uint64 range does not survive a browser's JSON number.
// ParseUint with a bit size of 64 is therefore the whole validation — it
// rejects a negative sign, a fraction, and anything that would overflow.
func parseSeedWord(raw string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
}

// handleCreateGameState sets a game up by writing its initial state at turn 0.
//
// Only the game's own game master may. It is POST and not PUT because it
// creates the one state a game ever has: the seed is what makes every later
// turn reproducible, so replacing it after play has begun would invalidate the
// turns already resolved. A second call is therefore a 409, not an overwrite.
//
// An inactive game is refused for the same reason seating an agent in one is:
// setting up a game nobody is playing is a mistake worth reporting rather than
// recording.
func (s *Server) handleCreateGameState(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	game, _, err := s.gameMasterSeat(c, gameID)
	if err != nil {
		return err
	}

	var request createGameStateRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}

	seed := engine.DefaultSeed()
	if request.Seed != nil {
		var fields []FieldError
		hi, hiErr := parseSeedWord(request.Seed.Hi)
		if hiErr != nil {
			fields = append(fields, FieldError{Field: "seed.hi", Message: seedRuleMessage})
		}
		lo, loErr := parseSeedWord(request.Seed.Lo)
		if loErr != nil {
			fields = append(fields, FieldError{Field: "seed.lo", Message: seedRuleMessage})
		}
		if len(fields) > 0 {
			return unprocessable("The seed is invalid.", fields...)
		}
		seed = engine.Seed{Hi: hi, Lo: lo}
	}

	if !game.IsActive {
		return conflict("An inactive game cannot be set up.")
	}

	state, err := s.db.CreateGameState(c.Request().Context(), gameID, seed.Hi, seed.Lo, store.Now())
	if err != nil {
		return s.storeError(err, "game state")
	}
	// The seed is logged deliberately. It is not a credential — internal/tokens
	// mints everything that is — and it is the one value an operator needs in
	// order to replay a turn and check what the engine did. That it is withheld
	// from players on the wire is a different rule: reproducibility is for the
	// people auditing the game, and the log is theirs, not a player's.
	s.log.Info("game set up",
		"game_id", gameID, "turn", state.Turn,
		"seed_hi", state.SeedHi, "seed_lo", state.SeedLo,
		"actor_id", identityOf(c).Actor.ID)

	// gameMasterSeat is the only way in, so the seed goes back: the game master
	// chose it, or accepted the default, and needs to be able to record it.
	return c.JSON(http.StatusCreated, envelope{Data: newGameStateView(state, true)})
}
