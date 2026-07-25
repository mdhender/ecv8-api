// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package store owns every ECV8 SQLite database.
//
// # Return type
//
// The three constructors return *store.DB rather than *sql.DB. ZombieZen's
// documentation states plainly that zombiezen.com/go/sqlite "is not a
// database/sql driver", and the module registers no driver. Producing a real
// *sql.DB would require either importing modernc.org/sqlite directly (which
// this project forbids) or hand-writing a database/sql/driver shim, which would
// also make sqlitemigration and SQLite's one-writer/many-reader pooling
// unusable. *DB is a thin wrapper over sqlitex.Pool that keeps those facilities.
//
// # Filesystem rules
//
// This package never creates a directory. Persistent constructors take a
// directory and append the fixed filename ecv8.db. An invalid, missing, or
// unusable directory is rejected before anything is written.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"

	"github.com/mdhender/ecv8-api/internal/cerrs"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	// ErrDatabaseExists reports that ecv8.db is already present.
	ErrDatabaseExists = cerrs.Error("database already exists")
	// ErrDatabaseNotFound reports that ecv8.db is absent.
	ErrDatabaseNotFound = cerrs.Error("database not found")
	// ErrInvalidDirectory reports a directory that is missing, is not a
	// directory, or cannot satisfy the requested operation.
	ErrInvalidDirectory = cerrs.Error("invalid database directory")
	// ErrNotECV8Database reports a SQLite file whose application_id is not ours.
	ErrNotECV8Database = cerrs.Error("not an ECV8 database")
	// ErrDatabaseTooNew reports a database migrated past this binary.
	ErrDatabaseTooNew = cerrs.Error("database is newer than this binary")
	// ErrInvalidName reports an unusable temporary-store name.
	ErrInvalidName = cerrs.Error("invalid temporary database name")
	// ErrReadOnly reports a write attempted against a read-only store.
	ErrReadOnly = cerrs.Error("database is open read-only")
	// ErrNotFound reports that a requested row does not exist.
	ErrNotFound = cerrs.Error("not found")
	// ErrConflict reports that a write violated a uniqueness rule.
	ErrConflict = cerrs.Error("conflict")
)

// busyTimeoutMillis is how long a connection waits for a lock before returning
// SQLITE_BUSY.
//
// SQLite in WAL mode allows many concurrent readers with a single writer.
// Readers never block, so this budget applies almost entirely to a writer
// waiting behind another writer. Writes here are short single-statement or
// small-transaction operations, so five seconds is far more than a healthy
// system needs and short enough that a wedged writer surfaces as an error
// instead of hanging an HTTP request. Combined with writeMu (which serialises
// this process's own writers) the remaining contention is only from other
// processes, such as an operator running the CLI against a live database.
const busyTimeoutMillis = 5000

// tempNamePattern restricts temporary-store names to characters that cannot
// change a SQLite URI's meaning. Rejecting everything else is what makes the
// URI built in OpenTemporaryStore safe: a caller can never inject '?', '&', or
// '#' and thereby append query parameters that alter the open mode.
var tempNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// DB is an initialised ECV8 database and its connection pool.
//
// The pool holds several connections so readers run concurrently. writeMu
// serialises this process's writers, matching SQLite's one-writer rule and
// keeping write transactions off the busy-retry path.
type DB struct {
	pool     *sqlitex.Pool
	writeMu  sync.Mutex
	readOnly bool
	label    string
}

// poolSize returns the connection count for a pool. One connection per CPU
// gives readers real concurrency; the floor keeps small machines usable and the
// ceiling keeps SQLite's per-connection page caches bounded.
func poolSize() int {
	n := runtime.NumCPU()
	switch {
	case n < 4:
		return 4
	case n > 16:
		return 16
	}
	return n
}

// CreatePersistentStore creates ecv8.db inside dir, applies every embedded
// migration, and seeds the optional initial administrator.
//
// dir must already exist; this function never creates it. The call fails if
// ecv8.db is already present: an existing database is never truncated,
// replaced, or reopened as if it were new. If initialisation fails after the
// file is created, the newly created file is removed; a pre-existing file is
// never removed, because this function never reaches initialisation when one
// exists.
func CreatePersistentStore(ctx context.Context, dir string) (*DB, error) {
	if err := validateDirectory(dir); err != nil {
		return nil, err
	}

	// Validate EC_ADMIN_EMAIL / EC_ADMIN_SECRET before touching the filesystem,
	// so an incomplete configuration never leaves a database behind.
	admin, err := initialAdminFromEnv()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dir, DatabaseName)

	// O_EXCL is the atomic "create only if absent" primitive, so there is no
	// window between checking for the file and creating it.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s: %w", path, ErrDatabaseExists)
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("%s: %w: cannot create %s", dir, ErrInvalidDirectory, DatabaseName)
		}
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		removeNewDatabase(path)
		return nil, fmt.Errorf("create %s: %w", path, err)
	}

	db, err := openPersistent(ctx, path, false)
	if err != nil {
		removeNewDatabase(path)
		return nil, err
	}
	if err := db.seedInitialAdmin(ctx, admin); err != nil {
		_ = db.Close()
		removeNewDatabase(path)
		return nil, err
	}
	return db, nil
}

// OpenPersistentStore opens the existing ecv8.db inside dir. It never creates a
// file as a side effect of opening one.
//
// When readOnly is true the database is opened in genuine SQLite read-only
// mode, no migration runs, and no write of any kind is attempted; a database
// newer than this binary is acceptable. When readOnly is false, pending
// migrations are applied automatically, and a database newer than this binary
// is rejected without being modified.
func OpenPersistentStore(ctx context.Context, dir string, readOnly bool) (*DB, error) {
	path, err := persistentPath(dir)
	if err != nil {
		return nil, err
	}
	return openPersistent(ctx, path, readOnly)
}

// OpenTemporaryStore opens the named shared in-memory database, applying
// migrations and seeding the development accounts the first time that name is
// initialised.
//
// The name selects the database: distinct names give isolated stores, and the
// same name reconnects to a store that is still live. Reconnecting is
// non-destructive — migrations are keyed on user_version and the seed uses
// INSERT OR IGNORE — so neither the schema nor the seed rows are duplicated.
//
// A shared in-memory database exists only while at least one connection to it
// is open. The returned DB holds its whole pool open until Close, so the
// database cannot vanish while the caller is still using it.
func OpenTemporaryStore(ctx context.Context, name string) (*DB, error) {
	if !tempNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: %q must match %s", ErrInvalidName, name, tempNamePattern)
	}

	// Safe because name is already restricted to characters with no meaning in
	// a URI. mode and cache are fixed here and cannot be overridden by name.
	uri := "file:ecv8-" + name + "?mode=memory&cache=shared"
	flags := sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenURI |
		sqlite.OpenMemory | sqlite.OpenSharedCache

	// WAL is deliberately absent: an in-memory database has no journal file and
	// does not provide normal WAL behaviour. Foreign keys are still enforced.
	pool, err := sqlitex.NewPool(uri, sqlitex.PoolOptions{
		Flags:       flags,
		PoolSize:    poolSize(),
		PrepareConn: prepareConn(false),
	})
	if err != nil {
		return nil, fmt.Errorf("open temporary store %q: %w", name, err)
	}
	db := &DB{pool: pool, label: "memory:" + name}

	conn, err := pool.Take(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("open temporary store %q: %w", name, err)
	}
	err = sqlitemigration.Migrate(ctx, conn, schema)
	pool.Put(conn)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("migrate temporary store %q: %w", name, err)
	}

	if err := db.seedDevelopmentAccounts(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// openPersistent opens an existing (possibly empty) database file, verifies the
// application marker, and brings it to the migration level this binary expects.
func openPersistent(ctx context.Context, path string, readOnly bool) (*DB, error) {
	// The path is passed as a plain filename with OpenURI unset, so SQLite does
	// no URI parsing at all. That removes any possibility of a caller-supplied
	// path smuggling query parameters that would change the open mode.
	//
	// OpenCreate is never set, so SQLite will not create a missing file.
	//
	// OpenWAL is deliberately absent from the read-only flags: ZombieZen
	// implements it by running "PRAGMA journal_mode = wal" on every connection,
	// which is a write. A read-only open must not write, and WAL is a property
	// already recorded in the file, so a read-only connection inherits it.
	flags := sqlite.OpenReadWrite | sqlite.OpenWAL
	if readOnly {
		flags = sqlite.OpenReadOnly
	}

	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{
		Flags:       flags,
		PoolSize:    poolSize(),
		PrepareConn: prepareConn(readOnly),
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	conn, err := pool.Take(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// The connection must go back to the pool before the pool is closed:
	// sqlitex.Pool.Close waits for every connection to be returned, so closing
	// while holding one deadlocks.
	initErr := initializePersistent(ctx, conn, path, readOnly)
	pool.Put(conn)
	if initErr != nil {
		_ = pool.Close()
		return nil, initErr
	}

	return &DB{pool: pool, readOnly: readOnly, label: path}, nil
}

// initializePersistent validates the application marker and brings a writable
// database up to date. It performs no write when readOnly is true.
func initializePersistent(ctx context.Context, conn *sqlite.Conn, path string, readOnly bool) error {
	if readOnly {
		// The marker is checked directly rather than through sqlitemigration,
		// which would set it.
		if err := verifyApplicationID(conn); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, err := pragmaInt(conn, "user_version"); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		// A newer database is acceptable read-only: nothing is migrated and
		// nothing is written, so an older binary can still inspect it.
		return nil
	}

	// A brand new file has no schema and application_id 0; sqlitemigration
	// adopts it. Any other mismatch must be rejected before we write anything.
	empty, err := isEmptyDatabase(conn)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !empty {
		if err := verifyApplicationID(conn); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	current, err := pragmaInt(conn, "user_version")
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if latest := LatestMigration(); current > latest {
		return fmt.Errorf("%s: %w: database is at migration %d, binary knows %d",
			path, ErrDatabaseTooNew, current, latest)
	}

	// WAL is a property of the file, not of a connection, but it is set and
	// verified on every writable open so a database restored from a non-WAL
	// copy is corrected rather than silently left in rollback-journal mode.
	if err := ensureWAL(conn); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := sqlitemigration.Migrate(ctx, conn, schema); err != nil {
		return fmt.Errorf("migrate %s: %w", path, err)
	}
	return nil
}

// prepareConn returns the per-connection setup for a pool.
//
// SQLite pragmas are per-connection, and sqlitex may open connections lazily,
// so these must be applied here rather than once at startup. Otherwise a
// connection acquired later would silently run without foreign keys.
func prepareConn(readOnly bool) sqlitex.ConnPrepareFunc {
	return func(conn *sqlite.Conn) error {
		if err := sqlitex.ExecuteTransient(conn,
			fmt.Sprintf("PRAGMA busy_timeout = %d;", busyTimeoutMillis), nil); err != nil {
			return fmt.Errorf("set busy_timeout: %w", err)
		}
		if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = ON;", nil); err != nil {
			return fmt.Errorf("enable foreign keys: %w", err)
		}
		if readOnly {
			// Belt and braces alongside SQLITE_OPEN_READONLY: query_only makes
			// an accidental write fail loudly inside this process too.
			if err := sqlitex.ExecuteTransient(conn, "PRAGMA query_only = ON;", nil); err != nil {
				return fmt.Errorf("set query_only: %w", err)
			}
			return nil
		}
		if err := sqlitex.ExecuteTransient(conn, "PRAGMA synchronous = NORMAL;", nil); err != nil {
			return fmt.Errorf("set synchronous: %w", err)
		}
		return nil
	}
}

// ensureWAL switches the database to WAL and verifies the switch took effect.
func ensureWAL(conn *sqlite.Conn) error {
	var mode string
	err := sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode = WAL;", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			mode = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("enable WAL: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("enable WAL: journal_mode is %q", mode)
	}
	return nil
}

// verifyApplicationID fails unless the file carries the ECV8 marker.
func verifyApplicationID(conn *sqlite.Conn) error {
	got, err := pragmaInt(conn, "application_id")
	if err != nil {
		return err
	}
	if int32(got) != ApplicationID {
		return fmt.Errorf("%w: application_id is %#x, want %#x",
			ErrNotECV8Database, uint32(int32(got)), uint32(ApplicationID))
	}
	return nil
}

// isEmptyDatabase reports whether the file contains no schema objects yet.
func isEmptyDatabase(conn *sqlite.Conn) (bool, error) {
	var count int
	err := sqlitex.ExecuteTransient(conn, "SELECT count(*) FROM sqlite_master;", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		return false, fmt.Errorf("read schema: %w", err)
	}
	return count == 0, nil
}

// pragmaInt reads a single-value integer pragma. The name is never
// caller-supplied: pragmas do not accept bound parameters.
func pragmaInt(conn *sqlite.Conn, name string) (int, error) {
	var value int
	err := sqlitex.ExecuteTransient(conn, "PRAGMA "+name+";", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		return 0, fmt.Errorf("read pragma %s: %w", name, err)
	}
	return value, nil
}

// validateDirectory rejects a directory this package may not use. It never
// creates anything.
func validateDirectory(dir string) error {
	if dir == "" {
		return fmt.Errorf("%w: path is empty", ErrInvalidDirectory)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w: does not exist", dir, ErrInvalidDirectory)
		}
		return fmt.Errorf("%s: %w: %v", dir, ErrInvalidDirectory, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: %w: not a directory", dir, ErrInvalidDirectory)
	}
	return nil
}

// persistentPath validates dir and returns the path of an existing ecv8.db.
func persistentPath(dir string) (string, error) {
	if err := validateDirectory(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, DatabaseName)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s: %w", path, ErrDatabaseNotFound)
		}
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s: %w: not a regular file", path, ErrDatabaseNotFound)
	}
	return path, nil
}

// removeNewDatabase deletes a database this package just created, along with
// its WAL sidecars. It is only ever called on a path that O_EXCL proved absent
// moments earlier, so it can never delete a pre-existing database.
func removeNewDatabase(path string) {
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(name)
	}
}

// ReadOnly reports whether writes are rejected.
func (db *DB) ReadOnly() bool { return db.readOnly }

// Read runs fn with a pooled connection. Reads execute concurrently.
func (db *DB) Read(ctx context.Context, fn func(conn *sqlite.Conn) error) error {
	conn, err := db.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer db.pool.Put(conn)
	return fn(conn)
}

// Write runs fn inside an immediate transaction, serialised against every other
// writer in this process. The transaction is rolled back if fn returns an
// error.
//
// BEGIN IMMEDIATE takes the write lock up front so contention is resolved by
// busy_timeout instead of surfacing as a late SQLITE_BUSY on first write.
func (db *DB) Write(ctx context.Context, fn func(conn *sqlite.Conn) error) (err error) {
	if db.readOnly {
		return fmt.Errorf("%s: %w", db.label, ErrReadOnly)
	}
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	conn, err := db.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer db.pool.Put(conn)

	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer end(&err)

	return fn(conn)
}

// MigrationVersion returns the database's current migration number, which is
// stored in PRAGMA user_version by sqlitemigration.
func (db *DB) MigrationVersion(ctx context.Context) (int, error) {
	var version int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		v, err := pragmaInt(conn, "user_version")
		version = v
		return err
	})
	return version, err
}

// Ping verifies the database still answers a trivial query. Readiness checks
// use it, so it must stay cheap and must not write.
func (db *DB) Ping(ctx context.Context) error {
	return db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn, "SELECT 1;", nil)
	})
}

// Close releases every connection. For a temporary store this discards the
// in-memory database.
func (db *DB) Close() error {
	if err := db.pool.Close(); err != nil {
		return fmt.Errorf("close %s: %w", db.label, err)
	}
	return nil
}
