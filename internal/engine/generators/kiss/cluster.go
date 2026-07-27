// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package kiss

import (
	"fmt"

	"github.com/mdhender/ecv8-api/internal/engine/generators"
)

// drawsPerStellium bounds how long Cluster will keep drawing.
//
// A sphere holds a finite number of distinct integer coordinates, so asking for
// more stelliums than fit is a request that never completes: the loop below
// would draw duplicates forever. The number of coordinates that fit is a
// property of drawPoint's own rounding rather than of the radius, so it cannot
// be checked from the arguments — the only honest answer is to give up after a
// generous number of attempts and say the radius is too small.
//
// It is generous on purpose. Filling a sphere to near its capacity is a coupon
// collector's problem and the last few coordinates are drawn rarely, so a
// budget that merely looks sufficient would reject requests that are only slow.
const drawsPerStellium = 1000

// Cluster implements generators.ClusterGenerator
func (g *Generator) Cluster() (*generators.Cluster, error) {
	// using a map for points prevents us from duplicating them
	points := map[generators.Point]bool{}
	for draws := 0; len(points) < g.count; draws++ {
		if draws == g.count*drawsPerStellium {
			return nil, fmt.Errorf("%d stelliums in radius %d: %w",
				g.count, g.radius, generators.ErrCrowded)
		}
		points[g.drawPoint()] = true
	}

	c := generators.Cluster{Radius: float64(g.radius)}
	for p := range points {
		c.Stelliums = append(c.Stelliums, &generators.Stellium{Coords: p})
	}

	return &c, nil
}
