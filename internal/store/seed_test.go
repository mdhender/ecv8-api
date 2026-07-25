// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"math"
	"strconv"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// seedWords are the values a round trip is most likely to break on: the two
// ends of the range, the boundary where a uint64 stops fitting in an int64, and
// a couple of ordinary values either side of it.
var seedWords = []uint64{
	0,
	1,
	42,
	math.MaxInt64 - 1,
	math.MaxInt64,     // the largest value that is positive on disk
	math.MaxInt64 + 1, // the smallest that is stored negative
	math.MaxUint64 - 1,
	math.MaxUint64,
}

// The round trip has to be exact for every value, not merely for the ones that
// happen to stay positive. A seed that comes back different makes a game
// unreplayable, which is the one property internal/engine exists to guarantee.
func TestSeedRoundTrip(t *testing.T) {
	for _, want := range seedWords {
		if got := parseSeed(formatSeed(want)); got != want {
			t.Errorf("parseSeed(formatSeed(%d)) = %d, want %d", want, got, want)
		}
	}
}

// The upper half of the range really is stored negative. This is the fact that
// looks like a bug in a SQLite shell, so it is worth pinning: someone who
// "fixes" it by clamping would break replay, and this test is where they find
// out.
func TestSeedUpperHalfIsStoredNegative(t *testing.T) {
	if got := formatSeed(math.MaxInt64); got != math.MaxInt64 {
		t.Errorf("formatSeed(MaxInt64) = %d, want %d", got, int64(math.MaxInt64))
	}
	if got := formatSeed(math.MaxInt64 + 1); got >= 0 {
		t.Errorf("formatSeed(2^63) = %d, want a negative value", got)
	}
	if got := formatSeed(math.MaxUint64); got != -1 {
		t.Errorf("formatSeed(MaxUint64) = %d, want -1", got)
	}
}

// Distinct seeds must stay distinct on disk. A conversion that folded the upper
// half onto the lower one would round-trip each value to something, but two
// different games could end up sharing a stream.
func TestSeedsStayDistinct(t *testing.T) {
	seen := make(map[int64]uint64, len(seedWords))
	for _, word := range seedWords {
		stored := formatSeed(word)
		if first, ok := seen[stored]; ok {
			t.Errorf("seeds %d and %d both store as %d", first, word, stored)
			continue
		}
		seen[stored] = word
	}
}

// The claim is about a round trip through SQLite, not just through Go's type
// system, so this one actually writes the values to a database and reads them
// back. An INTEGER column that silently promoted a large value to REAL would
// pass every test above and lose the low bits here.
func TestSeedRoundTripThroughSQLite(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for _, want := range seedWords {
		// The name must be unique per row, so it carries the seed.
		game, err := db.CreateGame(ctx, "seed-"+strconv.FormatUint(want, 10), Now())
		if err != nil {
			t.Fatalf("create game for seed %d: %v", want, err)
		}

		now := formatTime(Now())
		err = db.Write(ctx, func(conn *sqlite.Conn) error {
			return sqlitex.Execute(conn, `
				INSERT INTO game_state (game_id, turn, seed_hi, seed_lo, created_at, updated_at)
				VALUES (:game_id, 0, :hi, :lo, :now, :now);`,
				&sqlitex.ExecOptions{
					Named: map[string]any{
						":game_id": game.ID,
						":hi":      formatSeed(want),
						// Stored in the other column too, so a mix-up between
						// the two words would show up as a mismatch.
						":lo":  formatSeed(^want),
						":now": now,
					},
				})
		})
		if err != nil {
			t.Fatalf("store seed %d: %v", want, err)
		}

		var hi, lo uint64
		err = db.Read(ctx, func(conn *sqlite.Conn) error {
			return sqlitex.Execute(conn,
				`SELECT seed_hi, seed_lo FROM game_state WHERE game_id = :game_id;`,
				&sqlitex.ExecOptions{
					Named: map[string]any{":game_id": game.ID},
					ResultFunc: func(stmt *sqlite.Stmt) error {
						hi = parseSeed(stmt.GetInt64("seed_hi"))
						lo = parseSeed(stmt.GetInt64("seed_lo"))
						return nil
					},
				})
		})
		if err != nil {
			t.Fatalf("read seed %d: %v", want, err)
		}
		if hi != want {
			t.Errorf("seed_hi = %d, want %d", hi, want)
		}
		if lo != ^want {
			t.Errorf("seed_lo = %d, want %d", lo, ^want)
		}
	}
}
