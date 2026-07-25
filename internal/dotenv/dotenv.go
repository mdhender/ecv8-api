// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package dotenv wraps the joho/godotenv package to load environment files.
package dotenv

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/mdhender/ecv8-api/internal/cerrs"
)

const (
	ErrMissingEnvironment = cerrs.Error("missing environment")
	ErrUnknownEnvironment = cerrs.Error("unknown environment")
)

// Load uses the `joho/godotenv` package to load environment files in the working directory.
//
// The env must be one of "development", "test", "production", or "agent".
// The "agent" environment is reserved for the coding agent's local work.
//
// Load the following files depending on `env`, with the first file having the highest precedence,
// and .env having the lowest precedence:
//
// Priority  Filename__________   .gitignore?  Secrets?  Notes_______________________________
// Highest   .env.{{env}}.local   Yes          Yes       Environment-specific local overrides
// 2nd       .env.local           Yes          Yes       Local overrides
// 3rd       .env.{{env}}         No           Never     Shared environment-specific variables
// Lowest    .env                 No           Never     Shared for all environments
//
// godotenv.Load never overwrites a variable that is already set, so a value from
// the real process environment always wins over every file, and the first file in
// the list below that defines a key wins over the later ones.
//
// WARNING: we are incompatible with bkeepers/dotenv since we load `.env.local` in test.
// Read https://github.com/bkeepers/dotenv/issues/418 for the history of this decision.
func Load(env string) error {
	if env == "" {
		return ErrMissingEnvironment
	}
	if env != "development" && env != "test" && env != "production" && env != "agent" {
		return ErrUnknownEnvironment
	}

	for _, path := range []string{
		".env." + env + ".local", // highest priority: .env.{{env}}.local
		".env.local",             // 2nd     priority: .env.local
		".env." + env,            // 3rd     priority: .env.{{env}}
		".env",                   // lowest  priority: .env
	} {
		// verify path exists and is a regular file
		if sb, err := os.Stat(path); err != nil || !sb.Mode().IsRegular() {
			continue
		}
		// load file and return any errors
		if err := godotenv.Load(path); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	return nil
}
