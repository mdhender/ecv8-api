-- Copyright (c) 2026 Michael D Henderson. All rights reserved.
--
-- Migration 0001: accounts, activation links, sessions, games, memberships.
--
-- Timestamps are RFC 3339 UTC text ('2026-07-24T18:04:05Z'), which sorts
-- lexicographically in the same order it sorts chronologically.
--
-- sqlitemigration wraps this script in a transaction and bumps user_version on
-- success, so this file must not contain BEGIN/COMMIT of its own.

CREATE TABLE account (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    email         TEXT    NOT NULL,
    role          TEXT    NOT NULL,
    display_name  TEXT    NOT NULL,
    timezone      TEXT    NOT NULL DEFAULT 'UTC',
    admin_notes   TEXT    NOT NULL DEFAULT '',
    -- NULL only while an invited account has not activated. Never plaintext.
    password_hash TEXT,
    is_active     INTEGER NOT NULL DEFAULT 1,
    activated_at  TEXT,
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL,

    CONSTRAINT account_role_valid
        CHECK (role IN ('admin', 'user')),
    -- Emails are normalised to lowercase and trimmed before they reach SQLite;
    -- this constraint makes that policy part of the file format.
    CONSTRAINT account_email_normalized
        CHECK (email = lower(email)
               AND email = trim(email)
               AND length(email) BETWEEN 3 AND 254
               AND instr(email, '@') > 1),
    CONSTRAINT account_display_name_length
        CHECK (length(display_name) BETWEEN 1 AND 100),
    CONSTRAINT account_timezone_length
        CHECK (length(timezone) BETWEEN 1 AND 64),
    CONSTRAINT account_is_active_boolean
        CHECK (is_active IN (0, 1)),
    -- An account is activated exactly when it has a password.
    CONSTRAINT account_activation_consistent
        CHECK ((activated_at IS NULL AND password_hash IS NULL)
               OR (activated_at IS NOT NULL AND password_hash IS NOT NULL))
);

CREATE UNIQUE INDEX account_email_key ON account (email);

-- Composite parent key for game_account_role's role-checking foreign key. A
-- UNIQUE index over (id, role) is what makes that reference legal in SQLite.
CREATE UNIQUE INDEX account_id_role_key ON account (id, role);

-- Single-use magic links that let an invited account set its first password.
-- Only the SHA-256 fingerprint is stored; the plaintext exists once, in the
-- response the administrator copies.
CREATE TABLE account_activation (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id  INTEGER NOT NULL REFERENCES account (id) ON DELETE CASCADE,
    token_hash  TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    expires_at  TEXT    NOT NULL,
    redeemed_at TEXT,

    CONSTRAINT account_activation_expires_after_creation
        CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX account_activation_token_hash_key
    ON account_activation (token_hash);
CREATE INDEX account_activation_pending_idx
    ON account_activation (account_id) WHERE redeemed_at IS NULL;

-- Server-side sessions. The cookie carries an opaque token; only its
-- fingerprint is stored, so revocation is a row delete and is immediate.
CREATE TABLE session (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id              INTEGER NOT NULL REFERENCES account (id) ON DELETE CASCADE,
    token_hash              TEXT    NOT NULL,
    -- Non-NULL while an administrator is acting as another account. The row
    -- keeps account_id pointing at the real administrator so audit logging and
    -- "stop impersonating" never lose the true identity.
    impersonated_account_id INTEGER REFERENCES account (id) ON DELETE CASCADE,
    created_at              TEXT    NOT NULL,
    last_seen_at            TEXT    NOT NULL,
    expires_at              TEXT    NOT NULL,
    user_agent              TEXT    NOT NULL DEFAULT '',
    remote_ip               TEXT    NOT NULL DEFAULT '',

    CONSTRAINT session_impersonation_distinct
        CHECK (impersonated_account_id IS NULL
               OR impersonated_account_id <> account_id),
    CONSTRAINT session_expires_after_creation
        CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX session_token_hash_key ON session (token_hash);
CREATE INDEX session_account_idx ON session (account_id);
CREATE INDEX session_impersonated_idx ON session (impersonated_account_id);
CREATE INDEX session_expires_idx ON session (expires_at);

CREATE TABLE game (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    is_active  INTEGER NOT NULL DEFAULT 1,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,

    CONSTRAINT game_name_length
        CHECK (length(name) BETWEEN 1 AND 100 AND name = trim(name)),
    CONSTRAINT game_is_active_boolean
        CHECK (is_active IN (0, 1))
);

CREATE UNIQUE INDEX game_name_key ON game (name);

-- Membership of an account in a game.
--
-- account_role is redundant with account.role on purpose. Together with the
-- CHECK below and the composite foreign key it is a database-level guarantee
-- that an admin account can never be assigned to a game, no matter which code
-- path writes the row. ON UPDATE RESTRICT additionally blocks promoting a
-- member to admin while the membership exists.
CREATE TABLE game_account_role (
    game_id      INTEGER NOT NULL REFERENCES game (id) ON DELETE CASCADE,
    account_id   INTEGER NOT NULL,
    account_role TEXT    NOT NULL DEFAULT 'user',
    is_gm        INTEGER NOT NULL DEFAULT 0,
    is_active    INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,

    PRIMARY KEY (game_id, account_id),

    CONSTRAINT game_account_role_is_gm_boolean
        CHECK (is_gm IN (0, 1)),
    CONSTRAINT game_account_role_is_active_boolean
        CHECK (is_active IN (0, 1)),
    CONSTRAINT game_account_role_players_only
        CHECK (account_role = 'user'),

    FOREIGN KEY (account_id, account_role)
        REFERENCES account (id, role)
        ON UPDATE RESTRICT
        ON DELETE CASCADE
);

CREATE INDEX game_account_role_account_idx ON game_account_role (account_id);
