// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package engine is the foundation of the ECV8 game engine.
//
// It exists to establish and enforce one invariant that every rule must obey:
// all game randomness is drawn from math/rand/v2 with a PCG source, seeded
// reproducibly, so that a turn can be replayed and audited.
//
// The legacy math/rand package must never be imported anywhere in this module.
// Security material (session tokens, activation links) must come from
// internal/tokens, which uses crypto/rand; this PRNG is reproducible by design
// and is unsuitable for secrets.
//
// # What the engine knows
//
// The engine's state lives in the database's engine domain — game_state,
// faction, entity — and it is deliberately reachable without knowing that
// accounts exist. A faction is controlled by a player_id, which is a seat at a
// game's table (game_player.id), never an account id. Whether that seat is a
// person or an agent the engine plays itself is one boolean on the seat.
//
// Keep it that way. The engine may read game_id and player_id and nothing else
// from the application domain; internal/store/migrations/0002_engine.sql
// explains the seam and the foreign keys that hold it in place.
//
// The seeds a game is built from are stored as two SQLite INTEGERs, which are
// signed, and are uint64 here. The conversion is a bit reinterpretation and
// loses nothing, but it belongs in internal/store — not in this package and not
// in a handler.
package engine

import (
	"math/rand/v2"
)

// Seed identifies a reproducible engine stream. Two engines built from the same
// Seed produce the same sequence, which is what makes turn resolution auditable
// and replayable.
type Seed struct {
	Hi uint64
	Lo uint64
}

// Engine is the root of a game's deterministic state. It currently carries only
// its random source.
type Engine struct {
	seed Seed
	rng  *rand.Rand
}

// New returns an Engine whose random source is PCG, seeded from seed. PCG is
// the default and only source: callers cannot substitute another generator,
// which keeps every game reproducible from its seed alone.
func New(seed Seed) *Engine {
	return &Engine{
		seed: seed,
		rng:  rand.New(rand.NewPCG(seed.Hi, seed.Lo)),
	}
}

// Seed returns the seed this Engine was built from.
func (e *Engine) Seed() Seed { return e.seed }

// Rand returns the engine's PCG-backed generator. Every rule that needs
// randomness must draw from this generator so that a turn can be replayed.
func (e *Engine) Rand() *rand.Rand { return e.rng }
