// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const gameColumns = `id, name, is_active, created_at, updated_at`

// scanGame reads one row of gameColumns.
func scanGame(stmt *sqlite.Stmt) (Game, error) {
	game := Game{
		ID:       stmt.GetInt64("id"),
		Name:     stmt.GetText("name"),
		IsActive: stmt.GetInt64("is_active") != 0,
	}
	var err error
	if game.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
		return Game{}, err
	}
	if game.UpdatedAt, err = parseTime(stmt.GetText("updated_at")); err != nil {
		return Game{}, err
	}
	return game, nil
}

// GameFilter narrows a game listing.
type GameFilter struct {
	Query  string
	Active *bool
}

// ListGames returns one page of games ordered by name.
func (db *DB) ListGames(ctx context.Context, filter GameFilter, page Page) ([]Game, Page, error) {
	where := []string{"1 = 1"}
	args := map[string]any{}
	if q := strings.TrimSpace(filter.Query); q != "" {
		where = append(where, "name LIKE :q ESCAPE '\\'")
		args[":q"] = "%" + escapeLike(q) + "%"
	}
	if filter.Active != nil {
		where = append(where, "is_active = :is_active")
		args[":is_active"] = boolToInt(*filter.Active)
	}
	clause := strings.Join(where, " AND ")

	games := make([]Game, 0, page.Size)
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		countArgs := maps.Clone(args)
		if err := sqlitex.Execute(conn,
			`SELECT count(*) AS total FROM game WHERE `+clause+`;`,
			&sqlitex.ExecOptions{
				Named: countArgs,
				ResultFunc: func(stmt *sqlite.Stmt) error {
					page.Total = int(stmt.GetInt64("total"))
					return nil
				},
			}); err != nil {
			return err
		}

		pageArgs := maps.Clone(args)
		pageArgs[":limit"] = int64(page.Size)
		pageArgs[":offset"] = int64(page.Offset())

		return sqlitex.Execute(conn,
			`SELECT `+gameColumns+` FROM game WHERE `+clause+`
			 ORDER BY name LIMIT :limit OFFSET :offset;`,
			&sqlitex.ExecOptions{
				Named: pageArgs,
				ResultFunc: func(stmt *sqlite.Stmt) error {
					game, err := scanGame(stmt)
					if err != nil {
						return err
					}
					games = append(games, game)
					return nil
				},
			})
	})
	if err != nil {
		return nil, page, fmt.Errorf("list games: %w", err)
	}
	page.Entries = len(games)
	return games, page, nil
}

// GameByID returns one game, or ErrNotFound.
func (db *DB) GameByID(ctx context.Context, id int64) (*Game, error) {
	var found *Game
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+gameColumns+` FROM game WHERE id = :id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":id": id},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					game, err := scanGame(stmt)
					if err != nil {
						return err
					}
					found = &game
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load game %d: %w", id, err)
	}
	if found == nil {
		return nil, fmt.Errorf("game %d: %w", id, ErrNotFound)
	}
	return found, nil
}

// CreateGame adds a game.
func (db *DB) CreateGame(ctx context.Context, name string, now time.Time) (*Game, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, fmt.Errorf("%w: name must be 1 to 100 bytes", ErrConflict)
	}
	var id int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `
			INSERT INTO game (name, is_active, created_at, updated_at)
			VALUES (:name, 1, :now, :now);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":name": name, ":now": formatTime(now)},
			}); err != nil {
			return err
		}
		id = conn.LastInsertRowID()
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: a game with that name already exists", ErrConflict)
		}
		return nil, fmt.Errorf("create game: %w", err)
	}
	return db.GameByID(ctx, id)
}

// GameUpdate holds the editable fields of a game. A nil pointer leaves that
// field unchanged.
type GameUpdate struct {
	Name     *string
	IsActive *bool
}

// UpdateGame renames or deactivates a game. Games are never deleted.
func (db *DB) UpdateGame(ctx context.Context, id int64, update GameUpdate, now time.Time) (*Game, error) {
	sets := []string{"updated_at = :now"}
	args := map[string]any{":id": id, ":now": formatTime(now)}

	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if name == "" || len(name) > 100 {
			return nil, fmt.Errorf("%w: name must be 1 to 100 bytes", ErrConflict)
		}
		sets = append(sets, "name = :name")
		args[":name"] = name
	}
	if update.IsActive != nil {
		sets = append(sets, "is_active = :is_active")
		args[":is_active"] = boolToInt(*update.IsActive)
	}

	var changed int
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn,
			`UPDATE game SET `+strings.Join(sets, ", ")+` WHERE id = :id;`,
			&sqlitex.ExecOptions{Named: args}); err != nil {
			return err
		}
		changed = conn.Changes()
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: a game with that name already exists", ErrConflict)
		}
		return nil, fmt.Errorf("update game %d: %w", id, err)
	}
	if changed == 0 {
		return nil, fmt.Errorf("game %d: %w", id, ErrNotFound)
	}
	return db.GameByID(ctx, id)
}

const membershipColumns = `
	r.id           AS id,
	r.game_id      AS game_id,
	r.account_id   AS account_id,
	r.is_gm        AS is_gm,
	r.is_active    AS is_active,
	r.created_at   AS created_at,
	r.updated_at   AS updated_at,
	a.email        AS email,
	a.display_name AS display_name,
	g.name         AS game_name`

// scanMembership reads one row of membershipColumns.
func scanMembership(stmt *sqlite.Stmt) (Membership, error) {
	membership := Membership{
		ID:          stmt.GetInt64("id"),
		GameID:      stmt.GetInt64("game_id"),
		AccountID:   stmt.GetInt64("account_id"),
		Email:       stmt.GetText("email"),
		DisplayName: stmt.GetText("display_name"),
		GameName:    stmt.GetText("game_name"),
		IsGM:        stmt.GetInt64("is_gm") != 0,
		IsActive:    stmt.GetInt64("is_active") != 0,
	}
	var err error
	if membership.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
		return Membership{}, err
	}
	if membership.UpdatedAt, err = parseTime(stmt.GetText("updated_at")); err != nil {
		return Membership{}, err
	}
	return membership, nil
}

// ListMemberships returns every human membership of a game, ordered by email.
//
// Agent seats are excluded explicitly rather than left to the join. They have no
// account_id, so the join against account already drops them, but a membership
// listing that silently depended on that would break the moment the join
// changed. Agents are the engine's players and belong to a listing of their own.
func (db *DB) ListMemberships(ctx context.Context, gameID int64) ([]Membership, error) {
	memberships := make([]Membership, 0, 16)
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+membershipColumns+`
			   FROM game_player r
			   JOIN account a ON a.id = r.account_id
			   JOIN game    g ON g.id = r.game_id
			  WHERE r.game_id = :game_id AND r.is_agent = 0
			  ORDER BY a.email;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					membership, err := scanMembership(stmt)
					if err != nil {
						return err
					}
					memberships = append(memberships, membership)
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("list memberships for game %d: %w", gameID, err)
	}
	return memberships, nil
}

// ListMembershipsForAccount returns every game an account belongs to. The user
// dashboard uses it.
func (db *DB) ListMembershipsForAccount(ctx context.Context, accountID int64, activeOnly bool) ([]Membership, error) {
	clause := ""
	if activeOnly {
		clause = " AND r.is_active = 1 AND g.is_active = 1"
	}
	memberships := make([]Membership, 0, 8)
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+membershipColumns+`
			   FROM game_player r
			   JOIN account a ON a.id = r.account_id
			   JOIN game    g ON g.id = r.game_id
			  WHERE r.account_id = :account_id`+clause+`
			  ORDER BY g.name;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":account_id": accountID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					membership, err := scanMembership(stmt)
					if err != nil {
						return err
					}
					memberships = append(memberships, membership)
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("list memberships for account %d: %w", accountID, err)
	}
	return memberships, nil
}

// MembershipByID returns one membership, or ErrNotFound.
func (db *DB) MembershipByID(ctx context.Context, gameID, accountID int64) (*Membership, error) {
	var found *Membership
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+membershipColumns+`
			   FROM game_player r
			   JOIN account a ON a.id = r.account_id
			   JOIN game    g ON g.id = r.game_id
			  WHERE r.game_id = :game_id AND r.account_id = :account_id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":game_id": gameID, ":account_id": accountID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					membership, err := scanMembership(stmt)
					if err != nil {
						return err
					}
					found = &membership
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load membership: %w", err)
	}
	if found == nil {
		return nil, fmt.Errorf("membership: %w", ErrNotFound)
	}
	return found, nil
}

// CountActiveGameMasters reports how many active game masters a game has.
// Callers use it to refuse a change that would leave a game nobody can run,
// which is the same job CountActiveAdmins does for the service as a whole.
//
// is_agent = 0 is stated rather than relied upon. An agent can never be a game
// master — game_player_agent_not_gm forbids it — so the filter changes no
// answer today, and saying it means this count does not quietly start including
// agents if that ever changes.
func (db *DB) CountActiveGameMasters(ctx context.Context, gameID int64) (int, error) {
	var count int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT count(*) AS total FROM game_player
			  WHERE game_id = :game_id AND is_gm = 1 AND is_active = 1 AND is_agent = 0;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					count = int(stmt.GetInt64("total"))
					return nil
				},
			})
	})
	if err != nil {
		return 0, fmt.Errorf("count active game masters for game %d: %w", gameID, err)
	}
	return count, nil
}

// MembershipBySeatID returns one membership by its seat id, or ErrNotFound.
//
// The game is part of the lookup for the same reason it is in AgentSeatByID: a
// seat id from one game must never reach a seat in another, or a path-scoped
// endpoint is lying about what it operates on. is_agent = 0 is explicit rather
// than left to the join, so a seat id naming an agent is "not found" here
// instead of whatever the join happens to do with a NULL account.
func (db *DB) MembershipBySeatID(ctx context.Context, gameID, seatID int64) (*Membership, error) {
	var found *Membership
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+membershipColumns+`
			   FROM game_player r
			   JOIN account a ON a.id = r.account_id
			   JOIN game    g ON g.id = r.game_id
			  WHERE r.id = :id AND r.game_id = :game_id AND r.is_agent = 0;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":id": seatID, ":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					membership, err := scanMembership(stmt)
					if err != nil {
						return err
					}
					found = &membership
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load membership seat: %w", err)
	}
	if found == nil {
		return nil, fmt.Errorf("membership: %w", ErrNotFound)
	}
	return found, nil
}

// CreateMembership seats an account in a game, and fails if it already has a
// seat there.
//
// This is deliberately not UpsertMembership. An administrator saving a
// membership form is stating what the seat should be, so replacing an existing
// row is right; a game master adding someone to their game is stating that this
// person is not in it yet, and being wrong about that should be reported rather
// than quietly rewriting a seat they were not looking at. The unique index on
// (game_id, account_id) is what decides, so two simultaneous adds cannot both
// win.
//
// The role is hard-coded to 'user' and is_agent to 0 for the reasons
// UpsertMembership gives: the composite foreign key rejects an administrator
// whatever this code believes, and saying "human seat" out loud means the row is
// refused rather than reinterpreted if a default ever changes.
func (db *DB) CreateMembership(ctx context.Context, gameID, accountID int64, isGM bool, now time.Time) (*Membership, error) {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			INSERT INTO game_player (game_id, account_id, account_role, is_agent, is_gm, is_active, created_at, updated_at)
			VALUES (:game_id, :account_id, 'user', 0, :is_gm, 1, :now, :now);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":game_id":    gameID,
					":account_id": accountID,
					":is_gm":      boolToInt(isGM),
					":now":        formatTime(now),
				},
			})
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: that account is already in this game", ErrConflict)
		}
		if isConstraintViolation(err) {
			return nil, fmt.Errorf("%w: the game must exist and the account must exist with the %q role",
				ErrConflict, RoleUser)
		}
		return nil, fmt.Errorf("create membership: %w", err)
	}
	return db.MembershipByID(ctx, gameID, accountID)
}

// UpsertMembership adds or replaces an account's membership in a game.
//
// The composite foreign key on (account_id, account_role) means SQLite itself
// rejects an admin account here, whatever this code does. That is why the
// insert hard-codes 'user' rather than reading the account's role: a race that
// promoted the account to admin between check and write would still fail.
//
// is_agent is hard-coded to 0 for the same reason it is written at all: this
// creates human seats, and saying so means the row is rejected rather than
// quietly reinterpreted if the column's default ever changes.
func (db *DB) UpsertMembership(ctx context.Context, gameID, accountID int64, isGM, isActive bool, now time.Time) (*Membership, error) {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			INSERT INTO game_player (game_id, account_id, account_role, is_agent, is_gm, is_active, created_at, updated_at)
			VALUES (:game_id, :account_id, 'user', 0, :is_gm, :is_active, :now, :now)
			ON CONFLICT (game_id, account_id) DO UPDATE
			   SET is_gm = excluded.is_gm,
			       is_active = excluded.is_active,
			       updated_at = excluded.updated_at;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":game_id":    gameID,
					":account_id": accountID,
					":is_gm":      boolToInt(isGM),
					":is_active":  boolToInt(isActive),
					":now":        formatTime(now),
				},
			})
	})
	if err != nil {
		if isConstraintViolation(err) {
			return nil, fmt.Errorf("%w: the game must exist and the account must exist with the %q role",
				ErrConflict, RoleUser)
		}
		return nil, fmt.Errorf("save membership: %w", err)
	}
	return db.MembershipByID(ctx, gameID, accountID)
}
