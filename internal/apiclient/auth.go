// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// sessionEnvelope is the part of a session response this package reads: when
// the session expires. Everything else is passed through to the caller
// untouched, because interpreting it would be the client deciding what the API
// means.
type sessionEnvelope struct {
	Data struct {
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"data"`
}

// Login authenticates and saves the session for later commands.
//
// The credential is the session cookie. It is returned exactly once, in the
// Set-Cookie header of this response, and never appears in a response body — so
// capturing it here is the only opportunity there will be. It is written to the
// file both commands read, so signing in with one signs in with the other.
func (c *Client) Login(ctx context.Context, password string) error {
	email := strings.TrimSpace(c.email)
	if email == "" || password == "" {
		return fmt.Errorf("login needs an email and a password "+
			"(--email/--password, or %s_EMAIL/%s_PASSWORD)", EnvVarPrefix, EnvVarPrefix)
	}

	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return fmt.Errorf("encode login request: %w", err)
	}

	status, respBody, response, err := c.do(ctx, http.MethodPost, SessionPath, body, credential{})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return c.emit(http.MethodPost, SessionPath, status, respBody)
	}

	cookie, err := c.sessionCookie(response)
	if err != nil {
		return err
	}

	// Prefer the cookie's own expiry, which is what the server will enforce.
	// The body's expires_at is the same instant, and is the fallback for a
	// server that sends a session cookie without an explicit Expires.
	expiresAt := cookie.Expires
	if expiresAt.IsZero() {
		var envelope sessionEnvelope
		if err := json.Unmarshal(respBody, &envelope); err == nil {
			expiresAt = envelope.Data.ExpiresAt
		}
	}

	store, err := c.loadCredentials()
	if err != nil {
		return err
	}
	store.put(c.baseURL, email, credential{
		Cookie:     cookie.Value,
		CookieName: cookie.Name,
		ExpiresAt:  expiresAt,
	})
	if err := c.saveCredentials(store); err != nil {
		return err
	}

	// The note goes to stderr and the response to stdout, so `earl login | jq`
	// still works.
	if expiresAt.IsZero() {
		fmt.Fprintf(c.errOut, "logged in as %s at %s\n", strings.ToLower(email), c.baseURL)
	} else {
		fmt.Fprintf(c.errOut, "logged in as %s at %s (expires %s)\n",
			strings.ToLower(email), c.baseURL, expiresAt.Format(time.RFC3339))
	}
	c.writeBody(respBody)
	return nil
}

// Logout ends the saved session on the server and forgets it locally.
//
// The local credential is dropped whatever the server says. A session the
// server has already forgotten, or refuses to talk about, is not one worth
// keeping on disk — leaving it would only produce confusing 401s later. It is
// dropped from the shared file, so logging out of one command logs out of both:
// there is one session, and it has ended.
func (c *Client) Logout(ctx context.Context) error {
	store, err := c.loadCredentials()
	if err != nil {
		return err
	}
	who, saved := store.resolve(c.baseURL, c.email)
	if saved.Cookie == "" {
		return c.noSessionError(store)
	}

	status, respBody, _, err := c.do(ctx, http.MethodDelete, SessionPath, nil, saved)
	if err != nil {
		return err
	}

	store.drop(c.baseURL, who)
	if err := c.saveCredentials(store); err != nil {
		return err
	}

	switch {
	case status/100 == 2:
		fmt.Fprintf(c.errOut, "logged out %s at %s\n", who, c.baseURL)
		return nil
	case status == http.StatusUnauthorized:
		fmt.Fprintf(c.errOut, "session for %s was already invalid; forgotten locally\n", who)
		return nil
	default:
		return c.emit(http.MethodDelete, SessionPath, status, respBody)
	}
}

// Whoami reports the session the server sees for the saved credential. It is
// GET /session and nothing else — the server decides what a session is, and an
// answer assembled locally would not be evidence of anything.
func (c *Client) Whoami(ctx context.Context) error {
	return c.Request(ctx, http.MethodGet, SessionPath, nil, false)
}

// Identities lists the sessions saved for this base URL.
//
// It prints emails and expiries and never the cookie itself. Someone reading
// their terminal history, or a screen recording, should not find a live session
// token in it. It also prints the file, because that file is shared: seeing the
// path is how it becomes obvious that both commands are looking at one list.
func (c *Client) Identities() error {
	store, err := c.loadCredentials()
	if err != nil {
		return err
	}
	path, err := credentialsPath(c.env)
	if err != nil {
		return err
	}

	emails := store.emails(c.baseURL)
	if len(emails) == 0 {
		fmt.Fprintf(c.errOut, "no saved sessions for %s (%s)\n", c.baseURL, path)
		return nil
	}

	fmt.Fprintf(c.errOut, "%s (%s)\n", c.baseURL, path)
	for _, email := range emails {
		saved := store[c.baseURL][email]
		switch {
		case saved.ExpiresAt.IsZero():
			fmt.Fprintf(c.out, "%s\n", email)
		case saved.expired():
			fmt.Fprintf(c.out, "%s\texpired %s\n", email, saved.ExpiresAt.Format(time.RFC3339))
		default:
			fmt.Fprintf(c.out, "%s\texpires %s\n", email, saved.ExpiresAt.Format(time.RFC3339))
		}
	}
	if len(emails) > 1 && c.email == "" {
		fmt.Fprintf(c.errOut, "several saved; pass --email to choose one\n")
	}
	return nil
}

// noSessionError reports why no session could be used, naming the choices when
// the problem is that there are several, and naming the running command's own
// spelling of login when the problem is that there are none.
func (c *Client) noSessionError(store credentialStore) error {
	emails := store.emails(c.baseURL)
	if len(emails) > 1 && c.email == "" {
		return fmt.Errorf("several saved sessions for %s; pass --email (one of: %s)",
			c.baseURL, strings.Join(emails, ", "))
	}
	if c.email != "" {
		return fmt.Errorf("no saved session for %s at %s; run `%s` first",
			strings.ToLower(c.email), c.baseURL, c.loginCommand)
	}
	return fmt.Errorf("no saved session for %s; run `%s` first", c.baseURL, c.loginCommand)
}

// loadCredentials resolves the credential path and reads it.
func (c *Client) loadCredentials() (credentialStore, error) {
	path, err := credentialsPath(c.env)
	if err != nil {
		return nil, err
	}
	return loadCredentials(path)
}

// saveCredentials resolves the credential path and writes store to it.
func (c *Client) saveCredentials(store credentialStore) error {
	path, err := credentialsPath(c.env)
	if err != nil {
		return err
	}
	return saveCredentials(path, store)
}
