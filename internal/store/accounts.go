// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/mdhender/ecv8-api/internal/password"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// accountColumns is the shared projection for every account query, so scanAccount
// can rely on the column names being present.
const accountColumns = `
	id, email, role, display_name, timezone, admin_notes,
	coalesce(password_hash, '') AS password_hash,
	is_active,
	coalesce(activated_at, '') AS activated_at,
	created_at, updated_at`

// VerifyPassword reports whether plaintext is this account's secret. It returns
// password.ErrNoHash when the account has not activated.
func (a *Account) VerifyPassword(plaintext string) error {
	return password.Verify(a.passwordHash, plaintext)
}

// scanAccount reads one row of accountColumns.
func scanAccount(stmt *sqlite.Stmt) (Account, error) {
	account := Account{
		ID:           stmt.GetInt64("id"),
		Email:        stmt.GetText("email"),
		Role:         stmt.GetText("role"),
		DisplayName:  stmt.GetText("display_name"),
		Timezone:     stmt.GetText("timezone"),
		AdminNotes:   stmt.GetText("admin_notes"),
		IsActive:     stmt.GetInt64("is_active") != 0,
		passwordHash: stmt.GetText("password_hash"),
	}
	var err error
	if account.ActivatedAt, err = parseTime(stmt.GetText("activated_at")); err != nil {
		return Account{}, err
	}
	if account.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
		return Account{}, err
	}
	if account.UpdatedAt, err = parseTime(stmt.GetText("updated_at")); err != nil {
		return Account{}, err
	}
	account.Activated = !account.ActivatedAt.IsZero()
	return account, nil
}

// AccountByID returns one account, or ErrNotFound.
func (db *DB) AccountByID(ctx context.Context, id int64) (*Account, error) {
	var found *Account
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+accountColumns+` FROM account WHERE id = :id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":id": id},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					account, err := scanAccount(stmt)
					if err != nil {
						return err
					}
					found = &account
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load account %d: %w", id, err)
	}
	if found == nil {
		return nil, fmt.Errorf("account %d: %w", id, ErrNotFound)
	}
	return found, nil
}

// AccountByEmail returns one account by normalised email, or ErrNotFound.
func (db *DB) AccountByEmail(ctx context.Context, email string) (*Account, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	var found *Account
	err = db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+accountColumns+` FROM account WHERE email = :email;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":email": normalized},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					account, scanErr := scanAccount(stmt)
					if scanErr != nil {
						return scanErr
					}
					found = &account
					return nil
				},
			})
	})
	if err != nil {
		return nil, fmt.Errorf("load account by email: %w", err)
	}
	if found == nil {
		return nil, fmt.Errorf("account: %w", ErrNotFound)
	}
	return found, nil
}

// AccountFilter narrows an account listing.
type AccountFilter struct {
	// Query matches a substring of email or display name.
	Query string
	// Role, when set, restricts to that application role.
	Role string
	// Active, when non-nil, restricts to active or inactive accounts.
	Active *bool
}

// ListAccounts returns one page of accounts ordered by email, plus the page
// metadata including the unpaginated total.
func (db *DB) ListAccounts(ctx context.Context, filter AccountFilter, page Page) ([]Account, Page, error) {
	where := []string{"1 = 1"}
	args := map[string]any{}

	if q := strings.TrimSpace(filter.Query); q != "" {
		where = append(where, "(email LIKE :q ESCAPE '\\' OR display_name LIKE :q ESCAPE '\\')")
		args[":q"] = "%" + escapeLike(strings.ToLower(q)) + "%"
	}
	if filter.Role != "" {
		where = append(where, "role = :role")
		args[":role"] = filter.Role
	}
	if filter.Active != nil {
		where = append(where, "is_active = :is_active")
		args[":is_active"] = boolToInt(*filter.Active)
	}
	clause := strings.Join(where, " AND ")

	accounts := make([]Account, 0, page.Size)
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		countArgs := maps.Clone(args)
		if err := sqlitex.Execute(conn,
			`SELECT count(*) AS total FROM account WHERE `+clause+`;`,
			&sqlitex.ExecOptions{
				Named: countArgs,
				ResultFunc: func(stmt *sqlite.Stmt) error {
					page.Total = int(stmt.GetInt64("total"))
					return nil
				},
			}); err != nil {
			return err
		}

		pageArgs := maps.Clone(args)
		pageArgs[":limit"] = int64(page.Size)
		pageArgs[":offset"] = int64(page.Offset())

		return sqlitex.Execute(conn,
			`SELECT `+accountColumns+` FROM account WHERE `+clause+`
			 ORDER BY email LIMIT :limit OFFSET :offset;`,
			&sqlitex.ExecOptions{
				Named: pageArgs,
				ResultFunc: func(stmt *sqlite.Stmt) error {
					account, err := scanAccount(stmt)
					if err != nil {
						return err
					}
					accounts = append(accounts, account)
					return nil
				},
			})
	})
	if err != nil {
		return nil, page, fmt.Errorf("list accounts: %w", err)
	}
	page.Entries = len(accounts)
	return accounts, page, nil
}

// NewAccount describes an account an administrator is inviting.
type NewAccount struct {
	Email       string
	Role        string
	DisplayName string
	Timezone    string
	AdminNotes  string
}

// CreateAccount invites an account and mints its first activation link.
//
// The account is created without a password hash and is therefore inactive for
// login purposes until it activates. Both writes happen in one transaction, so
// an account is never left invited with no way to activate.
func (db *DB) CreateAccount(ctx context.Context, params NewAccount, tokenHash string, now time.Time) (*Account, time.Time, error) {
	email, err := NormalizeEmail(params.Email)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if params.Role != RoleAdmin && params.Role != RoleUser {
		return nil, time.Time{}, fmt.Errorf("%w: role must be %q or %q", ErrConflict, RoleAdmin, RoleUser)
	}
	displayName := strings.TrimSpace(params.DisplayName)
	if displayName == "" {
		displayName = displayNameFromEmail(email)
	}
	if len(displayName) > 100 {
		return nil, time.Time{}, fmt.Errorf("%w: display name must be at most 100 bytes", ErrConflict)
	}
	timezone := strings.TrimSpace(params.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}

	expiresAt := now.Add(ActivationTTL)
	var id int64

	err = db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `
			INSERT INTO account (email, role, display_name, timezone, admin_notes,
			                     password_hash, is_active, activated_at, created_at, updated_at)
			VALUES (:email, :role, :display_name, :timezone, :admin_notes,
			        NULL, 1, NULL, :now, :now);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":email":        email,
					":role":         params.Role,
					":display_name": displayName,
					":timezone":     timezone,
					":admin_notes":  strings.TrimSpace(params.AdminNotes),
					":now":          formatTime(now),
				},
			}); err != nil {
			return err
		}
		id = conn.LastInsertRowID()

		return sqlitex.Execute(conn, `
			INSERT INTO account_activation (account_id, token_hash, created_at, expires_at, redeemed_at)
			VALUES (:account_id, :token_hash, :now, :expires_at, NULL);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":account_id": id,
					":token_hash": tokenHash,
					":now":        formatTime(now),
					":expires_at": formatTime(expiresAt),
				},
			})
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, time.Time{}, fmt.Errorf("%w: an account with that email already exists", ErrConflict)
		}
		return nil, time.Time{}, fmt.Errorf("create account: %w", err)
	}

	account, err := db.AccountByID(ctx, id)
	if err != nil {
		return nil, time.Time{}, err
	}
	return account, expiresAt, nil
}

// AccountUpdate holds the administrator-editable fields of an account. A nil
// pointer leaves that field unchanged.
type AccountUpdate struct {
	Email       *string
	Role        *string
	DisplayName *string
	Timezone    *string
	AdminNotes  *string
	IsActive    *bool
}

// UpdateAccount applies an administrator's changes.
//
// Changing an account's role to admin while it holds game memberships is
// refused by the database's composite foreign key, which surfaces here as
// ErrConflict rather than a constraint dump.
func (db *DB) UpdateAccount(ctx context.Context, id int64, update AccountUpdate, now time.Time) (*Account, error) {
	sets := []string{"updated_at = :now"}
	args := map[string]any{":id": id, ":now": formatTime(now)}

	if update.Email != nil {
		email, err := NormalizeEmail(*update.Email)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		sets = append(sets, "email = :email")
		args[":email"] = email
	}
	if update.Role != nil {
		if *update.Role != RoleAdmin && *update.Role != RoleUser {
			return nil, fmt.Errorf("%w: role must be %q or %q", ErrConflict, RoleAdmin, RoleUser)
		}
		sets = append(sets, "role = :role")
		args[":role"] = *update.Role
	}
	if update.DisplayName != nil {
		name := strings.TrimSpace(*update.DisplayName)
		if name == "" || len(name) > 100 {
			return nil, fmt.Errorf("%w: display name must be 1 to 100 bytes", ErrConflict)
		}
		sets = append(sets, "display_name = :display_name")
		args[":display_name"] = name
	}
	if update.Timezone != nil {
		tz := strings.TrimSpace(*update.Timezone)
		if tz == "" || len(tz) > 64 {
			return nil, fmt.Errorf("%w: timezone must be 1 to 64 bytes", ErrConflict)
		}
		sets = append(sets, "timezone = :timezone")
		args[":timezone"] = tz
	}
	if update.AdminNotes != nil {
		sets = append(sets, "admin_notes = :admin_notes")
		args[":admin_notes"] = strings.TrimSpace(*update.AdminNotes)
	}
	if update.IsActive != nil {
		sets = append(sets, "is_active = :is_active")
		args[":is_active"] = boolToInt(*update.IsActive)
	}

	var changed int
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn,
			`UPDATE account SET `+strings.Join(sets, ", ")+` WHERE id = :id;`,
			&sqlitex.ExecOptions{Named: args}); err != nil {
			return err
		}
		changed = conn.Changes()
		return nil
	})
	if err != nil {
		switch {
		case isUniqueViolation(err):
			return nil, fmt.Errorf("%w: an account with that email already exists", ErrConflict)
		case isConstraintViolation(err):
			// The reachable constraints here are the composite role foreign key
			// and the column CHECKs, and only the former depends on other rows.
			return nil, fmt.Errorf("%w: this account belongs to at least one game, so it cannot become an admin", ErrConflict)
		}
		return nil, fmt.Errorf("update account %d: %w", id, err)
	}
	if changed == 0 {
		return nil, fmt.Errorf("account %d: %w", id, ErrNotFound)
	}
	return db.AccountByID(ctx, id)
}

// UpdateProfile applies the fields an account may change about itself.
func (db *DB) UpdateProfile(ctx context.Context, id int64, displayName, timezone *string, now time.Time) (*Account, error) {
	return db.UpdateAccount(ctx, id, AccountUpdate{
		DisplayName: displayName,
		Timezone:    timezone,
	}, now)
}

// SetPassword replaces an account's password hash and marks it activated. It is
// used by both the activation flow and a self-service password change.
func (db *DB) SetPassword(ctx context.Context, id int64, hash string, now time.Time) error {
	var changed int
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `
			UPDATE account
			   SET password_hash = :hash,
			       activated_at = coalesce(activated_at, :now),
			       updated_at = :now
			 WHERE id = :id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":id": id, ":hash": hash, ":now": formatTime(now)},
			}); err != nil {
			return err
		}
		changed = conn.Changes()
		return nil
	})
	if err != nil {
		return fmt.Errorf("set password for account %d: %w", id, err)
	}
	if changed == 0 {
		return fmt.Errorf("account %d: %w", id, ErrNotFound)
	}
	return nil
}

// CountActiveAdmins reports how many active administrators exist. Callers use
// it to refuse changes that would lock everyone out.
func (db *DB) CountActiveAdmins(ctx context.Context) (int, error) {
	var count int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT count(*) AS total FROM account WHERE role = 'admin' AND is_active = 1;`,
			&sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					count = int(stmt.GetInt64("total"))
					return nil
				},
			})
	})
	if err != nil {
		return 0, fmt.Errorf("count active admins: %w", err)
	}
	return count, nil
}

// escapeLike neutralises LIKE wildcards in user input so a search for "%"
// cannot match everything.
func escapeLike(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}

// resultCodeMask isolates a SQLite result code's primary code from its extended
// code. SQLITE_CONSTRAINT_UNIQUE, for example, is SQLITE_CONSTRAINT in its low
// byte plus a subtype in the high bits.
const resultCodeMask = 0xff

// isConstraintViolation reports whether err is any SQLite constraint failure.
//
// The primary code is what is tested, because SQLite does not always report an
// extended code: a foreign key declared ON UPDATE RESTRICT is enforced by an
// implicit trigger and surfaces as plain SQLITE_CONSTRAINT. Matching only the
// extended codes would let that failure fall through to a 500.
func isConstraintViolation(err error) bool {
	return sqlite.ErrCode(err)&resultCodeMask == sqlite.ResultConstraint
}

// isUniqueViolation reports whether err came from a UNIQUE or PRIMARY KEY
// constraint specifically.
func isUniqueViolation(err error) bool {
	code := sqlite.ErrCode(err)
	return code == sqlite.ResultConstraintUnique || code == sqlite.ResultConstraintPrimaryKey
}

// IsConstraintError reports whether err represents a rule the database or this
// package refused. Handlers turn it into a 409 without inspecting the message.
func IsConstraintError(err error) bool {
	return errors.Is(err, ErrConflict) || isConstraintViolation(err)
}
