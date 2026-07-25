// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"fmt"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// A game's state is the engine domain's entry point, and it is written exactly
// once. Everything else in this file follows from that: there is a read and an
// insert here and no update, because the seed is what makes a game replayable
// and replacing it after a turn has been resolved would invalidate every turn
// already played. Advancing a game changes turn, not the seed, and that belongs
// to whatever resolves a turn rather than here.

const gameStateColumns = `game_id, turn, seed_hi, seed_lo, created_at, updated_at`

// scanGameState reads one row of gameStateColumns.
func scanGameState(stmt *sqlite.Stmt) (GameState, error) {
	state := GameState{
		GameID: stmt.GetInt64("game_id"),
		Turn:   int(stmt.GetInt64("turn")),
		SeedHi: parseSeed(stmt.GetInt64("seed_hi")),
		SeedLo: parseSeed(stmt.GetInt64("seed_lo")),
	}
	var err error
	if state.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
		return GameState{}, err
	}
	if state.UpdatedAt, err = parseTime(stmt.GetText("updated_at")); err != nil {
		return GameState{}, err
	}
	return state, nil
}

// GameStateByGameID returns a game's state, or ErrNotFound when it has none.
//
// "Not found" here means the game has not been set up yet, which is an ordinary
// stage of a game's life and not a failure. Callers are expected to tell the
// two apart with errors.Is rather than treating an empty state as a zero one: a
// zero seed is a legitimate seed, so a zero value could not be distinguished
// from a game that was deliberately started with one.
func (db *DB) GameStateByGameID(ctx context.Context, gameID int64) (*GameState, error) {
	var found *GameState
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+gameStateColumns+` FROM game_state WHERE game_id = :game_id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					state, err := scanGameState(stmt)
					if err != nil {
						return err
					}
					found = &state
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load state for game %d: %w", gameID, err)
	}
	if found == nil {
		return nil, fmt.Errorf("state for game %d: %w", gameID, ErrNotFound)
	}
	return found, nil
}

// CreateGameState writes a game's initial state at turn 0.
//
// It only ever inserts. game_state.game_id is the primary key, so a second call
// fails on that constraint rather than overwriting the seed a game has already
// been played under — an accidental repeat is reported instead of silently
// making the game unreplayable.
//
// The seed words arrive as uint64 and are stored as the signed integers with
// the same bits. Nothing outside formatSeed does that conversion.
func (db *DB) CreateGameState(ctx context.Context, gameID int64, seedHi, seedLo uint64, now time.Time) (*GameState, error) {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			INSERT INTO game_state (game_id, turn, seed_hi, seed_lo, created_at, updated_at)
			VALUES (:game_id, 0, :seed_hi, :seed_lo, :now, :now);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":game_id": gameID,
					":seed_hi": formatSeed(seedHi),
					":seed_lo": formatSeed(seedLo),
					":now":     formatTime(now),
				},
			})
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: this game has already been set up", ErrConflict)
		}
		if isConstraintViolation(err) {
			return nil, fmt.Errorf("%w: the game must exist", ErrConflict)
		}
		return nil, fmt.Errorf("create state for game %d: %w", gameID, err)
	}
	return db.GameStateByGameID(ctx, gameID)
}
