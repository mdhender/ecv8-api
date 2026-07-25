// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package main

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

	"github.com/mdhender/ecv8-api/internal/version"
)

// earl is a configured client: where to talk, which saved identity to use, and
// where to write the result. out and errOut are separate so a response can be
// piped into jq while progress and errors stay on the terminal.
type earl struct {
	baseURL    string
	email      string
	cookieName string
	env        string
	http       *http.Client
	verbose    bool
	out        io.Writer
	errOut     io.Writer
}

// newHTTPClient builds the transport.
//
// Redirects are refused rather than followed: this is a tool for seeing what an
// endpoint actually returns, and silently following a 302 would hide it. It
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

// request performs one API call and prints the result: METHOD baseURL+path with
// the given body, carrying the saved session unless noAuth. A 2xx writes the
// response body to out; anything else is an error carrying the status and the
// server's Problem document.
func (e *earl) request(ctx context.Context, method, path string, body []byte, noAuth bool) error {
	var cookie string
	if !noAuth {
		store, err := e.loadCredentials()
		if err != nil {
			return err
		}
		who, credential := store.resolve(e.baseURL, e.email)
		cookie = credential.Cookie
		if cookie != "" && e.verbose {
			fmt.Fprintf(e.errOut, "# as %s\n", who)
		}
	}

	status, respBody, _, err := e.do(ctx, method, path, body, cookie)
	if err != nil {
		return err
	}
	return e.emit(method, path, status, respBody)
}

// do sends one request and returns the status, body, and response, without
// interpreting any of them. It is the shared transport beneath request, login,
// and logout.
//
// The session is attached as a cookie because that is the only credential this
// API accepts; there is no bearer scheme to fall back on.
func (e *earl) do(ctx context.Context, method, path string, body []byte, cookie string) (int, []byte, *http.Response, error) {
	target := joinURL(e.baseURL, path)

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
	// The server records the user agent on the session row, so saying who we
	// are makes an earl session identifiable in an admin session listing.
	request.Header.Set("User-Agent", "earl/"+version.Version.String())
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: e.cookieName, Value: cookie})
	}

	if e.verbose {
		// The cookie is deliberately absent from this line, and from every
		// other thing earl writes.
		fmt.Fprintf(e.errOut, "> %s %s (authenticated=%t, body=%d bytes)\n",
			method, target, cookie != "", len(body))
	}

	response, err := e.http.Do(request)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, target, err)
	}
	defer func() { _ = response.Body.Close() }()

	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read response from %s %s: %w", method, target, err)
	}
	if e.verbose {
		fmt.Fprintf(e.errOut, "< %d %s (%d bytes)\n",
			response.StatusCode, http.StatusText(response.StatusCode), len(respBody))
	}
	return response.StatusCode, respBody, response, nil
}

// emit renders a completed response. A 2xx writes the body to out and succeeds;
// anything else becomes an error carrying the status line and the server's
// Problem document, so the non-zero exit and the explanation arrive together.
func (e *earl) emit(method, path string, status int, body []byte) error {
	if status/100 == 2 {
		e.writeBody(body)
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
func (e *earl) writeBody(body []byte) {
	if len(body) == 0 {
		return
	}
	fmt.Fprintln(e.out, strings.TrimRight(formatJSON(body, isTerminal(e.out)), "\n"))
}

// sessionCookie returns the value of the session cookie in a response, or "" if
// the server did not set one.
func (e *earl) sessionCookie(response *http.Response) *http.Cookie {
	for _, cookie := range response.Cookies() {
		if cookie.Name == e.cookieName && cookie.Value != "" {
			return cookie
		}
	}
	return nil
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
		parsed.Path = defaultAPIPath
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// joinURL joins the base URL and an API path with exactly one slash between
// them.
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// readValue turns a -d or --password value into bytes: "" is nothing, "@-"
// reads stdin, "@name" reads a file, and anything else is the literal value.
//
// The indirection matters for a password as much as for a body: @- keeps the
// secret out of the command line, where it would otherwise be visible to anyone
// who can list processes, and out of the shell history.
func readValue(value string) ([]byte, error) {
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
