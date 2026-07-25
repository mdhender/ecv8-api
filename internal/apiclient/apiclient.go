// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package apiclient speaks HTTP to the ECV8 API on behalf of the command-line
// tools, and holds the sessions they share.
//
// It exists because there are two clients rather than one. `earl` is the raw
// one — the verb and path you would send are what you type — and `ec` is the
// convenience one a game master uses. They are separate commands with separate
// surfaces, but the same server, the same cookie rules, and the same saved
// credential file, so the transport and the credential store live here instead
// of being written twice and drifting apart.
//
// # HTTP and only HTTP
//
// This package imports no store package, opens no database, and knows no game
// rules. Anything it appears to know about accounts or games is only the shape
// of a JSON body being passed through. That restraint is what makes either
// client evidence that the server implements its own rules: a client that
// reimplements them stops proving anything. It belongs to the clients, and
// nothing the server builds may import it.
//
// # Credentials
//
// This API authenticates with a session cookie, not a bearer token. The cookie
// is HttpOnly and its value is returned exactly once, in the Set-Cookie header
// of a successful login — never in a response body. Login therefore captures it
// from that header and saves it, keyed by API base URL and account email, so
// one file can hold several identities at once (an administrator and an
// ordinary user, say) and Config.Email picks between them.
//
// The server's cookie name is configurable, and a client does not have to be
// told it: a successful login sets exactly one cookie, so whatever arrives is
// the session, and its name is saved alongside it for later requests.
// Config.CookieName exists only to break a tie when something in front of the
// API — a load balancer adding a routing cookie — makes "the only cookie"
// ambiguous.
//
// The saved value is a live session token. Anyone holding it is signed in as
// that account until it expires, so the file is written 0600 in a 0700
// directory, and nothing here ever prints it, logs it, or echoes it in an
// error — the same rule the server follows for tokens, cookies, and hashes.
//
//	$XDG_CONFIG_HOME/ecv8/<env>/credentials.json   (or ~/.config/ecv8/<env>/…)
//
// The <env> segment comes from EC_ENV, so a run against development and a run
// against production never share a credential file.
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/peterbourgon/ff/v4"
)

// EnvVarPrefix is the prefix ff uses for the client flags, and the prefix of
// the credential-path override.
//
// It is deliberately not the server's EC_: a client is pointed at whatever host
// is being worked on, and that should never mean editing — or risk disturbing —
// the variables a server reads. It is equally deliberately not per-command,
// because ECV8_BASE_URL selecting a server for one tool and not the other would
// point them at different servers, and sessions are keyed by base URL: the
// shared credential file would then quietly stop being shared.
const EnvVarPrefix = "ECV8"

// ConfigDirName is the directory under ~/.config that both clients keep their
// state in. It names the project, not a command, because both write it.
const ConfigDirName = "ecv8"

const (
	// DefaultBaseURL is the API a client talks to when nothing says otherwise: a
	// local server on its default listen address.
	//
	// It addresses the API directly rather than going through the development
	// proxy. These are not browsers, so none of the reasons to insist on the
	// proxy origin apply: they send no Origin header, cross-origin protection
	// therefore does not reject them, and they store the session cookie
	// themselves instead of relying on a browser's cookie rules.
	DefaultBaseURL = "http://localhost:3000"

	// DefaultAPIPath is appended to a base URL that carries no path of its own,
	// so --base-url https://ec.example.com does the obvious thing.
	DefaultAPIPath = "/api/v1"

	// DefaultCookieName matches the server's --cookie-name default. The real
	// name is learned from the login response, so this is only the last-resort
	// fallback for a saved session from before names were recorded.
	DefaultCookieName = "ec_session"

	// SessionPath is the endpoint that creates, reports, and ends a session. It
	// is the one path this package names, because login and logout have to know
	// where the cookie comes from.
	SessionPath = "/session"
)

// Config is everything a client needs: which server, which saved identity, and
// where to write the result.
//
// The first group is registered as flags by Bind. The rest is supplied by the
// command itself, because none of it is the caller's to choose — UserAgent is
// which binary is running, and Env comes from EC_ENV before flags are parsed.
type Config struct {
	BaseURL    string
	Email      string
	CookieName string
	Timeout    time.Duration
	Verbose    bool
	Insecure   bool

	// Env scopes the credential file, so a development session and a production
	// session are never in the same file.
	Env string

	// UserAgent identifies the binary. The server records it on the session
	// row, so saying who we are makes an `ec` session distinguishable from an
	// `earl` one in an admin session listing.
	UserAgent string

	// LoginCommand is how the running binary spells its own login command —
	// "earl login", "ec app login" — so an error that tells someone to sign in
	// names the command they are actually using. Defaults to "login".
	LoginCommand string

	// Out carries a response body and ErrOut carries progress, so a response
	// can be piped into jq while notes stay on the terminal. Both default to
	// the process's own streams.
	Out    io.Writer
	ErrOut io.Writer
}

// Bind registers the flags every client shares on fs and returns the Config
// they write into. The returned value is only valid after fs has been parsed.
//
// One helper registers them for both commands so the flag names, defaults, and
// help text cannot drift apart — the same reason internal/config registers
// --db-path once for two binaries.
func Bind(fs *ff.FlagSet) *Config {
	var cfg Config
	fs.StringVar(&cfg.BaseURL, 0, "base-url", DefaultBaseURL,
		"API base URL; "+DefaultAPIPath+" is appended when it carries no path of its own")
	fs.StringVar(&cfg.Email, 0, "email", "",
		"account whose saved session to use; required only when several are saved")
	// Empty by default so "not given" is distinguishable from "given as the
	// usual name". Login learns the name from the response; this only settles
	// which cookie is the session when something else set one too.
	fs.StringVar(&cfg.CookieName, 0, "cookie-name", "",
		"which cookie carries the session; needed only if the login response sets more than one")
	fs.DurationVar(&cfg.Timeout, 0, "timeout", 30*time.Second,
		"how long to wait for a response")
	fs.BoolVarDefault(&cfg.Verbose, 0, "verbose", false,
		"report each request and response status on stderr")
	fs.BoolVarDefault(&cfg.Insecure, 0, "insecure", false,
		"skip TLS verification; for the development proxy's internal CA, never for production")
	return &cfg
}

// Client is a configured client. Build it with New, after the flags Bind
// registered have been parsed.
type Client struct {
	baseURL      string
	email        string
	cookieName   string
	env          string
	userAgent    string
	loginCommand string
	http         *http.Client
	verbose      bool
	out          io.Writer
	errOut       io.Writer
}

// New validates the configuration and builds the client.
//
// It is called from inside a command's Exec rather than while the command tree
// is built, because the flag values are not parsed yet at that point — and
// because a --help request must not be refused for a --base-url it would never
// use.
func New(cfg *Config) (*Client, error) {
	baseURL, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	out, errOut := cfg.Out, cfg.ErrOut
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	loginCommand := cfg.LoginCommand
	if loginCommand == "" {
		loginCommand = "login"
	}
	return &Client{
		baseURL:      baseURL,
		email:        strings.TrimSpace(cfg.Email),
		cookieName:   cfg.CookieName,
		env:          cfg.Env,
		userAgent:    cfg.UserAgent,
		loginCommand: loginCommand,
		http:         newHTTPClient(cfg.Timeout, cfg.Insecure),
		verbose:      cfg.Verbose,
		out:          out,
		errOut:       errOut,
	}, nil
}

// newHTTPClient builds the transport.
//
// Redirects are refused rather than followed: these are tools for seeing what
// an endpoint actually returns, and silently following a 302 would hide it. It
// would also risk carrying the session cookie to wherever the redirect pointed.
func newHTTPClient(timeout time.Duration, insecure bool) *http.Client {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if insecure {
		// Only ever appropriate for the development proxy, whose certificate is
		// signed by Caddy's internal CA. `caddy trust` is the better answer;
		// this is the escape hatch when that is not possible.
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return client
}

// Request performs one API call and prints the result: METHOD baseURL+path with
// the given body, carrying the saved session unless noAuth. A 2xx writes the
// response body to out; anything else is an error carrying the status and the
// server's Problem document.
func (c *Client) Request(ctx context.Context, method, path string, body []byte, noAuth bool) error {
	var session credential
	if !noAuth {
		store, err := c.loadCredentials()
		if err != nil {
			return err
		}
		who, saved := store.resolve(c.baseURL, c.email)
		session = saved
		if session.Cookie != "" && c.verbose {
			fmt.Fprintf(c.errOut, "# as %s\n", who)
		}
	}

	status, respBody, _, err := c.do(ctx, method, path, body, session)
	if err != nil {
		return err
	}
	return c.emit(method, path, status, respBody)
}

// do sends one request and returns the status, body, and response, without
// interpreting any of them. It is the shared transport beneath Request, Login,
// and Logout.
//
// The session is attached as a cookie because that is the only credential this
// API accepts; there is no bearer scheme to fall back on. A zero session sends
// the request anonymously.
func (c *Client) do(ctx context.Context, method, path string, body []byte, session credential) (int, []byte, *http.Response, error) {
	target := joinURL(c.baseURL, path)

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		request.Header.Set("User-Agent", c.userAgent)
	}
	if session.Cookie != "" {
		request.AddCookie(&http.Cookie{Name: c.cookieNameFor(session), Value: session.Cookie})
	}

	if c.verbose {
		// The cookie is deliberately absent from this line, and from every
		// other thing this package writes.
		fmt.Fprintf(c.errOut, "> %s %s (authenticated=%t, body=%d bytes)\n",
			method, target, session.Cookie != "", len(body))
	}

	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, target, err)
	}
	defer func() { _ = response.Body.Close() }()

	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read response from %s %s: %w", method, target, err)
	}
	if c.verbose {
		fmt.Fprintf(c.errOut, "< %d %s (%d bytes)\n",
			response.StatusCode, http.StatusText(response.StatusCode), len(respBody))
	}
	return response.StatusCode, respBody, response, nil
}

// emit renders a completed response. A 2xx writes the body to out and succeeds;
// anything else becomes an error carrying the status line and the server's
// Problem document, so the non-zero exit and the explanation arrive together.
func (c *Client) emit(method, path string, status int, body []byte) error {
	if status/100 == 2 {
		c.writeBody(body)
		return nil
	}
	message := fmt.Sprintf("%s %s -> %d %s", method, path, status, http.StatusText(status))
	if len(body) > 0 {
		message += "\n" + formatJSON(body, true)
	}
	return fmt.Errorf("%s", message)
}

// writeBody writes a successful response: indented when out is a terminal, raw
// otherwise so a pipe into jq stays machine-readable. An empty body, as from a
// 204, writes nothing.
func (c *Client) writeBody(body []byte) {
	if len(body) == 0 {
		return
	}
	fmt.Fprintln(c.out, strings.TrimRight(formatJSON(body, isTerminal(c.out)), "\n"))
}

// sessionCookie picks the session cookie out of a login response.
//
// A client does not need to be told the server's cookie name: a successful
// login sets exactly one cookie, so whatever arrives is the session. That keeps
// a configurable server setting from being something the caller has to discover
// and repeat.
//
// The case that needs care is a load balancer adding a cookie of its own — an
// ALB's AWSALB, say — which would make "the only cookie" ambiguous. Rather than
// guess, and risk sending a routing cookie as a credential and saving the real
// session nowhere, the candidates are named and --cookie-name is asked for. An
// explicit --cookie-name always wins outright, so a caller who knows can say so
// and never depend on this reasoning at all.
func (c *Client) sessionCookie(response *http.Response) (*http.Cookie, error) {
	var candidates []*http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Value != "" {
			candidates = append(candidates, cookie)
		}
	}

	if c.cookieName != "" {
		for _, cookie := range candidates {
			if cookie.Name == c.cookieName {
				return cookie, nil
			}
		}
		return nil, fmt.Errorf("login succeeded but set no %q cookie%s; "+
			"check --cookie-name against the server's --cookie-name",
			c.cookieName, cookieNameList(candidates))
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("login succeeded but set no cookie, so there is no session to save")
	case 1:
		return candidates[0], nil
	default:
		return nil, fmt.Errorf("login set several cookies%s; "+
			"pass --cookie-name to say which one is the session",
			cookieNameList(candidates))
	}
}

// cookieNameFor returns the name to send a saved session back under.
//
// The name saved at login is authoritative. An explicit --cookie-name is the
// fallback, and the documented default the last resort, so a credential file
// written before names were saved still works instead of failing in a way that
// looks like an expired session.
func (c *Client) cookieNameFor(session credential) string {
	if session.CookieName != "" {
		return session.CookieName
	}
	if c.cookieName != "" {
		return c.cookieName
	}
	return DefaultCookieName
}

// cookieNameList renders the cookie names a response set, for an error message
// that tells the caller what there was to choose between. Only names are
// listed; a cookie's value is never shown.
func cookieNameList(cookies []*http.Cookie) string {
	if len(cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		names = append(names, cookie.Name)
	}
	return " (it set: " + strings.Join(names, ", ") + ")"
}

// normalizeBaseURL validates --base-url and fills in the API path when the URL
// carries none, so both of these address the same endpoint:
//
//	--base-url https://ec.example.com
//	--base-url https://ec.example.com/api/v1
func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("--base-url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("--base-url %q must be an absolute URL such as https://ec.example.com", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("--base-url scheme must be http or https, got %q", parsed.Scheme)
	}
	if path := strings.Trim(parsed.Path, "/"); path == "" {
		parsed.Path = DefaultAPIPath
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// joinURL joins the base URL and an API path with exactly one slash between
// them.
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// ReadValue turns a -d or --password value into bytes: "" is nothing, "@-"
// reads stdin, "@name" reads a file, and anything else is the literal value.
//
// The indirection matters for a password as much as for a body: @- keeps the
// secret out of the command line, where it would otherwise be visible to anyone
// who can list processes, and out of the shell history.
func ReadValue(value string) ([]byte, error) {
	switch {
	case value == "":
		return nil, nil
	case value == "@-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read from stdin: %w", err)
		}
		return bytes.TrimRight(data, "\r\n"), nil
	case strings.HasPrefix(value, "@"):
		data, err := os.ReadFile(value[1:])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", value[1:], err)
		}
		return data, nil
	default:
		return []byte(value), nil
	}
}

// formatJSON indents body when pretty is set and the bytes are valid JSON, and
// otherwise returns them unchanged. A Problem document and a data envelope are
// both JSON, so this covers everything the server sends.
func formatJSON(body []byte, pretty bool) string {
	if !pretty || !json.Valid(body) {
		return string(body)
	}
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, body, "", "  "); err != nil {
		return string(body)
	}
	return buffer.String()
}

// isTerminal reports whether w is a character device, which is what decides
// pretty against raw output. Anything that is not an *os.File is not a
// terminal.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
