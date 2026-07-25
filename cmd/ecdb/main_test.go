// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// These tests drive run with real arguments, the way a shell does, because the
// contract worth protecting is the command's: what it prints, what it refuses,
// and what it leaves on disk afterwards. They reach past the command only to
// inspect a database, never to arrange one a command could have made itself.
package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/version"
	"github.com/peterbourgon/ff/v4"
	zsqlite "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestDatabaseCreate(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()

	stdout, _, err := exec(t, "database", "create", "--db-path", dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	path := filepath.Join(dir, store.DatabaseName)
	if want := path + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := databaseSnapshot(t, dir); got.userVersion != store.LatestMigration() {
		t.Errorf("user_version = %d, want %d", got.userVersion, store.LatestMigration())
	}
}

// A second create must never truncate the first. This is the one command that
// can destroy a database by doing its own job twice.
func TestDatabaseCreateRefusesExisting(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	createDatabase(t, dir)
	before := digest(t, filepath.Join(dir, store.DatabaseName))

	_, _, err := exec(t, "database", "create", "--db-path", dir)
	if !errors.Is(err, store.ErrDatabaseExists) {
		t.Fatalf("create error = %v, want ErrDatabaseExists", err)
	}
	if after := digest(t, filepath.Join(dir, store.DatabaseName)); after != before {
		t.Error("existing database was modified by a refused create")
	}
}

// An operator who mistypes a directory must get an error, not a new database in
// a new directory that then looks empty to everything else.
func TestDatabaseCreateNeverCreatesTheDirectory(t *testing.T) {
	isolateEnv(t)
	missing := filepath.Join(t.TempDir(), "missing")

	_, _, err := exec(t, "database", "create", "--db-path", missing)
	if !errors.Is(err, store.ErrInvalidDirectory) {
		t.Fatalf("create error = %v, want ErrInvalidDirectory", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory was created: %v", err)
	}
}

func TestDatabaseCreateInitialAdministrator(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		secret    string
		wantAdmin int
		wantErr   bool
	}{
		{name: "neither", wantAdmin: 0},
		{name: "both", email: "admin@example.com", secret: "a-good-secret", wantAdmin: 1},
		{name: "email only", email: "admin@example.com", wantErr: true},
		{name: "secret only", secret: "a-good-secret", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv(config.EnvVarPrefix+"_ADMIN_EMAIL", tt.email)
			t.Setenv(config.EnvVarPrefix+"_ADMIN_SECRET", tt.secret)
			dir := t.TempDir()

			_, _, err := exec(t, "database", "create", "--db-path", dir)
			path := filepath.Join(dir, store.DatabaseName)
			if tt.wantErr {
				if err == nil {
					t.Fatal("create succeeded, want an error")
				}
				// Nothing is created, so an operator can fix the variable and
				// run the same command again.
				if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("database left behind after a refused create: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if got := countAccounts(t, dir); got != tt.wantAdmin {
				t.Errorf("accounts = %d, want %d", got, tt.wantAdmin)
			}
		})
	}
}

func TestDatabaseVerifyAndVersion(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	createDatabase(t, dir)
	latest := store.LatestMigration()

	stdout, _, err := exec(t, "database", "verify", "--db-path", dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	want := fmt.Sprintf("%s: migration %d of %d\n", filepath.Join(dir, store.DatabaseName), latest, latest)
	if stdout != want {
		t.Errorf("verify stdout = %q, want %q", stdout, want)
	}

	// version is the form a script compares, so it stays a bare number.
	stdout, _, err = exec(t, "database", "version", "--db-path", dir)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if want := strconv.Itoa(latest) + "\n"; stdout != want {
		t.Errorf("version stdout = %q, want %q", stdout, want)
	}
}

// Inspecting a database must never change it, including one this binary is too
// old for: an older build has to stay able to read a newer file.
func TestDatabaseInspectionNeverMutates(t *testing.T) {
	for _, command := range []string{"verify", "version"} {
		t.Run(command, func(t *testing.T) {
			isolateEnv(t)
			dir := t.TempDir()
			createDatabase(t, dir)
			ahead := store.LatestMigration() + 1
			setMigrationLevel(t, dir, ahead)
			before := databaseSnapshot(t, dir)

			stdout, _, err := exec(t, "database", command, "--db-path", dir)
			if err != nil {
				t.Fatalf("%s: %v", command, err)
			}
			if !strings.Contains(stdout, strconv.Itoa(ahead)) {
				t.Errorf("%s stdout = %q, want it to report migration %d", command, stdout, ahead)
			}
			if after := databaseSnapshot(t, dir); !after.equal(before) {
				t.Errorf("%s changed the database: %+v -> %+v", command, before, after)
			}
		})
	}
}

func TestDatabaseUpgrade(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	newEmptyDatabase(t, dir)
	path := filepath.Join(dir, store.DatabaseName)
	latest := store.LatestMigration()

	stdout, _, err := exec(t, "database", "upgrade", "--db-path", dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if want := fmt.Sprintf("%s: migrations applied (migration %d)\n", path, latest); stdout != want {
		t.Errorf("first upgrade stdout = %q, want %q", stdout, want)
	}

	// Reporting "no migrations applied" is the answer to "is this database
	// ready for the new binary?", so a second run must say so rather than
	// repeating the first run's line or falling silent.
	stdout, _, err = exec(t, "database", "upgrade", "--db-path", dir)
	if err != nil {
		t.Fatalf("second upgrade: %v", err)
	}
	if want := fmt.Sprintf("%s: no migrations applied (migration %d)\n", path, latest); stdout != want {
		t.Errorf("second upgrade stdout = %q, want %q", stdout, want)
	}
	if got := databaseSnapshot(t, dir); got.userVersion != latest {
		t.Errorf("user_version = %d, want %d", got.userVersion, latest)
	}
}

func TestDatabaseUpgradeQuiet(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	newEmptyDatabase(t, dir)

	stdout, _, err := exec(t, "--quiet", "database", "upgrade", "--db-path", dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if got := databaseSnapshot(t, dir); got.userVersion != store.LatestMigration() {
		t.Error("--quiet suppressed the work as well as the status line")
	}
}

func TestDatabaseBackup(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	createDatabase(t, dir)
	outputDir := t.TempDir()
	before := databaseSnapshot(t, dir)

	stdout, _, err := exec(t, "database", "backup", "--db-path", dir, "--output-path", outputDir, "--version")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	created := strings.TrimSuffix(stdout, "\n")

	pattern := filepath.Join(outputDir, store.DatabaseName+".*Z-"+strconv.Itoa(store.LatestMigration()))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(matches) != 1 || matches[0] != created {
		t.Fatalf("matches = %v, want exactly the printed path %q", matches, created)
	}

	// A backup holds every password hash the database does, so it must not be
	// left at the process umask.
	info, err := os.Stat(created)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %#o, want 0600", perm)
	}

	// The copy is a database in its own right, not just bytes on disk. Journal
	// mode is excluded deliberately: VACUUM INTO writes a fresh file in the
	// default rollback mode rather than WAL, which is what makes a backup a
	// single self-contained file with no sidecars to lose.
	if got := backupSnapshot(t, created); !got.sameContents(before) {
		t.Errorf("backup contents = %+v, want the source's %+v", got, before)
	}
	if after := databaseSnapshot(t, dir); !after.equal(before) {
		t.Error("backup modified its source")
	}
	if sidecars, _ := filepath.Glob(created + "-*"); len(sidecars) != 0 {
		t.Errorf("backup left sidecars: %v", sidecars)
	}
}

func TestDatabaseBackupDefaultsOutputPathToDBPath(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	createDatabase(t, dir)

	stdout, _, err := exec(t, "database", "backup", "--db-path", dir)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := filepath.Dir(strings.TrimSuffix(stdout, "\n")); got != dir {
		t.Errorf("backup directory = %q, want %q", got, dir)
	}
}

func TestDatabaseBackupRefusesMissingOutputDirectory(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	createDatabase(t, dir)
	missing := filepath.Join(t.TempDir(), "missing")

	_, _, err := exec(t, "database", "backup", "--db-path", dir, "--output-path", missing)
	if !errors.Is(err, store.ErrInvalidDirectory) {
		t.Fatalf("backup error = %v, want ErrInvalidDirectory", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing output directory was created: %v", err)
	}
}

// Backing up or compacting must never be the operation that changes a schema,
// and a refusal must leave the file exactly as it was found — journal mode
// included, which a writable open would otherwise rewrite on its way in.
func TestMaintenanceRefusesAMigrationMismatch(t *testing.T) {
	for _, command := range []string{"backup", "compact"} {
		t.Run(command, func(t *testing.T) {
			isolateEnv(t)
			dir := t.TempDir()
			newEmptyDatabase(t, dir)
			path := filepath.Join(dir, store.DatabaseName)
			before, beforeState := digest(t, path), databaseSnapshot(t, dir)

			outputDir := t.TempDir()
			args := []string{"database", command, "--db-path", dir}
			if command == "backup" {
				args = append(args, "--output-path", outputDir)
			}

			_, _, err := exec(t, args...)
			if !errors.Is(err, store.ErrMigrationMismatch) {
				t.Fatalf("%s error = %v, want ErrMigrationMismatch", command, err)
			}
			if after := digest(t, path); after != before {
				t.Errorf("%s modified a database it refused", command)
			}
			if after := databaseSnapshot(t, dir); !after.equal(beforeState) {
				t.Errorf("%s changed database state: %+v -> %+v", command, beforeState, after)
			}
			if entries, err := os.ReadDir(outputDir); err != nil || len(entries) != 0 {
				t.Errorf("output directory = %v (err %v), want it left empty", entries, err)
			}
		})
	}
}

// A database ahead of this binary is refused for writing without being touched.
// There is no downgrade path, so guessing would be worse than stopping.
func TestWritableCommandsRefuseANewerDatabase(t *testing.T) {
	for _, command := range []string{"upgrade", "compact"} {
		t.Run(command, func(t *testing.T) {
			isolateEnv(t)
			dir := t.TempDir()
			createDatabase(t, dir)
			setMigrationLevel(t, dir, store.LatestMigration()+1)
			before := databaseSnapshot(t, dir)

			_, _, err := exec(t, "database", command, "--db-path", dir)
			if !errors.Is(err, store.ErrDatabaseTooNew) {
				t.Fatalf("%s error = %v, want ErrDatabaseTooNew", command, err)
			}
			if after := databaseSnapshot(t, dir); !after.equal(before) {
				t.Errorf("%s changed a database it refused: %+v -> %+v", command, before, after)
			}
		})
	}
}

func TestDatabaseCompact(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	createDatabase(t, dir)
	before := databaseSnapshot(t, dir)

	stdout, _, err := exec(t, "database", "compact", "--db-path", dir)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	// Space is reclaimed; contents are not. Compaction that lost a row, a
	// migration level, or WAL mode would be a data-loss bug, not an optimisation.
	if after := databaseSnapshot(t, dir); !after.equal(before) {
		t.Errorf("compact changed the database: %+v -> %+v", before, after)
	}
}

func TestCommandsRejectMissingDatabase(t *testing.T) {
	commands := [][]string{
		{"database", "verify"},
		{"database", "version"},
		{"database", "upgrade"},
		{"database", "backup"},
		{"database", "compact"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateEnv(t)
			dir := t.TempDir()

			_, _, err := exec(t, append(args, "--db-path", dir)...)
			if !errors.Is(err, store.ErrDatabaseNotFound) {
				t.Fatalf("error = %v, want ErrDatabaseNotFound", err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("directory = %v, want nothing created", entries)
			}
		})
	}
}

// The build version answers a different question from the database's migration
// number, and must not be refused for a --db-path it never reads. Both commands
// are named "version", so this is the guard against dispatching on the name.
func TestVersionNeedsNoDatabase(t *testing.T) {
	isolateEnv(t)

	stdout, _, err := exec(t, "version", "--db-path", "")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if want := fmt.Sprintln(version.Version); stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	if _, _, err := exec(t, "database", "version", "--db-path", ""); err == nil ||
		!strings.Contains(err.Error(), "--db-path is required") {
		t.Fatalf("database version error = %v, want it to require --db-path", err)
	}
}

func TestDBPathComesFromTheEnvironment(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	createDatabase(t, dir)
	t.Setenv(config.EnvVarPrefix+"_DB_PATH", dir)

	stdout, _, err := exec(t, "database", "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if want := strconv.Itoa(store.LatestMigration()) + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestCommandSurface(t *testing.T) {
	tests := []struct {
		name string
		args []string
		// wantErr is a substring of the error run returns.
		wantErr string
		// wantUsage marks the failures that are "you have not named a command
		// yet", which answer themselves by printing the list of commands.
		wantUsage bool
		wantHelp  bool
	}{
		{name: "no command", args: nil, wantErr: "no command specified", wantUsage: true},
		{
			name:      "unknown command",
			args:      []string{"bogus"},
			wantErr:   `unknown command "bogus"`,
			wantUsage: true,
		},
		{
			// The prefix is what keeps a group's failure from reading like the
			// root's.
			name:      "group without a subcommand",
			args:      []string{"database"},
			wantErr:   "database: no command specified",
			wantUsage: true,
		},
		{
			name:      "unknown subcommand",
			args:      []string{"database", "bogus"},
			wantErr:   `database: unknown command "bogus"`,
			wantUsage: true,
		},
		{
			// A named command that then fails is a different thing: it is
			// answered by the error alone, not by a wall of usage text.
			name:    "unexpected arguments",
			args:    []string{"database", "verify", "extra"},
			wantErr: "unexpected arguments",
		},
		{name: "root help", args: []string{"--help"}, wantHelp: true},
		{name: "group help", args: []string{"database", "--help"}, wantHelp: true},
		{name: "subcommand help", args: []string{"database", "backup", "--help"}, wantHelp: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateEnv(t)

			_, stderr, err := exec(t, tt.args...)
			if tt.wantHelp {
				// main treats this as success, so --help must never look like
				// a failure to a script.
				if !errors.Is(err, ff.ErrHelp) {
					t.Fatalf("error = %v, want ff.ErrHelp", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			// Failing to name a command is not a reason to make someone go
			// looking for the list of them.
			if got := strings.Contains(stderr, "SUBCOMMANDS"); got != tt.wantUsage {
				t.Errorf("usage printed = %v, want %v; stderr = %q", got, tt.wantUsage, stderr)
			}
		})
	}
}

// --- helpers ---------------------------------------------------------------

// exec runs the command tree the way main does and returns what a user would
// have seen. The environment name is fixed: run never loads dotenv files, so it
// only reaches cfg.Env, and a test that depended on it would be testing nothing.
func exec(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var errBuf bytes.Buffer
	stdout, err = captureStdout(t, func() error {
		return run(t.Context(), "test", args, &errBuf)
	})
	return stdout, errBuf.String(), err
}

// isolateEnv removes every EC_-prefixed variable for the duration of the test.
//
// ff reads them, so a developer's own EC_DB_PATH or EC_QUIET would otherwise
// decide what these tests are actually running. t.Setenv records the original
// for restoration (and fails the test if it is parallel) before the variable is
// removed outright, which is the state a flag's default expects.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, config.EnvVarPrefix+"_") {
			continue
		}
		t.Setenv(name, value)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

// createDatabase makes a database through the command under test, because a
// fixture built any other way would not prove the commands agree about what a
// database is.
func createDatabase(t *testing.T, dir string) {
	t.Helper()
	if _, _, err := exec(t, "database", "create", "--db-path", dir); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
}

// newEmptyDatabase writes a file carrying the ECV8 marker and no schema: a
// database from before the first migration, which is what "behind this binary"
// looks like while only one migration exists.
func newEmptyDatabase(t *testing.T, dir string) {
	t.Helper()
	conn, err := zsqlite.OpenConn(filepath.Join(dir, store.DatabaseName),
		zsqlite.OpenReadWrite|zsqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open new database: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Pragmas take no bound parameters, so the value is formatted in. It comes
	// from the store package, never from a test's input.
	for _, query := range []string{
		fmt.Sprintf("PRAGMA application_id = %d;", store.ApplicationID),
		"PRAGMA user_version = 0;",
	} {
		if err := sqlitex.ExecuteTransient(conn, query, nil); err != nil {
			t.Fatalf("prepare empty database with %q: %v", query, err)
		}
	}
}

// setMigrationLevel rewrites user_version directly. It is how a test produces a
// database this binary considers too new without a second migration existing.
func setMigrationLevel(t *testing.T, dir string, level int) {
	t.Helper()
	conn, err := zsqlite.OpenConn(filepath.Join(dir, store.DatabaseName), zsqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA user_version = %d;", level), nil); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

func countAccounts(t *testing.T, dir string) int {
	t.Helper()
	conn, err := zsqlite.OpenConn(filepath.Join(dir, store.DatabaseName), zsqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var count int
	if err := sqlitex.ExecuteTransient(conn, "SELECT count(*) FROM account;", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zsqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	return count
}

// dbState is a database's logical contents: what a command may not change
// behind an operator's back. The -wal and -shm sidecars are deliberately absent
// — they are operational, and opening or closing a connection may create or
// remove one without the database itself differing.
type dbState struct {
	userVersion   int
	journalMode   string
	schemaObjects []string
	accounts      int
}

// equal is the strict comparison, used where a command claims to have changed
// nothing at all.
func (s dbState) equal(other dbState) bool {
	return s.journalMode == other.journalMode && s.sameContents(other)
}

// sameContents ignores journal mode, which is a property of how a file is
// written rather than of what it holds. Only a copy is compared this way.
func (s dbState) sameContents(other dbState) bool {
	return s.userVersion == other.userVersion &&
		s.accounts == other.accounts &&
		slices.Equal(s.schemaObjects, other.schemaObjects)
}

func databaseSnapshot(t *testing.T, dir string) dbState {
	t.Helper()
	return backupSnapshot(t, filepath.Join(dir, store.DatabaseName))
}

// backupSnapshot reads the same state from any file, so a backup can be
// compared against the database it came from.
func backupSnapshot(t *testing.T, path string) dbState {
	t.Helper()
	conn, err := zsqlite.OpenConn(path, zsqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = conn.Close() }()

	var state dbState
	read := func(query string, fn func(*zsqlite.Stmt) error) {
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{ResultFunc: fn}); err != nil {
			t.Fatalf("read %q from %s: %v", query, path, err)
		}
	}
	read("PRAGMA user_version;", func(stmt *zsqlite.Stmt) error {
		state.userVersion = stmt.ColumnInt(0)
		return nil
	})
	read("PRAGMA journal_mode;", func(stmt *zsqlite.Stmt) error {
		state.journalMode = stmt.ColumnText(0)
		return nil
	})
	read(`SELECT type || ':' || name || ':' || coalesce(sql, '')
	      FROM sqlite_schema
	      WHERE name NOT LIKE 'sqlite_%'
	      ORDER BY type, name;`, func(stmt *zsqlite.Stmt) error {
		state.schemaObjects = append(state.schemaObjects, stmt.ColumnText(0))
		return nil
	})
	if len(state.schemaObjects) != 0 {
		read("SELECT count(*) FROM account;", func(stmt *zsqlite.Stmt) error {
			state.accounts = stmt.ColumnInt(0)
			return nil
		})
	}
	return state
}

// digest is the stricter comparison: a refused command must not rewrite a byte,
// not merely preserve what a query can see.
func digest(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(body)
}

// captureStdout redirects os.Stdout for the duration of fn. The commands print
// with fmt.Println, which is what a user actually sees, so the test reads it the
// same way rather than being handed a writer the real binary never uses.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	runErr := fn()

	os.Stdout = original
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return string(output), runErr
}
