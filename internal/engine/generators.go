// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package engine

import (
	"fmt"
	"math/rand/v2"

	"github.com/mdhender/ecv8-api/internal/cerrs"
	"github.com/mdhender/ecv8-api/internal/engine/generators"
	"github.com/mdhender/ecv8-api/internal/engine/generators/kiss"
	"github.com/mdhender/ecv8-api/internal/prng"
)

// ErrNoSuchGenerator reports a cluster generator key this build cannot run.
const ErrNoSuchGenerator = cerrs.Error("no such cluster generator")

// GeneratorDescriptor is one cluster generator this build can run: the key a
// cluster records, the two strings a game master reads when choosing one, and
// the constructor that builds it.
//
// Key is the durable half, for the same reason an agent's is. It is written
// onto a game's cluster row and stays there for the life of the game, so it
// says which implementation drew that map — renaming one makes every cluster
// that recorded it name an implementation nothing has. Name and Description are
// the display half and may be reworded freely.
//
// The constructor is unexported, so a caller outside this package can read the
// catalogue but cannot build a generator except through GenerateCluster. That
// is what keeps the random source out of a handler's hands: which stream a
// cluster is drawn from is the engine's decision, not a caller's.
type GeneratorDescriptor struct {
	Key         string
	Name        string
	Description string

	new func(count, radius int, rng *rand.Rand) (generators.ClusterGenerator, error)
}

// DefaultGeneratorKey is the generator a game master gets when they express no
// preference. It is a key rather than an index so that reordering the catalogue
// below cannot change which generator a game is built with.
const DefaultGeneratorKey = "kiss"

// clusterGenerators is the catalogue of every cluster generator this binary can
// run, in the order a game master sees.
//
// It is an explicit list for the reasons agents is: no self-registration, no
// init-order dependence, and adding a generator is one line here beside the
// code that implements it. Keys must be unique and match the format migration
// 0004 enforces on the column; nothing checks that mechanically, which is the
// price of a plain list — it is short, and it is reviewed.
var clusterGenerators = []GeneratorDescriptor{
	{
		Key:  "kiss",
		Name: "KISS",
		Description: "Scatters stelliums uniformly through the sphere and does nothing else. " +
			"The simplest generator that could possibly work, and the one every other is compared against.",
		new: func(count, radius int, rng *rand.Rand) (generators.ClusterGenerator, error) {
			return kiss.NewGenerator(count, radius, rng)
		},
	},
}

// Generators returns every cluster generator this build can run, in display
// order. It returns a copy: the catalogue is fixed at build time, and a caller
// able to reorder or extend it would be changing what the engine can do.
func Generators() []GeneratorDescriptor {
	out := make([]GeneratorDescriptor, len(clusterGenerators))
	copy(out, clusterGenerators)
	return out
}

// GeneratorByKey looks a cluster generator up by key, reporting whether this
// build has one.
//
// It is the authority for the same reason AgentByKey is: a stored key may have
// been written by a different release, so whether the implementation still
// exists is a fact about this binary and can only be asked here.
func GeneratorByKey(key string) (GeneratorDescriptor, bool) {
	for _, descriptor := range clusterGenerators {
		if descriptor.Key == key {
			return descriptor, true
		}
	}
	return GeneratorDescriptor{}, false
}

// GeneratorKeys returns just the keys, for an error message that tells a caller
// what there was to choose between.
func GeneratorKeys() []string {
	keys := make([]string, 0, len(clusterGenerators))
	for _, descriptor := range clusterGenerators {
		keys = append(keys, descriptor.Key)
	}
	return keys
}

// GenerateCluster draws a game's cluster with the named generator.
//
// The random source is not a parameter. It is the game's cluster stream —
// prng.TagCluster addressed against the game's master seeds — so the same game
// regenerated from the same seed produces the same map whatever else has been
// drawn, and drawing a cluster cannot shift any other subsystem's numbers. A
// caller that could pass its own source could break both of those, which is why
// this takes a seed and builds the stream itself.
//
// An unknown key is an error rather than a fallback to the default: a game
// master who asked for one generator must not silently get another.
func GenerateCluster(seed Seed, key string, count, radius int) (*generators.Cluster, error) {
	descriptor, ok := GeneratorByKey(key)
	if !ok {
		return nil, fmt.Errorf("%q: %w", key, ErrNoSuchGenerator)
	}

	rng := rand.New(prng.New(seed.Hi, seed.Lo).Stream(prng.TagCluster))
	generator, err := descriptor.new(count, radius, rng)
	if err != nil {
		return nil, err
	}

	cluster, err := generator.Cluster()
	if err != nil {
		return nil, err
	}
	// Sorted before it leaves the engine so that the rows written for a cluster
	// are in a fixed order. The generator collects its points from a map, and Go
	// map iteration is deliberately random: without this, two runs of the same
	// seed would produce the same map with different row ids.
	cluster.SortStelliums()
	return cluster, nil
}
