// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package generators specifies the interfaces for cluster,
// stellium, planet, deposit, and colony generators.
package generators

import (
	"cmp"
	"slices"

	"github.com/mdhender/ecv8-api/internal/cerrs"
)

// The bounds a cluster's shape has to satisfy, and the values a game master is
// offered when they express no preference.
//
// They live here rather than in a handler, in the schema, or in the client
// because the generators are what they constrain: an implementation decides
// whether it can fill a sphere, so the range it may be asked for is part of the
// contract this package specifies. Every other layer names these constants —
// the CHECKs in migration 0004 restate them, deliberately and with a comment
// saying so, because a database is read by more than this binary.
//
// DefaultRadius is 15 because the canonical reference puts stellium coordinates
// between -15 and 15. MaxStelliumCount is not a rule of the game — the
// reference says a cluster holds exactly 100 — but a bound on how much work one
// request may ask for.
const (
	MinRadius     = 3
	MaxRadius     = 1024
	DefaultRadius = 15

	MinStelliumCount     = 1
	MaxStelliumCount     = 10000
	DefaultStelliumCount = 100
)

const (
	// ErrInvalidStelliumCount reports a count outside MinStelliumCount..MaxStelliumCount.
	ErrInvalidStelliumCount = cerrs.Error("invalid stellium count")

	// ErrInvalidRadius reports a radius outside MinRadius..MaxRadius.
	ErrInvalidRadius = cerrs.Error("invalid radius")

	// ErrCrowded reports that the requested number of stelliums does not fit in
	// the requested radius.
	//
	// It is a separate error because it is the one failure a caller cannot
	// predict from the arguments alone: how many distinct coordinates a sphere
	// holds is a property of the implementation's own rounding, so only the
	// generator can answer it. A caller that gets this should offer a larger
	// radius, not retry.
	ErrCrowded = cerrs.Error("too many stelliums for this radius")
)

type ClusterGenerator interface {
	Cluster() (*Cluster, error)
}

type Cluster struct {
	Radius float64 // 3.0 <= Radius <= 1024, default 15

	// Stelliums contains no nil entries. All coordinates are unique.
	Stelliums []*Stellium
}

// SortStelliums sorts the cluster's stelliums lexicographically
// by X coordinate, then Y coordinate, then Z coordinate.
func (c *Cluster) SortStelliums() {
	slices.SortFunc(c.Stelliums, func(a, b *Stellium) int {
		return a.Compare(*b)
	})
}

type Stellium struct {
	Coords Point
}

// Compare compares s and other by their coordinates.
func (s Stellium) Compare(other Stellium) int {
	return s.Coords.Compare(other.Coords)
}

type Point struct {
	X int
	Y int
	Z int
}

// Compare compares p and other lexicographically by X, then Y, then Z.
func (p Point) Compare(other Point) int {
	return cmp.Or(
		cmp.Compare(p.X, other.X),
		cmp.Compare(p.Y, other.Y),
		cmp.Compare(p.Z, other.Z),
	)
}
