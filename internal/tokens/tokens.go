// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package tokens mints and fingerprints the opaque secrets used for session
// cookies and account activation links.
//
// Every token here comes from crypto/rand. The game engine's PRNG
// (internal/engine) must never be used for security material, and this package
// must never be used to produce game randomness.
//
// Tokens are returned to the caller exactly once, in plaintext, so they can be
// handed to a browser or displayed to an administrator. Only Fingerprint values
// are ever persisted.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Bytes is the entropy of a generated token. 256 bits is well beyond what is
// needed to make online or offline guessing infeasible.
const Bytes = 32

// New returns a fresh URL-safe token. The value is a secret: do not log it.
func New() (string, error) {
	buf := make([]byte, Bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Fingerprint returns the lowercase hex SHA-256 of token. This is the only
// representation that may be written to the database, so a stolen database
// snapshot does not yield usable sessions or activation links.
//
// A plain digest is appropriate here (unlike for passwords) because the input
// already carries 256 bits of entropy and is not guessable.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Equal compares two fingerprints in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
