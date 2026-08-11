# nuzur-cli

nuzur cli tool

## Install

**macOS & Linux (and WSL)** — the one-liner. It resolves the latest release,
verifies its sha256 checksum against the release's own checksums file, and
installs `nuzur-cli` plus the `nuzur` alias. No sudo: it installs into
`~/.local/bin` (or `/usr/local/bin` when that is already writable).

```bash
curl -fsSL https://nuzur.com/install.sh | sh
```

Pin a version, or choose the directory — note the environment goes on the `sh`
side of the pipe, since that is the process reading it:

```bash
curl -fsSL https://nuzur.com/install.sh | NUZUR_VERSION=v1.6.1 sh
curl -fsSL https://nuzur.com/install.sh | NUZUR_INSTALL_DIR=$HOME/bin sh
```

**Windows** — Scoop (native Windows is not covered by the one-liner; inside WSL,
use the Linux instructions above):

```powershell
scoop bucket add nuzur https://github.com/nuzur/scoop-bucket
scoop install nuzur-cli
```

**Homebrew** (macOS/Linux):

```bash
brew install nuzur/tap/nuzur-cli
```

Signed archives for every platform, and everything else, are at
<https://nuzur.com/cli>.

## Keep it current

```bash
nuzur-cli update           # upgrade in place
nuzur-cli update --check   # just report whether a newer release exists
```

Worth doing before any deploy. Parts of the pipeline are resolved server-side at
run time — the SQL generator that renders your DDL is fetched at its latest
published version on every deploy — so an old binary quietly pairs old client
behavior with a current server, and the things that live *in* the binary (newer
flags, the `--plan` drift check, bug fixes) are simply absent rather than
reported as missing. A deploy now prints a one-line notice when a newer release
exists; the check is best-effort and never delays or fails the deploy.

`update` will not overwrite a binary Homebrew or Scoop owns — it prints that
manager's upgrade command instead, since replacing the file behind the manager's
back leaves its metadata wrong and the next `brew upgrade` reverts you. Downloads
are checksum-verified against the release's own manifest, exactly as `install.sh`
does, and the binary is swapped atomically: a failed update leaves the old one in
place.

## See what a deploy will do before it does it

`nuzur-cli deploy` is declarative: it reconciles your database to the published
model. That is the whole value of it — describe the schema once and the database
catches up by itself — and the other face of it is that anything in the database
which the model does not describe is, by definition, surplus.

So before deploying against a database that holds real data:

```bash
nuzur-cli deploy --plan --deployment <id>      # `deploy list` shows the ids
```

It prints the exact SQL the deploy would run, flags every statement that deletes
data, and exits having changed nothing — no server provisioned, no code generated,
nothing written to the box or to nuzur. Add `--json` for a machine-readable plan.

If the plan wants to drop tables or columns you still need, that is drift: the
database moved and the model didn't. Add them to the schema in nuzur and plan again.
A deploy will not delete data on its own — a migration that does is refused, and
applies nothing at all, until you pass `--allow-destructive`.

`--plan` also accepts a draft version, so you can check a reconciling fix before
sending it for review. Deploy itself still requires an approved or published one.

### `--plan` is also the drift check

"Does the deployed database still match this published version?" is the same
question, so there is no separate `schema diff` command — ask it with:

```bash
nuzur-cli deploy --plan --version <identifier-or-uuid> --connection <uuid>
```

Read the output as a drift report: an empty plan means no drift, `CREATE`/`ADD`
means the database is behind the model, and `DROP` means the database holds
something the model doesn't. Target it with `--deployment <id>`, `--connection
<uuid>`, `--host`, or `--local-agent`/`--local-agent-connection` on a machine
with no record of the box.

Don't write your own drift checker that regenerates its "expected" schema from
the live database — that compares live against a copy of live and can never
detect a model-vs-database mismatch, which is the mismatch that matters.

**On MySQL, expect noise.** nuzur cannot read a MySQL schema directly, so the
"existing" side of the diff is reconstructed by introspecting the database and
re-rendering it as DDL. Widths and types come back normalized, so a MySQL plan
often carries `MODIFY`/`CHANGE COLUMN` statements that change nothing and
reappear every time. The `CREATE`s and `DROP`s are real; treat a bare column
redefinition as suspect.

## Database-only deploys (`--db-only`)

`--db-only` gives you a nuzur-managed database and no application: it installs
the engine, pairs the agent, registers the connection and applies the schema, but
generates no API and no app. Right when you already run your own service against
the database and just want nuzur to own the schema.

What that costs is easy to miss, because several guarantees people read as
properties of *the model* are actually enforced by the **generated API layer**,
which `--db-only` never deploys. Not deployed, therefore not enforced:

- **`generated: true` timestamp population.** `created_at`/`updated_at` are
  filled in by the generated server. With your own writer, `updated_at` holds
  whatever that writer put there — for many people that means "loaded at", not
  "modified at".
- **The `version` optimistic-concurrency token.** Nothing increments it and
  nothing rejects a stale write.
- **Model-level validation** — `min_size`, `max_size`, `regex_validation`,
  `min_value`, `max_value`. These are checks the generated code performs, as
  distinct from the constraints that live in the DDL.

The database still enforces everything in the schema itself: column types, NOT
NULL, defaults, and unique/foreign-key/index constraints. A normal deploy (the
same command without `--db-only`) reuses the database, agent, schema and data and
adds the API.

## Connect a database on a server (headless)

To manage an existing database from nuzur, run the CLI on the machine that can
reach it. Servers usually can't open a browser, so pairing uses a token you copy
from the web app instead of an interactive login:

```bash
# on the server
nuzur-cli --version        # install from https://nuzur.com/cli
nuzur-cli connect
```

`connect` prints `https://app.nuzur.com/pair`. Open that on your own computer,
click **Pair a server**, copy the token, and paste it back at the prompt. The
CLI then asks for the database details, publishes the connection, and installs
the agent as a service. Afterwards the database appears in the web app under
**Via agent** — including in **Extensions → SQL Import**, which imports the
existing schema into a nuzur project.

The pairing token is single-use and expires after 15 minutes; if it fails, mint
a fresh one from the same page.

For scripted setups, pass everything up front:

```bash
nuzur-cli connect --non-interactive \
  --provisioning-token "$NUZUR_PROVISIONING_TOKEN" \
  --name prod-db --driver postgres \
  --dsn "host=localhost port=5432 user=app password=... dbname=app sslmode=disable"
```

### What ends up where

- **The DSN never leaves the machine.** It is stored locally (in your OS
  keychain where available); nuzur only receives the connection's name, type and
  default schema.
- Agent credentials live in the CLI's config directory (`~/.config/nuzur` on
  Linux), readable only by the user that paired the machine. nuzur stores only a
  hash of the token.
- Queries and imports reach the database only while the agent is running, over a
  connection the agent dials out — nothing is exposed to the internet.

### Keeping the agent running

The installed unit is a *user* service, which on Linux stops when the login
session ends. To keep it running after you log out:

```bash
loginctl enable-linger $USER
```

### If the agent is revoked

Revoking an agent from the web app invalidates its credentials, so publishing
fails with a message saying so. Pair the machine again with a new token:

```bash
nuzur-cli agent pair --force     # prompts for a fresh token on a headless box
```
