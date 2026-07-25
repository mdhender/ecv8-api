# `ecdb`

`ecdb` creates and maintains the SQLite database used by ECV8. It is the
database quarter of a four-way split: `ecdb` owns the file on disk, `ecapi`
serves HTTP and never creates a database, and `earl` and `ec` are clients that
never touch either. Keeping them apart means the long-running service can be
installed and confined without carrying the ability to create a database or seed
an administrator.

```text
ecdb [FLAGS] <SUBCOMMAND>
ecdb database <SUBCOMMAND> [FLAGS]
```

Every operation on the file lives under `database`, so what is a database
operation and what is not stays obvious. `ecdb --help` and `ecdb database --help`
list the flags in scope with their defaults.

The database is always named **`ecv8.db`**. Every path argument names the
*directory* that contains it; the filename is fixed and is never configurable.
An operator can point a command at the wrong directory and get a clear error,
but cannot accidentally create a second database under a different name.
**No command ever creates a directory.**

---

## Contents

- [Flags and environment](#flags-and-environment)
- [Creating a database](#creating-a-database)
- [Inspecting a database](#inspecting-a-database)
- [Upgrading a database](#upgrading-a-database)
- [Backups](#backups)
- [Compaction](#compaction)
- [Build version](#build-version)
- [Output and exit status](#output-and-exit-status)

---

## Flags and environment

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--db-path DIR` | `EC_DB_PATH` | `db` | Directory holding `ecv8.db`. Must exist. |
| `--quiet` | `EC_QUIET` | `false` | Suppress status lines. Values are still printed. |
| `--output-path DIR` | `EC_OUTPUT_PATH` | `--db-path` | `database backup` only. Where to write the backup. |
| `--version` | `EC_VERSION` | `false` | `database backup` only. Append the migration number to the filename. |

A flag beats an `EC_`-prefixed environment variable, which beats the default.
Dotenv files are loaded into the environment before flags are parsed, so they can
supply a variable but never override one; `EC_ENV` selects which files load and
must be exported rather than set in one. See `../../README.md` § Configuration.

Two flags are worth reading twice:

- `--version` belongs to `database backup` alone and is fed by `EC_VERSION`. It
  has nothing to do with either `version` command.
- `--quiet` suppresses status lines only — `database upgrade` reporting what it
  applied. A value a command was *asked* for, such as a path or a migration
  number, is always printed, because a script capturing one must never read an
  empty line as success.

`database create` also reads `EC_ADMIN_EMAIL` and `EC_ADMIN_SECRET`. No other
command reads them, and no other operation can seed an administrator.

---

## Creating a database

```text
ecdb database create [--db-path DIR]
```

Creates `ecv8.db`, applies every migration, seeds the optional initial
administrator, and prints the path it created. The directory must already
exist.

```sh
mkdir -p games/alpha
EC_ADMIN_EMAIL=admin@example.com EC_ADMIN_SECRET='choose-a-good-secret' \
    ecdb database create --db-path games/alpha
# games/alpha/ecv8.db
```

The path is the useful output: it is what an operator points the server at, and
it proves which directory was actually used.

It **fails if `ecv8.db` already exists**. An existing database is never
truncated, replaced, or reopened as if it were new. If initialisation fails
partway, the file it just created is removed along with its `-wal` and `-shm`
sidecars; a pre-existing file is never removed, because creation never reaches
initialisation when one exists.

### The initial administrator

`EC_ADMIN_EMAIL` and `EC_ADMIN_SECRET` are validated *before* the file is
created:

| Both unset or blank | The database is created with no administrator. |
|---------------------|------------------------------------------------|
| Exactly one set     | **Error.** Nothing is created. |
| Both set            | Validated, then one active administrator is created during initialisation. |

Opening an existing database ignores both variables entirely, so they cannot be
used to add or alter an administrator later. After creation, administrators
create other accounts through the API. Keep the secret in a `.local` dotenv file
or the command's environment, never in a tracked file.

---

## Inspecting a database

```text
ecdb database verify  [--db-path DIR]
ecdb database version [--db-path DIR]
```

Both open `ecv8.db` **read-only**: no migration runs and the database is never
written, so inspecting one is safe against a running server and a database newer
than this binary is perfectly acceptable — an older build can still read it.

`verify` reports the migration level as a sentence, for a person:

```sh
ecdb database verify --db-path games/alpha
# games/alpha/ecv8.db: migration 1 of 1
```

`version` prints the migration number alone, for a script:

```sh
ecdb database version --db-path games/alpha
# 1
```

Both refuse a SQLite file that is not an ECV8 database. `PRAGMA application_id`
is the authoritative marker, `0x65637638` — the four ASCII bytes of `ecv8`. It is
**not** the migration version, which lives in `PRAGMA user_version`.

Neither command answers the question "is this database ready for the new
binary?" on its own; compare what they report against the migration count the
binary knows, which `verify` prints as the second number.

---

## Upgrading a database

```text
ecdb database upgrade [--db-path DIR]
```

Applies any migrations the database is missing, and says whether it applied any.

```sh
ecdb database upgrade --db-path games/alpha
# games/alpha/ecv8.db: migrations applied (migration 2)

ecdb database upgrade --db-path games/alpha
# games/alpha/ecv8.db: no migrations applied (migration 2)
```

`ecapi serve` migrates on open, so nothing *has* to be run before a deploy. This
command exists for the operator: without it, the only way to migrate a schema is
to launch the server, which means the upgrade happens at the moment of least
control — during a restart, against a live listener, reported only in a startup
log. `upgrade` lets it happen deliberately: at a chosen moment, before the new
binary starts, or against a restored backup, with the result on stdout.

Saying so either way is the point. "No migrations applied" is the answer an
operator needs, and it should not have to be inferred from silence. `--quiet`
suppresses both lines when a script only wants the exit status.

A database **ahead** of this binary is refused without being modified. There is
no downgrade path: deploy the newer binary, or restore an older backup.

---

## Backups

```text
ecdb database backup [--db-path DIR] [--output-path DIR] [--version]
```

Writes a consistent, compacted copy and prints its path.

```sh
mkdir -p /var/backups/ecv8
ecdb database backup --db-path /var/lib/ecv8 --output-path /var/backups/ecv8 --version
# /var/backups/ecv8/ecv8.db.20260725T142530Z-1
```

The name carries a UTC timestamp; `--version` appends the migration number.
`--output-path` defaults to `--db-path`, and both directories must already
exist.

- The command uses SQLite's `VACUUM INTO`, which reads **one committed
  snapshot** and writes a single self-contained file with no sidecars. It is
  safe to run against a live server.
- Doing it by hand is why that matters: the database runs in **WAL** mode, so
  `ecv8.db` alone is not a complete copy, and `cp` on a live database can capture
  a torn state. Copying `ecv8.db`, `ecv8.db-wal`, and `ecv8.db-shm` together
  works, but only all three together and only with the service stopped.
- The source is opened read-only and is never modified.
- The output file is created `0600`, and an **existing file is never
  overwritten**. If the copy fails partway, the part-written file is removed
  rather than left looking like a backup.
- A backup holds bcrypt password hashes, activation-token hashes, and live
  session fingerprints. Treat it as a secret: an attacker with one can reissue
  nothing, but can attempt offline password cracking.

Restoring is a file copy — put the backup where `ecv8.db` goes. Restoring an
older backup does not invalidate sessions issued after it was taken; those
simply cease to exist, so their holders are signed out.

Taking a backup is a command. Deciding *when* to take one is not: there is no
scheduler and no retention policy here, because that is cron's job and an
operator's policy.

---

## Compaction

```text
ecdb database compact [--db-path DIR]
```

Reclaims unused space inside the live file with `VACUUM`, and is silent on
success. It rewrites the whole database and holds an exclusive lock while doing
so, needing room for a second copy, so it is for an idle database after a large
deactivation — not something to run on a schedule.

### Neither one migrates

`backup` and `compact` both **refuse a database that is not at this binary's
migration level** rather than bringing it forward first:

```sh
ecdb database compact --db-path games/alpha
# ecdb: games/alpha/ecv8.db: database is not at this binary's migration level:
#       database is at migration 1, binary knows 2
```

An operator asking for a copy must not be handed a schema change as a side
effect, and `VACUUM` must never be the thing that changes a schema. A refusal
leaves the file exactly as it was found, down to its journal mode. Bringing a
database forward is `upgrade`'s job, and it is a separate decision.

---

## Build version

```text
ecdb version
```

Prints the version of the binary. It opens nothing, needs no `--db-path`, and is
the same version `ecapi` reports — one module, one build, and the migrations both
know about are compiled from the same source.

The build version and the database's migration number are unrelated, and neither
answers the other's question. **Do not use the build version to decide whether a
database needs upgrading**; use `database verify` or `database version`.

---

## Output and exit status

- `create` prints the path it created. `backup` prints the path it wrote.
  `database version` prints a number. `version` prints the build version.
  `verify` and `upgrade` print a status line. `compact` is silent.
- `--quiet` suppresses status lines only, never a value that was asked for.
- Failures write `ecdb: <message>` to standard error and exit non-zero. The
  message names the file or the flag at fault; it never contains a SQL
  statement, a hash, or a secret.
- `--help` prints usage and exits zero, at every level of the tree. Naming a
  group without a subcommand — `ecdb`, or `ecdb database` — prints that group's
  help and exits non-zero, because it is not a request to do anything.
