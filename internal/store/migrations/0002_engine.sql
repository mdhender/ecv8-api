-- Copyright (c) 2026 Michael D Henderson. All rights reserved.
--
-- Migration 0002: the engine domain, and the seat table that bridges into it.
--
-- Migration 0001 is entirely the application domain: who may sign in, and what
-- games exist. This migration adds the engine domain — what is happening inside
-- a game — and the two are kept apart on purpose.
--
-- The seam is one column. game_player.id is the engine's player_id, and it is
-- the only identity that crosses: no engine table references account, and the
-- engine never needs to know that accounts exist. game_id crosses too, because
-- every engine row has to be scoped to a game and there is no way around that.
--
-- They share one SQLite file, so the separation is a discipline rather than a
-- boundary the storage enforces — and the composite foreign keys below are the
-- compensation. Each one makes a rule that would otherwise live in Go a fact
-- the database checks: a faction's player is seated in the same game, that
-- player is not the game master, an entity's faction is in the same game.
--
-- sqlitemigration wraps this script in a transaction and bumps user_version on
-- success, so this file must not contain BEGIN/COMMIT of its own. PRAGMA
-- foreign_keys is ON (see prepareConn) and cannot be changed inside a
-- transaction, so nothing here tries: the table rebuild below is safe with
-- enforcement on because no table references game_account_role.

-- ----------------------------------------------------------------- the seam

-- Seats at a game's table, replacing game_account_role.
--
-- The rename is not cosmetic. A seat is now held by a human *or* by an agent,
-- and an agent has no account at all, so "account_role" no longer describes
-- what a row is. The table had to be rebuilt regardless — SQLite cannot add a
-- PRIMARY KEY column with ALTER TABLE — so the better name came for free.
--
-- id is AUTOINCREMENT, which in SQLite means monotonically increasing and never
-- reused, even after a row is deleted. That is a requirement rather than a
-- preference: this id is the engine's player_id, it is written into engine
-- state, and a reused value would silently reassign a faction to whoever sat
-- down next.
--
-- Agents are seats, not accounts. The alternative — an account row whose
-- password hash is an invalid bcrypt string — would make "this bot cannot sign
-- in" a guarantee held up by a third-party parser's error path, and would need
-- a fabricated email and activation timestamp to satisfy 0001's CHECKs. Here it
-- is a schema fact: an agent has no account_id, so there is nothing to sign in
-- as, and account still means exactly "a human who can sign in".
CREATE TABLE game_player (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    game_id      INTEGER NOT NULL REFERENCES game (id) ON DELETE CASCADE,

    -- NULL for an agent seat.
    account_id   INTEGER,
    -- Redundant with account.role, exactly as it was in game_account_role, so
    -- that the composite foreign key below can keep admins out of games.
    account_role TEXT    NOT NULL DEFAULT 'user',

    -- Explicit rather than derived from "account_id IS NULL", because the
    -- engine has to ask "do I play this seat myself?" and must be able to
    -- answer it without reading a column from the application domain.
    is_agent     INTEGER NOT NULL DEFAULT 0,
    agent_name   TEXT,

    -- A game master is technically a player, and never controls a faction.
    -- faction enforces that; see below.
    is_gm        INTEGER NOT NULL DEFAULT 0,
    is_active    INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,

    CONSTRAINT game_player_is_agent_boolean
        CHECK (is_agent IN (0, 1)),
    CONSTRAINT game_player_is_gm_boolean
        CHECK (is_gm IN (0, 1)),
    CONSTRAINT game_player_is_active_boolean
        CHECK (is_active IN (0, 1)),
    CONSTRAINT game_player_players_only
        CHECK (account_role = 'user'),

    -- A human seat has an account and no agent name; an agent seat is the
    -- reverse. is_agent can never disagree with account_id.
    CONSTRAINT game_player_seat_kind
        CHECK ((is_agent = 0 AND account_id IS NOT NULL AND agent_name IS NULL)
               OR (is_agent = 1 AND account_id IS NULL
                                AND agent_name IS NOT NULL
                                AND agent_name = trim(agent_name)
                                AND length(agent_name) BETWEEN 1 AND 100)),

    -- The engine plays agents. It does not run games.
    CONSTRAINT game_player_agent_not_gm
        CHECK (is_agent = 0 OR is_gm = 0),

    -- An admin account can never hold a seat, whatever code writes the row, and
    -- ON UPDATE RESTRICT blocks promoting a seated account to admin. A NULL
    -- account_id satisfies this vacuously: SQLite imposes no parent-key
    -- requirement when any column of a composite foreign key is NULL, which is
    -- what lets an agent seat share the table with a human one.
    FOREIGN KEY (account_id, account_role)
        REFERENCES account (id, role)
        ON UPDATE RESTRICT
        ON DELETE CASCADE
);

-- One seat per account per game. NULLs are distinct in a SQLite unique index,
-- so this constrains humans and leaves the number of agents in a game open.
CREATE UNIQUE INDEX game_player_game_account_key ON game_player (game_id, account_id);
-- Lookups by account alone ("which games is this person in?") cannot use the
-- index above, because game_id is its leading column.
CREATE INDEX game_player_account_idx ON game_player (account_id);

-- Composite parent keys. Neither is a lookup index: they exist so that
-- faction's foreign key is legal, which is what turns "same game" and "not the
-- GM" into database guarantees.
CREATE UNIQUE INDEX game_player_id_game_key ON game_player (id, game_id);
CREATE UNIQUE INDEX game_player_id_game_gm_key ON game_player (id, game_id, is_gm);

-- Every existing membership is a human seat. Ordered so the ids assigned here
-- are reproducible rather than dependent on the old table's page layout.
INSERT INTO game_player (game_id, account_id, account_role,
                         is_agent, agent_name, is_gm, is_active,
                         created_at, updated_at)
SELECT game_id, account_id, account_role,
       0, NULL, is_gm, is_active,
       created_at, updated_at
  FROM game_account_role
 ORDER BY game_id, account_id;

DROP TABLE game_account_role;

-- ------------------------------------------------------------ engine domain

-- One row per game: where that game has got to.
--
-- game_id is the primary key rather than a plain reference, which makes "a game
-- has exactly one state" something the schema knows instead of something the
-- code remembers. Turn 0 is setup.
--
-- The two seeds are the PCG state internal/engine is built from, and they are
-- uint64 in Go. SQLite's INTEGER is signed 64-bit, so a seed at or above 2^63
-- is stored as a negative number. The round trip uint64 -> int64 -> uint64 is a
-- bit reinterpretation and loses nothing, but a seed read in a SQLite shell
-- will sometimes look like a negative value, and that is not corruption. The
-- conversion belongs in one pair of helpers in internal/store, alongside
-- formatTime and parseTime, so that no handler and no engine code ever casts.
CREATE TABLE game_state (
    game_id    INTEGER PRIMARY KEY REFERENCES game (id) ON DELETE CASCADE,
    turn       INTEGER NOT NULL DEFAULT 0,
    seed_hi    INTEGER NOT NULL,
    seed_lo    INTEGER NOT NULL,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,

    CONSTRAINT game_state_turn_non_negative
        CHECK (turn >= 0)
);

-- A faction is what a player or an agent actually controls.
--
-- player_id is NOT NULL because a faction must always have a controller: an
-- ownerless faction is not a state this game has.
--
-- player_is_gm is redundant with game_player.is_gm on purpose, in the same
-- idiom 0001 used to keep admins out of games. With the CHECK and the composite
-- foreign key it makes "a game master never controls a faction" a database
-- guarantee rather than a rule every write path has to remember, and ON UPDATE
-- RESTRICT additionally blocks promoting a player to GM while they still
-- control one. The cost is that a column of application-domain data lives in an
-- engine table; the guarantee is worth it.
--
-- ON DELETE RESTRICT, not CASCADE, and deliberately against the surrounding
-- style: nothing in this application is deleted, only deactivated, so a delete
-- reaching here is a mistake. Refusing it loudly beats silently taking a game's
-- state with it.
CREATE TABLE faction (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    game_id      INTEGER NOT NULL,
    player_id    INTEGER NOT NULL,
    player_is_gm INTEGER NOT NULL DEFAULT 0,
    name         TEXT    NOT NULL,
    is_active    INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,

    CONSTRAINT faction_name_length
        CHECK (length(name) BETWEEN 1 AND 100 AND name = trim(name)),
    CONSTRAINT faction_is_active_boolean
        CHECK (is_active IN (0, 1)),
    CONSTRAINT faction_not_run_by_gm
        CHECK (player_is_gm = 0),

    FOREIGN KEY (player_id, game_id, player_is_gm)
        REFERENCES game_player (id, game_id, is_gm)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT
);

CREATE UNIQUE INDEX faction_game_name_key ON faction (game_id, name);
CREATE INDEX faction_player_idx ON faction (player_id);

-- Composite parent key, so entity can prove its faction is in the same game.
CREATE UNIQUE INDEX faction_id_game_key ON faction (id, game_id);

-- A thing a faction owns: a ship or a colony today, more later.
--
-- This is deliberately a stub. The engine is being built now, and inventing
-- columns for rules that do not exist yet would be inventing the rules. kind
-- and name are what every entity has regardless of what it turns out to be;
-- everything else arrives with the rule that needs it.
--
-- faction_id is NOT NULL: every entity is controlled by a faction, and there
-- are no unowned or neutral entities in this model.
CREATE TABLE entity (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    game_id    INTEGER NOT NULL,
    faction_id INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,

    CONSTRAINT entity_kind_valid
        CHECK (kind IN ('ship', 'colony')),
    CONSTRAINT entity_name_length
        CHECK (length(name) BETWEEN 1 AND 100 AND name = trim(name)),

    FOREIGN KEY (faction_id, game_id)
        REFERENCES faction (id, game_id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT
);

CREATE INDEX entity_game_faction_idx ON entity (game_id, faction_id);
