// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// sessionEnvelope is the part of a session response earl reads: when the
// session expires. Everything else is passed through to the caller untouched,
// because interpreting it would be the client deciding what the API means.
type sessionEnvelope struct {
	Data struct {
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"data"`
}

// login authenticates and saves the session for later commands.
//
// The credential is the session cookie. It is returned exactly once, in the
// Set-Cookie header of this response, and never appears in a response body — so
// capturing it here is the only opportunity there will be.
func (e *earl) login(ctx context.Context, password string) error {
	email := strings.TrimSpace(e.email)
	if email == "" || password == "" {
		return fmt.Errorf("login needs an email and a password " +
			"(--email/--password, or EARL_EMAIL/EARL_PASSWORD)")
	}

	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return fmt.Errorf("encode login request: %w", err)
	}

	status, respBody, response, err := e.do(ctx, http.MethodPost, "/session", body, credential{})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return e.emit(http.MethodPost, "/session", status, respBody)
	}

	cookie, err := e.sessionCookie(response)
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

	store, err := e.loadCredentials()
	if err != nil {
		return err
	}
	store.put(e.baseURL, email, credential{
		Cookie:     cookie.Value,
		CookieName: cookie.Name,
		ExpiresAt:  expiresAt,
	})
	if err := e.saveCredentials(store); err != nil {
		return err
	}

	// The note goes to stderr and the response to stdout, so `earl login | jq`
	// still works.
	if expiresAt.IsZero() {
		fmt.Fprintf(e.errOut, "logged in as %s at %s\n", strings.ToLower(email), e.baseURL)
	} else {
		fmt.Fprintf(e.errOut, "logged in as %s at %s (expires %s)\n",
			strings.ToLower(email), e.baseURL, expiresAt.Format(time.RFC3339))
	}
	e.writeBody(respBody)
	return nil
}

// logout ends the saved session on the server and forgets it locally.
//
// The local credential is dropped whatever the server says. A session the
// server has already forgotten, or refuses to talk about, is not one worth
// keeping on disk — leaving it would only produce confusing 401s later.
func (e *earl) logout(ctx context.Context) error {
	store, err := e.loadCredentials()
	if err != nil {
		return err
	}
	who, saved := e.resolveOrExplain(store, "")
	if saved.Cookie == "" {
		return e.noSessionError(store)
	}

	status, respBody, _, err := e.do(ctx, http.MethodDelete, "/session", nil, saved)
	if err != nil {
		return err
	}

	store.drop(e.baseURL, who)
	if err := e.saveCredentials(store); err != nil {
		return err
	}

	switch {
	case status/100 == 2:
		fmt.Fprintf(e.errOut, "logged out %s at %s\n", who, e.baseURL)
		return nil
	case status == http.StatusUnauthorized:
		fmt.Fprintf(e.errOut, "session for %s was already invalid; forgotten locally\n", who)
		return nil
	default:
		return e.emit(http.MethodDelete, "/session", status, respBody)
	}
}

// identities lists the sessions saved for this base URL.
//
// It prints emails and expiries and never the cookie itself. Someone reading
// their terminal history, or a screen recording, should not find a live session
// token in it.
func (e *earl) identities() error {
	store, err := e.loadCredentials()
	if err != nil {
		return err
	}
	path, err := credentialsPath(e.env)
	if err != nil {
		return err
	}

	emails := store.emails(e.baseURL)
	if len(emails) == 0 {
		fmt.Fprintf(e.errOut, "no saved sessions for %s (%s)\n", e.baseURL, path)
		return nil
	}

	fmt.Fprintf(e.errOut, "%s (%s)\n", e.baseURL, path)
	for _, email := range emails {
		saved := store[e.baseURL][email]
		switch {
		case saved.ExpiresAt.IsZero():
			fmt.Fprintf(e.out, "%s\n", email)
		case saved.expired():
			fmt.Fprintf(e.out, "%s\texpired %s\n", email, saved.ExpiresAt.Format(time.RFC3339))
		default:
			fmt.Fprintf(e.out, "%s\texpires %s\n", email, saved.ExpiresAt.Format(time.RFC3339))
		}
	}
	if len(emails) > 1 && e.email == "" {
		fmt.Fprintf(e.errOut, "several saved; pass --email to choose one\n")
	}
	return nil
}

// resolveOrExplain picks the active session, defaulting the email to the
// client's.
func (e *earl) resolveOrExplain(store credentialStore, email string) (string, credential) {
	if email == "" {
		email = e.email
	}
	return store.resolve(e.baseURL, email)
}

// noSessionError reports why no session could be used, naming the choices when
// the problem is that there are several.
func (e *earl) noSessionError(store credentialStore) error {
	emails := store.emails(e.baseURL)
	if len(emails) > 1 && e.email == "" {
		return fmt.Errorf("several saved sessions for %s; pass --email (one of: %s)",
			e.baseURL, strings.Join(emails, ", "))
	}
	if e.email != "" {
		return fmt.Errorf("no saved session for %s at %s; run `earl login` first",
			strings.ToLower(e.email), e.baseURL)
	}
	return fmt.Errorf("no saved session for %s; run `earl login` first", e.baseURL)
}

// loadCredentials resolves the credential path and reads it.
func (e *earl) loadCredentials() (credentialStore, error) {
	path, err := credentialsPath(e.env)
	if err != nil {
		return nil, err
	}
	return loadCredentials(path)
}

// saveCredentials resolves the credential path and writes store to it.
func (e *earl) saveCredentials(store credentialStore) error {
	path, err := credentialsPath(e.env)
	if err != nil {
		return err
	}
	return saveCredentials(path, store)
}
