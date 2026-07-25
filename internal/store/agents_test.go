// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// These tests run against a real migrated database rather than a stub, because
// most of what is worth protecting here is enforced by SQLite: an agent seat
// has no account, several may share a game, a game master may not control a
// faction. A fake store would assert only that this package calls itself
// correctly.
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// testDB opens an in-memory database of its own, named for the test so no two
// share one, and closes it when the test ends.
func testDB(t *testing.T) *DB {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, t.Name())

	db, err := OpenTemporaryStore(context.Background(), name)
	if err != nil {
		t.Fatalf("open temporary store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return db
}

// testGame creates a game to seat agents in.
func testGame(t *testing.T, db *DB, name string) *Game {
	t.Helper()
	game, err := db.CreateGame(context.Background(), name, Now())
	if err != nil {
		t.Fatalf("create game %q: %v", name, err)
	}
	return game
}

func TestCreateAgentSeat(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Seating")

	seat, err := db.CreateAgentSeat(ctx, game.ID, "passive", "Passive", true, Now())
	if err != nil {
		t.Fatalf("seat agent: %v", err)
	}
	if seat.ID == 0 {
		t.Error("seat has no id; player_id is what the engine refers to")
	}
	if seat.AgentKey != "passive" {
		t.Errorf("agent_key = %q, want %q", seat.AgentKey, "passive")
	}
	if seat.AgentName != "Passive" {
		t.Errorf("agent_name = %q, want %q", seat.AgentName, "Passive")
	}
	if !seat.IsActive {
		t.Error("seat is inactive, want active")
	}
	if seat.GameName != game.Name {
		t.Errorf("game_name = %q, want %q", seat.GameName, game.Name)
	}
}

// An agent seat is not a membership. If it ever appeared as one, the roster
// would show a bot as a person and a GM would have no way to tell them apart.
func TestAgentSeatIsNotAMembership(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Separation")

	if _, err := db.CreateAgentSeat(ctx, game.ID, "passive", "Passive", true, Now()); err != nil {
		t.Fatalf("seat agent: %v", err)
	}
	memberships, err := db.ListMemberships(ctx, game.ID)
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships) != 0 {
		t.Errorf("agent seat appeared in the roster: %+v", memberships)
	}
}

// Several agents may play one game. The unique index tolerates it only because
// account_id is NULL and SQLite treats NULLs as distinct, so this is worth
// asserting: a well-meaning index change would break it silently.
func TestSeveralAgentsMayShareAGame(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Crowded")

	first, err := db.CreateAgentSeat(ctx, game.ID, "passive", "Passive North", true, Now())
	if err != nil {
		t.Fatalf("seat first agent: %v", err)
	}
	second, err := db.CreateAgentSeat(ctx, game.ID, "passive", "Passive South", true, Now())
	if err != nil {
		t.Fatalf("seat second agent: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("both seats have player_id %d; seating twice must produce two players", first.ID)
	}

	seats, err := db.ListAgentSeats(ctx, game.ID)
	if err != nil {
		t.Fatalf("list agent seats: %v", err)
	}
	if len(seats) != 2 {
		t.Fatalf("listed %d seats, want 2", len(seats))
	}
	// Oldest first, so a listing matches the player_ids the engine will see.
	if seats[0].ID > seats[1].ID {
		t.Errorf("seats are out of order: %d before %d", seats[0].ID, seats[1].ID)
	}
}

// A seat id is meaningless without its game. If one game could reach another's
// seat, every path-scoped endpoint would be lying about what it operates on.
func TestAgentSeatIsScopedToItsGame(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	mine := testGame(t, db, "Mine")
	yours := testGame(t, db, "Yours")

	seat, err := db.CreateAgentSeat(ctx, mine.ID, "passive", "Passive", true, Now())
	if err != nil {
		t.Fatalf("seat agent: %v", err)
	}

	if _, err := db.AgentSeatByID(ctx, yours.ID, seat.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("AgentSeatByID across games: err = %v, want ErrNotFound", err)
	}
	if _, err := db.UpdateAgentSeat(ctx, yours.ID, seat.ID, "Renamed", true, Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateAgentSeat across games: err = %v, want ErrNotFound", err)
	}

	// And the seat is untouched by either attempt.
	after, err := db.AgentSeatByID(ctx, mine.ID, seat.ID)
	if err != nil {
		t.Fatalf("reload seat: %v", err)
	}
	if after.AgentName != "Passive" {
		t.Errorf("agent_name = %q after a cross-game update, want %q", after.AgentName, "Passive")
	}
}

func TestUpdateAgentSeat(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Updating")

	seat, err := db.CreateAgentSeat(ctx, game.ID, "passive", "Passive", true, Now())
	if err != nil {
		t.Fatalf("seat agent: %v", err)
	}

	updated, err := db.UpdateAgentSeat(ctx, game.ID, seat.ID, "Retired", false, Now())
	if err != nil {
		t.Fatalf("update seat: %v", err)
	}
	if updated.AgentName != "Retired" {
		t.Errorf("agent_name = %q, want %q", updated.AgentName, "Retired")
	}
	if updated.IsActive {
		t.Error("seat is active, want deactivated")
	}
	// The key is the identity the engine dispatches on; an update must never
	// move a seat onto different code.
	if updated.AgentKey != "passive" {
		t.Errorf("agent_key = %q after update, want %q", updated.AgentKey, "passive")
	}
	if updated.ID != seat.ID {
		t.Errorf("player_id changed from %d to %d", seat.ID, updated.ID)
	}
}

func TestAgentSeatNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Missing")

	if _, err := db.AgentSeatByID(ctx, game.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("AgentSeatByID: err = %v, want ErrNotFound", err)
	}
	if _, err := db.UpdateAgentSeat(ctx, game.ID, 99, "X", true, Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateAgentSeat: err = %v, want ErrNotFound", err)
	}
}

// A human membership is not reachable through the agent methods, which is the
// other half of the separation: is_agent is filtered explicitly, not left to
// the absence of an account to imply.
func TestMembershipIsNotAnAgentSeat(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Humans")

	account, err := db.AccountByEmail(ctx, "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	membership, err := db.UpsertMembership(ctx, game.ID, account.ID, false, true, Now())
	if err != nil {
		t.Fatalf("save membership: %v", err)
	}

	if _, err := db.AgentSeatByID(ctx, game.ID, membership.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("AgentSeatByID on a human seat: err = %v, want ErrNotFound", err)
	}
	seats, err := db.ListAgentSeats(ctx, game.ID)
	if err != nil {
		t.Fatalf("list agent seats: %v", err)
	}
	if len(seats) != 0 {
		t.Errorf("human seat appeared as an agent: %+v", seats)
	}
}

// Human and agent seats draw player_id from one sequence, because the engine
// must not care which kind it is dealing with.
func TestSeatsShareOnePlayerIDSpace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Shared")

	account, err := db.AccountByEmail(ctx, "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	human, err := db.UpsertMembership(ctx, game.ID, account.ID, false, true, Now())
	if err != nil {
		t.Fatalf("save membership: %v", err)
	}
	agent, err := db.CreateAgentSeat(ctx, game.ID, "passive", "Passive", true, Now())
	if err != nil {
		t.Fatalf("seat agent: %v", err)
	}
	if human.ID == agent.ID {
		t.Errorf("a human seat and an agent seat share player_id %d", human.ID)
	}
}

// The remaining tests reach the schema directly. They are the reason these run
// against a real database: each asserts a rule that must hold no matter which
// code path writes the row, so the only honest way to test it is to try the
// write and require SQLite to refuse.

// insertSeat performs a raw insert, returning the error SQLite gave.
func insertSeat(t *testing.T, db *DB, columns string, values map[string]any) error {
	t.Helper()
	return db.Write(context.Background(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, columns, &sqlitex.ExecOptions{Named: values})
	})
}

func TestSchemaRefusesAgentWithoutKey(t *testing.T) {
	db := testDB(t)
	game := testGame(t, db, "NoKey")

	err := insertSeat(t, db, `
		INSERT INTO game_player (game_id, account_id, account_role, is_agent,
		                         agent_key, agent_name, is_gm, is_active,
		                         created_at, updated_at)
		VALUES (:game_id, NULL, 'user', 1, NULL, 'Keyless', 0, 1, :now, :now);`,
		map[string]any{":game_id": game.ID, ":now": formatTime(Now())})
	if err == nil {
		t.Fatal("an agent seat with no agent_key was accepted")
	}
	if !isConstraintViolation(err) {
		t.Errorf("err = %v, want a constraint violation", err)
	}
}

func TestSchemaRefusesMalformedAgentKey(t *testing.T) {
	db := testDB(t)
	game := testGame(t, db, "BadKey")

	for _, key := range []string{"UPPERCASE", " leading", "trailing ", strings.Repeat("x", 41)} {
		err := insertSeat(t, db, `
			INSERT INTO game_player (game_id, account_id, account_role, is_agent,
			                         agent_key, agent_name, is_gm, is_active,
			                         created_at, updated_at)
			VALUES (:game_id, NULL, 'user', 1, :key, 'Bad', 0, 1, :now, :now);`,
			map[string]any{":game_id": game.ID, ":key": key, ":now": formatTime(Now())})
		if err == nil {
			t.Errorf("agent_key %q was accepted", key)
		}
	}
}

// An agent is played by the engine; it does not run games.
func TestSchemaRefusesAgentAsGameMaster(t *testing.T) {
	db := testDB(t)
	game := testGame(t, db, "AgentGM")

	err := insertSeat(t, db, `
		INSERT INTO game_player (game_id, account_id, account_role, is_agent,
		                         agent_key, agent_name, is_gm, is_active,
		                         created_at, updated_at)
		VALUES (:game_id, NULL, 'user', 1, 'passive', 'Passive', 1, 1, :now, :now);`,
		map[string]any{":game_id": game.ID, ":now": formatTime(Now())})
	if err == nil {
		t.Fatal("an agent was accepted as game master")
	}
}

// A human seat carries no agent fields, and an agent seat carries no account.
// The two halves of a seat can never disagree about which kind it is.
func TestSchemaRefusesMixedSeatKinds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Mixed")

	account, err := db.AccountByEmail(ctx, "user1@example.com")
	if err != nil {
		t.Fatalf("load seeded account: %v", err)
	}
	now := formatTime(Now())

	cases := []struct {
		name   string
		values map[string]any
		sql    string
	}{
		{
			name: "human carrying an agent key",
			sql: `INSERT INTO game_player (game_id, account_id, account_role, is_agent,
			                               agent_key, agent_name, is_gm, is_active, created_at, updated_at)
			      VALUES (:game_id, :account_id, 'user', 0, 'passive', NULL, 0, 1, :now, :now);`,
			values: map[string]any{":game_id": game.ID, ":account_id": account.ID, ":now": now},
		},
		{
			name: "agent holding an account",
			sql: `INSERT INTO game_player (game_id, account_id, account_role, is_agent,
			                               agent_key, agent_name, is_gm, is_active, created_at, updated_at)
			      VALUES (:game_id, :account_id, 'user', 1, 'passive', 'Passive', 0, 1, :now, :now);`,
			values: map[string]any{":game_id": game.ID, ":account_id": account.ID, ":now": now},
		},
		{
			name: "agent with no name",
			sql: `INSERT INTO game_player (game_id, account_id, account_role, is_agent,
			                               agent_key, agent_name, is_gm, is_active, created_at, updated_at)
			      VALUES (:game_id, NULL, 'user', 1, 'passive', NULL, 0, 1, :now, :now);`,
			values: map[string]any{":game_id": game.ID, ":now": now},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := insertSeat(t, db, testCase.sql, testCase.values); err == nil {
				t.Error("the row was accepted")
			}
		})
	}
}

// Seating an agent in a game that does not exist must fail rather than create
// an orphan the engine would later have to interpret.
func TestCreateAgentSeatRequiresAGame(t *testing.T) {
	db := testDB(t)

	_, err := db.CreateAgentSeat(context.Background(), 999, "passive", "Passive", true, Now())
	if err == nil {
		t.Fatal("an agent was seated in a game that does not exist")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

// Timestamps go through Now(), which truncates to the precision the storage
// format keeps. A seat that round-trips to a different instant would make the
// engine's ordering of events wrong.
func TestAgentSeatTimestampsRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Timestamps")

	before := Now()
	seat, err := db.CreateAgentSeat(ctx, game.ID, "passive", "Passive", true, before)
	if err != nil {
		t.Fatalf("seat agent: %v", err)
	}
	if !seat.CreatedAt.Equal(before) {
		t.Errorf("created_at = %v, want %v", seat.CreatedAt, before)
	}
	if !seat.UpdatedAt.Equal(before) {
		t.Errorf("updated_at = %v, want %v", seat.UpdatedAt, before)
	}

	later := before.Add(time.Minute)
	updated, err := db.UpdateAgentSeat(ctx, game.ID, seat.ID, "Passive", false, later)
	if err != nil {
		t.Fatalf("update seat: %v", err)
	}
	if !updated.CreatedAt.Equal(before) {
		t.Errorf("created_at moved to %v, want %v", updated.CreatedAt, before)
	}
	if !updated.UpdatedAt.Equal(later) {
		t.Errorf("updated_at = %v, want %v", updated.UpdatedAt, later)
	}
}
