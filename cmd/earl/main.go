// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Command earl is a command-line client for the ECV8 API.
//
// It is a client and nothing more. It never opens the database, imports no
// store package, and knows no game rules: it sends HTTP requests to the routes
// the server publishes and prints what comes back. Anything it appears to know
// about accounts or games is only the shape of a JSON body being passed
// through. That restraint is the point — a test client that reimplements the
// server's rules stops being evidence that the server implements them.
//
// The REST surface is the command line. The verb and path you would send are
// what you type:
//
//	earl get /session
//	earl get /admin/accounts
//	earl post /admin/accounts -d '{"email":"t@x.com","role":"user","display_name":"T"}'
//	earl patch /admin/games/1 -d @game.json
//	earl put /me/password -d @- < body.json
//
// so earl covers every endpoint without per-endpoint code, and stays correct as
// endpoints are added. Only the commands that touch the saved credential are
// special: login captures a session, logout ends and forgets one, identities
// lists what is saved. whoami is the one convenience alias, for `get /session`.
//
// # Credentials
//
// This API authenticates with a session cookie, not a bearer token. The cookie
// is HttpOnly and its value is returned exactly once, in the Set-Cookie header
// of a successful login — never in a response body. earl therefore captures it
// from that header and saves it, keyed by API base URL and account email, so it
// can hold several identities at once (an administrator and an ordinary user,
// say) and pick between them with --email.
//
// The saved value is a live session token. Anyone holding it is signed in as
// that account until it expires, so the file is written 0600 in a 0700
// directory, and earl never prints it, logs it, or echoes it in an error — the
// same rule the server follows for tokens, cookies, and hashes.
//
//	$XDG_CONFIG_HOME/earl/<env>/credentials.json   (or ~/.config/earl/<env>/…)
//
// The <env> segment comes from EC_ENV, so a run against development and a run
// against production never share a credential file. EARL_CREDENTIALS overrides
// the path entirely.
//
// # Configuration
//
// earl's own flags read EARL_-prefixed environment variables (--base-url is fed
// by EARL_BASE_URL), keeping them clear of the EC_ namespace the server and
// ecdb use — pointing earl at another host should not require touching, or risk
// disturbing, a server's configuration. The one variable shared with the rest
// of the project is EC_ENV, because a checkout has one idea of which
// environment it is working in. Dotenv files are loaded from the working
// directory before flags are parsed, exactly as elsewhere.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/dotenv"
	"github.com/mdhender/ecv8-api/internal/version"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

// envVarPrefix is the prefix ff uses for earl's own flags. It is deliberately
// not the server's EC_: earl is pointed at whatever host is being tested, and
// that should never mean editing the variables a server reads.
const envVarPrefix = "EARL"

const (
	// defaultBaseURL is the API earl talks to when nothing says otherwise: a
	// local server on its default listen address.
	//
	// It addresses the API directly rather than going through the development
	// proxy. earl is not a browser, so none of the reasons to insist on the
	// proxy origin apply to it: it sends no Origin header, cross-origin
	// protection therefore does not reject it, and it stores the session cookie
	// itself instead of relying on a browser's cookie rules.
	defaultBaseURL = "http://localhost:3000"

	// defaultAPIPath is appended to a base URL that carries no path of its own,
	// so --base-url https://ec.example.com does the obvious thing.
	defaultAPIPath = "/api/v1"

	// defaultCookieName matches the server's --cookie-name default. A server
	// configured with a different name needs --cookie-name to match, because
	// the cookie earl is looking for in the login response is named by it.
	defaultCookieName = "ec_session"
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
		fmt.Fprintf(os.Stderr, "earl: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, env, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, ff.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "earl: %v\n", err)
		os.Exit(1)
	}
}

// run parses args and executes the selected command.
func run(ctx context.Context, env string, args []string, stderr io.Writer) error {
	cmd := command(env)

	// ff, like the standard flag package, stops parsing flags at the first
	// positional argument, so `earl post /admin/accounts -d '{…}'` would treat
	// -d as a stray argument. Hoisting each subcommand's flags ahead of its
	// path lets flags follow the path, which is how anyone who has used curl
	// expects to be able to type it.
	if err := cmd.Parse(reorderArgs(args), ff.WithEnvVarPrefix(envVarPrefix)); err != nil {
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
	return cmd.Run(ctx)
}

// command builds the command tree.
func command(env string) *ff.Command {
	rootFlags := ff.NewFlagSet("earl")

	var (
		baseURL    string
		email      string
		cookieName string
		timeout    time.Duration
		verbose    bool
		insecure   bool
	)
	rootFlags.StringVar(&baseURL, 0, "base-url", defaultBaseURL,
		"API base URL; "+defaultAPIPath+" is appended when it carries no path of its own")
	rootFlags.StringVar(&email, 0, "email", "",
		"account whose saved session to use; required only when several are saved")
	rootFlags.StringVar(&cookieName, 0, "cookie-name", defaultCookieName,
		"name of the session cookie; must match the server's --cookie-name")
	rootFlags.DurationVar(&timeout, 0, "timeout", 30*time.Second,
		"how long to wait for a response")
	rootFlags.BoolVarDefault(&verbose, 0, "verbose", false,
		"report each request and response status on stderr")
	rootFlags.BoolVarDefault(&insecure, 0, "insecure", false,
		"skip TLS verification; for the development proxy's internal CA, never for production")

	root := &ff.Command{
		Name:      "earl",
		Usage:     "earl [FLAGS] <SUBCOMMAND> ...",
		ShortHelp: "command-line client for the ECV8 API",
		LongHelp: "The verb and path you would send are what you type:\n" +
			"\n" +
			"  earl get /session\n" +
			"  earl post /admin/accounts -d '{\"email\":\"t@x.com\",\"role\":\"user\"}'\n" +
			"\n" +
			"Paths are relative to --base-url, which already includes " + defaultAPIPath + ".\n" +
			"Flags are fed by EARL_-prefixed environment variables: --base-url by\n" +
			"EARL_BASE_URL. A session captured by `earl login` is saved per base URL\n" +
			"and account and attached automatically; --email picks between several.",
		Flags: rootFlags,
	}

	// newEarl builds the client from the resolved flags. Deferring it to each
	// Exec keeps the flag values authoritative: they are not parsed yet here.
	newEarl := func() (*earl, error) {
		resolved, err := normalizeBaseURL(baseURL)
		if err != nil {
			return nil, err
		}
		return &earl{
			baseURL:    resolved,
			email:      strings.TrimSpace(email),
			cookieName: cookieName,
			env:        env,
			http:       newHTTPClient(timeout, insecure),
			verbose:    verbose,
			out:        os.Stdout,
			errOut:     os.Stderr,
		}, nil
	}

	// verbCmd builds a command that sends no body: a single positional PATH.
	verbCmd := func(name, method string) *ff.Command {
		fs := ff.NewFlagSet(name).SetParent(rootFlags)
		var noAuth bool
		fs.BoolVarDefault(&noAuth, 0, "no-auth", false,
			"send without the saved session, to exercise what an anonymous caller sees")
		return &ff.Command{
			Name:      name,
			Usage:     "earl " + name + " [FLAGS] PATH",
			ShortHelp: method + " the given API path",
			Flags:     fs,
			Exec: func(ctx context.Context, args []string) error {
				path, err := pathArg(name, args)
				if err != nil {
					return err
				}
				e, err := newEarl()
				if err != nil {
					return err
				}
				return e.request(ctx, method, path, nil, noAuth)
			},
		}
	}

	// bodyCmd builds a command that may carry a body: a positional PATH plus an
	// optional -d.
	bodyCmd := func(name, method string) *ff.Command {
		fs := ff.NewFlagSet(name).SetParent(rootFlags)
		var (
			data   string
			noAuth bool
		)
		fs.StringVar(&data, 'd', "data", "",
			"request body: inline JSON, @file, or @- for stdin")
		fs.BoolVarDefault(&noAuth, 0, "no-auth", false,
			"send without the saved session, to exercise what an anonymous caller sees")
		return &ff.Command{
			Name:      name,
			Usage:     "earl " + name + " [FLAGS] PATH",
			ShortHelp: method + " the given API path",
			Flags:     fs,
			Exec: func(ctx context.Context, args []string) error {
				path, err := pathArg(name, args)
				if err != nil {
					return err
				}
				body, err := readValue(data)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				e, err := newEarl()
				if err != nil {
					return err
				}
				return e.request(ctx, method, path, body, noAuth)
			},
		}
	}

	loginFlags := ff.NewFlagSet("login").SetParent(rootFlags)
	var loginPassword string
	loginFlags.StringVar(&loginPassword, 0, "password", "",
		"account password; @- reads stdin, @file reads a file, or set EARL_PASSWORD")
	login := &ff.Command{
		Name:      "login",
		Usage:     "earl login [--email EMAIL] [--password PASSWORD]",
		ShortHelp: "authenticate and save the session for later commands",
		LongHelp: "A password given on the command line is visible to anyone who can list\n" +
			"processes. Prefer EARL_PASSWORD, or --password @- to read it from stdin.",
		Flags: loginFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("login takes no positional arguments")
			}
			secret, err := readValue(loginPassword)
			if err != nil {
				return fmt.Errorf("login: %w", err)
			}
			e, err := newEarl()
			if err != nil {
				return err
			}
			return e.login(ctx, string(secret))
		},
	}

	logoutFlags := ff.NewFlagSet("logout").SetParent(rootFlags)
	logout := &ff.Command{
		Name:      "logout",
		Usage:     "earl logout [--email EMAIL]",
		ShortHelp: "end the saved session and forget it",
		Flags:     logoutFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("logout takes no positional arguments")
			}
			e, err := newEarl()
			if err != nil {
				return err
			}
			return e.logout(ctx)
		},
	}

	whoamiFlags := ff.NewFlagSet("whoami").SetParent(rootFlags)
	whoami := &ff.Command{
		Name:      "whoami",
		Usage:     "earl whoami",
		ShortHelp: "show the current session (GET /session)",
		Flags:     whoamiFlags,
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("whoami takes no positional arguments")
			}
			e, err := newEarl()
			if err != nil {
				return err
			}
			return e.request(ctx, http.MethodGet, "/session", nil, false)
		},
	}

	identitiesFlags := ff.NewFlagSet("identities").SetParent(rootFlags)
	identities := &ff.Command{
		Name:      "identities",
		Usage:     "earl identities",
		ShortHelp: "list the saved sessions for this base URL",
		Flags:     identitiesFlags,
		Exec: func(_ context.Context, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("identities takes no positional arguments")
			}
			e, err := newEarl()
			if err != nil {
				return err
			}
			return e.identities()
		},
	}

	versionFlags := ff.NewFlagSet("version").SetParent(rootFlags)
	versionCmd := &ff.Command{
		Name:      "version",
		Usage:     "earl version",
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

	root.Subcommands = append(root.Subcommands,
		verbCmd("get", http.MethodGet),
		bodyCmd("post", http.MethodPost),
		bodyCmd("put", http.MethodPut),
		bodyCmd("patch", http.MethodPatch),
		// No endpoint accepts a body on DELETE, so neither does earl.
		verbCmd("delete", http.MethodDelete),
		login, logout, whoami, identities, versionCmd,
	)
	return root
}

// pathArg returns the single PATH positional for a verb command, or an error
// naming the command.
func pathArg(cmd string, args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("%s requires exactly one PATH argument", cmd)
	}
	return args[0], nil
}

// valueFlags are the flags that consume the following argument as their value.
// reorderArgs needs them so a flag's value is never mistaken for the positional
// path. Keep it in step with the flags defined in command.
var valueFlags = map[string]bool{
	"-d":            true,
	"--data":        true,
	"--email":       true,
	"--password":    true,
	"--base-url":    true,
	"--cookie-name": true,
	"--timeout":     true,
}

// reorderArgs rewrites a command line so each subcommand's flags precede its
// positional path, letting `earl post /admin/accounts -d '{…}'` parse under ff.
//
// It is section-aware: the leading root flags and the subcommand name keep their
// place — ff routes on the subcommand name, which must stay the first
// positional — and only the tokens after it are hoisted, flags (with the values
// they consume) first. A literal "--" ends flag processing, matching ff.
func reorderArgs(args []string) []string {
	out := make([]string, 0, len(args))

	// Root section: copy root flags, and any values they take, through to and
	// including the subcommand name.
	i := 0
	for i < len(args) {
		tok := args[i]
		if tok == "--" {
			return append(out, args[i:]...)
		}
		if isFlag(tok) {
			out = append(out, tok)
			i++
			if takesValue(tok) && i < len(args) {
				out = append(out, args[i])
				i++
			}
			continue
		}
		out = append(out, tok) // the subcommand name
		i++
		break
	}

	// Subcommand section: hoist flags ahead of positionals.
	var flags, positionals []string
	for i < len(args) {
		tok := args[i]
		if tok == "--" {
			positionals = append(positionals, args[i:]...)
			break
		}
		if isFlag(tok) {
			flags = append(flags, tok)
			i++
			if takesValue(tok) && i < len(args) {
				flags = append(flags, args[i])
				i++
			}
			continue
		}
		positionals = append(positionals, tok)
		i++
	}
	out = append(out, flags...)
	return append(out, positionals...)
}

// isFlag reports whether tok is a flag token rather than a positional. A bare
// "-" is a positional; "--" is handled by the caller.
func isFlag(tok string) bool {
	return len(tok) >= 2 && tok[0] == '-' && tok != "--"
}

// takesValue reports whether a flag token consumes the next argument. A flag
// written with "=" carries its own value and consumes nothing more.
func takesValue(tok string) bool {
	if strings.Contains(tok, "=") {
		return false
	}
	return valueFlags[tok]
}
