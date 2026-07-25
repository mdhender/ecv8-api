// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package cerrs defines constant errors.
package cerrs

// Error is a string that implements the error interface, allowing sentinel
// errors to be declared as untyped constants.
type Error string

// Error implements the error interface.
func (e Error) Error() string { return string(e) }
