-- Copyright (c) 2026 Michael D Henderson. All rights reserved.
--
-- Migration 0003: name an agent seat's implementation, instead of trusting a
-- free-text label.
--
-- 0002 gave an agent seat only agent_name, which is a display string. Nothing
-- tied a seat to the code that plays it, so two seats could claim the same bot
-- and a typo produced an agent nothing could resolve. agent_key is the stable
-- identifier the engine dispatches on; agent_name stays as the label a GM sees.
--
-- # Why the valid keys are not in the database
--
-- Which agents exist is a property of the *binary*, not of the file. The
-- catalogue lives in internal/engine and is served from there, so adding an
-- agent is one commit — write the code, add it to the registry — with no second
-- artefact to forget.
--
-- The rejected alternative was an `agent` table populated by a migration
-- whenever agent code was committed. Migrations are forward-only here, so a
-- rollback would leave the database advertising an agent the running binary
-- cannot play, and a seat referencing it would fail at turn resolution rather
-- than at deploy. A foreign key onto such a table looks like integrity but its
-- parent row is only an assertion that some code exists; SQLite cannot check
-- the thing that actually matters. The registry can, and does, at seat time.
--
-- So the CHECK below constrains the *format* of a key and never the set of
-- valid keys. Format is stable; membership changes with every release, and
-- enumerating it here would reintroduce the per-agent migration by another
-- name.
--
-- # Why this is ADD COLUMN and not a table rebuild
--
-- faction references game_player through two composite foreign keys. Dropping
-- and recreating a parent table with children pointing at it needs
-- PRAGMA foreign_keys = OFF, which is a no-op inside a transaction — and
-- sqlitemigration wraps this file in one. ADD COLUMN accepts a CHECK that
-- reads another column, which is enough to express the whole rule, so no
-- rebuild is needed.
--
-- SQLite *does* evaluate the new CHECK against the rows already in the table,
-- so this migration fails — loudly, and with the transaction rolled back — on a
-- database that already holds an agent seat, because such a seat has no key and
-- cannot satisfy the constraint.
--
-- That is the right outcome and not a case worth working around. No endpoint has
-- ever created an agent seat: 0002 added the columns and nothing else, so the
-- only way to have one is to have inserted it by hand. There is also no correct
-- automatic backfill, because nothing in the database records which
-- implementation a keyless seat was meant to be — picking one would be inventing
-- an answer.
--
-- The remedy is to remove such a seat before upgrading, not to give it a key:
-- the column does not exist yet, so there is nothing to set. A faction it
-- controls must go first, because faction's foreign key is ON DELETE RESTRICT.
--
--	DELETE FROM entity  WHERE faction_id IN (SELECT id FROM faction
--	                    WHERE player_id IN (SELECT id FROM game_player WHERE is_agent = 1));
--	DELETE FROM faction WHERE player_id  IN (SELECT id FROM game_player WHERE is_agent = 1);
--	DELETE FROM game_player WHERE is_agent = 1;

ALTER TABLE game_player
    ADD COLUMN agent_key TEXT
        CONSTRAINT game_player_agent_key_kind
        CHECK ((is_agent = 0 AND agent_key IS NULL)
               OR (is_agent = 1
                   AND agent_key IS NOT NULL
                   AND agent_key = lower(agent_key)
                   AND agent_key = trim(agent_key)
                   AND length(agent_key) BETWEEN 1 AND 40));
