// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Command ec is the game master's convenience client for ECV8.
//
// It does nothing earl cannot do. earl is the raw client — the verb and path
// you would send are what you type — which makes it the right tool for
// exercising the API and the wrong one for running a game, because a game
// master should not have to remember which path a task lives behind. ec is the
// other half of that trade: named commands over the same HTTP, so the common
// work reads as what it is.
//
// Being a convenience is the whole of it. ec speaks HTTP and only HTTP, through
// internal/apiclient, exactly as earl does: it never opens the database,
// imports no store package, and implements no rule the server owns. A shortcut
// that reached past the API would make ec more capable than a real client, and
// the point of both tools is that they are not.
//
//	ec app login --email gm1@example.com
//	ec app whoami
//	ec version
//
// # Why the commands sit a level down
//
// ec is expected to grow a broad surface — a game master's work is broad — so
// it groups from the start rather than filling its top level with verbs and
// having to move them later. Everything about who you are signed in as is
// "app": login, logout, whoami, identities. version stands alone at the top
// because it reports the build and talks to nothing.
//
// # Configuration
//
// ec's flags read ECV8_-prefixed environment variables (--base-url is fed by
// ECV8_BASE_URL), the same ones earl reads, and the two share one saved
// credential file. That sharing is the reason the prefix is shared: sessions
// are keyed by base URL, so if each command read its own variable they could be
// pointed at different servers and a login through one would not be a login
// through the other.
//
// The prefix is not the server's EC_, so pointing a client at another host
// never means touching a server's configuration. EC_ENV is shared with the rest
// of the project, because a checkout has one idea of which environment it is
// working in, and it scopes the credential file so a development session and a
// production session are never confused.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mdhender/ecv8-api/internal/apiclient"
	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/dotenv"
	"github.com/mdhender/ecv8-api/internal/version"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

func main() {
	// EC_ENV selects which dotenv files load, and scopes the credential file.
	// It is read before flag parsing because those files populate the
	// environment ff then reads.
	env, ok := os.LookupEnv(config.EnvVarPrefix + "_ENV")
	if !ok {
		env = "development"
	}
	// Load also rejects an unknown environment, which is what lets env be used
	// as a path segment in the credential file without further checking.
	if err := dotenv.Load(env); err != nil {
		fmt.Fprintf(os.Stderr, "ec: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, env, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, ff.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "ec: %v\n", err)
		os.Exit(1)
	}
}

// run parses args and executes the selected command.
func run(ctx context.Context, env string, args []string, stderr io.Writer) error {
	cmd := command(env)

	if err := cmd.Parse(args, ff.WithEnvVarPrefix(apiclient.EnvVarPrefix)); err != nil {
		fmt.Fprint(stderr, ffhelp.Command(cmd.GetSelected()))
		return err
	}

	// A group such as "app" has no Exec of its own, so naming one is not a
	// request to do anything. The prefix keeps "ec app" from reporting the same
	// bare message as "ec".
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

// command builds the command tree. The client configuration its flags write
// into is captured by the subcommands that read it, so it never leaves this
// function.
func command(env string) *ff.Command {
	rootFlags := ff.NewFlagSet("ec")
	cfg := apiclient.Bind(rootFlags)
	cfg.Env = env
	cfg.UserAgent = "ec/" + version.Version.String()
	cfg.LoginCommand = "ec app login"

	root := &ff.Command{
		Name:      "ec",
		Usage:     "ec [FLAGS] <SUBCOMMAND> ...",
		ShortHelp: "the game master's client for the ECV8 API",
		LongHelp: "ec is a convenience over the same API earl speaks, and can do nothing\n" +
			"earl cannot. Every flag can also be set from an environment variable:\n" +
			"--base-url is fed by ECV8_BASE_URL. earl reads the same variables and\n" +
			"the same saved sessions, so signing in with either signs in for both.",
		Flags: rootFlags,
	}

	appFlags := ff.NewFlagSet("app").SetParent(rootFlags)
	app := &ff.Command{
		Name:      "app",
		Usage:     "ec app <SUBCOMMAND> [FLAGS]",
		ShortHelp: "sign in to the API, and see who you are signed in as",
		LongHelp: "A session belongs to the machine, not to one command: `ec app login`\n" +
			"and `earl login` write the same file, and `ec app logout` ends the\n" +
			"session for both. --email picks between several saved accounts.",
		Flags: appFlags,
	}

	// newClient builds the client from the resolved flags. Deferring it to each
	// Exec keeps the flag values authoritative — they are not parsed yet here —
	// and keeps a --help request from being refused over a --base-url it would
	// never use.
	newClient := func() (*apiclient.Client, error) {
		return apiclient.New(cfg)
	}

	loginFlags := ff.NewFlagSet("login").SetParent(appFlags)
	var loginPassword string
	loginFlags.StringVar(&loginPassword, 0, "password", "",
		"account password; @- reads stdin, @file reads a file, or set ECV8_PASSWORD")
	login := &ff.Command{
		Name:      "login",
		Usage:     "ec app login [--email EMAIL] [--password PASSWORD]",
		ShortHelp: "authenticate and save the session for later commands",
		LongHelp: "A password given on the command line is visible to anyone who can list\n" +
			"processes. Prefer ECV8_PASSWORD, or --password @- to read it from stdin.",
		Flags: loginFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("login takes no positional arguments")
			}
			secret, err := apiclient.ReadValue(loginPassword)
			if err != nil {
				return fmt.Errorf("login: %w", err)
			}
			client, err := newClient()
			if err != nil {
				return err
			}
			return client.Login(ctx, string(secret))
		},
	}

	logoutFlags := ff.NewFlagSet("logout").SetParent(appFlags)
	logout := &ff.Command{
		Name:      "logout",
		Usage:     "ec app logout [--email EMAIL]",
		ShortHelp: "end the saved session and forget it",
		Flags:     logoutFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("logout takes no positional arguments")
			}
			client, err := newClient()
			if err != nil {
				return err
			}
			return client.Logout(ctx)
		},
	}

	whoamiFlags := ff.NewFlagSet("whoami").SetParent(appFlags)
	whoami := &ff.Command{
		Name:      "whoami",
		Usage:     "ec app whoami",
		ShortHelp: "show the current session (GET " + apiclient.SessionPath + ")",
		Flags:     whoamiFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("whoami takes no positional arguments")
			}
			client, err := newClient()
			if err != nil {
				return err
			}
			return client.Whoami(ctx)
		},
	}

	identitiesFlags := ff.NewFlagSet("identities").SetParent(appFlags)
	identities := &ff.Command{
		Name:      "identities",
		Usage:     "ec app identities",
		ShortHelp: "list the saved sessions for this base URL",
		Flags:     identitiesFlags,
		Exec: func(_ context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("identities takes no positional arguments")
			}
			client, err := newClient()
			if err != nil {
				return err
			}
			return client.Identities()
		},
	}

	app.Subcommands = append(app.Subcommands, login, logout, whoami, identities)

	versionFlags := ff.NewFlagSet("version").SetParent(rootFlags)
	versionCmd := &ff.Command{
		Name:      "version",
		Usage:     "ec version",
		ShortHelp: "print the build version",
		Flags:     versionFlags,
		Exec: func(_ context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
			// One module, one build: this is the same version ecapi, ecdb, and
			// earl report, which is what makes it worth asking a client for.
			fmt.Println(version.Version)
			return nil
		},
	}

	root.Subcommands = append(root.Subcommands, app, versionCmd)
	return root
}
