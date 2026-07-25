# CLAUDE.md — working in `api/`

The Go HTTP API for ECV8. `README.md` in this directory explains _what_ the
service is, how it is configured, and why it makes the choices it does; read it
before changing anything structural. This file is the shorter list of _how to
work here_ — the conventions, the checks, and the traps.

## Orientation

| Question                          | Where the answer is                    |
| --------------------------------- | -------------------------------------- |
| What the service does, why        | `README.md` (this directory)           |
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
go vet ./...          # these three are the checks — run all of them
air                   # live reload; config in .air.toml, binaries in tmp/
go run ./cmd/ecapi serve --memory dev       # seeded in-memory database
go run ./cmd/ecdb verify --db-path games/alpha
```

Four binaries are built from this module. `cmd/ecdb` owns the database file —
`create`, `verify`, and whatever storage-only work comes later — and takes only
`--db-path`. `cmd/ecapi` serves HTTP and never creates a database. `cmd/earl`
is a client. `cmd/ecv8-api` is the original binary that did both; it is
**unchanged and will be removed whole** when the split finishes, so add nothing
to it and do not trim it piecemeal. `air` builds `ecapi`.

**`earl` speaks HTTP and only HTTP.** It must never import `internal/store`,
never open a database, and never implement a rule the server owns — it sends a
request and prints what comes back. Its value is that it is not privileged: if
`earl` can do something, a real client can. A convenience that shortcuts the API
would quietly destroy that. The REST surface is its command line
(`earl get /admin/accounts`), so a new endpoint needs no `earl` change; only the
commands that touch the saved session — `login`, `logout`, `identities` — are
special, plus `whoami` as the one alias.

The saved session cookie is a live credential. `earl` writes it `0600` and never
prints it, on the same rule the server follows: never log a token, a cookie
value, a password, or a hash.

`cmd/ecapi/ecapi.service` is a sample systemd unit, documentation only. Nothing
builds, installs, or reads it, and deployment automation stays out of scope.

**There are no tests, by design** (`README.md` § Tests). `gofmt -l . && go build
./... && go vet ./...` are the checks. Run all three after any change and report
the real output. Do not add `_test.go` placeholders or reintroduce the generated
test files that were deliberately removed; if the user asks for real tests, add
them and update `README.md` § Tests in the same change.

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
6. `gofmt -l . && go build ./... && go vet ./...`.
7. Update the endpoint table in `README.md` § HTTP API.

## Adding a migration

New file in `internal/store/migrations/`, next ordinal, zero-padded
(`0002_….sql`). It is embedded at build time and applied in filename order.

- **Never edit a migration that has been applied.** `user_version` is the count
  of migrations applied, so an edited file is silently skipped on any existing
  database.
- No `BEGIN`/`COMMIT` in the file — `sqlitemigration` wraps it and bumps
  `user_version` itself.
- There is no downgrade path. A database ahead of the binary is refused for
  writing.
- `PRAGMA application_id` (`0x65637638`) is the "this is an ECV8 database"
  marker and must never change. It is not the migration version.

## Adding a configuration option

Four places, all of them: the field and its flag in `internal/config/config.go`
(`Bind`), any cross-field rule in `Validate` — the error must name the flag that
is wrong — the table in `README.md` § Configuration, and a commented entry in
`.env.example`. Flags map to `EC_`-prefixed environment variables
automatically; dotenv files populate the environment before flags parse, so they
can never override a flag.

An option `ecdb` also needs goes on `config.Database` in
`internal/config/database.go` too. Anything both binaries bind is registered by
one helper — see `bindDBPath` — so the two can never describe the same flag
differently.

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
- backup automation — `README.md` § Backups explains the manual procedure and
  why it stays manual
- a separate database-upgrade utility — `serve` migrates on open, and that is
  what keeps the binary and schema in step
- email delivery: the application never sends mail. An activation URL is
  returned once for an administrator to deliver out of band.
- public web-based registration: administrators create every account
- gameplay. `internal/engine` exists only to fix the PRNG invariant; it invents
  no game rules.

**Do not add speculative abstractions for any of these** — no hook, no
interface, no config flag held open for a feature that is not being built. If a
change seems to need one, say so and stop rather than building the scaffolding.
