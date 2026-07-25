// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Command ecv8-api serves the ECV8 HTTP API and manages its database.
//
// Configuration comes from flags, then environment variables prefixed with EC_,
// then built-in defaults. Dotenv files are loaded into the process environment
// before flags are parsed, so they can supply a variable but never override a
// real environment variable or an explicit flag. See internal/config.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/dotenv"
	"github.com/mdhender/ecv8-api/internal/server"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/version"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

func main() {
	// EC_ENV selects which dotenv files load. It is read before flag parsing
	// because those files are what populate the environment ff then reads.
	env, ok := os.LookupEnv(config.EnvVarPrefix + "_ENV")
	if !ok {
		env = "development"
	}
	if err := dotenv.Load(env); err != nil {
		fmt.Fprintf(os.Stderr, "ecv8-api: %v\n", err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM cancel this context, which is what triggers graceful
	// shutdown all the way down to in-flight database work.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, env, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, ff.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "ecv8-api: %v\n", err)
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
		if err := config.Validate(cfg); err != nil {
			return err
		}
	}
	return cmd.Run(ctx)
}

// command builds the command tree and the Config its flags write into.
func command(env string) (*ff.Command, *config.Config) {
	rootFlags := ff.NewFlagSet("ecv8-api")
	cfg := config.Bind(rootFlags)
	cfg.Env = env

	root := &ff.Command{
		Name:      "ecv8-api",
		Usage:     "ecv8-api <SUBCOMMAND> [FLAGS]",
		ShortHelp: "serve the ECV8 API and manage its database",
		LongHelp: "Every flag can also be set from an environment variable: --listen-addr\n" +
			"is fed by EC_LISTEN_ADDR. Flags win over the environment, which wins\n" +
			"over the defaults shown below.",
		Flags: rootFlags,
	}

	serveFlags := ff.NewFlagSet("serve").SetParent(rootFlags)
	serve := &ff.Command{
		Name:      "serve",
		Usage:     "ecv8-api serve [FLAGS]",
		ShortHelp: "open the database and serve the HTTP API",
		Flags:     serveFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
			return runServe(ctx, cfg)
		},
	}

	dbFlags := ff.NewFlagSet("db").SetParent(rootFlags)
	db := &ff.Command{
		Name:      "db",
		Usage:     "ecv8-api db <SUBCOMMAND> [FLAGS]",
		ShortHelp: "manage the persistent database",
		Flags:     dbFlags,
	}

	createFlags := ff.NewFlagSet("create").SetParent(dbFlags)
	create := &ff.Command{
		Name:  "create",
		Usage: "ecv8-api db create [--db-path DIR]",
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
			fmt.Println(filepath.Join(cfg.DBPath, store.DatabaseName))
			return nil
		},
	}

	verifyFlags := ff.NewFlagSet("verify").SetParent(dbFlags)
	verify := &ff.Command{
		Name:      "verify",
		Usage:     "ecv8-api db verify [--db-path DIR]",
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

	db.Subcommands = append(db.Subcommands, create, verify)

	versionFlags := ff.NewFlagSet("version").SetParent(rootFlags)
	versionCmd := &ff.Command{
		Name:      "version",
		Usage:     "ecv8-api version",
		ShortHelp: "print the build version",
		Flags:     versionFlags,
		Exec: func(_ context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
			fmt.Println(version.Version)
			return nil
		},
	}

	root.Subcommands = append(root.Subcommands, serve, db, versionCmd)
	return root, cfg
}

// runServe opens the database and serves until the context is cancelled.
func runServe(ctx context.Context, cfg *config.Config) error {
	log, err := newLogger(cfg)
	if err != nil {
		return err
	}

	var (
		db     *store.DB
		source string
	)
	if cfg.Memory != "" {
		// A development convenience: a seeded in-memory database that vanishes
		// on exit. It is never used in production, so the warning is loud.
		db, err = store.OpenTemporaryStore(ctx, cfg.Memory)
		source = "memory:" + cfg.Memory
		if err == nil {
			log.Warn("serving an in-memory database; all data is discarded on exit",
				"name", cfg.Memory)
		}
	} else {
		db, err = store.OpenPersistentStore(ctx, cfg.DBPath, cfg.ReadOnly)
		source = filepath.Join(cfg.DBPath, store.DatabaseName)
	}
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Error("close database", "error", closeErr)
		}
	}()

	migration, err := db.MigrationVersion(ctx)
	if err != nil {
		return err
	}
	log.Info("database ready",
		"source", source,
		"migration", migration,
		"latest", store.LatestMigration(),
		"read_only", db.ReadOnly(),
	)

	srv, err := server.New(cfg, db, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// newLogger builds the one logger the process uses. It is passed explicitly to
// everything that needs it; there is no package-level logger anywhere.
func newLogger(cfg *config.Config) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", cfg.LogLevel, err)
	}
	options := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	} else {
		handler = slog.NewTextHandler(os.Stderr, options)
	}
	return slog.New(handler).With("env", cfg.Env, "version", version.Version.String()), nil
}
