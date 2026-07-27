// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package prng

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
)

// Key is a single element of a key path (an "address"). Domain tags, ids, and
// coordinates all coerce into this one type: whether an element names a purpose
// or an instance is a matter of convention and position, not of type. Key is
// int64 and is written into the hash big-endian; changing that coercion is a
// frozen-surface change (see the package doc).
type Key int64

// Seeds is a game's two uint64 master seeds — the root of all randomness. It is
// a factory, not a draw source: it produces Streams to draw from and child
// Seeds for subsystems. Same seeds -> identical game on any machine.
type Seeds struct {
	seed1, seed2 uint64
}

// New returns the Seeds rooted at the given master seeds.
func New(seed1, seed2 uint64) Seeds {
	return Seeds{seed1: seed1, seed2: seed2}
}

// Stream returns the draw source addressed by path, a pure function of the
// seeds and the path:
//
//	stream = PCG( SHA-256(seed1, seed2, len(path), path...) )
//
// The first path element must be a domain tag (see tags.go); the rest identify
// the instance. Two calls with the same path return equivalent streams; when
// and in what order they are computed does not matter.
func (s Seeds) Stream(path ...Key) *Stream {
	digest := s.hash(path...)
	seed1 := binary.BigEndian.Uint64(digest[0:8])
	seed2 := binary.BigEndian.Uint64(digest[8:16])
	return &Stream{pcg: rand.NewPCG(seed1, seed2)}
}

// Derive returns a child Seeds addressed by path, for a subsystem that carries
// its own randomness (and may itself Stream or Derive). It uses the identical
// hash as Stream; only the destination of the first 128 bits differs — here
// they become a new (seed1, seed2) root rather than PCG state.
func (s Seeds) Derive(path ...Key) Seeds {
	digest := s.hash(path...)
	return Seeds{
		seed1: binary.BigEndian.Uint64(digest[0:8]),
		seed2: binary.BigEndian.Uint64(digest[8:16]),
	}
}

// hash computes SHA-256 over the length-prefixed, big-endian encoding of the
// seeds and the path:
//
//	seed1 (uint64) || seed2 (uint64) || len(path) (int64) || path[0] (int64) || ...
//
// The length prefix keeps different path depths distinct (e.g. [K,q] and
// [K,q,r] do not hash alike). This encoding is a FROZEN SURFACE: once any game
// exists its outcomes are welded to it, so element order, the int64/uint64
// coercions, the big-endian layout, and the length prefix must never change.
func (s Seeds) hash(path ...Key) [32]byte {
	h := sha256.New()
	var buf [8]byte

	binary.BigEndian.PutUint64(buf[:], s.seed1)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], s.seed2)
	h.Write(buf[:])

	binary.BigEndian.PutUint64(buf[:], uint64(int64(len(path))))
	h.Write(buf[:])

	for _, k := range path {
		binary.BigEndian.PutUint64(buf[:], uint64(int64(k)))
		h.Write(buf[:])
	}

	var digest [32]byte
	h.Sum(digest[:0])
	return digest
}

// Stream is a private draw source seeded from a hashed key path. It implements
// math/rand/v2.Source, so callers can wrap it with rand.New(stream) for the
// full *rand.Rand API.
type Stream struct {
	pcg *rand.PCG
}

// Uint64 returns the next 64 random bits, satisfying math/rand/v2.Source.
func (s *Stream) Uint64() uint64 {
	return s.pcg.Uint64()
}
