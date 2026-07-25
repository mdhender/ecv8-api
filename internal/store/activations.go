// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"fmt"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ErrActivationInvalid is the single failure returned for every unusable
// activation token: unknown, expired, or already redeemed.
//
// Collapsing those cases is deliberate. A caller who can distinguish "unknown
// token" from "expired token" learns which tokens once existed, and a caller
// who can distinguish "already redeemed" learns that a particular invitation was
// accepted. Neither is information an unauthenticated client should have.
const ErrActivationInvalid = errActivation("activation link is invalid or has expired")

type errActivation string

func (e errActivation) Error() string { return string(e) }

// IssueActivationLink invalidates every outstanding link for an account and
// records a new one.
//
// Marking the previous links redeemed in the same transaction as the insert
// means a reissued invitation immediately supersedes the old one, so a link an
// administrator has already sent cannot be used after a replacement is created.
func (db *DB) IssueActivationLink(ctx context.Context, accountID int64, tokenHash string, now time.Time) (time.Time, error) {
	expiresAt := now.Add(ActivationTTL)

	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		var exists bool
		if err := sqlitex.Execute(conn,
			`SELECT 1 AS found FROM account WHERE id = :id;`,
			&sqlitex.ExecOptions{
				Named:      map[string]any{":id": accountID},
				ResultFunc: func(*sqlite.Stmt) error { exists = true; return nil },
			}); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("account %d: %w", accountID, ErrNotFound)
		}

		if err := sqlitex.Execute(conn, `
			UPDATE account_activation
			   SET redeemed_at = :now
			 WHERE account_id = :account_id AND redeemed_at IS NULL;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":account_id": accountID, ":now": formatTime(now)},
			}); err != nil {
			return err
		}

		return sqlitex.Execute(conn, `
			INSERT INTO account_activation (account_id, token_hash, created_at, expires_at, redeemed_at)
			VALUES (:account_id, :token_hash, :now, :expires_at, NULL);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":account_id": accountID,
					":token_hash": tokenHash,
					":now":        formatTime(now),
					":expires_at": formatTime(expiresAt),
				},
			})
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("issue activation link: %w", err)
	}
	return expiresAt, nil
}

// RedeemActivation consumes a single-use token and sets the account's first
// password.
//
// The lookup, the single-use mark, and the password write all happen inside one
// immediate transaction, so a token cannot be redeemed twice even under
// concurrent requests: the second attempt sees redeemed_at already set and
// fails. The redeeming UPDATE is itself conditional on redeemed_at IS NULL, so
// the check and the claim are one atomic statement.
func (db *DB) RedeemActivation(ctx context.Context, tokenHash, passwordHash string, now time.Time) (accountID int64, err error) {
	err = db.Write(ctx, func(conn *sqlite.Conn) error {
		// Claim the token. Restricting the UPDATE to unredeemed, unexpired rows
		// makes this the atomic single-use gate.
		if err := sqlitex.Execute(conn, `
			UPDATE account_activation
			   SET redeemed_at = :now
			 WHERE token_hash = :token_hash
			   AND redeemed_at IS NULL
			   AND expires_at > :now;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":token_hash": tokenHash, ":now": formatTime(now)},
			}); err != nil {
			return err
		}
		if conn.Changes() != 1 {
			return ErrActivationInvalid
		}

		if err := sqlitex.Execute(conn,
			`SELECT account_id FROM account_activation WHERE token_hash = :token_hash;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":token_hash": tokenHash},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					accountID = stmt.GetInt64("account_id")
					return nil
				},
			}); err != nil {
			return err
		}
		if accountID == 0 {
			return ErrActivationInvalid
		}

		// An invitation to an account that has since been deactivated must not
		// produce a usable login.
		var isActive bool
		if err := sqlitex.Execute(conn,
			`SELECT is_active FROM account WHERE id = :id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":id": accountID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					isActive = stmt.GetInt64("is_active") != 0
					return nil
				},
			}); err != nil {
			return err
		}
		if !isActive {
			return ErrActivationInvalid
		}

		return sqlitex.Execute(conn, `
			UPDATE account
			   SET password_hash = :hash,
			       activated_at = coalesce(activated_at, :now),
			       updated_at = :now
			 WHERE id = :id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":id": accountID, ":hash": passwordHash, ":now": formatTime(now)},
			})
	})
	if err != nil {
		return 0, err
	}
	return accountID, nil
}

// PendingActivation reports whether an account has an outstanding link and when
// it expires. It is admin-only information used to render the accounts list.
func (db *DB) PendingActivation(ctx context.Context, accountID int64, now time.Time) (expiresAt time.Time, ok bool, err error) {
	var raw string
	err = db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			SELECT expires_at
			  FROM account_activation
			 WHERE account_id = :account_id
			   AND redeemed_at IS NULL
			   AND expires_at > :now
			 ORDER BY expires_at DESC
			 LIMIT 1;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":account_id": accountID, ":now": formatTime(now)},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					raw = stmt.GetText("expires_at")
					return nil
				},
			})
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read pending activation: %w", err)
	}
	if raw == "" {
		return time.Time{}, false, nil
	}
	expiresAt, err = parseTime(raw)
	if err != nil {
		return time.Time{}, false, err
	}
	return expiresAt, true, nil
}
