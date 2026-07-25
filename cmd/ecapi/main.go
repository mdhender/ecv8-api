// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Command ecapi serves the ECV8 HTTP API.
//
// It is the server half of what began as one binary, and the counterpart to
// ecdb, which owns the database file. ecapi never creates a database and never
// seeds an administrator: it opens what is already there, migrates it forward,
// and serves. That division is what lets the long-running service be installed,
// confined, and restarted without carrying the privileges or the flags that
// creating a database needs.
//
// The listener is plain HTTP on a loopback address by design. TLS is terminated
// by the reverse proxy in front of it — nginx in production, Caddy in
// development — so this process never handles a certificate. See ecapi.service
// in this directory for the unit that runs it that way.
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
		fmt.Fprintf(os.Stderr, "ecapi: %v\n", err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM cancel this context, which is what triggers graceful
	// shutdown all the way down to in-flight database work. systemd stops the
	// service with SIGTERM, so this is the production shutdown path, not only
	// the Ctrl-C one.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, env, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, ff.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "ecapi: %v\n", err)
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
	rootFlags := ff.NewFlagSet("ecapi")
	cfg := config.Bind(rootFlags)
	cfg.Env = env

	root := &ff.Command{
		Name:      "ecapi",
		Usage:     "ecapi <SUBCOMMAND> [FLAGS]",
		ShortHelp: "serve the ECV8 HTTP API",
		LongHelp: "Every flag can also be set from an environment variable: --listen-addr\n" +
			"is fed by EC_LISTEN_ADDR. Flags win over the environment, which wins\n" +
			"over the defaults shown below.",
		Flags: rootFlags,
	}

	serveFlags := ff.NewFlagSet("serve").SetParent(rootFlags)
	serve := &ff.Command{
		Name:      "serve",
		Usage:     "ecapi serve [FLAGS]",
		ShortHelp: "open the database and serve the HTTP API",
		Flags:     serveFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
			return runServe(ctx, cfg)
		},
	}

	versionFlags := ff.NewFlagSet("version").SetParent(rootFlags)
	versionCmd := &ff.Command{
		Name:      "version",
		Usage:     "ecapi version",
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

	root.Subcommands = append(root.Subcommands, serve, versionCmd)
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
		// Opening never creates the file. A missing database is an operator
		// error to be reported, not something to paper over by making one:
		// creating it is ecdb's job, and only ecdb may seed an administrator.
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
//
// It writes to stderr because that is what an init system captures: under the
// unit in this directory every line below lands in the journal, with no log
// file to rotate.
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
