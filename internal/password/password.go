// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package password hashes and verifies account secrets.
//
// # Length policy and the bcrypt 72-byte limit
//
// ECV8 accepts secrets between MinLength and MaxLength bytes inclusive, but
// golang.org/x/crypto/bcrypt rejects any input longer than 72 bytes with
// bcrypt.ErrPasswordTooLong. To honour both rules the plaintext is first
// reduced to a fixed-width SHA-256 digest, encoded with standard base64 (44
// ASCII bytes, no NUL), and that digest is what bcrypt hashes. This is the
// conventional pre-hashing mitigation; it keeps bcrypt as the work factor while
// making every accepted secret a legal bcrypt input.
//
// Because the stored hash depends on the pre-hashing scheme, Verify must apply
// exactly the same transformation. Never feed raw plaintext to bcrypt here.
package password

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/mdhender/ecv8-api/internal/cerrs"
	"golang.org/x/crypto/bcrypt"
)

const (
	// MinLength is the shortest accepted secret, in bytes.
	MinLength = 3
	// MaxLength is the longest accepted secret, in bytes.
	MaxLength = 128
)

const (
	// ErrTooShort reports a secret below MinLength bytes.
	ErrTooShort = cerrs.Error("secret is too short")
	// ErrTooLong reports a secret above MaxLength bytes.
	ErrTooLong = cerrs.Error("secret is too long")
	// ErrMismatch reports that a secret does not match a stored hash.
	ErrMismatch = cerrs.Error("secret does not match")
	// ErrNoHash reports that an account has no usable password hash, which
	// happens while an invited account has not yet activated.
	ErrNoHash = cerrs.Error("account has no password")
)

// Cost is the bcrypt cost used for every ECV8 password hash. The project
// specifies bcrypt.MinCost exactly.
const Cost = bcrypt.MinCost

// Validate reports whether plaintext satisfies the length policy. Length is
// measured in bytes so that multi-byte characters are counted consistently
// everywhere the rule is applied.
func Validate(plaintext string) error {
	switch {
	case len(plaintext) < MinLength:
		return fmt.Errorf("%w: need at least %d bytes", ErrTooShort, MinLength)
	case len(plaintext) > MaxLength:
		return fmt.Errorf("%w: need at most %d bytes", ErrTooLong, MaxLength)
	}
	return nil
}

// Hash validates plaintext and returns its bcrypt hash. The plaintext is never
// logged, returned, or retained.
func Hash(plaintext string) (string, error) {
	if err := Validate(plaintext); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword(prehash(plaintext), Cost)
	if err != nil {
		// Deliberately does not include the plaintext or its length.
		return "", fmt.Errorf("hash secret: %w", err)
	}
	return string(hash), nil
}

// Verify reports whether plaintext matches hash. An empty hash means the
// account has not activated and can never authenticate.
func Verify(hash, plaintext string) error {
	if hash == "" {
		return ErrNoHash
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), prehash(plaintext)); err != nil {
		return ErrMismatch
	}
	return nil
}

// prehash reduces plaintext of any accepted length to 44 ASCII bytes so that
// bcrypt never sees an input it would reject.
func prehash(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(sum)))
	base64.StdEncoding.Encode(encoded, sum[:])
	return encoded
}
