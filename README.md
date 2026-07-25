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
  - [ecdb](#ecdb)
  - [ecapi](#ecapi)
  - [earl](#earl)
  - [ec](#ec)
  - [The shared session](#the-shared-session)
- [The database](#the-database)
  - [Creating and opening](#creating-and-opening)
  - [Two domains, one file](#two-domains-one-file)
  - [Migrations](#migrations)
  - [The initial administrator](#the-initial-administrator)
  - [Backups](#backups)
- [Architecture](#architecture)
- [Security notes](#security-notes)
- [HTTP API](#http-api)
- [Development](#development)
  - [Primary setup: system Caddy over HTTPS](#primary-setup-system-caddy-over-https)
  - [Alternative: standalone plain-HTTP proxy](#alternative-standalone-plain-http-proxy)
- [Validation](#validation)
- [Tests](#tests)

---

## Requirements

| Tool  | Version  | Notes |
|-------|----------|-------|
| Go    | 1.26.4   | Exact version this module declares. |
| Caddy | 2.x      | Development proxy only; production uses nginx. |
| Air   | latest   | Optional. Live reload for the API; see `.air.toml`. |

The SQLite engine is compiled in through ZombieZen, which is CGO-free. There is
nothing to install beyond Go.

---

## Quick start

```bash
# 1. The database directory must already exist. The store never creates one.
mkdir -p db

# 2. Create ecv8.db and seed the first administrator.
EC_ADMIN_EMAIL=admin@example.com EC_ADMIN_SECRET='choose-a-good-secret' \
  go run ./cmd/ecdb database create --db-path db

# 3. Serve it.
go run ./cmd/ecapi serve --db-path db

# 4. Check it is alive.
curl -s localhost:3000/api/v1/health/ready
```

That serves the API on its own. To use it from a browser you also need the Ember
dev server and a proxy putting both behind one origin — see
[Development](#development).

To work on the frontend without a database on disk, run against a seeded
in-memory database instead:

```bash
go run ./cmd/ecapi serve --memory dev
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

`ecdb` accepts `--db-path` and `--quiet`, and nothing else. Every other flag
below belongs to `ecapi`.

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--db-path` | `EC_DB_PATH` | `db` | Directory holding `ecv8.db`. Must exist. |
| `--quiet` | `EC_QUIET` | `false` | `ecdb` only. Suppress status lines. A value a command was asked for — a path, a version — is still printed. |
| `--memory` | `EC_MEMORY` | *(unset)* | Serve a seeded in-memory database instead. Development only. |
| `--read-only` | `EC_READ_ONLY` | `false` | Open SQLite read-only. Write endpoints fail. |
| `--listen-addr` | `EC_LISTEN_ADDR` | `127.0.0.1:3000` | Private HTTP listener. |
| `--public-base-url` | `EC_PUBLIC_BASE_URL` | `https://ecv8.localhost:8443` | Origin browsers use. Activation links are built from it. |
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

**`ecdb` owns the database file. `ecapi` serves HTTP. `earl` and `ec` are
clients.** They are separate binaries so the long-running service can be
installed and confined without carrying the ability to create a database or seed
an administrator, and so neither client has any way to reach the database at
all.

```
ecdb database create              create ecv8.db and seed the initial admin
ecdb database verify              open ecv8.db read-only and print its migration
ecdb database version             print ecv8.db's migration number, and nothing else
ecdb database upgrade             apply any migrations ecv8.db is missing
ecdb database backup              write a consistent, compacted copy of ecv8.db
ecdb database compact             reclaim unused space in ecv8.db
ecdb version                      print the build version

ecapi serve                       open the database and serve the HTTP API
ecapi version                     print the build version

earl get|post|put|patch|delete    send that method to an API path
earl login | logout               authenticate, and save or forget the session
earl whoami | identities          show the current session, or the saved ones
earl version                      print the build version

ec app login | logout             authenticate, and save or forget the session
ec app whoami | identities        show the current session, or the saved ones
ec version                        print the build version
```

Every command accepts `--help`, which lists the flags in scope with their
defaults. `ecdb` takes only `--db-path` and `--quiet`: a command that never
opens a listener should not accept `--listen-addr`, nor be refused for a
`--public-base-url` it would never use.

### ecdb

Everything that touches the file sits under `database`, so what is a database
operation and what is not stays obvious — `ecdb version` reports the build and
opens nothing, while `ecdb database version` opens `ecv8.db` and reports the
schema.

**Every subcommand is documented in [`cmd/ecdb/README.md`](cmd/ecdb/README.md)**:
what each one prints, what it refuses, the initial-administrator variables, the
backup procedure, and why `backup` and `compact` never migrate.

### ecapi

`cmd/ecapi/ecapi.service` is a sample systemd unit for running `ecapi` behind
nginx as an unprivileged `ecapi:ecapi`, with everything under one tree:

```
/opt/ecv8/bin/   ecapi and ecdb, root-owned and read-only to the service
/opt/ecv8/app/   the built Ember client, served by nginx; the API never reads it
/var/lib/ecv8/   the database — a systemd StateDirectory, and the one path the
                 service may write
```

It is a starting point to copy, not something the build installs — deployment
stays an operator's decision, and nothing in this repository reads it.

### earl

`earl` exercises the API the way a client does, over HTTP and nothing else. It
imports no store package and cannot open the database, which is what makes it
evidence: if `earl` can do it, a real client can.

The REST surface is the command line — the verb and path you would send are what
you type — so it covers every endpoint without per-endpoint code:

```bash
earl login --email admin@example.com          # password from ECV8_PASSWORD
earl whoami                                   # sugar for: earl get /session
earl get /admin/accounts
earl post /admin/accounts -d '{"email":"t@x.com","role":"user","display_name":"T"}'
earl patch /me -d @profile.json
earl get /me --no-auth                        # see what an anonymous caller gets
```

Paths are relative to `--base-url` (`ECV8_BASE_URL`), which defaults to
`http://localhost:3000` and has `/api/v1` appended when it carries no path of
its own. `earl` addresses the API directly rather than through the development
proxy, because none of the reasons a browser must use the proxy apply to it: it
sends no `Origin`, so cross-origin protection does not reject it, and it stores
the session cookie itself.

### ec

`ec` is the game master's client. It can do nothing `earl` cannot — same HTTP,
same rules, same restraint about the database — and exists because `earl` is the
wrong shape for running a game: a game master should not have to remember which
path a task lives behind.

```bash
ec app login --email gm1@example.com          # password from ECV8_PASSWORD
ec app whoami
ec app identities
ec app logout
ec version
```

The commands sit under `app` rather than at the top level because `ec` is
expected to grow a broad surface, and grouping from the start is cheaper than
moving verbs later. `version` stands alone because it reports the build and
talks to nothing.

### The shared session

Both clients read and write **one** credential file, so a game master signs in
once and both tools are signed in — `ec app login` then `earl get /admin/games`
works, and `ec app logout` ends the session for both.

This API authenticates with an HttpOnly session cookie, and that cookie's value
is returned exactly once, in the `Set-Cookie` header of a successful login.
Login captures it and saves it in `~/.config/ecv8/{EC_ENV}/credentials.json`,
keyed by base URL and account, so several identities can be held at once and
`--email` picks between them. **The saved value is a live session token**, so
the file is `0600` in a `0700` directory and neither client ever prints it.
`ECV8_CREDENTIALS` overrides the path.

Neither client needs to be told the cookie's name, even though `--cookie-name`
makes it configurable: a login sets exactly one cookie, so whatever arrives is
the session, and its name is saved with it for later requests. `--cookie-name`
is only a tie-breaker for when something in front of the API — a load balancer
adding a routing cookie — sets one too. Faced with two, the client names them
and stops rather than guessing, because guessing would mean sending a routing
cookie as a credential and saving no session at all.

Client flags read `ECV8_`-prefixed variables rather than `EC_`, so pointing a
client at another host never means touching a server's configuration. The prefix
is shared between the two clients rather than being per-command **because the
sessions are**: they are keyed by base URL, so if `ec` and `earl` read different
variables they could be pointed at different servers, and the shared file would
silently stop being shared.

The prefix covers flags only. **The environment variable is `EC_ENV`, not
`ECV8_ENV`**: there is no `--env` flag, so nothing reads `ECV8_ENV`. `EC_ENV` is
read directly at startup, before flags parse, exactly as `ecapi` and `ecdb` read
it — a checkout has one idea of which environment it is in, and it scopes the
credential file so a development session and a production session are never
confused.

The transport, the cookie rules, and the credential store are
`internal/apiclient`, written once for both. The server never imports it.

---

## The database

One SQLite file, always named **`ecv8.db`**. Commands take the *directory* that
contains it; the filename is fixed and is never configurable. This is deliberate:
an operator can point a command at the wrong directory and get a clear error,
but cannot accidentally create a second database under a different name.

### Creating and opening

`ecdb database create` creates the file. It fails if `ecv8.db` already exists — an
existing database is never truncated, replaced, or reopened as if it were new.
The directory must already exist; **the store never creates a directory**, and it
rejects an empty path, a missing path, a path that is not a directory, and a
directory it cannot write to, without touching the filesystem first.

If initialisation fails partway, the file it just created is removed, along with
its `-wal` and `-shm` sidecars. A pre-existing file is never removed, because
creation never reaches initialisation when one exists.

Opening is separate and never creates anything:

- **Writable** (`serve`, `ecdb database upgrade`): the application marker is
  checked, pending migrations are applied automatically, and WAL is asserted. A
  database *newer* than the binary is rejected without being modified.
- **Writable without migrating** (`ecdb database compact`): as above, but the
  database is left at whatever level it is on and the command refuses unless
  that is this binary's level. VACUUM must never be the thing that changes a
  schema.
- **Read-only** (`ecdb database verify`, `ecdb database version`,
  `ecdb database backup`, `serve --read-only`): SQLite is opened in genuine
  read-only mode. No migration runs and the database itself is never written, so
  one newer than the binary is perfectly acceptable — an older build can still
  inspect it. `backup` writes only the new file it creates.

`PRAGMA application_id` is the authoritative marker that a file was created by
ECV8. Its value is `0x65637638` (`1701017144`), the four ASCII bytes of `ecv8`.
It is **not** the migration version, which lives in `PRAGMA user_version`.
Opening a SQLite file that is not an ECV8 database fails with a clear error
rather than being migrated into one.

### Two domains, one file

The schema is two domains, and one table bridges them:

```
application            seam                engine
-----------            ----                ------
account            game_player          game_state    turn, PCG seeds
account_activation   .id is the         faction       controlled by a player_id
session               engine's          entity        a ship or a colony
game                  player_id
```

**`game_player.id` is the engine's `player_id`, and it is the only identity that
crosses.** No engine table references `account`; the engine never needs to know
that accounts exist. `game_id` crosses as well, because every engine row has to
be scoped to a game.

A seat is held by a **human** — which is an account — or by an **agent**, which
the engine plays itself and which has no account at all. That is what makes "an
agent cannot sign in" a fact about the schema rather than a property of some
unusable password value, and it keeps `account` meaning exactly "a human who can
sign in".

**Which agents exist is a property of the binary, not of the database.** The
catalogue lives in `internal/engine` (`agents.go`) and is served from there, so
adding an agent is one commit — write the code, add it to the list — with no
migration and no second artefact to forget. A seat stores `agent_key`, and the
schema constrains that key's *format* and deliberately not the set of valid
keys: that set changes with every release, and enumerating it in a `CHECK` would
mean a migration per agent.

The rejected alternative was an `agent` table populated by a migration whenever
agent code was committed. Migrations here are forward-only, so a rollback would
leave the database advertising an agent the running binary cannot play, and the
failure would surface at turn resolution rather than at deploy. A foreign key
onto such a table looks like integrity, but its parent row is only an assertion
that some code exists — SQLite cannot check the thing that actually matters.
The registry can, at seat time, and the listing reports a stranded seat as
`playable: false` instead of hiding it.

If the same code ever needs to run at different settings — "Aggressive (hard)"
and "Aggressive (easy)" both dispatching to `aggressive` — that is an
`agent_profile` table, and it *is* legitimately database-resident: it would be
data a game master curates rather than a manifest of what the binary contains.
It does not exist yet and should not be built before the tuning it would hold.

Both domains live in one SQLite file, so the separation is a discipline rather
than something the storage enforces. The compensation is composite foreign keys,
which turn rules that would otherwise live in Go into things SQLite checks: a
faction's controller is seated in the *same* game, that controller is not the
game master, an entity's faction is in the same game, an admin never holds a
seat. `internal/store/migrations/0002_engine.sql` explains each one where it is
declared.

Engine tables use `ON DELETE RESTRICT` rather than the `CASCADE` the application
tables use. Nothing here is deleted, only deactivated, so a delete reaching an
engine table is a mistake and fails loudly instead of taking a game's state with
it.

### Migrations

Migrations are SQL files embedded in the binary and applied by ZombieZen's
`sqlitemigration` in filename order. A database's `user_version` is the count of
migrations applied to it.

- `0001_initial.sql` — the application domain.
- `0002_engine.sql` — the engine domain, and the rebuild of `game_account_role`
  into `game_player`.
- `0003_agent_key.sql` — `agent_key` on a seat, naming the implementation that
  plays it.

- Writable opens migrate forward automatically.
- Read-only opens never migrate.
- A database ahead of the binary is refused for writing. Deploy the newer binary
  or restore an older snapshot; there is no downgrade path.

`serve` migrates on open, which is what keeps the running binary and the schema
in step, so nothing has to be run before a deploy. An operator who would rather
migrate deliberately — at a chosen moment, before the new binary starts, or
against a restored backup — uses `ecdb database upgrade` instead of being forced
to launch a server to do it. See
[`cmd/ecdb/README.md`](cmd/ecdb/README.md#upgrading-a-database).

### The initial administrator

`ecdb database create` is the **only** operation that may seed an administrator,
and it reads exactly two variables, `EC_ADMIN_EMAIL` and `EC_ADMIN_SECRET`, both
validated before the database file is created. Opening an existing database
ignores them entirely, so they cannot be used to add or alter an administrator
later; after creation, administrators create other accounts through the API. The
rules for setting one, both, or neither are in
[`cmd/ecdb/README.md`](cmd/ecdb/README.md#the-initial-administrator).

### Backups

`ecdb database backup` takes one, using SQLite's `VACUUM INTO` so a copy is
consistent even against a running server. The procedure, the naming, the file
permissions, and what restoring does to live sessions are documented with the
command, in [`cmd/ecdb/README.md`](cmd/ecdb/README.md#backups).

Scheduling and retention are not in scope here. Taking a backup is a command;
deciding when to take one is cron's job and an operator's policy.

---

## Architecture

```
cmd/ecdb/              database commands: create, verify, version, upgrade,
                       backup, compact — all under `ecdb database`
                       README.md: the command reference for all of them
cmd/ecapi/             server entry point, command tree, logger construction
                       ecapi.service: sample systemd unit, installed by hand
cmd/earl/              raw API client: the verb and path you type are what it sends
cmd/ec/                the game master's client; convenience over the same API
internal/
  apiclient/           the clients' shared transport, cookie rules, and sessions
  cerrs/               constant sentinel errors
  config/              flags, environment, validation, precedence
  dotenv/              dotenv loading, patterned after ../ecv7
  engine/              game-engine foundation: math/rand/v2 with PCG; its state
                       is the engine domain, keyed by player_id
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
- **Two clients, one client package.** `earl` and `ec` are separate commands
  with separate surfaces, but the same server, the same cookie rules, and the
  same saved sessions, so all of that lives in `internal/apiclient` rather than
  being written twice and drifting. It depends on nothing the server owns, and
  nothing the server builds may import it.
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
- **The game engine** establishes one invariant: game randomness comes from
  `math/rand/v2` with a PCG source, seeded reproducibly, so a turn can be
  replayed. The legacy `math/rand` is not imported anywhere. Security material
  comes from `internal/tokens`, which uses `crypto/rand`; the two must never be
  swapped. Its state is the engine domain described above, reachable through
  `player_id` without knowing that accounts exist.

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

### Games, as their players see them

**Authorisation here is the seat, not the role.**

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/games/{id}` | One game, its state, and whether you are its game master. |
| `POST` | `/games/{id}/state` | Set the game up: write its initial state at turn 0. Game master only. |
| `GET` | `/games/{id}/players` | The human roster, active and inactive. Game master only. |
| `POST` | `/games/{id}/players` | Add an account by `email`, as a player or `is_gm`. Game master only. |
| `PATCH` | `/games/{id}/players/{playerId}` | Promote to `is_gm`, or set `is_active`. Game master only. |

An administrator can never hold a seat — a composite foreign key forbids it — so
an administrator reaches neither of these. That is deliberate and matches the
rest of the service: admin rights stop at impersonation, and an administrator who
needs a player's view impersonates them. An account with no seat is answered
`404`, not `403`, because "there is a game here you may not see" is itself
something about a game they are not in. A deactivated *seat* is answered the same
way; a deactivated *game* is not, and stays readable with `is_active: false` by
the people who played it.

`GET` returns `state: null` until the game has been set up, which is an ordinary
stage of a game's life rather than a failure. Alongside it, and **only** for the
game master of a game in that state, comes `default_seed` — the values the setup
form starts from:

```json
{
  "data": {
    "id": 3, "name": "Alpha", "is_active": true, "is_gm": true,
    "state": null,
    "default_seed": { "hi": "19", "lo": "42" }
  }
}
```

`POST /games/{id}/state` takes an optional `seed`:

```json
{ "seed": { "hi": "19", "lo": "42" } }
```

Omitting it is the ordinary case and yields the same default, which comes from
`engine.DefaultSeed` so the value written and the value offered cannot drift
apart. A second call is `409`, not an overwrite: the seed is what makes every
later turn reproducible, so replacing it after play began would invalidate the
turns already resolved. Setting up an inactive game is `409` too.

#### The roster

One rule covers all three roster endpoints: **a game master may change player
seats, and a game master's seat is an administrator's business.**

Everything else follows from it rather than being a rule of its own. Promotion
is allowed and is one-way, because a demotion would be a change to a GM seat.
Deactivating a player is allowed and deactivating any game master is not —
including your own seat, so a game cannot be left with nobody able to run it.
`is_gm: false` is refused with `403` rather than ignored, so a client is never
told a demotion happened when it did not.

The escape hatch is what makes that safe to be strict: `PUT
/admin/games/{id}/memberships/{accountId}` still sets any seat to any state, so
every refusal here has somewhere to go.

Accounts are added **by email address**, never by id. Listing accounts is an
administrator's endpoint and stays that way — a game master inviting someone
already knows the address, and a directory is precisely what running a game
should not hand out. For the same reason every refusal reads alike: an address
belonging to nobody, to an administrator, or to a deactivated account all answer
`422` with "That account cannot join a game", because distinguishing them would
turn the endpoint into a way to discover which accounts exist.

Adding is a create, not a save. An account already seated is a `409` rather than
an overwrite, since a game master adding someone is asserting they are *not* in
the game; being wrong about that is worth reporting. Bringing back a removed
player is `PATCH … {"is_active": true}` on the seat they still have.

The roster includes inactive seats, because deactivating one is how a player is
removed and a roster that hid them could not undo it. It excludes agent seats,
which are the engine's players and have a listing of their own.

**Seed words are decimal strings, not JSON numbers.** This is the one place the
API sends an integer as a string. A seed word is a full-range `uint64` and a JSON
number is an IEEE 754 double in every browser, so anything above 2^53 would reach
the client rounded and come back changed — and a seed that does not round-trip
exactly makes a game unreplayable, which is the single property
`internal/engine` exists to guarantee.

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
| `GET` | `/admin/games/{id}/memberships` | The roster of **human** seats. |
| `PUT` | `/admin/games/{id}/memberships/{accountId}` | Add or update a membership (`is_gm`, `is_active`). |
| `GET` | `/admin/agents` | The agents **this build** can play. Reads no database. |
| `GET` | `/admin/games/{id}/agents` | The agents seated in a game. |
| `POST` | `/admin/games/{id}/agents` | Seat one (`agent_key`, optional `agent_name`, `is_active`). |
| `PATCH` | `/admin/games/{id}/agents/{playerId}` | Rename or deactivate a seated agent. |

Guards that return `409`: deactivating the last active administrator,
deactivating your own account, revoking your own sessions, promoting an account
that belongs to a game, assigning an administrator to a game, and seating an
agent in an inactive game.

`POST /admin/games/{id}/agents` is a POST rather than a PUT because a seat is
not identified by its path: several agents may play one game, and seating the
same implementation twice legitimately produces two players. `agent_key` is
validated against `GET /admin/agents` and a bad one returns `422` listing the
valid keys. It is not updatable and `PATCH` rejects it outright — the key is
what the engine dispatches on and is written into a game's state, so changing it
would hand a faction to different code mid-game. Retiring an agent means
deactivating its seat and adding another; as everywhere else, there is no
`DELETE`.

Each seat carries `playable`, which is computed rather than stored: it reports
whether this build still has the implementation the seat names. A database
written by a later release can hold a key this binary does not know, and a game
master should see that in the listing rather than discover it when a turn is
resolved.

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

## Development

Production serves the Ember build and this API from **one origin** behind nginx,
which terminates TLS. Development reproduces that shape with Caddy, because
otherwise the Ember dev server (`:4200`) and this API (`:3000`) are different
origins, and the session cookie, `SameSite`, and cross-origin protection all
behave differently than they will in production.

```
browser ──▶ https://ecv8.localhost:8443  (Caddy, tls internal)
                 ├── /api/*  ──▶ localhost:3000   this service
                 └── /*      ──▶ localhost:4200   Ember dev server
```

### Primary setup: system Caddy over HTTPS

A long-running Caddy — a Homebrew service, for instance — serves
`ecv8.localhost:8443` with `tls internal`, so development uses real HTTPS from
Caddy's own CA. This matches production most closely: `--cookie-secure` stays at
its default `true`, exactly as it will be behind nginx.

Add a site to the system Caddyfile (`/opt/homebrew/etc/Caddyfile` on Homebrew):

```caddy
ecv8.localhost:8080 {
	redir https://ecv8.localhost:8443{uri}
}

ecv8.localhost:8443 {
	tls internal
	encode zstd gzip

	# The API owns this prefix. handle blocks are mutually exclusive, so the
	# first match wins and nothing here falls through to Ember.
	@api path /api/*
	handle @api {
		reverse_proxy localhost:3000
	}

	# Everything else is the Ember dev server. reverse_proxy passes WebSocket
	# upgrades through unchanged, which is what Vite's HMR needs.
	handle {
		reverse_proxy localhost:4200
	}

	# Do not pin HSTS on a hostname you also use over plain http in development.
	header {
		Strict-Transport-Security "max-age=0"
	}
}
```

Reload Caddy, then start both processes. A `Procfile.dev` in the directory above
both repositories runs them together:

```
backend: cd api && air
frontend: cd app && pnpm start
```

```bash
overmind start -f Procfile.dev     # or: foreman, hivemind, two terminals
```

Caddy is already running as a service, so it is deliberately not in the
Procfile. `air` rebuilds and restarts the API on save; its configuration is
`.air.toml`, and it writes binaries to the git-ignored `tmp/`.

Then browse **https://ecv8.localhost:8443**. Do not browse
`http://localhost:4200` directly: that bypasses the proxy and the cookie will not
be sent.

Set the matching values in `.env.development.local`:

```bash
EC_PUBLIC_BASE_URL=https://ecv8.localhost:8443
EC_LISTEN_ADDR=localhost:3000
EC_DB_PATH=games/alpha
```

`EC_PUBLIC_BASE_URL` must match the origin **including the port**, because
activation links are built from it. A mismatch produces links that look right and
lead nowhere.

### Alternative: standalone plain-HTTP proxy

`dev/Caddyfile` is a self-contained proxy for when there is no system Caddy, or
when you want one that starts and stops with the project. It serves plain HTTP on
`http://localhost:8081`, so Secure cookies cannot be used:

```bash
caddy run --config dev/Caddyfile

go run ./cmd/ecapi serve --db-path db \
  --cookie-secure=false --public-base-url http://localhost:8081
```

`8081` is used because `8080` is commonly occupied. If you move it, change the
site address in `dev/Caddyfile` and `EC_PUBLIC_BASE_URL` together.

### Notes that apply to both

**Cookies.** Over HTTPS, leave `--cookie-secure` at `true`. Over plain HTTP it
must be `false`, because a browser silently discards a `Secure` cookie sent over
`http`. `SameSite=Lax` behaves identically either way, since the browser sees one
origin.

**Cross-origin protection.** Requests the browser makes from the proxy origin to
`/api/...` on that same origin are same-origin and pass with no configuration.
Anything from another origin is rejected, which is the point. To allow a specific
extra origin, name it with `--trusted-origin` rather than loosening the proxy.

**Client addresses in logs.** Caddy forwards `X-Forwarded-For`, but this service
ignores forwarding headers unless a proxy is explicitly trusted. Run with
`--trusted-proxy 127.0.0.1/32` if you want request logs to show the real client
address rather than the proxy's.

---

## Validation

```bash
gofmt -l .                 # no output means formatted
go build ./...
go vet ./...
go test ./...              # see Tests, below, for what is covered
```

---

## Tests

```bash
go test ./...
```

Covered today:

| Suite | What it protects |
|-------|------------------|
| `cmd/ecdb/main_test.go` | The database commands: what each prints, refuses, and leaves on disk. |
| `internal/engine/agents_test.go` | The agent catalogue's invariants — keys unique, storable, and exactly reported. |
| `internal/store/agents_test.go` | Agent seats, and the schema constraints that hold whatever code writes a row. |
| `internal/store/seed_test.go` | A PCG seed's round trip through SQLite's signed `INTEGER`. |
| `internal/server/handlers_admin_agents_test.go` | The agent endpoints: statuses, validation, scoping, and authorisation. |
| `internal/server/handlers_games_test.go` | The player-facing game endpoints: who sees a game, and setting one up. |
| `internal/server/handlers_game_players_test.go` | The roster a game master manages, and the four things they may not do to it. |

### The database commands

`cmd/ecdb/main_test.go` drives `run` with real arguments — the same entry point
`main` uses — and asserts what each subcommand prints, what it refuses, and what
it leaves on disk. It reaches past the command only to inspect a database, never
to arrange one a command could have made itself.

The tests exist because the file on disk is the one thing here with no undo. The
invariants they hold are the destructive ones:

- `create` never truncates an existing database, and never creates a directory.
- A refused command changes nothing — not a byte, not the journal mode. This is
  checked by digest, not just by query, because a writable open can rewrite a
  file on its way in to deciding it should not have been opened.
- Inspection never mutates, including a database newer than the binary.
- A backup is created `0600`, is a valid database with the source's contents,
  and leaves the source untouched.
- `backup` and `compact` refuse a migration mismatch rather than migrating.

They use the standard library only, and each one builds its own database under
`t.TempDir()`. Every `EC_`-prefixed variable is cleared for the duration of a
test, so a developer's own `EC_DB_PATH` cannot decide what is being run.

### Agents

The agent suites follow the same principle one layer up: they exercise the real
thing rather than a stand-in. The store tests run against a migrated in-memory
database, so a rule the schema enforces is tested by attempting the write and
requiring SQLite to refuse it — an agent with no key, a malformed key, an agent
made game master, a seat that is half human and half agent. The handler tests
drive the real router with real requests and authenticate through a genuine
session cookie, so a change that broke authorisation fails there too.

What they hold:

- The served catalogue is the engine's, exactly, and never anything stored.
- An unknown `agent_key` is a `422` that names the valid keys, and a key is
  normalised before it is stored so one agent cannot enter under two spellings.
- `agent_key` cannot be changed on an existing seat.
- A seat id from one game cannot reach another game's seat.
- Agents and memberships stay separate listings in both directions.
- Several agents may share a game, and human and agent seats draw `player_id`
  from one sequence.

### Seeds

A game's PCG seed is two `uint64` words and SQLite's `INTEGER` is signed, so a
word at or above 2^63 is stored as a negative number. That is exact — Go defines
a conversion between integer types of the same size as a reinterpretation of the
bits — but it looks like corruption in a SQLite shell, and the obvious "fix" is
to clamp it.

`seed_test.go` is there to make that fix fail. It checks the round trip at both
ends of the range and at the 2^63 boundary, asserts that distinct seeds stay
distinct on disk, and writes the values through a real database rather than only
through Go's type system — a column that promoted a large value to `REAL` would
pass every in-memory check and lose the low bits. A seed that does not
round-trip exactly makes a game unreplayable, which is the one property
`internal/engine` exists to guarantee.

### Setting a game up

`handlers_games_test.go` covers the endpoints a player uses, and its subject is
as much *who* gets an answer as what the answer is. Every case is a different
seeded account with a real session cookie — a player, that game's master, an
account seated elsewhere, an administrator — rather than one account with a flag
flipped, because the boundary being tested is the seat and nothing else.

What it holds:

- A game with no state answers `state: null`, and only its game master is given
  the `default_seed` the setup form starts from. It stops being offered once the
  game has been set up.
- Omitting the seed writes `engine.DefaultSeed` at turn 0, so the value the form
  offers and the value the API writes are checked against the same source.
- A seed of 2^64 − 1 and one of 2^53 + 1 survive the round trip **as sent**.
  This is the case the string wire format exists for; a JSON number would round
  both.
- A negative, fractional, overflowing, or empty seed word is a `422` naming
  `seed.hi` or `seed.lo`.
- A player at the same table cannot set the game up, a second setup is `409` and
  leaves the first seed in place, and an unseated account — including an
  administrator — gets `404` from both endpoints rather than `403`.
- A seat deactivated after the fact stops being able to read the game.

### The roster

`handlers_game_players_test.go` is mostly a test of what is refused, because
the feature is one rule — a game master may change player seats, a GM seat is
an administrator's business — and a rule is only worth stating if something
enforces it.

What it holds:

- Demoting another game master, deactivating one, demoting **yourself**, and
  deactivating yourself are each `403`, and the roster is unchanged afterwards.
  Self is checked explicitly: it is the case a rule written as "not *another*
  game master" would have let through, and it is the one that strands a game.
- An administrator can still demote and deactivate the same seat, so every
  refusal above has somewhere to go.
- A promoted player really can run the game — the test signs in as them and
  reads the roster, rather than believing the `is_gm` in the response.
- The three refusals for an unusable address — nobody, an administrator, a
  malformed string — are compared against each other and must be *identical*,
  which is what stops the endpoint reporting which accounts exist.
- Adding somebody already seated is a `409` that leaves their existing seat
  alone, rather than an overwrite.
- A player at the same table gets `403` from all three endpoints; an account
  with no seat and an administrator get `404`; anonymous gets `401`.

### What is not covered

Accounts, sessions, activation, and the administrative endpoints have no tests.
The generated placeholders were removed rather than kept as meaningless green
checks. Adding coverage needs no permission — when you do, list it above.
