// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package prng_test

import (
	"encoding/json"
	"flag"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdhender/ecv8-api/internal/prng"
)

// update regenerates testdata/golden.json from the current code. Run once when
// intentionally establishing the frozen surface:
//
//	go test ./internal/prng/ -update
//
// then eyeball the diff and commit. Never run it to "fix" a failing golden test:
// a failure means the addressing, hashing, or generator changed, which silently
// rewrites every live game.
var update = flag.Bool("update", false, "regenerate testdata/golden.json")

const goldenPath = "testdata/golden.json"

// drawsPerStream is how many uint64 each golden stream pins.
const drawsPerStream = 4

// rollsPerVector is how many rolls each golden roll vector pins.
const rollsPerVector = 8

// golden is the on-disk shape of the frozen vectors. New sections are APPENDED
// after the original streams/derives so those vectors stay byte-identical.
type golden struct {
	Streams []streamVector `json:"streams"`
	Derives []deriveVector `json:"derives"`
	Rolls   []rollVector   `json:"rolls"`
	Roots   []rootVector   `json:"roots"`
}

type streamVector struct {
	Seed1 uint64     `json:"seed1"`
	Seed2 uint64     `json:"seed2"`
	Path  []prng.Key `json:"path"`
	Draws []uint64   `json:"draws"`
}

type deriveVector struct {
	Seed1 uint64     `json:"seed1"`
	Seed2 uint64     `json:"seed2"`
	Path  []prng.Key `json:"path"`
	// child seeds are exposed only via their observable behavior; we pin the
	// first draw of the child's own default stream so the vector stays black-box.
	WantChildDraw uint64 `json:"want_child_draw"`
}

// rollVector pins a Roller's output sequence for a fixed seed + address. Kind
// selects the call: "rolln" repeats RollN(N, Sides); "rollrange" repeats
// RollRange(Lo, Hi). Pinning the whole sequence catches any change to the
// draw-to-die mapping, the draw order, or the reduction.
type rollVector struct {
	Seed1 uint64     `json:"seed1"`
	Seed2 uint64     `json:"seed2"`
	Path  []prng.Key `json:"path"`
	Kind  string     `json:"kind"`
	N     int        `json:"n"`
	Sides int        `json:"sides"`
	Lo    int        `json:"lo"`
	Hi    int        `json:"hi"`
	Out   []int      `json:"out"`
}

// rootVector pins the generator seed-root encoding (ADR-0016 / issue #79
// decision 1): each stage roots at Derive(stageTag, generatorID, version), and
// below that root the generator owns its addressing. We freeze one fixed
// (genID, version) encoding so T2-T4 inherit a frozen root convention. GenID and
// Version are plain Key integers.
type rootVector struct {
	Seed1    uint64     `json:"seed1"`
	Seed2    uint64     `json:"seed2"`
	StageTag prng.Key   `json:"stage_tag"`
	GenID    prng.Key   `json:"gen_id"`
	Version  prng.Key   `json:"version"`
	SubPath  []prng.Key `json:"sub_path"`
	Draw     uint64     `json:"draw"`
}

// goldenInputs enumerates the addresses whose outputs we freeze. Extend by
// APPENDING; never change an existing entry's seeds or path.
func goldenInputs() golden {
	streamPaths := [][]prng.Key{
		{prng.TagCluster},
		{prng.TagStellium, 0, 0, 0},
		{prng.TagStellium, 3, -7, 12},
		{prng.TagSystem, 3, -7, 12},       // shorter path (length is part of the address)
		{prng.TagSystem, 3, -7, 12, 1},    // stellium (3, -7, 12), system A
		{prng.TagSystem, 3, -7, 12, 2},    // sibling system B must differ
		{prng.TagPlanet, 3, -7, 12, 1, 5}, // system A, orbit 5
		{prng.TagPlayer, 1},
		{prng.TagPlayer, 2},
		{prng.TagDeposit, 3, -7, 12, 1, 5, 1}, // planet's deposit ordinal 1
		{prng.TagDeposit, 3, -7, 12, 1, 5, 2},
	}
	derivePaths := [][]prng.Key{
		{prng.TagCluster},
		{prng.TagPlayer, 42},
		{prng.TagDeposit, 3, -7, 12, 1, 5, 1},
	}
	const s1, s2 = 0x0123456789abcdef, 0xfedcba9876543210

	var g golden
	seeds := prng.New(s1, s2)
	for _, p := range streamPaths {
		st := seeds.Stream(p...)
		draws := make([]uint64, drawsPerStream)
		for i := range draws {
			draws[i] = st.Uint64()
		}
		g.Streams = append(g.Streams, streamVector{Seed1: s1, Seed2: s2, Path: p, Draws: draws})
	}
	for _, p := range derivePaths {
		child := seeds.Derive(p...)
		g.Derives = append(g.Derives, deriveVector{
			Seed1: s1, Seed2: s2, Path: p,
			WantChildDraw: child.Stream(prng.TagCluster).Uint64(),
		})
	}

	// Roll sequences: pin RollN and RollRange output order for fixed addresses.
	rollNInputs := []struct {
		path     []prng.Key
		n, sides int
	}{
		{[]prng.Key{prng.TagDeposit, 3, -7, 12, 1, 5, 1}, 3, 4}, // 3d4
		{[]prng.Key{prng.TagSystem, 3, -7, 12, 1}, 2, 6},        // 2d6
	}
	for _, in := range rollNInputs {
		roller := seeds.Roller(in.path...)
		out := make([]int, rollsPerVector)
		for i := range out {
			out[i] = roller.RollN(in.n, in.sides)
		}
		g.Rolls = append(g.Rolls, rollVector{
			Seed1: s1, Seed2: s2, Path: in.path, Kind: "rolln",
			N: in.n, Sides: in.sides, Out: out,
		})
	}
	rollRangeInputs := []struct {
		path   []prng.Key
		lo, hi int
	}{
		{[]prng.Key{prng.TagCluster}, 1, 10},
		{[]prng.Key{prng.TagDeposit, 3, -7, 12, 1, 5, 2}, -3, 3},
	}
	for _, in := range rollRangeInputs {
		roller := seeds.Roller(in.path...)
		out := make([]int, rollsPerVector)
		for i := range out {
			out[i] = roller.RollRange(in.lo, in.hi)
		}
		g.Rolls = append(g.Rolls, rollVector{
			Seed1: s1, Seed2: s2, Path: in.path, Kind: "rollrange",
			Lo: in.lo, Hi: in.hi, Out: out,
		})
	}

	// Generator seed roots: Derive(stageTag, genID, version), then a
	// generator-owned sub-path. Fixed (genID, version) = (1, 1) here.
	rootInputs := []struct {
		stageTag, genID, version prng.Key
		subPath                  []prng.Key
	}{
		{prng.TagCluster, 1, 1, []prng.Key{1, 0, 0}},
		{prng.TagDeposit, 1, 1, []prng.Key{1, 0, 0}},
	}
	for _, in := range rootInputs {
		root := seeds.Derive(in.stageTag, in.genID, in.version)
		g.Roots = append(g.Roots, rootVector{
			Seed1: s1, Seed2: s2,
			StageTag: in.stageTag, GenID: in.genID, Version: in.version,
			SubPath: in.subPath,
			Draw:    root.Stream(in.subPath...).Uint64(),
		})
	}
	return g
}

func TestGolden(t *testing.T) {
	if *update {
		writeGolden(t, goldenInputs())
		t.Log("wrote", goldenPath)
	}

	want := readGolden(t)

	for _, v := range want.Streams {
		st := prng.New(v.Seed1, v.Seed2).Stream(v.Path...)
		for i, w := range v.Draws {
			if got := st.Uint64(); got != w {
				t.Errorf("Stream(%v) draw %d = %d, want %d (frozen surface changed?)", v.Path, i, got, w)
			}
		}
	}
	for _, v := range want.Derives {
		child := prng.New(v.Seed1, v.Seed2).Derive(v.Path...)
		if got := child.Stream(prng.TagCluster).Uint64(); got != v.WantChildDraw {
			t.Errorf("Derive(%v) child draw = %d, want %d (frozen surface changed?)", v.Path, got, v.WantChildDraw)
		}
	}
	for _, v := range want.Rolls {
		roller := prng.New(v.Seed1, v.Seed2).Roller(v.Path...)
		for i, w := range v.Out {
			var got int
			switch v.Kind {
			case "rolln":
				got = roller.RollN(v.N, v.Sides)
			case "rollrange":
				got = roller.RollRange(v.Lo, v.Hi)
			default:
				t.Fatalf("unknown roll kind %q", v.Kind)
			}
			if got != w {
				t.Errorf("Roller(%v).%s roll %d = %d, want %d (frozen surface changed?)", v.Path, v.Kind, i, got, w)
			}
		}
	}
	for _, v := range want.Roots {
		root := prng.New(v.Seed1, v.Seed2).Derive(v.StageTag, v.GenID, v.Version)
		if got := root.Stream(v.SubPath...).Uint64(); got != v.Draw {
			t.Errorf("Derive(%d,%d,%d).Stream(%v) = %d, want %d (frozen root convention changed?)",
				v.StageTag, v.GenID, v.Version, v.SubPath, got, v.Draw)
		}
	}
}

// TestOrderIndependence: an address's output depends only on the address, never
// on when it is computed relative to other draws.
func TestOrderIndependence(t *testing.T) {
	seeds := prng.New(1, 2)
	a := []prng.Key{prng.TagSystem, 5, 9, -2, 1}
	b := []prng.Key{prng.TagSystem, 8, 1, 4, 2}

	// Reference: draw A on its own.
	ref := drawN(seeds.Stream(a...), 3)

	// Draw B first, then A — A must be unchanged.
	seeds.Stream(b...) // exercised, discarded
	got := drawN(seeds.Stream(a...), 3)

	if !equal(ref, got) {
		t.Errorf("A's draws changed with order: %v vs %v", ref, got)
	}
}

// TestDistinctAddresses: distinct tags, distinct instances, and distinct path
// lengths all yield uncorrelated streams (first draws differ).
func TestDistinctAddresses(t *testing.T) {
	seeds := prng.New(7, 11)
	cases := map[string][]prng.Key{
		"cluster":          {prng.TagCluster},
		"stellium-0-0-0":   {prng.TagStellium, 0, 0, 0},
		"stellium-0-0-0-0": {prng.TagStellium, 0, 0, 0, 0}, // length is part of the address
		"stellium-1-0-0":   {prng.TagStellium, 1, 0, 0},
		"system-0-0-0-A":   {prng.TagSystem, 0, 0, 0, 1}, // tag separates same-coordinate domains
		"player-1":         {prng.TagPlayer, 1},
		"player-2":         {prng.TagPlayer, 2},
	}
	seen := map[uint64]string{}
	for name, path := range cases {
		first := seeds.Stream(path...).Uint64()
		if other, ok := seen[first]; ok {
			t.Errorf("address %q collides with %q (first draw %d)", name, other, first)
		}
		seen[first] = name
	}
}

// Stream must satisfy math/rand/v2.Source (so rand.New can wrap it).
var _ rand.Source = (*prng.Stream)(nil)

func TestStreamWrapsRand(t *testing.T) {
	r := rand.New(prng.New(3, 4).Stream(prng.TagPlayer, 99))
	if n := r.IntN(6); n < 0 || n >= 6 {
		t.Errorf("IntN(6) out of range: %d", n)
	}
}

func drawN(s *prng.Stream, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = s.Uint64()
	}
	return out
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readGolden(t *testing.T) golden {
	t.Helper()
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	var g golden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	return g
}

func writeGolden(t *testing.T, g golden) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	if err := os.WriteFile(goldenPath, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}
