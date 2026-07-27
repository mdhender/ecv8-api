-- Copyright (c) 2026 Michael D Henderson. All rights reserved.
--
-- Migration 0004: a game's map — the cluster it was generated with, and the
-- stelliums that generation produced.
--
-- This is the engine domain, and it obeys that domain's rules: nothing here
-- references account, game_id is the only application-domain identity that
-- crosses, and a delete that should never happen is refused rather than
-- cascaded. 0002_engine.sql explains the seam at length.
--
-- # Why the parameters are stored and not just the result
--
-- The 100 stelliums are the answer; generator_key, stellium_count, and radius
-- are the question. Keeping the question is what makes the answer checkable: a
-- cluster is drawn from the game's seed through prng.TagCluster, so those three
-- values plus game_state's two seed words reproduce the map exactly. Without
-- them a map is a pile of coordinates nobody can verify, and "which generator
-- drew this?" becomes unanswerable the moment a second one exists.
--
-- generator_key is constrained by FORMAT and never by the set of valid keys,
-- for the reason 0003_agent_key.sql sets out for agents: which generators exist
-- is a property of the binary, the catalogue lives in internal/engine, and a
-- table of valid keys populated by a migration would advertise generators the
-- running code may not have.
--
-- # Why one cluster per game
--
-- game_id is the primary key rather than a plain reference, exactly as it is on
-- game_state, so "a game has at most one cluster" is something the schema knows
-- instead of something a handler remembers. A second generation is refused by
-- this constraint. That is the intended behaviour and not a limitation to work
-- around: the map is the ground every later turn is resolved on, so replacing
-- it after a game has begun would invalidate everything played on the old one.
--
-- The bounds below restate the constants in internal/engine/generators. The
-- duplication is deliberate — a database outlives any one binary and is read by
-- more than this one — but they must be changed together, and widening the Go
-- side alone will surface here as a constraint failure rather than silently.

CREATE TABLE cluster (
    game_id        INTEGER PRIMARY KEY REFERENCES game (id) ON DELETE CASCADE,

    -- Which implementation drew this map. See above: format only.
    generator_key  TEXT    NOT NULL,

    -- What was asked for. The number of stellium rows should equal
    -- stellium_count; SQLite cannot express that, and the generator guarantees
    -- it, so this records the request rather than restating the result.
    stellium_count INTEGER NOT NULL,
    radius         INTEGER NOT NULL,

    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,

    CONSTRAINT cluster_generator_key_format
        CHECK (generator_key = lower(generator_key)
               AND generator_key = trim(generator_key)
               AND length(generator_key) BETWEEN 1 AND 40),
    CONSTRAINT cluster_radius_range
        CHECK (radius BETWEEN 3 AND 1024),
    CONSTRAINT cluster_stellium_count_range
        CHECK (stellium_count BETWEEN 1 AND 10000)
);

-- One gravitationally associated group of systems, at integer map coordinates.
--
-- The reference identifies a stellium by its integer id and treats (x, y, z) as
-- a display form, so id is what everything else will point at. It is
-- AUTOINCREMENT for the reason game_player.id is: the value ends up in engine
-- state, and SQLite reusing it would silently move a stellium's contents to a
-- different place on the map.
--
-- Coordinates are addresses, not just data. prng addresses a stellium's own
-- draws by (x, y, z) precisely so that what is in a stellium does not depend on
-- the order rows were written, which is why the uniqueness below is a schema
-- guarantee rather than a promise from the generator: two stelliums sharing a
-- coordinate would share every draw addressed to it.
--
-- The range check is the absolute bound any radius allows, not this cluster's
-- radius — a CHECK cannot read the parent row. It is here to catch a coordinate
-- that is wrong by orders of magnitude, which is the mistake worth catching.
--
-- The reference to cluster rather than to game is what makes a stellium without
-- a cluster impossible, and ON DELETE RESTRICT is this domain's rule: deleting
-- a game's map is not an operation this application has, so a delete that
-- reaches here fails loudly instead of quietly emptying a game.
CREATE TABLE stellium (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    game_id    INTEGER NOT NULL REFERENCES cluster (game_id) ON DELETE RESTRICT,
    x          INTEGER NOT NULL,
    y          INTEGER NOT NULL,
    z          INTEGER NOT NULL,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,

    CONSTRAINT stellium_coordinates_range
        CHECK (x BETWEEN -1024 AND 1024
               AND y BETWEEN -1024 AND 1024
               AND z BETWEEN -1024 AND 1024)
);

CREATE UNIQUE INDEX stellium_game_coords_key ON stellium (game_id, x, y, z);
