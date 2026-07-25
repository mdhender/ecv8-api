# ECV8 API

The Go HTTP API for **ECV8**, a turn-based 4X science-fiction game. It owns the
database, accounts, sessions, games, and administration. The Ember client lives
in the sibling `app/` repository and is developed against this service through a
Caddy proxy so both are served from one origin, as they are in production.

This repository is independent. It is not a submodule of `app/`, and there is no
parent repository above the two.

---

## Contents

- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Commands](#commands)
- [The database](#the-database)
  - [Creating and opening](#creating-and-opening)
  - [Migrations](#migrations)
  - [The initial administrator](#the-initial-administrator)
  - [Backups](#backups)
- [Architecture](#architecture)
- [Security notes](#security-notes)
- [HTTP API](#http-api)
- [Development with Caddy](#development-with-caddy)
- [Validation](#validation)
- [Tests](#tests)

---

## Requirements

| Tool  | Version  | Notes |
|-------|----------|-------|
| Go    | 1.26.4   | Exact version this module declares. |
| Caddy | 2.x      | Development proxy only; production uses nginx. |

The SQLite engine is compiled in through ZombieZen, which is CGO-free. There is
nothing to install beyond Go.

---

## Quick start

```bash
# 1. The database directory must already exist. The store never creates one.
mkdir -p db

# 2. Create ecv8.db and seed the first administrator.
EC_ADMIN_EMAIL=admin@example.com EC_ADMIN_SECRET='choose-a-good-secret' \
  go run ./cmd/ecv8-api db create --db-path db

# 3. Serve it.
go run ./cmd/ecv8-api serve --db-path db --cookie-secure=false

# 4. Check it is alive.
curl -s localhost:3000/api/v1/health/ready
```

`--cookie-secure=false` is needed only because the default development origin is
plain HTTP. See [Development with Caddy](#development-with-caddy).

To work on the frontend without a database on disk, run against a seeded
in-memory database instead:

```bash
go run ./cmd/ecv8-api serve --memory dev --cookie-secure=false
```

That gives you four accounts — `admin@example.com/admin`, `gm1@example.com/gm1`,
`user1@example.com/user1`, `user2@example.com/user2` — and discards everything on
exit. It is a development convenience and must never be used in production.

---

## Configuration

Every setting can come from three places. **Highest priority wins:**

1. a command-line flag — `--listen-addr 127.0.0.1:3000`
2. an environment variable — `EC_LISTEN_ADDR=127.0.0.1:3000`
3. the built-in default

The process environment is itself populated from dotenv files *before* flags are
parsed, so a file can supply a value but can never override a real environment
variable or an explicit flag. `EC_ENV` selects which files load
(`development`, `test`, `production`, or `agent`; default `development`), and
within that the first file below that defines a key wins:

| Priority | File                   | Git-ignored? | Secrets? |
|----------|------------------------|--------------|----------|
| Highest  | `.env.{EC_ENV}.local`  | yes          | yes      |
| 2nd      | `.env.local`           | yes          | yes      |
| 3rd      | `.env.{EC_ENV}`        | no           | **never**|
| Lowest   | `.env`                 | no           | **never**|

Copy `.env.example` to `.env.development.local` to get started.

Flags map to environment variables by upper-casing and prefixing with `EC_`:
`--read-header-timeout` is fed by `EC_READ_HEADER_TIMEOUT`.

### Options

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--db-path` | `EC_DB_PATH` | `db` | Directory holding `ecv8.db`. Must exist. |
| `--memory` | `EC_MEMORY` | *(unset)* | Serve a seeded in-memory database instead. Development only. |
| `--read-only` | `EC_READ_ONLY` | `false` | Open SQLite read-only. Write endpoints fail. |
| `--listen-addr` | `EC_LISTEN_ADDR` | `127.0.0.1:3000` | Private HTTP listener. |
| `--public-base-url` | `EC_PUBLIC_BASE_URL` | `http://localhost:8081` | Origin browsers use. Activation links are built from it. |
| `--read-timeout` | `EC_READ_TIMEOUT` | `15s` | Whole-request read deadline. |
| `--read-header-timeout` | `EC_READ_HEADER_TIMEOUT` | `5s` | Header read deadline. |
| `--write-timeout` | `EC_WRITE_TIMEOUT` | `30s` | Response write deadline. |
| `--idle-timeout` | `EC_IDLE_TIMEOUT` | `120s` | Keep-alive idle deadline. |
| `--shutdown-timeout` | `EC_SHUTDOWN_TIMEOUT` | `20s` | Grace period for in-flight requests. |
| `--max-body-bytes` | `EC_MAX_BODY_BYTES` | `1048576` | Largest accepted request body. |
| `--session-ttl` | `EC_SESSION_TTL` | `168h` | Absolute session lifetime from login. |
| `--session-idle-ttl` | `EC_SESSION_IDLE_TTL` | `24h` | Inactivity window; never exceeds the absolute lifetime. |
| `--cookie-name` | `EC_COOKIE_NAME` | `ec_session` | Session cookie name. |
| `--cookie-secure` | `EC_COOKIE_SECURE` | `true` | `Secure` attribute. Disable only for plain-HTTP local development. |
| `--cookie-samesite` | `EC_COOKIE_SAMESITE` | `lax` | `lax`, `strict`, or `none`. |
| `--trusted-proxy` | `EC_TRUSTED_PROXY` | *(none)* | CIDR whose forwarding headers may be believed. Repeatable. |
| `--trusted-origin` | `EC_TRUSTED_ORIGIN` | *(none)* | Extra origin allowed by cross-origin protection. Repeatable. |
| `--log-level` | `EC_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `--log-format` | `EC_LOG_FORMAT` | `text` | `text` or `json`. |

Configuration is validated before the server starts, and the error names the
flag that is wrong. A few rules are enforced because getting them wrong produces
a silent authentication failure rather than an obvious one:

- `--cookie-samesite=none` requires `--cookie-secure`.
- `--cookie-secure` with a plain-`http` non-loopback `--public-base-url` is
  refused: the browser would discard the cookie.
- `--session-idle-ttl` may not exceed `--session-ttl`.
- `--memory` and `--read-only` are mutually exclusive.

---

## Commands

```
ecv8-api serve                    open the database and serve the HTTP API
ecv8-api db create                create ecv8.db and seed the initial admin
ecv8-api db verify                open ecv8.db read-only and print its migration
ecv8-api version                  print the build version
```

Every command accepts `--help`, which lists the flags in scope with their
defaults.

---

## The database

One SQLite file, always named **`ecv8.db`**. Commands take the *directory* that
contains it; the filename is fixed and is never configurable. This is deliberate:
an operator can point a command at the wrong directory and get a clear error,
but cannot accidentally create a second database under a different name.

### Creating and opening

`ecv8-api db create` creates the file. It fails if `ecv8.db` already exists — an
existing database is never truncated, replaced, or reopened as if it were new.
The directory must already exist; **the store never creates a directory**, and it
rejects an empty path, a missing path, a path that is not a directory, and a
directory it cannot write to, without touching the filesystem first.

If initialisation fails partway, the file it just created is removed, along with
its `-wal` and `-shm` sidecars. A pre-existing file is never removed, because
creation never reaches initialisation when one exists.

Opening is separate and never creates anything:

- **Writable** (`serve`): the application marker is checked, pending migrations
  are applied automatically, and WAL is asserted. A database *newer* than the
  binary is rejected without being modified.
- **Read-only** (`db verify`, `serve --read-only`): SQLite is opened in genuine
  read-only mode. No migration runs and nothing is written, so a database newer
  than the binary is perfectly acceptable — an older build can still inspect it.

`PRAGMA application_id` is the authoritative marker that a file was created by
ECV8. Its value is `0x65637638` (`1701017144`), the four ASCII bytes of `ecv8`.
It is **not** the migration version, which lives in `PRAGMA user_version`.
Opening a SQLite file that is not an ECV8 database fails with a clear error
rather than being migrated into one.

### Migrations

Migrations are SQL files embedded in the binary and applied by ZombieZen's
`sqlitemigration` in filename order. A database's `user_version` is the count of
migrations applied to it.

- Writable opens migrate forward automatically.
- Read-only opens never migrate.
- A database ahead of the binary is refused for writing. Deploy the newer binary
  or restore an older snapshot; there is no downgrade path.

There is no separate upgrade utility. `serve` migrates on open, which is what
keeps the running binary and the schema in step.

### The initial administrator

`ecv8-api db create` is the **only** operation that may seed an administrator,
and it reads exactly two variables:

- `EC_ADMIN_EMAIL`
- `EC_ADMIN_SECRET`

They are validated *before* the database file is created:

| Both unset or blank | Database is created with no administrator. |
|---------------------|--------------------------------------------|
| Exactly one set     | **Error.** Nothing is created. |
| Both set            | Validated, then one active administrator is created during initialisation. |

Opening an existing database ignores these variables entirely, so they cannot be
used to add or alter an administrator later. After creation, administrators
create other accounts through the API.

### Backups

Not automated, and deliberately so. A few things worth knowing:

- The database is in **WAL** mode, so `ecv8.db` alone is not a complete copy.
  Copying the file while the server is running can capture a torn state.
- The correct way to take a consistent copy of a live database is SQLite's
  `VACUUM INTO 'destination.db'`, or `sqlite3 ecv8.db ".backup out.db"`. Both
  produce a single self-contained file with no sidecars.
- Stopping the service first and copying `ecv8.db`, `ecv8.db-wal`, and
  `ecv8.db-shm` together also works, but only all three together.
- A backup contains bcrypt password hashes, activation-token hashes, and live
  session fingerprints. Treat it as a secret: an attacker with a backup can
  reissue nothing, but can attempt offline password cracking.
- Restoring an older backup does not invalidate sessions issued after it was
  taken; those simply cease to exist, so their holders are signed out.

---

## Architecture

```
cmd/ecv8-api/          entry point, command tree, logger construction
internal/
  cerrs/               constant sentinel errors
  config/              flags, environment, validation, precedence
  dotenv/              dotenv loading, patterned after ../ecv7
  engine/              game-engine foundation: math/rand/v2 with PCG
  password/            bcrypt hashing at MinCost, 3-128 byte policy
  server/              Echo v5 routes, middleware, handlers, Problem Details
  store/               every SQLite database, migrations, queries, seeding
  tokens/              crypto/rand session and activation tokens
  version/             build version
dev/Caddyfile          development reverse proxy
```

Notable choices:

- **Echo v5** is the router. Its `Context` is a struct, its error handler
  returns nothing, and `log/slog` is built in — none of the v4 idioms apply.
- **The logger is injected**, never global. It lives on the server struct and is
  passed explicitly to everything that needs it.
- **The store returns `*store.DB`, not `*sql.DB`.** ZombieZen states plainly that
  it "is not a `database/sql` driver" and registers none. Producing a real
  `*sql.DB` would mean importing `modernc.org/sqlite` directly, which this
  project forbids, or hand-writing a driver shim, which would also make
  `sqlitemigration` and SQLite's one-writer pooling unusable. `*store.DB` is a
  thin type over `sqlitex.Pool` that keeps both.
- **Concurrency** follows SQLite's model: a pool sized to the CPU count gives
  readers real parallelism, and a mutex serialises this process's writers so
  write transactions do not contend. Writes run in `BEGIN IMMEDIATE` so lock
  contention is resolved by `busy_timeout` (5s) rather than surfacing as a late
  `SQLITE_BUSY`.
- **Pragmas are per-connection**, so `foreign_keys`, `busy_timeout`, and (for
  read-only pools) `query_only` are applied in the pool's `PrepareConn` hook. A
  connection opened lazily later cannot silently omit them.
- **The game engine** exists only to establish one invariant: game randomness
  comes from `math/rand/v2` with a PCG source, seeded reproducibly. The legacy
  `math/rand` is not imported anywhere. Security material comes from
  `internal/tokens`, which uses `crypto/rand`; the two must never be swapped.

---

## Security notes

**Passwords.** Hashed with bcrypt at `bcrypt.MinCost`. The accepted length is 3
to 128 bytes, but bcrypt rejects anything over 72 bytes outright, so the
plaintext is first reduced to a SHA-256 digest and base64-encoded (44 bytes)
before hashing. This is the conventional pre-hashing mitigation and is what makes
both rules hold at once. Plaintext is never logged, returned, or stored.

**Sessions.** A session is a random 256-bit token in an `HttpOnly`,
`SameSite=Lax`, `Secure` cookie. Only its SHA-256 fingerprint is stored, so a
database snapshot yields no usable sessions. Nothing is kept in `localStorage`
and no token is readable by JavaScript. The idle deadline slides forward on
activity but never past the absolute lifetime.

**Deactivation is not revocation.** Deactivating an account blocks *new*
sign-ins. It deliberately does **not** terminate sessions the account already
holds; those remain valid until they expire. To sign an account out everywhere,
an administrator calls `DELETE /admin/accounts/{id}/sessions`, which also ends
any session currently impersonating that account. Keeping these separate means
"stop this person signing in again" and "kick them out right now" are distinct,
audited decisions.

**Impersonation.** An administrator may act as an active, activated `user`
account. The session row keeps its own `account_id` and gains
`impersonated_account_id`, so the real administrator is always recorded and
stopping never needs a fresh login. While impersonating, the session has exactly
the impersonated user's rights — every `/admin/*` endpoint is refused, and
passwords cannot be changed. Administrators cannot be impersonated.

**Activation links.** Single-use, expire after 48 hours, generated with
`crypto/rand`, stored only as a hash, and redeemed atomically — the claiming
`UPDATE` is conditional on the token still being unredeemed and unexpired, so a
concurrent replay loses. Issuing a replacement invalidates every outstanding link
for that account in the same transaction. Unknown, expired, and already-redeemed
tokens all return the same `410`, so the endpoint reveals nothing about which
invitations exist or were accepted. The application does not send email; the URL
is returned once for an administrator to deliver.

**Cross-origin protection.** The standard library's `http.CrossOriginProtection`
replaces a form-token CSRF scheme. It inspects `Sec-Fetch-Site` and `Origin` and
rejects unsafe cross-origin browser requests; `GET`, `HEAD`, and `OPTIONS` are
always allowed, which is why no endpoint changes state on those methods. It is
**not** a substitute for the cookie's `HttpOnly`, `Secure`, and `SameSite`
attributes — those defend against script access and network interception, which
cross-origin protection does not address.

**Reverse proxy.** This service speaks plain HTTP on a private listener; nginx
terminates TLS (1.3) in production. Forwarding headers are **not** believed
unless `--trusted-proxy` names the proxy's CIDR; otherwise the socket's peer
address is used and `X-Forwarded-For` is ignored, so no client can forge its
apparent address.

**Enumeration.** Login returns one message for a wrong password, an unknown
address, and a deactivated account, and hashes a decoy on the unknown-account
path so the timing matches.

**Admins are never in games.** Beyond the API check, the schema enforces it:
`game_account_role` carries a redundant `account_role` column constrained to
`'user'` with a composite foreign key to `account(id, role)`. No code path can
insert an administrator, and `ON UPDATE RESTRICT` also blocks promoting an
existing member to administrator.

**Never exposed:** password hashes, activation-token hashes, session tokens,
SQL, and filesystem paths. Responses are built from explicit view types, so a
new database column cannot leak by accident.

---

## HTTP API

Base path `/api/v1`. Requests and responses are JSON. Successful responses are
wrapped:

```json
{ "data": … , "meta": { "page": 1, "per_page": 25, "total": 42, "total_pages": 2 } }
```

`meta` appears on list endpoints only. Lists accept `?page=` and `?per_page=`
(default 25, maximum 100).

Failures are **RFC 9457 Problem Details** with `Content-Type:
application/problem+json`:

```json
{
  "type": "about:blank",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "The password does not meet the requirements.",
  "instance": "/api/v1/activation",
  "errors": [{ "field": "password", "message": "must be 3 to 128 bytes" }]
}
```

`errors` is an extension member carrying per-field validation failures.

Request bodies must be `application/json`, are bounded by `--max-body-bytes`, and
**unknown fields are rejected** — a typo becomes a clear `400` rather than a
silently ignored field.

### Session and activation

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `POST` | `/session` | — | Log in. Sets the session cookie. |
| `GET` | `/session` | session | The current session and account. `401` when absent. |
| `DELETE` | `/session` | — | Log out. Always succeeds; clears the cookie. |
| `POST` | `/session/impersonation` | admin | Start impersonating `{account_id}`. |
| `DELETE` | `/session/impersonation` | session | Stop impersonating. |
| `POST` | `/activation` | — | Redeem a magic link, set the first password, sign in. |

### The authenticated account

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/me` | The account. |
| `PATCH` | `/me` | Update `display_name`, `timezone`. |
| `PUT` | `/me/password` | Change the password. Requires the current one; revokes all other sessions. |
| `GET` | `/me/games` | Active memberships. |

### Administration

All require an administrator who is **not** impersonating.

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/admin/accounts` | List. Filters: `q`, `role`, `active`. |
| `POST` | `/admin/accounts` | Invite. Returns the account **and its one-time activation link**. |
| `GET` | `/admin/accounts/{id}` | One account. |
| `PATCH` | `/admin/accounts/{id}` | Update, or deactivate via `is_active`. |
| `POST` | `/admin/accounts/{id}/activation-link` | Reissue; invalidates prior links. |
| `DELETE` | `/admin/accounts/{id}/sessions` | Revoke every session. |
| `GET` | `/admin/games` | List. Filters: `q`, `active`. |
| `POST` | `/admin/games` | Create. |
| `GET` | `/admin/games/{id}` | One game. |
| `PATCH` | `/admin/games/{id}` | Rename, or deactivate via `is_active`. |
| `GET` | `/admin/games/{id}/memberships` | The roster. |
| `PUT` | `/admin/games/{id}/memberships/{accountId}` | Add or update a membership (`is_gm`, `is_active`). |

Guards that return `409`: deactivating the last active administrator,
deactivating your own account, revoking your own sessions, promoting an account
that belongs to a game, and assigning an administrator to a game.

### Health

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/health/live` | The process is running. Never touches the database. |
| `GET` | `/health/ready` | Ready to serve. Queries the database and checks the migration level; `503` otherwise. |

Both are unauthenticated so a proxy or supervisor can reach them, and neither
exposes anything beyond up/down and version.

### Deletion

There is none. Accounts and games are **deactivated**, never removed, so history
and referential integrity survive. `is_active=false` is the whole story.

---

## Development with Caddy

Production serves the Ember build and this API from **one origin** behind nginx.
Development reproduces that with Caddy, because otherwise the Ember dev server
(`:4200`) and this API (`:3000`) are different origins and the session cookie,
`SameSite`, and cross-origin protection all behave differently than they will in
production.

```
browser ──▶ http://localhost:8081  (Caddy)
                 ├── /api/*  ──▶ 127.0.0.1:3000   this service
                 └── /*      ──▶ 127.0.0.1:4200   Ember dev server
```

Three terminals:

```bash
# 1. API
go run ./cmd/ecv8-api serve --db-path db --cookie-secure=false

# 2. Ember dev server, in ../app
pnpm start

# 3. Proxy
caddy run --config dev/Caddyfile
```

Then browse **http://localhost:8081**. Do not browse `http://localhost:4200`
directly: that bypasses the proxy and the cookie will not be sent.

**Cookies in development.** The API must run with `--cookie-secure=false`,
because this origin is plain HTTP and a browser silently discards a `Secure`
cookie sent over it. Production sets it back to `true`. `SameSite=Lax` behaves
identically either way, since the browser sees one origin.

**Cross-origin protection in development.** Requests the browser makes from
`http://localhost:8081` to `http://localhost:8081/api/...` are same-origin and
pass with no configuration. Anything from another origin is rejected, which is
the point. To allow a specific extra origin, name it with `--trusted-origin`
rather than loosening the proxy.

**Closer to production.** Change the site address in `dev/Caddyfile` to
`https://ec.localhost`, set `--cookie-secure=true`, and set
`EC_PUBLIC_BASE_URL=https://ec.localhost`. Caddy issues a certificate from its
internal CA; the first run asks for `sudo` to trust it locally.

**Port note.** `8081` is used because `8080` is commonly occupied. Change the
site address in `dev/Caddyfile`, `EC_PUBLIC_BASE_URL`, and the fallback proxy in
`../app/vite.config.mjs` together if you move it.

---

## Validation

```bash
gofmt -l .                 # no output means formatted
go build ./...
go vet ./...
```

---

## Tests

There are none, by design, in this first version. No Go unit, integration, or
end-to-end tests exist, and the generated placeholders were removed rather than
kept as meaningless green checks. `go build` and `go vet` are the checks that
run. When tests are added, this section is the place to say so.
