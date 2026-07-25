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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// accountsPath is where accounts are created. It lives here rather than in
// internal/apiclient, which deliberately names only the one path login has to
// know: a client that started collecting endpoint constants would slowly become
// the API surface earl proves it does not need to be.
const accountsPath = "/admin/accounts"

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

	accountFlags := ff.NewFlagSet("account").SetParent(rootFlags)
	account := &ff.Command{
		Name:      "account",
		Usage:     "ec account <SUBCOMMAND> [FLAGS]",
		ShortHelp: "create accounts, as an administrator",
		LongHelp: "Administrators create every account; there is no public registration.\n" +
			"These commands need a session belonging to one.",
		Flags: accountFlags,
	}

	createFlags := ff.NewFlagSet("create").SetParent(accountFlags)
	var (
		createSecret      string
		createRole        string
		createDisplayName string
		createActive      bool
	)
	// Neither the address nor the credential here may be spelled the way `ec app
	// login` spells them, and for one reason: every flag is fed by an ECV8_
	// environment variable derived from its name, and a developer who has set
	// ECV8_EMAIL and ECV8_PASSWORD so that `ec app login` works has set exactly
	// the two variables those spellings would read.
	//
	// The failure is silent and it is bad. `ec account create someone@example.com`
	// would take the operator's own sign-in password, hash it, and hand it to a
	// brand-new activated account — no flag typed, no warning, an account whose
	// password is the administrator's.
	//
	// So the address is positional, which also makes it the subject of the
	// sentence the way `earl get PATH` does, and the credential is --secret. They
	// are genuinely two different secrets: the password you authenticate with,
	// and the password you are assigning to somebody else. Sharing one word for
	// them is what shares the variable.
	createFlags.StringVar(&createSecret, 0, "secret", "",
		"first password for the new account; @- reads stdin, @file reads a file, or set ECV8_SECRET")
	createFlags.StringVar(&createRole, 0, "role", "", "user (default) or admin")
	createFlags.StringVar(&createDisplayName, 0, "display-name", "", "defaults to the part before the @")
	// Positive and defaulting true, so deactivating is --active=false rather
	// than a flag whose name is already a negative.
	createFlags.BoolVarDefault(&createActive, 0, "active", true, "whether the account may sign in")
	create := &ff.Command{
		Name: "create",
		// Flags before the address, as `earl post [FLAGS] PATH` has it: flag
		// parsing stops at the first positional argument, so the address has to
		// come last.
		Usage:     "ec account create [FLAGS] EMAIL",
		ShortHelp: "create an account (POST " + accountsPath + ")",
		LongHelp: "EMAIL is the address of the account being created, and --secret is its\n" +
			"first password. Neither is --email or --password: those are the root\n" +
			"flags for who *you* sign in as, they are fed by ECV8_EMAIL and\n" +
			"ECV8_PASSWORD, and reusing the spelling here would silently create\n" +
			"accounts using your own address and password.\n" +
			"\n" +
			"With --secret the account is activated on the spot and can sign in\n" +
			"immediately, which is what makes a test fixture one request instead of\n" +
			"an invitation and a redemption. Without it this invites, exactly as the\n" +
			"administration interface does, and prints the one-time activation link.\n" +
			"\n" +
			"A secret given on the command line is visible to anyone who can list\n" +
			"processes. Prefer ECV8_SECRET, or --secret @- to read it from stdin.",
		Flags: createFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("create takes exactly one argument, the email address")
			}
			createEmail := args[0]
			secret, err := apiclient.ReadValue(createSecret)
			if err != nil {
				return fmt.Errorf("create: %w", err)
			}

			// Only what was asked for is sent. Omitting a field lets the server
			// apply its own default rather than this command inventing a second
			// copy of one.
			body := map[string]any{"email": createEmail}
			if len(secret) != 0 {
				body["password"] = string(secret)
			}
			if createRole != "" {
				body["role"] = createRole
			}
			if createDisplayName != "" {
				body["display_name"] = createDisplayName
			}
			if !createActive {
				body["is_active"] = false
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("create: %w", err)
			}

			client, err := newClient()
			if err != nil {
				return err
			}
			return client.Request(ctx, http.MethodPost, accountsPath, encoded, false)
		},
	}
	account.Subcommands = append(account.Subcommands, create)

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

	root.Subcommands = append(root.Subcommands, app, account, versionCmd)
	return root
}
