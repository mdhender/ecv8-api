# CLAUDE.md — working in `api/`

The Go HTTP API for ECV8. `README.md` in this directory explains _what_ the
service is, how it is configured, and why it makes the choices it does; read it
before changing anything structural. This file is the shorter list of _how to
work here_ — the conventions, the checks, and the traps.

## Orientation

| Question                          | Where the answer is                    |
| --------------------------------- | -------------------------------------- |
| What the service does, why        | `README.md` (this directory)           |
| What every `ecdb` subcommand does | `cmd/ecdb/README.md`                   |
| What the clients do, and their one shared session | `README.md` § Commands |
| Every flag and environment variable | `README.md` § Configuration, `.env.example` |
| Endpoint list and payload shapes  | `README.md` § HTTP API                 |
| Why a security choice was made    | `README.md` § Security notes           |
| The Ember client                  | `../app/README.md`, `../app/CLAUDE.md` |
| Original brief                    | `../project-prompt.txt`                |
| The `ff/v4` and dotenv patterns   | `../../ecv7` — read it, never modify it |

This directory is its own git repository. `../app` is a separate one, and there
is no parent repository above them — do not stage or commit across the two.

`internal/config` and `internal/dotenv` are patterned after the previous
version, `../../ecv7` (`README.md` says "`../ecv7`" because it is written from
the directory above this one). It is a reference to read for the established
shape, not a dependency and not somewhere to make changes. Reuse the ideas; do
not copy code that the current Go release or this project's rules have moved
past.

## Commands

Run everything from this directory.

```bash
gofmt -l .            # no output means formatted
go build ./...
go vet ./...
go test ./...         # these four are the checks — run all of them
air                   # live reload; config in .air.toml, binaries in tmp/
go run ./cmd/ecapi serve --memory dev       # seeded in-memory database
go run ./cmd/ecdb database verify --db-path games/alpha
```

Four binaries are built from this module, and the split between them is the
point. `cmd/ecdb` owns the database file and takes only `--db-path` and
`--quiet`; every operation on the file lives under its `database` subcommand
(`create`, `verify`, `version`, `upgrade`, `backup`, `compact`), so a new
storage-only command has an obvious home and the group's existence keeps
non-database work visibly separate. `cmd/ecapi` serves HTTP and never creates a
database. `cmd/earl` and `cmd/ec` are clients and never touch either. `air`
builds `ecapi`.

Keep the boundaries. A command that creates a database must not also be able to
serve one, because the long-running service is then confined to what it
actually needs. Storage work goes in `ecdb`, not `ecapi`.

**The clients speak HTTP and only HTTP.** Neither may import `internal/store`,
open a database, or implement a rule the server owns — they send a request and
print what comes back. Their value is that they are not privileged: if a client
can do something, a real client can. A convenience that shortcuts the API would
quietly destroy that, and applies to `ec` first, because a convenience command
is exactly where the shortcut would look reasonable.

`earl` is the raw client. The REST surface is its command line
(`earl get /admin/accounts`), so a new endpoint needs no `earl` change; only the
commands that touch the saved session — `login`, `logout`, `identities` — are
special, plus `whoami` as the one alias.

`ec` is the game master's convenience client and does nothing `earl` cannot. Its
commands sit a level down (`ec app login`, `ec version`) because its surface is
expected to grow broad; new work goes in a group, not at the top level. A new
endpoint needs no `ec` change either unless a game master wants a name for it.

**Everything both clients share is `internal/apiclient`** — the transport,
`--base-url` and the other common flags (registered once by `apiclient.Bind`,
for the same reason as `bindDBPath`), the cookie rules, and the credential
store. Do not add a second copy in a `cmd/`. The package belongs to the clients:
nothing under `internal/server` may import it.

**One saved session, shared.** Both clients read and write
`~/.config/ecv8/{EC_ENV}/credentials.json`, so a login through either is a login
for both. That is why their flags share the `ECV8_` prefix rather than having
one each: sessions are keyed by base URL, so two prefixes would let the two
commands be pointed at different servers and stop sharing without saying so.
`ECV8_` is deliberately not the server's `EC_`.

`ECV8_` prefixes *flags* only. **The environment is `EC_ENV`, not `ECV8_ENV`** —
there is no `--env` flag, so nothing reads `ECV8_ENV`, and setting it selects
nothing while looking like it should. `EC_ENV` is read directly in `main`,
before flags parse, exactly as `ecapi` and `ecdb` read it: a checkout has one
idea of which environment it is in, and the clients do not get a second one.

The saved session cookie is a live credential. It is written `0600` and never
printed, on the same rule the server follows: never log a token, a cookie
value, a password, or a hash.

`cmd/ecapi/ecapi.service` is a sample systemd unit, documentation only. Nothing
builds, installs, or reads it, and deployment automation stays out of scope.

**`gofmt -l . && go build ./... && go vet ./... && go test ./...` are the
checks.** Run all four after any change and report the real output.

**Tests are welcome.** Add them where they earn their keep, without asking
first. `README.md` § Tests lists what is covered today; keep it current in the
same change that changes coverage.

What the existing suites have in common, and what a new one should match:

- **Standard library only.** No assertion package, no mocking framework, no
  fixtures loaded from disk. `t.Errorf` with "got X, want Y" is the house style.
- **Test the contract, not the implementation.** `cmd/ecdb/main_test.go` drives
  `run` with real arguments the way a shell does, because what is worth
  protecting is what the command prints, what it refuses, and what it leaves on
  disk. Reach past the surface only to inspect a result, never to arrange a
  state the surface could have arranged itself.
- **Every test builds its own database.** `t.TempDir()` for a persistent one,
  `store.OpenTemporaryStore` with a name derived from `t.Name()` for an
  in-memory one. Nothing is shared between tests, and `isolateEnv` clears every
  `EC_` variable before a command test runs.
- **Assert on what the schema guarantees.** A constraint that exists to make a
  rule true regardless of the code path is worth a test that tries the code path
  and expects failure.

Still off the table: `_test.go` placeholders, tests that assert nothing, and
reintroducing the generated test files that were removed. A new `ecdb`
subcommand is expected to bring a test with it.

**Go 1.26.4 is pinned exactly**, and `go.mod` declares it. If the local
toolchain disagrees, stop and report the mismatch — do not edit the `go` line,
add a `toolchain` directive, or lower a version to make a build succeed.

The local development database is `games/alpha/ecv8.db`, set by
`EC_DB_PATH=games/alpha` in `.env.development.local`. Both `games/` and `*.db`
are git-ignored, and so is `.env.development.local` — it holds a real admin
secret. Never commit it, never echo its contents into a message or a log.

## Do not write Echo v4 or `database/sql` from recall

Two dependencies here look familiar and are not.

**Echo v5.** `Context` is a struct, so handlers take `*echo.Context`, not the v4
interface. The error handler returns nothing. `log/slog` is built in and passed
through `echo.Config`. Route-not-found is `e.RouteNotFound`. None of the v4
idioms — `echo.Context` as an interface, `echo.New()` plus a `Logger` field,
`c.Bind`, `middleware.CSRF` — belong in this codebase. Check
`internal/server/server.go` and `middleware.go` for the shapes actually in use.

**ZombieZen SQLite.** It "is not a `database/sql` driver" and registers none, so
there is no `*sql.DB`, no `sql.Rows`, no `QueryContext`. Never import
`modernc.org/sqlite` directly — the project forbids it, and doing so would break
`sqlitemigration` and the pool's one-writer model. All database access goes
through `*store.DB`; see `internal/store/store.go`, which explains why.

**This is a deliberate deviation from the brief.** `../project-prompt.txt`
specifies that the three store constructors return `*sql.DB`. They return
`*store.DB` instead, for the reasons in the `internal/store` package doc, and
the deviation was accepted. Do not "fix" the signatures back to match the
prompt.

## Conventions this codebase already follows

Match them; do not introduce a second style alongside them.

- **Every file opens with** `// Copyright (c) 2026 Michael D Henderson. All
  rights reserved.` and every package, type, and exported function carries a
  comment explaining _why_ it exists, not what it does. New files should too.
  Long-form rationale lives in a package doc comment (see
  `internal/store/store.go`, `internal/password/password.go`).
- **Sentinel errors are untyped constants** of `cerrs.Error`, declared in the
  package that owns them (`store.ErrNotFound`, `password.ErrTooLong`). Wrap with
  `%w` and match with `errors.Is`.
- **The logger is injected.** It lives on the `Server` struct and is passed
  explicitly. There is no package-level logger and there must not be one.
- **JSON stays snake_case** — `display_name`, `is_active`, `active_sessions`.
  The Ember client reads these field names directly.
- **Timestamps go through `store.Now()`**, which truncates to the precision the
  storage format keeps, and through `formatTime`/`parseTime`. Do not call
  `time.Now()` in a handler or a query.
- Handlers are `func (s *Server) handleX(c *echo.Context) error`, grouped by
  area in `handlers_*.go`; request body types are declared next to the handler
  that uses them.

## Rules with teeth

**Every failure is a `*Problem`.** Handlers return `badRequest`, `unauthorized`,
`forbidden`, `notFound`, `conflict`, `unprocessable`, or a bare `error` (which
becomes a detail-free 500). `errorHandler` in `problem.go` is the single place
that decides what a client learns — do not write an error body from a handler.
Detail strings are written for an operator or an end user. **Never put a SQL
statement, a filesystem path, a hash, or a wrapped internal error into one.**

**Store failures go through `s.storeError(err, subject)`**, which maps
`ErrNotFound`, constraint violations, and `ErrReadOnly` onto the right status.
Returning a raw store error means a 500.

**Never serialise a `store` model.** Responses are built from the view types in
`views.go`, which is what stops a new database column leaking. Adding a field to
a response means adding it to a view.

**Decode with `s.bindJSON`.** It enforces `application/json`, bounds the body,
rejects unknown fields, and rejects trailing content. `c.Bind` does none of that
and is not used anywhere.

**The store never creates a directory.** `internal/store` takes a directory,
appends the fixed filename `ecv8.db`, and rejects an empty path, a missing path,
a non-directory, or one it cannot write to — before touching the filesystem. No
`os.MkdirAll` belongs anywhere in that package. An operator pointing a command
at the wrong directory must get a clear error, not a new database in a new
directory.

**Maintenance never migrates as a side effect.** `store.BackupPersistent` and
`store.CompactPersistent` refuse a database that is not at this binary's
migration level (`ErrMigrationMismatch`) instead of bringing it forward. An
operator asking for a copy must not be handed a schema change, and `VACUUM` must
not be the thing that changes a schema. Migrating is `MigratePersistent`'s job
and a separate decision. `internal/store/maintenance.go` says why at more length.

**`bcrypt.MinCost` is the specified cost, not an oversight.** The brief calls
for it exactly, and `internal/password` documents it. A cost of 4 reads like a
security defect, so the reflex is to raise it — don't. Changing it also
invalidates every stored hash, including the seeded development accounts. Raise
it only if the user asks, and say what it breaks.

**Secrets and game randomness come from different packages and never swap.**
`internal/tokens` (`crypto/rand`) mints session and activation tokens;
`internal/engine` (`math/rand/v2` + PCG) is for game randomness only. The legacy
`math/rand` must never be imported anywhere in this module.

**Only fingerprints are persisted.** A session token or activation token is
returned to the caller exactly once, in plaintext; the database stores
`tokens.Fingerprint(...)`. Never log a token, a cookie value, a password, or a
hash.

**Nothing changes state on `GET`, `HEAD`, or `OPTIONS`.** Cross-origin
protection always allows those methods, so a state-changing GET would be an
open CSRF hole.

**There is no deletion.** Accounts and games are deactivated (`is_active =
false`), never removed, so history and referential integrity survive. Do not add
a `DELETE` that removes a row — the existing `DELETE` endpoints only end
sessions or stop impersonation.

**Deactivation is not revocation, and admin rights stop at impersonation.**
Both are deliberate (`README.md` § Security notes). `requireAdmin` is the actual
authorisation boundary; the client's route guards are UX. `identity.Actor` is
who logged in, `identity.Effective` is who the request acts as — log the actor,
authorise on the rules in `identity`, and never assume they are the same.

## Adding an endpoint — the whole checklist

1. Handler in the matching `internal/server/handlers_*.go`, with a comment
   saying why it exists and what it refuses.
2. Request body type beside it; decode with `s.bindJSON`; validate into
   `[]FieldError` and return `unprocessable("…", fields...)`.
3. Store method in `internal/store/`, using `db.Read` or `db.Write` with named
   parameters. Never build SQL by concatenating a value.
4. Response type in `views.go` — no store model on the wire.
5. Register it in `s.routes()` under the right group so `requireAuth` or
   `requireAdmin` applies.
6. `gofmt -l . && go build ./... && go vet ./... && go test ./...`.
7. Update the endpoint table in `README.md` § HTTP API.

## The two domains

The schema is two domains in one SQLite file, and keeping them apart is a rule,
not an accident of naming.

- **Application** (`0001`): `account`, `account_activation`, `session`, `game`.
  Who may sign in, and what games exist.
- **Engine** (`0002`): `game_state`, `faction`, `entity`. What is happening
  inside a game.
- **The seam**: `game_player`, one row per seat at a game's table.

**`game_player.id` is the engine's `player_id`, and it is the only identity that
crosses.** No engine table references `account`, and the engine must never learn
that accounts exist — that is why `is_agent` is a column on the seat rather than
something derived from `account_id IS NULL`. `game_id` crosses too, because
every engine row has to be scoped to a game. Nothing else may.

The id is `AUTOINCREMENT` so SQLite never reuses it. That is load-bearing: the
value is written into engine state, and a reused one would silently reassign a
faction to whoever sat down next. Do not replace it with a composite key.

**Agents are seats, not accounts.** An agent has no `account_id` at all, so
"this bot cannot sign in" is a schema fact. Do not implement an agent as an
account with an unusable password hash — that makes the guarantee depend on
bcrypt's parser rejecting a malformed value, and needs a fabricated email and
activation timestamp to get past `0001`'s CHECKs. `account` means exactly "a
human who can sign in", and it should stay that way.

**The agent catalogue is code, not data.** `internal/engine/agents.go` holds an
explicit list of every agent this build can play, and `GET /admin/agents` serves
it. A seat stores `agent_key`; the schema checks that key's *format* and never
the set of valid keys, because that set changes with every release.

Do not add an `agent` table populated by a migration. Migrations here are
forward-only, so a rollback would leave the database offering an agent the
binary cannot play, and a foreign key onto such a table checks only that
somebody once asserted the code existed. `engine.AgentByKey` is the authority
and is asked at seat time; a seat naming a missing implementation is reported as
`playable: false` rather than hidden or fatal.

**Cross-domain rules are composite foreign keys, not Go.** `faction` carries a
redundant `player_is_gm`, and the seat carries a redundant `account_role`, so
that "a GM never controls a faction" and "an admin never holds a seat" are
checked by SQLite whatever code writes the row. When a new engine table needs a
rule like this, add the composite parent index and the redundant column; do not
settle for a check in a handler. `0002_engine.sql` explains each one.

Deleting is still not a thing. Engine tables use `ON DELETE RESTRICT` rather
than the `CASCADE` the application tables use, so a delete that should never
happen fails loudly instead of taking a game's state with it.

## Adding an agent

One commit, and no migration:

1. Write the implementation in `internal/engine`.
2. Add a `Descriptor` to the `agents` list in `agents.go`, next to it.
3. `gofmt -l . && go build ./... && go vet ./... && go test ./...`.

That is the whole procedure. `GET /admin/agents` picks it up, and a game master
can seat it immediately.

**A key that has shipped is permanent.** It is written onto seats and into a
game's state, so renaming one orphans every seat that referenced it — those
seats report `playable: false` and cannot be resolved. `Name` and `Description`
are display strings and may be reworded freely; `Key` may not. Withdrawing an
agent means removing it from the list and accepting that existing seats become
unplayable, which is visible in the listing and is the intended behaviour.

## Adding a migration

New file in `internal/store/migrations/`, next ordinal, zero-padded
(`0003_….sql`). It is embedded at build time and applied in filename order.

- **Never edit a migration that has been applied.** `user_version` is the count
  of migrations applied, so an edited file is silently skipped on any existing
  database.
- No `BEGIN`/`COMMIT` in the file — `sqlitemigration` wraps it and bumps
  `user_version` itself.
- There is no downgrade path. A database ahead of the binary is refused for
  writing.
- `PRAGMA application_id` (`0x65637638`) is the "this is an ECV8 database"
  marker and must never change. It is not the migration version.
- `PRAGMA foreign_keys` is ON when migrations run (`prepareConn`), and it cannot
  be changed inside a transaction, so a migration cannot turn it off. Order a
  table rebuild so that nothing references the table being dropped.

**Alpha: the database is disposable.** Dropping and rebuilding is expected, and
`earl` scripts repopulate. That is a licence to change the shape freely in a new
migration — it is *not* a licence to edit an applied one, because a developer
whose database is at the older `user_version` would silently skip the edit and
end up with a schema nobody else has.

## Adding a configuration option

Four places, all of them: the field and its flag in `internal/config/config.go`
(`Bind`), any cross-field rule in `Validate` — the error must name the flag that
is wrong — the table in `README.md` § Configuration, and a commented entry in
`.env.example`. Flags map to `EC_`-prefixed environment variables
automatically; dotenv files populate the environment before flags parse, so they
can never override a flag.

An option `ecdb` also needs goes on `config.Database` in
`internal/config/database.go` too, and in the flag table in `cmd/ecdb/README.md`
§ Flags and environment. Anything both binaries bind is registered by one helper
— see `bindDBPath` — so the two can never describe the same flag differently.

## Adding an `ecdb` subcommand

Under `database`, unless it does not touch the database file at all. Wrap the
`Exec` in `databaseExec` so it rejects positional arguments and validates
`--db-path` the same way every other one does. Storage work goes in
`internal/store`, never inline in the command. Then document it in
`cmd/ecdb/README.md` — that file is the command's reference, and `README.md` in
this directory only points at it — and add a test to `cmd/ecdb/main_test.go`
that drives `run` with real arguments.

## Seeing a change in a browser

`air` in this directory plus `pnpm start` in `../app`, both behind the Caddy
proxy at **https://ecv8.localhost:8443** (`README.md` § Development). A
`Procfile.dev` one directory up runs the pair. Hitting `localhost:3000` directly
from a browser bypasses the proxy: the request is cross-origin, so the session
cookie is not sent and cross-origin protection rejects anything unsafe.

`EC_PUBLIC_BASE_URL` must match that origin **including the port** — activation
links are built from it, and a mismatch produces links that look right and lead
nowhere.

`--memory dev` serves a throwaway in-memory database seeded with
`admin@example.com/admin`, `gm1@example.com/gm1`, `user1@example.com/user1`,
`user2@example.com/user2`. It is a development convenience and must never reach
production.

## Out of scope

The brief rules these out, and "no" is the finished answer, not a gap to fill:

- CI configuration, Dockerfiles, Docker Compose, deployment automation
- nginx configuration (production TLS is an operator concern, documented only)
- backup *scheduling* and retention — `ecdb database backup` takes a backup when
  it is asked to. When to ask is cron's job and an operator's policy, and
  neither belongs in this binary. Same for `compact`.
- email delivery: the application never sends mail. An activation URL is
  returned once for an administrator to deliver out of band.
- public web-based registration: administrators create every account

**Gameplay is no longer on this list.** The brief ruled it out and that has been
superseded: engine development started on 2026-07-25 with migration `0002`. Do
not reinstate "gameplay is out of scope" from the brief or from an older
comment — see § The two domains.

**Do not add speculative abstractions for any of these** — no hook, no
interface, no config flag held open for a feature that is not being built. If a
change seems to need one, say so and stop rather than building the scaffolding.
