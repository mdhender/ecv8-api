// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package prng_test

import (
	"math/rand/v2"
	"testing"

	"github.com/mdhender/ecv8-api/internal/prng"
)

// TestRollNBounds: RollN(n, sides) always lands in [n, n*sides].
func TestRollNBounds(t *testing.T) {
	roller := prng.New(1, 2).Roller(prng.TagDeposit, 0, 0)
	cases := []struct{ n, sides int }{
		{1, 6}, {3, 4}, {2, 6}, {4, 10}, {10, 20},
	}
	for _, c := range cases {
		lo, hi := c.n, c.n*c.sides
		for range 1000 {
			got := roller.RollN(c.n, c.sides)
			if got < lo || got > hi {
				t.Fatalf("RollN(%d,%d) = %d, out of [%d,%d]", c.n, c.sides, got, lo, hi)
			}
		}
	}
}

// TestRollNSingleDie: RollN(1, sides) is a plain [1, sides] die and hits both
// ends over enough draws.
func TestRollNSingleDie(t *testing.T) {
	roller := prng.New(5, 6).Roller(prng.TagDeposit, 1, 1)
	const sides = 6
	seen := map[int]bool{}
	for range 2000 {
		v := roller.RollN(1, sides)
		if v < 1 || v > sides {
			t.Fatalf("RollN(1,%d) = %d out of range", sides, v)
		}
		seen[v] = true
	}
	for face := 1; face <= sides; face++ {
		if !seen[face] {
			t.Errorf("face %d never rolled in %d draws", face, 2000)
		}
	}
}

// TestRollRangeInclusive: RollRange(lo, hi) stays in [lo, hi] and reaches both
// endpoints.
func TestRollRangeInclusive(t *testing.T) {
	roller := prng.New(9, 10).Roller(prng.TagCluster)
	const lo, hi = -3, 3
	sawLo, sawHi := false, false
	for range 5000 {
		v := roller.RollRange(lo, hi)
		if v < lo || v > hi {
			t.Fatalf("RollRange(%d,%d) = %d out of range", lo, hi, v)
		}
		if v == lo {
			sawLo = true
		}
		if v == hi {
			sawHi = true
		}
	}
	if !sawLo || !sawHi {
		t.Errorf("RollRange(%d,%d) did not reach both endpoints (lo=%v hi=%v)", lo, hi, sawLo, sawHi)
	}
}

// TestRollRangePanics: RollRange must panic when !(lo < hi) — a programmer-error
// guard on constant bounds.
func TestRollRangePanics(t *testing.T) {
	cases := []struct{ lo, hi int }{
		{5, 5},  // equal
		{5, 4},  // inverted
		{0, -1}, // inverted across zero
	}
	for _, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RollRange(%d,%d) did not panic", c.lo, c.hi)
				}
			}()
			prng.New(1, 1).Roller(prng.TagCluster).RollRange(c.lo, c.hi)
		}()
	}
}

// TestRollerReproducible: two Rollers built at the same address produce
// identical sequences — the Roller advances one stream, and address is the only
// input.
func TestRollerReproducible(t *testing.T) {
	seeds := prng.New(0xabc, 0xdef)
	a := seeds.Roller(prng.TagDeposit, 3, -7)
	b := seeds.Roller(prng.TagDeposit, 3, -7)
	for i := range 50 {
		if x, y := a.RollN(2, 6), b.RollN(2, 6); x != y {
			t.Fatalf("roll %d differs between equal-address Rollers: %d vs %d", i, x, y)
		}
	}
}

// TestRollerMatchesStream: a Roller and a fresh rand.New(Stream) at the same
// address agree draw-for-draw — the Roller is exactly rand.New over the stream,
// with one IntN(sides)+1 per die.
func TestRollerMatchesStream(t *testing.T) {
	seeds := prng.New(42, 43)
	path := []prng.Key{prng.TagSystem, 2, 2}

	roller := seeds.Roller(path...)
	rng := rand.New(seeds.Stream(path...))

	for i := range 50 {
		const n, sides = 3, 4
		want := 0
		for range n {
			want += rng.IntN(sides) + 1
		}
		if got := roller.RollN(n, sides); got != want {
			t.Fatalf("roll %d: Roller.RollN = %d, hand-rolled Stream = %d", i, got, want)
		}
	}
}

// TestRollerDistinctAddresses: different addresses give uncorrelated first rolls
// (sanity that Roller inherits Stream's domain separation).
func TestRollerDistinctAddresses(t *testing.T) {
	seeds := prng.New(7, 11)
	// Wide range so a collision would signal shared state, not chance.
	x := seeds.Roller(prng.TagDeposit, 0, 0).RollRange(0, 1<<30)
	y := seeds.Roller(prng.TagDeposit, 0, 1).RollRange(0, 1<<30)
	if x == y {
		t.Errorf("distinct deposit addresses produced identical first roll %d", x)
	}
}

// TestRollNZeroDice: a non-positive die count sums to zero (no draws taken).
func TestRollNZeroDice(t *testing.T) {
	roller := prng.New(1, 2).Roller(prng.TagCluster)
	if got := roller.RollN(0, 6); got != 0 {
		t.Errorf("RollN(0,6) = %d, want 0", got)
	}
}

// TestShuffleDeterministic: Shuffle draws from the Roller's stream, so equal
// addresses shuffle identically.
func TestShuffleDeterministic(t *testing.T) {
	seeds := prng.New(3, 4)
	perm := func() []int {
		s := []int{0, 1, 2, 3, 4, 5, 6, 7}
		seeds.Roller(prng.TagCluster).Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		return s
	}
	a, b := perm(), perm()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("Shuffle not deterministic at %d: %v vs %v", i, a, b)
		}
	}
}

// TestPermDeterministic: Perm draws from the Roller's stream, so equal addresses
// permute identically and the result is a valid permutation of [0,n).
func TestPermDeterministic(t *testing.T) {
	seeds := prng.New(3, 4)
	a := seeds.Roller(prng.TagCluster).Perm(10)
	b := seeds.Roller(prng.TagCluster).Perm(10)
	if len(a) != 10 {
		t.Fatalf("Perm(10) len = %d", len(a))
	}
	seen := make([]bool, 10)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("Perm not deterministic at %d: %v vs %v", i, a, b)
		}
		if a[i] < 0 || a[i] >= 10 || seen[a[i]] {
			t.Fatalf("Perm(10) not a valid permutation: %v", a)
		}
		seen[a[i]] = true
	}
}
