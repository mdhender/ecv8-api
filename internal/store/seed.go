// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mdhender/ecv8-api/internal/cerrs"
	"github.com/mdhender/ecv8-api/internal/password"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	// EnvAdminEmail names the initial administrator's email address.
	EnvAdminEmail = "EC_ADMIN_EMAIL"
	// EnvAdminSecret names the initial administrator's password.
	EnvAdminSecret = "EC_ADMIN_SECRET"

	// ErrIncompleteAdminConfig reports that exactly one of the initial
	// administrator variables was supplied.
	ErrIncompleteAdminConfig = cerrs.Error("incomplete initial administrator configuration")
	// ErrInvalidAdminConfig reports an unusable initial administrator pair.
	ErrInvalidAdminConfig = cerrs.Error("invalid initial administrator configuration")
)

// initialAdmin is a validated EC_ADMIN_EMAIL / EC_ADMIN_SECRET pair.
type initialAdmin struct {
	present bool
	email   string
	secret  string
}

// initialAdminFromEnv reads and validates the initial administrator pair.
//
// Neither set (or both blank) means "create the database with no
// administrator". Exactly one set is a configuration error and must abort
// before the database file is created. Both set are validated here so an
// invalid password or address is reported before any filesystem change.
//
// Only CreatePersistentStore calls this. Opening an existing database never
// consults these variables, so they cannot be used to add or alter an
// administrator after creation.
func initialAdminFromEnv() (initialAdmin, error) {
	email := strings.TrimSpace(os.Getenv(EnvAdminEmail))
	secret := os.Getenv(EnvAdminSecret)

	switch {
	case email == "" && strings.TrimSpace(secret) == "":
		return initialAdmin{}, nil
	case email == "":
		return initialAdmin{}, fmt.Errorf("%w: %s is set but %s is not",
			ErrIncompleteAdminConfig, EnvAdminSecret, EnvAdminEmail)
	case strings.TrimSpace(secret) == "":
		return initialAdmin{}, fmt.Errorf("%w: %s is set but %s is not",
			ErrIncompleteAdminConfig, EnvAdminEmail, EnvAdminSecret)
	}

	normalized, err := NormalizeEmail(email)
	if err != nil {
		return initialAdmin{}, fmt.Errorf("%w: %s: %v", ErrInvalidAdminConfig, EnvAdminEmail, err)
	}
	// Reports the rule that was broken, never the secret or its length.
	if err := password.Validate(secret); err != nil {
		return initialAdmin{}, fmt.Errorf("%w: %s: %v", ErrInvalidAdminConfig, EnvAdminSecret, err)
	}

	return initialAdmin{present: true, email: normalized, secret: secret}, nil
}

// seedInitialAdmin inserts the one active administrator described by admin. It
// is a no-op when no pair was configured.
func (db *DB) seedInitialAdmin(ctx context.Context, admin initialAdmin) error {
	if !admin.present {
		return nil
	}
	hash, err := password.Hash(admin.secret)
	if err != nil {
		return fmt.Errorf("seed initial administrator: %w", err)
	}
	now := Now()

	err = db.Write(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			INSERT INTO account (email, role, display_name, timezone, admin_notes,
			                     password_hash, is_active, activated_at, created_at, updated_at)
			VALUES (:email, 'admin', :display_name, 'UTC', '',
			        :password_hash, 1, :now, :now, :now);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":email":         admin.email,
					":display_name":  displayNameFromEmail(admin.email),
					":password_hash": hash,
					":now":           formatTime(now),
				},
			})
	})
	if err != nil {
		return fmt.Errorf("seed initial administrator: %w", err)
	}
	return nil
}

// devAccount is a development login seeded into temporary stores.
type devAccount struct {
	email  string
	secret string
	role   string
}

// developmentAccounts are the fixed logins available in a temporary store.
// Their secrets are development conveniences and are hashed by the same
// password service as real accounts, so no plaintext is ever persisted.
var developmentAccounts = []devAccount{
	{email: "admin@example.com", secret: "admin", role: RoleAdmin},
	// Game-master status is per game, not an application role, so gm1 is a
	// plain user here and is granted is_gm on whichever games need it.
	{email: "gm1@example.com", secret: "gm1", role: RoleUser},
	{email: "user1@example.com", secret: "user1", role: RoleUser},
	{email: "user2@example.com", secret: "user2", role: RoleUser},
}

// seedDevelopmentAccounts inserts the development logins into a temporary
// store.
//
// INSERT OR IGNORE keyed on the unique email index makes this safe to run every
// time a named in-memory database is opened: reconnecting to a still-live store
// leaves the existing rows, and their hashes, untouched.
func (db *DB) seedDevelopmentAccounts(ctx context.Context) error {
	now := formatTime(Now())

	for _, account := range developmentAccounts {
		hash, err := password.Hash(account.secret)
		if err != nil {
			return fmt.Errorf("seed development account %s: %w", account.email, err)
		}
		err = db.Write(ctx, func(conn *sqlite.Conn) error {
			return sqlitex.Execute(conn, `
				INSERT OR IGNORE INTO account (email, role, display_name, timezone, admin_notes,
				                               password_hash, is_active, activated_at, created_at, updated_at)
				VALUES (:email, :role, :display_name, 'UTC', '',
				        :password_hash, 1, :now, :now, :now);`,
				&sqlitex.ExecOptions{
					Named: map[string]any{
						":email":         account.email,
						":role":          account.role,
						":display_name":  displayNameFromEmail(account.email),
						":password_hash": hash,
						":now":           now,
					},
				})
		})
		if err != nil {
			return fmt.Errorf("seed development account %s: %w", account.email, err)
		}
	}
	return nil
}

// displayNameFromEmail derives a starting display name from the local part of
// an address, so a seeded account is never nameless.
func displayNameFromEmail(email string) string {
	local, _, found := strings.Cut(email, "@")
	if !found || local == "" {
		return email
	}
	if len(local) > 100 {
		local = local[:100]
	}
	return local
}
