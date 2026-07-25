// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"zombiezen.com/go/sqlite/sqlitemigration"
)

// ApplicationID is the value ECV8 writes to PRAGMA application_id. It is the
// four ASCII bytes of "ecv8" read as a big-endian integer:
//
//	'e' 0x65  'c' 0x63  'v' 0x76  '8' 0x38  ->  0x65637638  ->  1701017144
//
// It is part of the on-disk format and must never change. It is not the
// migration version: that lives in PRAGMA user_version and is managed by
// sqlitemigration.
const ApplicationID int32 = 0x65637638

// DatabaseName is the fixed filename of a persistent ECV8 database inside its
// directory. Callers pass a directory; this name is always appended.
const DatabaseName = "ecv8.db"

//go:embed migrations/*.sql
var migrationFS embed.FS

// schema is the migration set compiled into this binary. Migrations are applied
// in filename order, and a database's user_version is the count of migrations
// already applied.
var schema = sqlitemigration.Schema{
	AppID:      ApplicationID,
	Migrations: mustLoadMigrations(),
}

// LatestMigration is the newest migration number known to this binary. A
// database whose user_version exceeds it was written by a newer build.
func LatestMigration() int { return len(schema.Migrations) }

// mustLoadMigrations reads the embedded migrations in lexical filename order.
// The files are named with a zero-padded ordinal prefix so lexical order and
// numeric order agree.
func mustLoadMigrations() []string {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		panic(fmt.Sprintf("store: glob migrations: %v", err))
	}
	if len(entries) == 0 {
		panic("store: no embedded migrations found")
	}
	sort.Strings(entries)

	migrations := make([]string, 0, len(entries))
	for _, name := range entries {
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("store: read %s: %v", path.Base(name), err))
		}
		migrations = append(migrations, string(body))
	}
	return migrations
}
