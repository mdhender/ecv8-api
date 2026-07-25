// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package config

import (
	"fmt"
	"strings"

	"github.com/peterbourgon/ff/v4"
)

// Database is the configuration a database command needs, and nothing more.
//
// It is separate from Config because ecdb is a separate binary from the server.
// A command that never opens a listener should not accept --listen-addr, and
// must not fail validation over a --public-base-url or a cookie policy it will
// never use. The one setting the two binaries genuinely share, --db-path, is
// registered by the same helper for both, so the flag name, default, and help
// text cannot drift apart as the two diverge.
type Database struct {
	// Env selects which dotenv files were loaded. It is read from EC_ENV before
	// flag parsing, so it is not itself a flag.
	Env string

	// DBPath is the directory holding ecv8.db. The filename is fixed.
	DBPath string

	// Quiet suppresses the status lines that report what a command did. It
	// never suppresses a value a command was asked to produce — a path, a
	// version number — because a script that captures those would otherwise
	// read an empty string as a success.
	Quiet bool
}

// BindDatabase registers every database flag on fs and returns the Database the
// flags write into. The returned value is only valid after fs has been parsed
// and ValidateDatabase has succeeded.
func BindDatabase(fs *ff.FlagSet) *Database {
	cfg := Database{
		Env:    defaultEnv,
		DBPath: defaultDBPath,
	}
	bindDBPath(fs, &cfg.DBPath)
	// Not registered by a shared helper: ecapi has no use for it, and a server
	// that could be told to say less about what it is doing would be a worse
	// server, not a quieter one.
	fs.BoolVar(&cfg.Quiet, 0, "quiet", "suppress status output; values a command was asked for are still printed")
	return &cfg
}

// ValidateDatabase checks the parsed configuration and reports the first
// problem in terms an operator can act on.
//
// It deliberately says nothing about whether the directory exists or holds a
// database: internal/store owns those rules, applies them to the real path at
// the moment it is used, and reports them without the race a check here would
// introduce.
func ValidateDatabase(cfg *Database) error {
	if strings.TrimSpace(cfg.DBPath) == "" {
		return fmt.Errorf("--db-path is required")
	}
	return nil
}
