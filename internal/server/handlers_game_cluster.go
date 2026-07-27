// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/engine"
	"github.com/mdhender/ecv8-api/internal/engine/generators"
	"github.com/mdhender/ecv8-api/internal/store"
)

// A game's cluster is its map. Reading it is anybody's at the table; making one
// is the game master's job in the same way setting the game up is. So the two
// endpoints here resolve different seats — playerSeat for the GET, and
// gameMasterSeat for the POST — and the seat decides, exactly as it does
// everywhere under /games. An account with no seat gets 404 from both.
//
// **The map is not the seed, and is not withheld the way the seed is.** A seed
// makes the engine reproducible, so a player holding one can resolve events
// before the game does; a coordinate list says nothing about the future. The
// reference has players reading stellium coordinates off their turn reports,
// and space being a known shape is what makes a course worth plotting.
//
// What is *in* a stellium is a different question, and it is not on the wire
// here because nothing stores it yet. When systems, planets, and deposits
// arrive, whether a player sees the ones they have not surveyed is a rule about
// those tables, not about this one — do not read the openness of the coordinate
// list as having settled it.

// clusterOptions is the starting point and the bounds the generate form works
// inside, taken from the engine so that the values offered and the values
// accepted come from one place.
func clusterOptions() clusterOptionsView {
	catalogue := engine.Generators()
	view := clusterOptionsView{
		Generators:       make([]clusterGeneratorView, 0, len(catalogue)),
		Generator:        engine.DefaultGeneratorKey,
		StelliumCount:    generators.DefaultStelliumCount,
		Radius:           generators.DefaultRadius,
		MinStelliumCount: generators.MinStelliumCount,
		MaxStelliumCount: generators.MaxStelliumCount,
		MinRadius:        generators.MinRadius,
		MaxRadius:        generators.MaxRadius,
	}
	for _, descriptor := range catalogue {
		view.Generators = append(view.Generators, newClusterGeneratorView(descriptor))
	}
	return view
}

// handleGetGameCluster returns a game's map to anybody seated at the game, and
// to a game master additionally what it would take to make one.
//
// A game with no cluster is not an error, and neither is a game that has not
// been set up: they are the two stages before a map exists, and a page shows
// something different for each. The options for the form travel only with a
// caller who could actually submit it — the game master of a game that is
// active, set up, and has no cluster yet. A player never gets them however
// ready the game is, so the form is not something a client has to decide to
// hide; there is nothing to render.
//
// is_gm travels for the same reason it does on a game: it is what lets a page
// say "you have not generated this yet" to one reader and "it has not been
// generated yet" to another, without either of them working out whose page it
// is from the shape of the rest of the answer.
func (s *Server) handleGetGameCluster(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	game, seat, err := s.playerSeat(c, gameID)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	view := gameClusterView{
		GameID:   game.ID,
		GameName: game.Name,
		IsActive: game.IsActive,
		IsGM:     seat.IsGM,
	}

	switch _, err := s.db.GameStateByGameID(ctx, gameID); {
	case err == nil:
		view.IsSetUp = true
	case errors.Is(err, store.ErrNotFound):
		view.IsSetUp = false
	default:
		return err
	}

	cluster, err := s.db.ClusterByGameID(ctx, gameID)
	switch {
	case err == nil:
		stelliums, err := s.db.StelliumsByGameID(ctx, gameID)
		if err != nil {
			return err
		}
		rendered := newClusterView(cluster, stelliums)
		view.Cluster = &rendered
	case errors.Is(err, store.ErrNotFound):
		if seat.IsGM && view.IsSetUp && game.IsActive {
			options := clusterOptions()
			view.Options = &options
		}
	default:
		return err
	}

	return c.JSON(http.StatusOK, envelope{Data: view})
}

// createClusterRequest is the body of POST /api/v1/games/:gameID/cluster.
//
// Every field is a pointer, and every one is optional. A game master with no
// opinion about the shape of their map should be able to ask for one without
// filling anything in, and the defaults they get are the engine's — the same
// ones GET offers — rather than a second set written here. Pointers rather than
// zero values because 0 is outside every range below, so an omitted field and a
// rejected one have to be distinguishable.
type createClusterRequest struct {
	Generator     *string `json:"generator"`
	StelliumCount *int    `json:"stellium_count"`
	Radius        *int    `json:"radius"`
}

// handleCreateGameCluster generates a game's map and stores it.
//
// It is POST and not PUT because it creates the one cluster a game ever has. A
// second call is 409 rather than a regeneration: every turn is resolved on the
// map, so replacing it after play has begun would invalidate what has already
// happened — the same rule, for the same reason, as the seed a game is set up
// with.
//
// The order of the checks is deliberate. Everything that can be decided from
// the request alone is decided first and reported against the field that is
// wrong, so a game master correcting a form is told about all of it at once.
// The state of the game comes next, because those are conditions to wait for
// rather than mistakes in the request. Generating happens last, since it is the
// only expensive step.
func (s *Server) handleCreateGameCluster(c *echo.Context) error {
	gameID, err := pathID(c, "gameID")
	if err != nil {
		return err
	}
	game, _, err := s.gameMasterSeat(c, gameID)
	if err != nil {
		return err
	}

	var request createClusterRequest
	if err := s.bindJSON(c, &request); err != nil {
		return err
	}

	generatorKey := engine.DefaultGeneratorKey
	stelliumCount := generators.DefaultStelliumCount
	radius := generators.DefaultRadius

	var fields []FieldError
	if request.Generator != nil {
		generatorKey = *request.Generator
		// Resolved against the catalogue rather than a format check, because the
		// question is whether this build can run it — which only the engine can
		// answer, and the schema deliberately does not.
		if _, ok := engine.GeneratorByKey(generatorKey); !ok {
			fields = append(fields, FieldError{
				Field:   "generator",
				Message: fmt.Sprintf("must be one of %v", engine.GeneratorKeys()),
			})
		}
	}
	if request.StelliumCount != nil {
		stelliumCount = *request.StelliumCount
		if stelliumCount < generators.MinStelliumCount || stelliumCount > generators.MaxStelliumCount {
			fields = append(fields, FieldError{
				Field: "stellium_count",
				Message: fmt.Sprintf("must be a whole number from %d to %d",
					generators.MinStelliumCount, generators.MaxStelliumCount),
			})
		}
	}
	if request.Radius != nil {
		radius = *request.Radius
		if radius < generators.MinRadius || radius > generators.MaxRadius {
			fields = append(fields, FieldError{
				Field: "radius",
				Message: fmt.Sprintf("must be a whole number from %d to %d",
					generators.MinRadius, generators.MaxRadius),
			})
		}
	}
	if len(fields) > 0 {
		return unprocessable("The cluster settings are invalid.", fields...)
	}

	if !game.IsActive {
		return conflict("An inactive game cannot be given a cluster.")
	}

	// The seed is what the map is drawn from, so a game that has not been set up
	// has nothing to draw with. This is 409 and not 422: nothing in the request
	// is wrong, and the answer changes as soon as the game master sets the game
	// up on the page they came from.
	state, err := s.db.GameStateByGameID(c.Request().Context(), gameID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return conflict("Set the game up before generating its cluster.")
		}
		return err
	}

	// Checked here so an accidental repeat is refused before a hundred draws are
	// made. It is not the guarantee — the primary key on cluster.game_id is, and
	// CreateCluster reports it — because two requests arriving together would
	// both pass this.
	if _, err := s.db.ClusterByGameID(c.Request().Context(), gameID); err == nil {
		return conflict("This game already has a cluster.")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	cluster, err := engine.GenerateCluster(
		engine.Seed{Hi: state.SeedHi, Lo: state.SeedLo}, generatorKey, stelliumCount, radius)
	if err != nil {
		// The one failure the checks above cannot predict: how many distinct
		// coordinates a sphere holds is a property of the generator's rounding,
		// so only generating can find out. It is reported against radius because
		// raising it is the fix a game master has.
		if errors.Is(err, generators.ErrCrowded) {
			return unprocessable("The cluster settings are invalid.", FieldError{
				Field:   "radius",
				Message: fmt.Sprintf("is too small to hold %d stelliums", stelliumCount),
			})
		}
		return err
	}

	coordinates := make([]store.Coordinate, 0, len(cluster.Stelliums))
	for _, stellium := range cluster.Stelliums {
		coordinates = append(coordinates, store.Coordinate{
			X: stellium.Coords.X,
			Y: stellium.Coords.Y,
			Z: stellium.Coords.Z,
		})
	}

	stored, err := s.db.CreateCluster(c.Request().Context(), gameID, store.NewCluster{
		GeneratorKey:  generatorKey,
		StelliumCount: stelliumCount,
		Radius:        radius,
		Coordinates:   coordinates,
	}, store.Now())
	if err != nil {
		return s.storeError(err, "cluster")
	}

	stelliums, err := s.db.StelliumsByGameID(c.Request().Context(), gameID)
	if err != nil {
		return err
	}

	// Logged for the operator who has to answer "where did this map come from?".
	// The parameters and the game's seed are together the whole answer, and
	// unlike the seed none of this is withheld from anybody.
	s.log.Info("cluster generated",
		"game_id", gameID, "generator", generatorKey,
		"stellium_count", stelliumCount, "radius", radius,
		"actor_id", identityOf(c).Actor.ID)

	return c.JSON(http.StatusCreated, envelope{Data: newClusterView(stored, stelliums)})
}
