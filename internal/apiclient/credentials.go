// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package apiclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// credential is one saved session: the cookie the server issued, the name it
// arrived under, and when it expires.
//
// The cookie value is a live session token. The server stores only its
// fingerprint and cannot recover it, so this file is the only copy — and anyone
// who reads it is signed in as that account until it expires. That is why the
// file is 0600 inside a 0700 directory, and why nothing in this package ever
// prints this field.
//
// CookieName is saved with it because the server's cookie name is configurable,
// and a client learns it from the login response rather than being told. Saving
// it is what lets later requests send the cookie back correctly without the
// caller having to know the name either.
type credential struct {
	Cookie     string    `json:"cookie"`
	CookieName string    `json:"cookie_name"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// expired reports whether the saved expiry has passed. It is advisory: the
// server is the authority on whether a session is still good, so the cookie is
// sent regardless and a 401 is the answer. This only shapes what Identities
// shows.
func (c credential) expired() bool {
	return !c.ExpiresAt.IsZero() && time.Now().After(c.ExpiresAt)
}

// credentialStore maps an API base URL to the sessions saved against it, keyed
// by lowercased account email.
//
// Keying by both means one file can hold an administrator and an ordinary user
// for the same server at once — the usual shape of exercising an authorisation
// boundary — and keying by base URL means a development session is never sent
// to production.
type credentialStore map[string]map[string]credential

// credentialsPath returns the file sessions are saved in: ECV8_CREDENTIALS when
// set, else $XDG_CONFIG_HOME/ecv8/<env>/credentials.json, else
// ~/.config/ecv8/<env>/credentials.json.
//
// The directory is named for the project rather than for either command,
// because both commands share it: `earl login` and `ec app login` write the same
// file, so a game master signs in once and both tools are signed in. That is
// also why the environment variable is ECV8_ and not a per-command name — two
// tools reading two different overrides would quietly stop sharing.
//
// The env segment keeps environments apart, so a development session and a
// production session cannot end up in the same file and be picked by accident.
// os.UserConfigDir is deliberately not used: on macOS it resolves to ~/Library/
// Application Support, which is not where a command-line tool's state belongs.
func credentialsPath(env string) (string, error) {
	if path := os.Getenv(EnvVarPrefix + "_CREDENTIALS"); path != "" {
		return path, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, ConfigDirName, env, "credentials.json"), nil
}

// loadCredentials reads the store. A missing file is not an error: it yields an
// empty store, so the first login has somewhere to write.
func loadCredentials(path string) (credentialStore, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return credentialStore{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	store := credentialStore{}
	if len(data) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return store, nil
}

// saveCredentials writes the store, creating the directory 0700 and the file
// 0600 so the sessions are not readable by other users.
//
// It writes a temporary file in the same directory and renames it, so an
// interrupted write cannot leave a truncated file that loses every saved
// session. Two commands share this file, so the rename also means one of them
// writing cannot be seen half-done by the other.
func saveCredentials(path string, store credentialStore) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(dir, ".credentials-*.json")
	if err != nil {
		return fmt.Errorf("create temporary credential file: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once the rename succeeds

	// Chmod before writing, so the token is never briefly on disk under the
	// default 0600-minus-umask that CreateTemp leaves behind.
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("secure temporary credential file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary credential file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary credential file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// put records a session for (baseURL, email), replacing any session already
// saved for that account on that server.
func (s credentialStore) put(baseURL, email string, c credential) {
	perServer := s[baseURL]
	if perServer == nil {
		perServer = map[string]credential{}
		s[baseURL] = perServer
	}
	perServer[strings.ToLower(email)] = c
}

// drop removes the session for (baseURL, email), and the server entry with it
// when that was the last one. It reports whether anything was removed.
func (s credentialStore) drop(baseURL, email string) bool {
	perServer := s[baseURL]
	if perServer == nil {
		return false
	}
	key := strings.ToLower(email)
	if _, ok := perServer[key]; !ok {
		return false
	}
	delete(perServer, key)
	if len(perServer) == 0 {
		delete(s, baseURL)
	}
	return true
}

// emails returns the accounts with a saved session for baseURL, sorted so error
// messages and listings are stable.
func (s credentialStore) emails(baseURL string) []string {
	perServer := s[baseURL]
	out := make([]string, 0, len(perServer))
	for email := range perServer {
		out = append(out, email)
	}
	sort.Strings(out)
	return out
}

// resolve picks the session to attach to a request against baseURL, given the
// requested email, which may be empty.
//
// It never fails. A request with no usable session is sent anonymously and the
// server decides: public routes answer, protected ones return 401, which is
// often exactly what someone testing the API wants to see. Resolution is:
// an explicit email selects that account; with no email and exactly one saved
// session, that one; otherwise none, because guessing between two identities
// would silently exercise the wrong one.
func (s credentialStore) resolve(baseURL, email string) (string, credential) {
	perServer := s[baseURL]
	if len(perServer) == 0 {
		return "", credential{}
	}
	if email != "" {
		key := strings.ToLower(email)
		return key, perServer[key]
	}
	if len(perServer) == 1 {
		for who, c := range perServer {
			return who, c
		}
	}
	return "", credential{}
}
