// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"fmt"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// A game's cluster is written exactly once, which is why there is a read and an
// insert here and no update. The map is the ground every later turn is resolved
// on, so regenerating it after a game has begun would invalidate the turns
// already played — the same rule that makes a game's seed write-once, and for
// the same reason.
//
// The cluster row and its stelliums are one write. Write runs inside an
// immediate transaction, so a failure part-way through a hundred inserts leaves
// no cluster at all rather than a cluster missing some of its map.

const clusterColumns = `game_id, generator_key, stellium_count, radius, created_at, updated_at`

// scanCluster reads one row of clusterColumns.
func scanCluster(stmt *sqlite.Stmt) (Cluster, error) {
	cluster := Cluster{
		GameID:        stmt.GetInt64("game_id"),
		GeneratorKey:  stmt.GetText("generator_key"),
		StelliumCount: int(stmt.GetInt64("stellium_count")),
		Radius:        int(stmt.GetInt64("radius")),
	}
	var err error
	if cluster.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
		return Cluster{}, err
	}
	if cluster.UpdatedAt, err = parseTime(stmt.GetText("updated_at")); err != nil {
		return Cluster{}, err
	}
	return cluster, nil
}

// ClusterByGameID returns a game's cluster, or ErrNotFound when it has none.
//
// "Not found" is an ordinary stage of a game's life rather than a failure: a
// game is created, set up, and only then given a map. Callers tell the two
// apart with errors.Is, as they do for game state.
func (db *DB) ClusterByGameID(ctx context.Context, gameID int64) (*Cluster, error) {
	var found *Cluster
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+clusterColumns+` FROM cluster WHERE game_id = :game_id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					cluster, err := scanCluster(stmt)
					if err != nil {
						return err
					}
					found = &cluster
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load cluster for game %d: %w", gameID, err)
	}
	if found == nil {
		return nil, fmt.Errorf("cluster for game %d: %w", gameID, ErrNotFound)
	}
	return found, nil
}

// StelliumsByGameID returns a game's stelliums, ordered by coordinate.
//
// The order is the map's own, not the order the rows were written, so a caller
// rendering a cluster gets the same list whatever sequence the ids were
// assigned in. A game with no cluster has no stelliums and gets an empty slice
// rather than ErrNotFound: whether the cluster exists is ClusterByGameID's
// question, and answering it twice would let the two disagree.
func (db *DB) StelliumsByGameID(ctx context.Context, gameID int64) ([]Stellium, error) {
	var found []Stellium
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			SELECT id, game_id, x, y, z, created_at, updated_at
			  FROM stellium
			 WHERE game_id = :game_id
			 ORDER BY x, y, z;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":game_id": gameID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					stellium := Stellium{
						ID:     stmt.GetInt64("id"),
						GameID: stmt.GetInt64("game_id"),
						X:      int(stmt.GetInt64("x")),
						Y:      int(stmt.GetInt64("y")),
						Z:      int(stmt.GetInt64("z")),
					}
					var err error
					if stellium.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
						return err
					}
					if stellium.UpdatedAt, err = parseTime(stmt.GetText("updated_at")); err != nil {
						return err
					}
					found = append(found, stellium)
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load stelliums for game %d: %w", gameID, err)
	}
	return found, nil
}

// NewCluster is a generated map on its way to storage: the parameters it was
// drawn with, and the coordinates that came out.
//
// The coordinates are a separate type from Stellium because they have no ids
// yet — the database assigns those, and nothing may pretend to know one before
// it does.
type NewCluster struct {
	GeneratorKey  string
	StelliumCount int
	Radius        int
	Coordinates   []Coordinate
}

// Coordinate is one stellium's position, with the origin at the centre of the
// cluster.
type Coordinate struct {
	X int
	Y int
	Z int
}

// CreateCluster writes a game's map: the cluster row and every stellium in it,
// in one transaction.
//
// It only ever inserts. cluster.game_id is the primary key, so a second call
// fails on that constraint rather than replacing the map a game has already
// been played on. A duplicate coordinate fails the same way, on the unique
// index, which is what keeps a generator's promise of distinct positions from
// being the only thing holding it up.
func (db *DB) CreateCluster(ctx context.Context, gameID int64, cluster NewCluster, now time.Time) (*Cluster, error) {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `
			INSERT INTO cluster (game_id, generator_key, stellium_count, radius, created_at, updated_at)
			VALUES (:game_id, :generator_key, :stellium_count, :radius, :now, :now);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":game_id":        gameID,
					":generator_key":  cluster.GeneratorKey,
					":stellium_count": cluster.StelliumCount,
					":radius":         cluster.Radius,
					":now":            formatTime(now),
				},
			}); err != nil {
			return err
		}

		for _, coordinate := range cluster.Coordinates {
			if err := sqlitex.Execute(conn, `
				INSERT INTO stellium (game_id, x, y, z, created_at, updated_at)
				VALUES (:game_id, :x, :y, :z, :now, :now);`,
				&sqlitex.ExecOptions{
					Named: map[string]any{
						":game_id": gameID,
						":x":       coordinate.X,
						":y":       coordinate.Y,
						":z":       coordinate.Z,
						":now":     formatTime(now),
					},
				}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: this game already has a cluster", ErrConflict)
		}
		if isConstraintViolation(err) {
			return nil, fmt.Errorf("%w: the game must exist and be set up", ErrConflict)
		}
		return nil, fmt.Errorf("create cluster for game %d: %w", gameID, err)
	}
	return db.ClusterByGameID(ctx, gameID)
}
