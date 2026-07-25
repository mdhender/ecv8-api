// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"zombiezen.com/go/sqlite/sqlitex"
)

// This file is the storage half of the ecdb maintenance commands: migrating,
// backing up, and compacting a database that already exists.
//
// None of it is reachable from the server. ecapi migrates on open because that
// is what keeps the running binary and the schema in step, and it has no reason
// to VACUUM a live database; these operations are deliberate, operator-driven,
// and belong to the command that owns the file.
//
// Backup and compaction both refuse a database that is not at this binary's
// migration level, rather than migrating it first. An operator asking for a
// copy of a database must not be handed a schema change as a side effect, and
// an operator compacting one must not discover that ecdb upgraded it. Bringing
// a database forward is what MigratePersistent is for, and it is a separate
// decision.

// backupTimeLayout names a backup for the instant it was taken, in UTC, with a
// literal Z appended by the caller. It is deliberately free of punctuation that
// needs quoting in a shell.
const backupTimeLayout = "20060102T150405"

// MigratePersistent applies any pending migrations to the ecv8.db in dir and
// reports whether it applied any.
//
// It exists so an operator can bring a database forward at a moment of their
// choosing — before deploying a new binary, or against a restored backup —
// rather than discovering the migration when the server next starts. Opening
// writably is what migrates, so this is that open and nothing else.
func MigratePersistent(ctx context.Context, dir string) (applied bool, err error) {
	path, err := persistentPath(dir)
	if err != nil {
		return false, err
	}

	// The version reported is the one the file carried before this open, so the
	// comparison describes what this call did rather than what it found.
	db, before, err := openPersistent(ctx, path, persistentOptions{})
	if err != nil {
		return false, err
	}
	if err := db.Close(); err != nil {
		return false, err
	}
	return before < LatestMigration(), nil
}

// BackupPersistent writes a consistent, compacted copy of the ecv8.db in dir to
// outputDir and returns the path it created.
//
// Both directories must already exist; this package never creates one. The
// source is opened read-only, so a backup can be taken while the server is
// running and cannot modify what it is copying. VACUUM INTO is used rather than
// a file copy because it reads a single committed snapshot: copying ecv8.db
// with cp while a writer is active can capture a torn file whose WAL sidecar
// does not match it.
func BackupPersistent(ctx context.Context, dir, outputDir string, includeVersion bool) (outputPath string, err error) {
	if err := validateDirectory(outputDir); err != nil {
		return "", err
	}
	path, err := persistentPath(dir)
	if err != nil {
		return "", err
	}

	db, version, err := openPersistent(ctx, path, persistentOptions{readOnly: true, allowVacuumInto: true})
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
	}()

	if latest := LatestMigration(); version != latest {
		return "", fmt.Errorf("%s: %w: database is at migration %d, binary knows %d",
			path, ErrMigrationMismatch, version, latest)
	}

	name := DatabaseName + "." + Now().Format(backupTimeLayout) + "Z"
	if includeVersion {
		name += "-" + strconv.Itoa(version)
	}
	outputPath = filepath.Join(outputDir, name)

	// Refusing an existing file here, rather than letting SQLite refuse it, is
	// what makes the cleanup below safe: after this check, anything at
	// outputPath was created by this call and can be removed. It also means a
	// backup can never overwrite an earlier one.
	if _, statErr := os.Stat(outputPath); statErr == nil {
		return "", fmt.Errorf("%s: %w", outputPath, ErrDatabaseExists)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", outputPath, statErr)
	}

	if err := db.vacuum(ctx, "VACUUM INTO ?;", outputPath); err != nil {
		// A failed VACUUM INTO leaves its part-written target behind, and a
		// truncated file that looks like a backup is worse than no backup.
		_ = os.Remove(outputPath)
		return "", fmt.Errorf("back up %s to %s: %w", path, outputPath, err)
	}

	// SQLite creates the backup with the process umask, which is usually 0644.
	// A backup holds every password hash and session fingerprint the database
	// does, so it gets the same 0600 the database itself is created with.
	if err := os.Chmod(outputPath, 0o600); err != nil {
		_ = os.Remove(outputPath)
		return "", fmt.Errorf("back up %s to %s: %w", path, outputPath, err)
	}
	return outputPath, nil
}

// CompactPersistent reclaims unused space in the ecv8.db in dir.
//
// VACUUM rewrites the whole database into a fresh file and swaps it in, so it
// needs room for a second copy and takes an exclusive lock for the duration.
// That makes it an operator's decision on an idle database, not something the
// server should ever do to itself.
func CompactPersistent(ctx context.Context, dir string) (err error) {
	path, err := persistentPath(dir)
	if err != nil {
		return err
	}

	db, version, err := openPersistent(ctx, path, persistentOptions{skipMigrate: true})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
	}()

	if latest := LatestMigration(); version != latest {
		return fmt.Errorf("%s: %w: database is at migration %d, binary knows %d",
			path, ErrMigrationMismatch, version, latest)
	}
	if err := db.vacuum(ctx, "VACUUM;"); err != nil {
		return fmt.Errorf("compact %s: %w", path, err)
	}
	return nil
}

// vacuum runs a VACUUM statement on a pooled connection.
//
// It cannot go through Write, because SQLite refuses to VACUUM inside a
// transaction and Write opens one. It still takes writeMu, so a VACUUM is
// serialised against this process's other writers exactly as a write would be;
// concurrency with another process is left to busy_timeout.
func (db *DB) vacuum(ctx context.Context, query string, args ...any) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	conn, err := db.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer db.pool.Put(conn)

	var opts *sqlitex.ExecOptions
	if len(args) != 0 {
		opts = &sqlitex.ExecOptions{Args: args}
	}
	return sqlitex.ExecuteTransient(conn, query, opts)
}
