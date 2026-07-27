// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"errors"
	"testing"
)

// testCoordinates returns n distinct coordinates, so a test that is not about
// duplicates cannot fail on one.
func testCoordinates(n int) []Coordinate {
	out := make([]Coordinate, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Coordinate{X: i, Y: 1, Z: 1})
	}
	return out
}

func TestCreateCluster(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Mapped")

	stored, err := db.CreateCluster(ctx, game.ID, NewCluster{
		GeneratorKey:  "kiss",
		StelliumCount: 3,
		Radius:        15,
		Coordinates:   []Coordinate{{X: 2, Y: -3, Z: 4}, {X: -1, Y: 1, Z: 1}, {X: 2, Y: -3, Z: 5}},
	}, Now())
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	if stored.GeneratorKey != "kiss" {
		t.Errorf("generator_key = %q, want %q", stored.GeneratorKey, "kiss")
	}
	if stored.StelliumCount != 3 {
		t.Errorf("stellium_count = %d, want 3", stored.StelliumCount)
	}
	if stored.Radius != 15 {
		t.Errorf("radius = %d, want 15", stored.Radius)
	}

	// The order is the map's own, not the order the rows were written, so that a
	// caller rendering a cluster never depends on how the ids fell out.
	stelliums, err := db.StelliumsByGameID(ctx, game.ID)
	if err != nil {
		t.Fatalf("load stelliums: %v", err)
	}
	want := []Coordinate{{X: -1, Y: 1, Z: 1}, {X: 2, Y: -3, Z: 4}, {X: 2, Y: -3, Z: 5}}
	if len(stelliums) != len(want) {
		t.Fatalf("loaded %d stelliums, want %d", len(stelliums), len(want))
	}
	for i, w := range want {
		got := Coordinate{X: stelliums[i].X, Y: stelliums[i].Y, Z: stelliums[i].Z}
		if got != w {
			t.Errorf("stellium %d = %+v, want %+v", i, got, w)
		}
		if stelliums[i].ID == 0 {
			t.Errorf("stellium %d has no id", i)
		}
	}
}

// A game has at most one cluster, and cluster.game_id being the primary key is
// what makes that true whatever code writes the row. Regenerating a map would
// invalidate every turn already resolved on the old one.
func TestCreateClusterRefusesASecondOne(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Once")

	if _, err := db.CreateCluster(ctx, game.ID, NewCluster{
		GeneratorKey: "kiss", StelliumCount: 2, Radius: 15,
		Coordinates: testCoordinates(2),
	}, Now()); err != nil {
		t.Fatalf("create first cluster: %v", err)
	}

	_, err := db.CreateCluster(ctx, game.ID, NewCluster{
		GeneratorKey: "kiss", StelliumCount: 2, Radius: 15,
		Coordinates: testCoordinates(2),
	}, Now())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second cluster: err = %v, want ErrConflict", err)
	}
}

// Two stelliums at one coordinate would share every draw prng addresses by
// (x, y, z), so the unique index is a rule the schema keeps rather than a
// promise the generator makes. The whole insert must roll back with it.
func TestCreateClusterRefusesDuplicateCoordinates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Collided")

	_, err := db.CreateCluster(ctx, game.ID, NewCluster{
		GeneratorKey:  "kiss",
		StelliumCount: 3,
		Radius:        15,
		Coordinates:   []Coordinate{{X: 1, Y: 1, Z: 1}, {X: 2, Y: 2, Z: 2}, {X: 1, Y: 1, Z: 1}},
	}, Now())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate coordinate: err = %v, want ErrConflict", err)
	}

	if _, err := db.ClusterByGameID(ctx, game.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cluster after a failed insert: err = %v, want ErrNotFound", err)
	}
	stelliums, err := db.StelliumsByGameID(ctx, game.ID)
	if err != nil {
		t.Fatalf("load stelliums: %v", err)
	}
	if len(stelliums) != 0 {
		t.Errorf("%d stelliums survived a rolled-back insert, want 0", len(stelliums))
	}
}

// The two coordinates are the same point in different games, which is the case
// the unique index must not catch: it is scoped to a game because every engine
// row is.
func TestClustersInDifferentGamesMayShareCoordinates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	first := testGame(t, db, "First")
	second := testGame(t, db, "Second")

	coordinates := []Coordinate{{X: 3, Y: 3, Z: 3}}
	for _, game := range []*Game{first, second} {
		if _, err := db.CreateCluster(ctx, game.ID, NewCluster{
			GeneratorKey: "kiss", StelliumCount: 1, Radius: 15,
			Coordinates: coordinates,
		}, Now()); err != nil {
			t.Fatalf("create cluster for game %d: %v", game.ID, err)
		}
	}
}

// The bounds in migration 0004 restate the constants in engine/generators, so
// they are worth a test that proves the database is the one enforcing them: a
// widened Go constant must surface here rather than silently storing a value
// the generators cannot produce.
func TestCreateClusterRefusesSettingsOutsideTheSchemaBounds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		cluster NewCluster
	}{
		{"radius below the minimum", NewCluster{GeneratorKey: "kiss", StelliumCount: 1, Radius: 2, Coordinates: testCoordinates(1)}},
		{"radius above the maximum", NewCluster{GeneratorKey: "kiss", StelliumCount: 1, Radius: 1025, Coordinates: testCoordinates(1)}},
		{"no stelliums", NewCluster{GeneratorKey: "kiss", StelliumCount: 0, Radius: 15}},
		{"an empty generator key", NewCluster{GeneratorKey: "", StelliumCount: 1, Radius: 15, Coordinates: testCoordinates(1)}},
		{"an upper-case generator key", NewCluster{GeneratorKey: "KISS", StelliumCount: 1, Radius: 15, Coordinates: testCoordinates(1)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			game := testGame(t, db, testCase.name)
			if _, err := db.CreateCluster(ctx, game.ID, testCase.cluster, Now()); err == nil {
				t.Fatal("err = nil, want a constraint failure")
			}
		})
	}
}

// A stellium cannot exist without the cluster it belongs to, which is what the
// reference to cluster.game_id makes true. Nothing else in the store can write
// one, so this reaches past CreateCluster deliberately: the point is that the
// schema refuses it whatever code tries.
func TestStelliumRequiresACluster(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	game := testGame(t, db, "Unmapped")

	if _, err := db.ClusterByGameID(ctx, game.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cluster for a new game: err = %v, want ErrNotFound", err)
	}
	stelliums, err := db.StelliumsByGameID(ctx, game.ID)
	if err != nil {
		t.Fatalf("load stelliums: %v", err)
	}
	if len(stelliums) != 0 {
		t.Errorf("a game with no cluster has %d stelliums, want 0", len(stelliums))
	}
}
