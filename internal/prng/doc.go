// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package prng is the determinism foundation for the EC engine: same master
// seeds -> identical game on any machine, independent of the order draws are
// made or of Go-map iteration order.
//
// # Mechanism
//
// A game has two uint64 master seeds held by [Seeds]. Randomness is not a
// sequence you consume but a function you query: every draw has an address — a
// [Key] path whose first element is a domain tag (see tags.go) and whose
// remaining elements identify the instance (a stellium's coordinates, a player
// number). Instance keys must be intrinsic to the game — map coordinates, never
// SQLite autoincrement row ids, whose values depend on insertion order.
// A private stream is derived by hashing that address with the seeds:
//
//	stream = PCG( SHA-256(seed1, seed2, len(path), path...) )   // all big-endian
//
// The first 16 bytes of the digest seed a math/rand/v2 PCG. Order independence
// and reproducibility fall out of the construction: nothing in the address
// depends on iteration order, so the system at (3, -7, 12, A) has one address,
// one stream, one set of contents — fixed forever for a given pair of seeds.
//
// # Two operations, one hash
//
//   - [Seeds.Stream] returns a *[Stream] — the draw source. The 128 bits become
//     PCG state you read numbers from. Stream implements math/rand/v2.Source, so
//     rand.New(stream) yields the full *rand.Rand API.
//   - [Seeds.Derive] returns a child [Seeds] — a new (seed1, seed2) root for a
//     subsystem that carries its own randomness and may itself Stream or Derive.
//
// Both use the identical hash; only the destination of the first 128 bits
// differs.
//
// # Frozen surfaces — do not change while any game exists
//
// The key-path encoding (element order, the int64/uint64 coercions, the
// big-endian layout, the length prefix) and the domain-tag numbering
// (append-only, starting at 1) are a compatibility surface the moment any game
// exists — no less than a save-file format. Golden vectors in testdata pin
// (seed1, seed2, path) -> output and fail CI on any drift.
//
// SHA-256 is heavier than a purpose-built mixer, but EC draws thousands of
// numbers, not billions, so the cost is invisible and mixing quality comes for
// free.
//
// The spec is doc/determinism.md; the rationale and prior art (Random123, NumPy
// SeedSequence, JAX fold_in) are in doc/counter-based-prng.md; the decision is
// ADR-0001. See
// https://github.com/mdhender/ecv6-api/blob/main/doc/determinism.md
package prng
