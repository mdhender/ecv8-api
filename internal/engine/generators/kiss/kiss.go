// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package kiss implements the simplest generator that could possibly work.
package kiss

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/mdhender/ecv8-api/internal/engine/generators"
)

// NewGenerator returns a generator that draws stelliums distinct count
// coordinates from rng, all of them inside a sphere of the given radius.
//
// Both bounds are checked here rather than in Cluster so that arguments nobody
// could satisfy are refused before any work is done. Whether the radius has
// room for that many distinct coordinates is a different question and cannot be
// answered here; see Cluster.
func NewGenerator(count, radius int, rng *rand.Rand) (*Generator, error) {
	if count < generators.MinStelliumCount || count > generators.MaxStelliumCount {
		return nil, fmt.Errorf("%d: %w", count, generators.ErrInvalidStelliumCount)
	}
	if radius < generators.MinRadius || radius > generators.MaxRadius {
		return nil, fmt.Errorf("%d: %w", radius, generators.ErrInvalidRadius)
	}
	if rng == nil {
		return nil, fmt.Errorf("kiss: a random source is required")
	}
	return &Generator{
		count:  count,
		radius: radius,
		rng:    rng,
	}, nil
}

type Generator struct {
	count  int
	radius int
	rng    *rand.Rand
}

func (g *Generator) drawPoint() generators.Point {
	u1, u2, u3 := g.rng.Float64(), g.rng.Float64(), g.rng.Float64()

	theta := 2 * math.Pi * u1
	zDir := 2*u2 - 1
	xyScale := math.Sqrt(1 - zDir*zDir)

	r := float64(g.radius) * math.Cbrt(u3)

	return generators.Point{
		X: ceilAwayFromZero(r*xyScale*math.Cos(theta), g.radius),
		Y: ceilAwayFromZero(r*xyScale*math.Sin(theta), g.radius),
		Z: ceilAwayFromZero(r*zDir, g.radius),
	}
}

// ceilAwayFromZero rounds a float to an integer, pushing it away from 0
func ceilAwayFromZero(v float64, radius int) int {
	if v == 0 {
		return 0
	}

	n := int(math.Copysign(math.Ceil(math.Abs(v)), v))

	// Protect against any floating-point overshoot.
	if n > radius {
		return radius
	}
	if n < -radius {
		return -radius
	}

	return n
}
