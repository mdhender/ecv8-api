// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package store

import (
	"context"
	"fmt"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// sessionColumns is the shared projection for session queries.
const sessionColumns = `
	id, account_id,
	coalesce(impersonated_account_id, 0) AS impersonated_account_id,
	created_at, last_seen_at, expires_at, user_agent, remote_ip`

// scanSession reads one row of sessionColumns.
func scanSession(stmt *sqlite.Stmt) (Session, error) {
	session := Session{
		ID:                    stmt.GetInt64("id"),
		AccountID:             stmt.GetInt64("account_id"),
		ImpersonatedAccountID: stmt.GetInt64("impersonated_account_id"),
		UserAgent:             stmt.GetText("user_agent"),
		RemoteIP:              stmt.GetText("remote_ip"),
	}
	var err error
	if session.CreatedAt, err = parseTime(stmt.GetText("created_at")); err != nil {
		return Session{}, err
	}
	if session.LastSeenAt, err = parseTime(stmt.GetText("last_seen_at")); err != nil {
		return Session{}, err
	}
	if session.ExpiresAt, err = parseTime(stmt.GetText("expires_at")); err != nil {
		return Session{}, err
	}
	return session, nil
}

// NewSession describes a session about to be created.
type NewSession struct {
	AccountID int64
	TokenHash string
	ExpiresAt time.Time
	UserAgent string
	RemoteIP  string
}

// CreateSession records a new authenticated session.
func (db *DB) CreateSession(ctx context.Context, params NewSession, now time.Time) (*Session, error) {
	var id int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `
			INSERT INTO session (account_id, token_hash, impersonated_account_id,
			                     created_at, last_seen_at, expires_at, user_agent, remote_ip)
			VALUES (:account_id, :token_hash, NULL, :now, :now, :expires_at, :user_agent, :remote_ip);`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":account_id": params.AccountID,
					":token_hash": params.TokenHash,
					":now":        formatTime(now),
					":expires_at": formatTime(params.ExpiresAt),
					":user_agent": truncate(params.UserAgent, 256),
					":remote_ip":  truncate(params.RemoteIP, 64),
				},
			}); err != nil {
			return err
		}
		id = conn.LastInsertRowID()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &Session{
		ID:         id,
		AccountID:  params.AccountID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  params.ExpiresAt,
		UserAgent:  params.UserAgent,
		RemoteIP:   params.RemoteIP,
	}, nil
}

// SessionByTokenHash returns a live session and the account that owns it, plus
// the impersonated account when one is set.
//
// Expired rows are treated as absent. They are not deleted here: this runs on
// every authenticated request and must not take the write lock. A background
// sweep removes them.
func (db *DB) SessionByTokenHash(ctx context.Context, tokenHash string, now time.Time) (*Session, *Account, *Account, error) {
	var session *Session
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`SELECT `+sessionColumns+` FROM session
			  WHERE token_hash = :token_hash AND expires_at > :now;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":token_hash": tokenHash, ":now": formatTime(now)},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					s, err := scanSession(stmt)
					if err != nil {
						return err
					}
					session = &s
					return nil
				},
			})
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load session: %w", err)
	}
	if session == nil {
		return nil, nil, nil, fmt.Errorf("session: %w", ErrNotFound)
	}

	account, err := db.AccountByID(ctx, session.AccountID)
	if err != nil {
		return nil, nil, nil, err
	}

	var impersonated *Account
	if session.IsImpersonating() {
		impersonated, err = db.AccountByID(ctx, session.ImpersonatedAccountID)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return session, account, impersonated, nil
}

// TouchSession records activity and slides the idle deadline forward.
func (db *DB) TouchSession(ctx context.Context, id int64, now, expiresAt time.Time) error {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			UPDATE session SET last_seen_at = :now, expires_at = :expires_at WHERE id = :id;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{
					":id":         id,
					":now":        formatTime(now),
					":expires_at": formatTime(expiresAt),
				},
			})
	})
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSessionByTokenHash revokes one session. Logout uses it.
func (db *DB) DeleteSessionByTokenHash(ctx context.Context, tokenHash string) error {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			`DELETE FROM session WHERE token_hash = :token_hash;`,
			&sqlitex.ExecOptions{Named: map[string]any{":token_hash": tokenHash}})
	})
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteSessionsForAccount revokes every session belonging to an account, and
// every session currently impersonating it, and reports how many were removed.
//
// Deactivating an account does not call this: an existing session survives
// deactivation until it expires or an administrator revokes it here. Including
// impersonating sessions matters because those are held by an administrator but
// act as this account.
func (db *DB) DeleteSessionsForAccount(ctx context.Context, accountID int64) (int, error) {
	var removed int
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn,
			`DELETE FROM session
			  WHERE account_id = :account_id OR impersonated_account_id = :account_id;`,
			&sqlitex.ExecOptions{Named: map[string]any{":account_id": accountID}}); err != nil {
			return err
		}
		removed = conn.Changes()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	return removed, nil
}

// DeleteOtherSessionsForAccount revokes every session for an account except the
// one identified by keepTokenHash. A password change uses it so a stolen
// session cannot outlive the credential it was created with.
func (db *DB) DeleteOtherSessionsForAccount(ctx context.Context, accountID int64, keepTokenHash string) (int, error) {
	var removed int
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn,
			`DELETE FROM session WHERE account_id = :account_id AND token_hash <> :keep;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":account_id": accountID, ":keep": keepTokenHash},
			}); err != nil {
			return err
		}
		removed = conn.Changes()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("revoke other sessions: %w", err)
	}
	return removed, nil
}

// CountSessionsForAccount reports how many live sessions an account has, so the
// admin UI can show whether revocation would do anything.
func (db *DB) CountSessionsForAccount(ctx context.Context, accountID int64, now time.Time) (int, error) {
	var count int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `
			SELECT count(*) AS total FROM session
			 WHERE (account_id = :account_id OR impersonated_account_id = :account_id)
			   AND expires_at > :now;`,
			&sqlitex.ExecOptions{
				Named: map[string]any{":account_id": accountID, ":now": formatTime(now)},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					count = int(stmt.GetInt64("total"))
					return nil
				},
			})
	})
	if err != nil {
		return 0, fmt.Errorf("count sessions: %w", err)
	}
	return count, nil
}

// SetImpersonation points a session at another account, or clears it when
// targetID is zero.
//
// The session keeps its own account_id, so the real administrator remains
// recorded for auditing and stopping impersonation never requires a new login.
func (db *DB) SetImpersonation(ctx context.Context, sessionID, targetID int64) error {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		named := map[string]any{":id": sessionID, ":target": any(nil)}
		if targetID != 0 {
			named[":target"] = targetID
		}
		return sqlitex.Execute(conn,
			`UPDATE session SET impersonated_account_id = :target WHERE id = :id;`,
			&sqlitex.ExecOptions{Named: named})
	})
	if err != nil {
		return fmt.Errorf("set impersonation: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions past their deadline and reports the
// count. A background sweep calls it; nothing depends on it for correctness,
// because every read already filters on expires_at.
func (db *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	var removed int
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn,
			`DELETE FROM session WHERE expires_at <= :now;`,
			&sqlitex.ExecOptions{Named: map[string]any{":now": formatTime(now)}}); err != nil {
			return err
		}
		removed = conn.Changes()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sweep expired sessions: %w", err)
	}
	return removed, nil
}

// truncate limits a stored string to n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

// utf8Start reports whether b begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
