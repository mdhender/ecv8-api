// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Command ecdb creates and maintains the ECV8 database.
//
// It is the database half of a deliberate split. ecapi serves HTTP; ecdb owns
// the file on disk, and is where operations that only touch storage — creating,
// migrating, backing up, and compacting a database — belong. Keeping them out
// of the server means an operator can run them without a server's
// configuration, and a mistake in one cannot take the other down.
//
// Every operation on the file sits under the "database" subcommand, so the room
// left for work that is not about the file itself stays obvious.
//
// Configuration follows the same rules as the rest of the project: a flag beats
// an EC_-prefixed environment variable, which beats the built-in default.
// Dotenv files are loaded into the process environment before flags are parsed,
// so they can supply a variable but never override one. See internal/config.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/dotenv"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/version"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

func main() {
	// EC_ENV selects which dotenv files load. It is read before flag parsing
	// because those files are what populate the environment ff then reads, and
	// because EC_ADMIN_EMAIL and EC_ADMIN_SECRET usually come from one.
	env, ok := os.LookupEnv(config.EnvVarPrefix + "_ENV")
	if !ok {
		env = "development"
	}
	if err := dotenv.Load(env); err != nil {
		fmt.Fprintf(os.Stderr, "ecdb: %v\n", err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM cancel this context. Most of this work is short, but it
	// migrates and vacuums, so it gets the same interruptible treatment as the
	// server rather than being killed mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, env, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, ff.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "ecdb: %v\n", err)
		os.Exit(1)
	}
}

// run parses args and executes the selected command.
func run(ctx context.Context, env string, args []string, stderr io.Writer) error {
	cmd := command(env)

	if err := cmd.Parse(args, ff.WithEnvVarPrefix(config.EnvVarPrefix)); err != nil {
		fmt.Fprint(stderr, ffhelp.Command(cmd.GetSelected()))
		return err
	}

	// A group such as "database" has no Exec of its own, so naming one is not a
	// request to do anything. The prefix keeps "ecdb database" from reporting
	// the same bare message as "ecdb".
	selected := cmd.GetSelected()
	if selected.Exec == nil {
		fmt.Fprint(stderr, ffhelp.Command(selected))
		prefix := ""
		if selected != cmd {
			prefix = selected.Name + ": "
		}
		if extra := selected.Flags.GetArgs(); len(extra) != 0 {
			return fmt.Errorf("%sunknown command %q", prefix, extra[0])
		}
		return fmt.Errorf("%sno command specified", prefix)
	}
	return cmd.Run(ctx)
}

// databaseExec adapts a database subcommand to ff's Exec signature.
//
// Every subcommand under "database" rejects positional arguments and needs a
// validated --db-path, and validation belongs here rather than before dispatch:
// running inside Exec is what keeps a --help request, and the commands that
// never open a database, from failing over a flag they do not use.
func databaseExec(cfg *config.Database, exec func(ctx context.Context) error) func(context.Context, []string) error {
	return func(ctx context.Context, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("unexpected arguments: %v", args)
		}
		if err := config.ValidateDatabase(cfg); err != nil {
			return err
		}
		return exec(ctx)
	}
}

// command builds the command tree. The Database config its flags write into is
// captured by the subcommands that read it, so it never leaves this function.
func command(env string) *ff.Command {
	rootFlags := ff.NewFlagSet("ecdb")
	cfg := config.BindDatabase(rootFlags)
	cfg.Env = env

	root := &ff.Command{
		Name:      "ecdb",
		Usage:     "ecdb [FLAGS] <SUBCOMMAND>",
		ShortHelp: "create and maintain the ECV8 database",
		LongHelp: "Every flag can also be set from an environment variable: --db-path\n" +
			"is fed by EC_DB_PATH. Flags win over the environment, which wins\n" +
			"over the defaults shown below.",
		Flags: rootFlags,
	}

	databaseFlags := ff.NewFlagSet("database").SetParent(rootFlags)
	database := &ff.Command{
		Name:      "database",
		Usage:     "ecdb database <SUBCOMMAND> [FLAGS]",
		ShortHelp: "create and maintain the database file",
		LongHelp: "Every subcommand works on the directory named by --db-path and the\n" +
			"file ecv8.db inside it. The filename is fixed and the directory is\n" +
			"never created.",
		Flags: databaseFlags,
	}

	backupFlags := ff.NewFlagSet("backup").SetParent(databaseFlags)
	// EC_VERSION feeds this flag, and only when running this subcommand; it has
	// nothing to do with `ecdb version`, which reports the build.
	backupOutputPath := backupFlags.StringLong("output-path", "",
		"directory to write the backup into; defaults to --db-path")
	backupIncludeVersion := backupFlags.BoolLong("version",
		"append the database's migration number to the backup filename")
	backup := &ff.Command{
		Name:      "backup",
		Usage:     "ecdb database backup [--db-path DIR] [--output-path DIR] [--version]",
		ShortHelp: "write a consistent, compacted copy of ecv8.db",
		Flags:     backupFlags,
		Exec: databaseExec(cfg, func(ctx context.Context) error {
			outputDir := *backupOutputPath
			if outputDir == "" {
				outputDir = cfg.DBPath
			}
			createdPath, err := store.BackupPersistent(ctx, cfg.DBPath, outputDir, *backupIncludeVersion)
			if err != nil {
				return err
			}
			// The backup's path is the point of the command, not a progress
			// report, so --quiet leaves it alone: a script capturing this would
			// otherwise read an empty line as success.
			fmt.Println(createdPath)
			return nil
		}),
	}

	compactFlags := ff.NewFlagSet("compact").SetParent(databaseFlags)
	compact := &ff.Command{
		Name:      "compact",
		Usage:     "ecdb database compact [--db-path DIR]",
		ShortHelp: "reclaim unused space in ecv8.db",
		Flags:     compactFlags,
		Exec: databaseExec(cfg, func(ctx context.Context) error {
			// Silent on success. Compaction changes nothing an operator needs
			// told back to them, and the freed space is visible from the
			// filesystem.
			return store.CompactPersistent(ctx, cfg.DBPath)
		}),
	}

	createFlags := ff.NewFlagSet("create").SetParent(databaseFlags)
	create := &ff.Command{
		Name:  "create",
		Usage: "ecdb database create [--db-path DIR]",
		ShortHelp: "create ecv8.db in an existing directory and seed the initial admin " +
			"from EC_ADMIN_EMAIL and EC_ADMIN_SECRET",
		Flags: createFlags,
		Exec: databaseExec(cfg, func(ctx context.Context) error {
			db, err := store.CreatePersistentStore(ctx, cfg.DBPath)
			if err != nil {
				return err
			}
			if err := db.Close(); err != nil {
				return err
			}
			// The path is the useful output: it is what an operator points the
			// server at, and it proves which directory was actually used.
			fmt.Println(filepath.Join(cfg.DBPath, store.DatabaseName))
			return nil
		}),
	}

	upgradeFlags := ff.NewFlagSet("upgrade").SetParent(databaseFlags)
	upgrade := &ff.Command{
		Name:      "upgrade",
		Usage:     "ecdb database upgrade [--db-path DIR]",
		ShortHelp: "apply any migrations ecv8.db is missing",
		Flags:     upgradeFlags,
		Exec: databaseExec(cfg, func(ctx context.Context) error {
			applied, err := store.MigratePersistent(ctx, cfg.DBPath)
			if err != nil {
				return err
			}
			if cfg.Quiet {
				return nil
			}
			// Saying so either way matters: "no migrations applied" is the
			// answer to "is this database ready for the new binary?", and an
			// operator should not have to infer it from silence.
			path := filepath.Join(cfg.DBPath, store.DatabaseName)
			if !applied {
				fmt.Printf("%s: no migrations applied (migration %d)\n", path, store.LatestMigration())
				return nil
			}
			fmt.Printf("%s: migrations applied (migration %d)\n", path, store.LatestMigration())
			return nil
		}),
	}

	verifyFlags := ff.NewFlagSet("verify").SetParent(databaseFlags)
	verify := &ff.Command{
		Name:      "verify",
		Usage:     "ecdb database verify [--db-path DIR]",
		ShortHelp: "open ecv8.db read-only and report its migration version",
		Flags:     verifyFlags,
		Exec: databaseExec(cfg, func(ctx context.Context) error {
			// Read-only so verifying never migrates or otherwise mutates a
			// database an operator only wanted to inspect.
			db, err := store.OpenPersistentStore(ctx, cfg.DBPath, true)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			migration, err := db.MigrationVersion(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("%s: migration %d of %d\n",
				filepath.Join(cfg.DBPath, store.DatabaseName), migration, store.LatestMigration())
			return nil
		}),
	}

	databaseVersionFlags := ff.NewFlagSet("version").SetParent(databaseFlags)
	databaseVersion := &ff.Command{
		Name:      "version",
		Usage:     "ecdb database version [--db-path DIR]",
		ShortHelp: "print the migration version of ecv8.db",
		Flags:     databaseVersionFlags,
		Exec: databaseExec(cfg, func(ctx context.Context) error {
			db, err := store.OpenPersistentStore(ctx, cfg.DBPath, true)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			migration, err := db.MigrationVersion(ctx)
			if err != nil {
				return err
			}
			// Bare number, unlike verify's sentence: this is the form a script
			// compares, so it stays free of anything that would need parsing.
			fmt.Println(migration)
			return nil
		}),
	}

	database.Subcommands = append(database.Subcommands, backup, compact, create, upgrade, verify, databaseVersion)

	versionFlags := ff.NewFlagSet("version").SetParent(rootFlags)
	versionCmd := &ff.Command{
		Name:      "version",
		Usage:     "ecdb version",
		ShortHelp: "print the build version",
		Flags:     versionFlags,
		Exec: func(_ context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
			// Not a databaseExec: this one opens nothing, so it must not be
			// refused for a --db-path it never reads. The version is the same
			// as the server's — one module, one build, and the migrations both
			// know about are compiled from the same source.
			fmt.Println(version.Version)
			return nil
		},
	}

	root.Subcommands = append(root.Subcommands, database, versionCmd)
	return root
}
