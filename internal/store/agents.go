// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"fmt"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Agent seats live in game_player alongside human ones, because the engine
// must not care which kind a player_id refers to. What separates them is
// is_agent, and every query here filters on it explicitly rather than relying
// on the absence of an account to do it — an agent seat has no account_id, so
// a join would exclude it by accident, and a rule that holds by accident stops
// holding when the join changes.

const agentSeatColumns = `
	r.id         AS id,
	r.game_id    AS game_id,
	r.agent_key  AS agent_key,
	r.agent_name AS agent_name,
	r.is_active  AS is_active,
	r.created_at AS created_at,
	r.updated_at AS updated_at,
	g.name       AS game_name`

// scanAgentSeat reads one row of agentSeatColumns.
func scanAgentSeat(stmt *sqlite.Stmt) (AgentSeat, error) {
	seat := AgentSeat{
		ID:        stmt.GetInt64("id"),
		GameID:    stmt.GetInt64("game_id"),
		GameName:  stmt.GetText("game_name"),
		AgentKey:  stmt.GetText("agent_key"),
		AgentName: stmt.GetText("agent_name"),
		IsActive:  stmt.GetInt64("is_active") != 0,
	}
	var err error
	if seat.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
		return AgentSeat{}, err
	}
	if seat.UpdatedAt, err = parseTime(stmt.GetText("updated_at")); err != nil {
		return AgentSeat{}, err
	}
	return seat, nil
}

// ListAgentSeats returns every agent seated in a game, oldest first.
//
// The order is by id rather than by name, so a listing matches the order the
// seats were added and the player_ids the engine will see.
func (db *DB) ListAgentSeats(ctx context.Context, gameID int64) ([]AgentSeat, error) {
	seats := make([]AgentSeat, 0, 8)
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+agentSeatColumns+`
			   FROM game_player r
			   JOIN game g ON g.id = r.game_id
			  WHERE r.game_id = :game_id AND r.is_agent = 1
			  ORDER BY r.id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					seat, err := scanAgentSeat(stmt)
					if err != nil {
						return err
					}
					seats = append(seats, seat)
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("list agent seats for game %d: %w", gameID, err)
	}
	return seats, nil
}

// AgentSeatByID returns one agent seat, or ErrNotFound.
//
// The game is part of the lookup so that a seat id from one game can never be
// used to reach a seat in another, which is what keeps a path-scoped endpoint
// honest.
func (db *DB) AgentSeatByID(ctx context.Context, gameID, seatID int64) (*AgentSeat, error) {
	var found *AgentSeat
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+agentSeatColumns+`
			   FROM game_player r
			   JOIN game g ON g.id = r.game_id
			  WHERE r.id = :id AND r.game_id = :game_id AND r.is_agent = 1;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":id": seatID, ":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					seat, err := scanAgentSeat(stmt)
					if err != nil {
						return err
					}
					found = &seat
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load agent seat: %w", err)
	}
	if found == nil {
		return nil, fmt.Errorf("agent seat: %w", ErrNotFound)
	}
	return found, nil
}

// CreateAgentSeat seats an agent in a game and returns the new seat.
//
// It always inserts. Unlike a membership there is nothing to conflict with —
// several agents may sit in one game, and the unique index tolerates that
// because account_id is NULL and SQLite treats NULLs as distinct — so seating
// the same agent twice is a deliberate act that produces two players, not an
// accident that overwrites one.
//
// agentKey is written as given. Whether this build can play it is a question
// for internal/engine, and the caller must have asked before reaching here; the
// store's job is to record what it was told.
func (db *DB) CreateAgentSeat(ctx context.Context, gameID int64, agentKey, agentName string, isActive bool, now time.Time) (*AgentSeat, error) {
	var seatID int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `
			INSERT INTO game_player (game_id, account_id, account_role,
			                         is_agent, agent_key, agent_name,
			                         is_gm, is_active, created_at, updated_at)
			VALUES (:game_id, NULL, 'user',
			        1, :agent_key, :agent_name,
			        0, :is_active, :now, :now);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":game_id":    gameID,
					":agent_key":  agentKey,
					":agent_name": agentName,
					":is_active":  boolToInt(isActive),
					":now":        formatTime(now),
				},
			}); err != nil {
			return err
		}
		seatID = conn.LastInsertRowID()
		return nil
	})
	if err != nil {
		if isConstraintViolation(err) {
			return nil, fmt.Errorf("%w: the game must exist", ErrConflict)
		}
		return nil, fmt.Errorf("seat agent: %w", err)
	}
	return db.AgentSeatByID(ctx, gameID, seatID)
}

// UpdateAgentSeat changes what can be changed about a seated agent: its label
// and whether it is active.
//
// agent_key is not among them. The key is the identity the engine dispatches
// on and is written into a game's state, so changing it would silently hand a
// faction to different code mid-game. Retiring an agent is deactivating its
// seat and adding another.
func (db *DB) UpdateAgentSeat(ctx context.Context, gameID, seatID int64, agentName string, isActive bool, now time.Time) (*AgentSeat, error) {
	var changed int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `
			UPDATE game_player
			   SET agent_name = :agent_name,
			       is_active  = :is_active,
			       updated_at = :now
			 WHERE id = :id AND game_id = :game_id AND is_agent = 1;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":id":         seatID,
					":game_id":    gameID,
					":agent_name": agentName,
					":is_active":  boolToInt(isActive),
					":now":        formatTime(now),
				},
			}); err != nil {
			return err
		}
		changed = int64(conn.Changes())
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("update agent seat %d: %w", seatID, err)
	}
	if changed == 0 {
		return nil, fmt.Errorf("agent seat %d: %w", seatID, ErrNotFound)
	}
	return db.AgentSeatByID(ctx, gameID, seatID)
}
