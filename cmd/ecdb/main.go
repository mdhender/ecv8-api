// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Command ecdb creates and inspects the ECV8 database.
//
// It is the database half of what began as one binary. ecv8-api serves HTTP;
// ecdb owns the file on disk, and is where operations that only touch storage —
// creating a database today, backing one up or compacting one later — belong.
// Keeping them out of the server means an operator can run them without a
// server's configuration, and a mistake in one cannot take the other down.
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

	// SIGINT and SIGTERM cancel this context. Creating a database is short, but
	// it still runs migrations, so it gets the same interruptible treatment as
	// the server rather than being killed mid-write.
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
	cmd, cfg := command(env)

	if err := cmd.Parse(args, ff.WithEnvVarPrefix(config.EnvVarPrefix)); err != nil {
		fmt.Fprint(stderr, ffhelp.Command(cmd.GetSelected()))
		return err
	}

	selected := cmd.GetSelected()
	if selected.Exec == nil {
		fmt.Fprint(stderr, ffhelp.Command(selected))
		if extra := selected.Flags.GetArgs(); len(extra) != 0 {
			return fmt.Errorf("unknown command %q", extra[0])
		}
		return errors.New("no command specified")
	}

	// Validation happens after parsing so a --help request never fails on an
	// unrelated configuration problem.
	if selected.Name != "version" {
		if err := config.ValidateDatabase(cfg); err != nil {
			return err
		}
	}
	return cmd.Run(ctx)
}

// command builds the command tree and the Database config its flags write into.
func command(env string) (*ff.Command, *config.Database) {
	rootFlags := ff.NewFlagSet("ecdb")
	cfg := config.BindDatabase(rootFlags)
	cfg.Env = env

	root := &ff.Command{
		Name:      "ecdb",
		Usage:     "ecdb <SUBCOMMAND> [FLAGS]",
		ShortHelp: "create and inspect the ECV8 database",
		LongHelp: "Every flag can also be set from an environment variable: --db-path\n" +
			"is fed by EC_DB_PATH. Flags win over the environment, which wins\n" +
			"over the defaults shown below.",
		Flags: rootFlags,
	}

	createFlags := ff.NewFlagSet("create").SetParent(rootFlags)
	create := &ff.Command{
		Name:  "create",
		Usage: "ecdb create [--db-path DIR]",
		ShortHelp: "create ecv8.db in an existing directory and seed the initial admin " +
			"from EC_ADMIN_EMAIL and EC_ADMIN_SECRET",
		Flags: createFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
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
		},
	}

	verifyFlags := ff.NewFlagSet("verify").SetParent(rootFlags)
	verify := &ff.Command{
		Name:      "verify",
		Usage:     "ecdb verify [--db-path DIR]",
		ShortHelp: "open ecv8.db read-only and report its migration version",
		Flags:     verifyFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
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
		},
	}

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
			// The same version as the server: one module, one build, and the
			// migrations both know about are compiled from the same source.
			fmt.Println(version.Version)
			return nil
		},
	}

	root.Subcommands = append(root.Subcommands, create, verify, versionCmd)
	return root, cfg
}
