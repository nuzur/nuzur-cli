# Running extensions non-interactively (AI agents & scripts)

The `run-extension` and `go-code-gen` commands support a fully non-interactive
mode designed for AI agents, CI, and any automation that can't answer prompts.
The flow is **describe → run**:

1. **`describe`** returns a machine-readable JSON schema of the config an
   extension needs — including the concrete allowed values for uuid/enum fields,
   so the caller never has to guess an entity/connection/store UUID.
2. **`run`** accepts the whole config as JSON in one shot, validates it against
   that schema, runs the extension, and (with `--json`) reports a structured
   result.

The JSON shapes below are a **stable contract**: they are intended to be
consumed directly by an MCP tool wrapping this CLI. Treat changes to field names
as breaking. The Go types backing them live in
[`extensionrun/config_schema.go`](../extensionrun/config_schema.go),
[`extensionrun/config_apply.go`](../extensionrun/config_apply.go), and
[`extensionrun/run.go`](../extensionrun/run.go).

## 1. Discover the config: `describe`

```bash
nuzur-cli go-code-gen describe --project my-project --version v3
# or, for any extension:
nuzur-cli run-extension describe --project my-project --version v3 --extension go-code-gen
```

`--project` accepts a project name or UUID; `--version` a version identifier or
UUID. Output (stdout) is JSON:

```jsonc
{
  "extension": { "identifier": "go-code-gen", "display_name": "Go Code Gen", "version": "1.4.0", "version_uuid": "…" },
  "project":         { "uuid": "…", "identifier": "my-project" },
  "project_version": { "uuid": "…", "identifier": "v3" },
  "fields": [
    { "identifier": "module_name", "type": "string",  "required": true },
    { "identifier": "port",        "type": "integer", "required": false },
    { "identifier": "root_entity", "type": "uuid",    "required": true,
      "options": [
        { "value": "6f…uuid", "label": "user" },
        { "value": "9a…uuid", "label": "order" }
      ] },
    { "identifier": "features", "type": "enum", "required": false, "multiple": true,
      "options": [ { "value": "rest" }, { "value": "grpc" }, { "value": "graphql" } ] }
  ],
  "last_used_config": { "module_name": "acme", "root_entity": "6f…uuid" }
}
```

Field schema semantics:

| key           | meaning |
|---------------|---------|
| `type`        | one of `string`, `integer`, `float`, `boolean`, `uuid`, `enum`, `date`, `datetime` |
| `required`    | whether the field must be present |
| `multiple`    | `true` for arrays / multi-select enums — supply a JSON array |
| `options`     | for `uuid`/`enum`: the **only** accepted values. Put `option.value` in the config; `option.label` is a human name |

If `options` is absent for a uuid field, the allowed set couldn't be enumerated
and any string is accepted.

### Paired extensions: `sql-push` and `sql-import`

Two capabilities ship as a pair of backend extensions that do the same job over
a different connection path:

| what you ask for | direct connection | via a local agent |
|------------------|-------------------|-------------------|
| `sql-push`   | `sql-push` — `store`, `connection`, `schema` | `sql-push-local` — `local_agent`, `local_agent_connection`, `local_agent_schema` |
| `sql-import` | `sql-import` — same three, plus `mode` and `infer_weak_relationships` | `sql-import-local` — same three, plus `mode` and `infer_weak_relationships` |

Both are addressed as one extension (`--extension sql-push`); the connection
mode selects the member that actually runs:

- **`--connection-mode remote`** (aliases `direct`) — nuzur connects to a team
  connection over the network. This is the **default** for `describe` and for
  non-interactive runs with no other signal.
- **`--connection-mode local`** (aliases `agent`) — the run goes through a local
  agent running next to the database. Requires an **online** agent
  (`nuzur-cli agent start`); `local_agent`/`local_agent_connection` options list
  only online agents you own.

When the flag is absent, the mode is inferred from the config you supply: a
config carrying `local_agent` runs the agent-side member, one carrying `store` or
`connection` runs the direct member. Naming a member outright still works
(`--extension sql-push-local`) and implies local mode; combining it with
`--connection-mode remote` is an error rather than a silent choice.

```bash
# describe the agent-side variant
nuzur-cli run-extension describe --project my-project --version v3 \
  --extension sql-push --connection-mode local

# run it (mode inferred from the config's fields)
nuzur-cli run-extension --project my-project --version v3 --extension sql-push \
  --config '{"local_agent":"…","local_agent_connection":"…","local_agent_schema":"public"}' \
  --confirm-steps --json
```

Interactively, picking `sql-push` or `sql-import` asks for the connection mode,
starting on whichever member you ran last for that project version.

Last-used configs are stored per **executed** member, so the direct and agent
configs of a pair are remembered separately and neither overwrites the other.
The web editor reads the same records, so a mode chosen in one client is the
default in the other.

## 2. Run with a full config

```bash
nuzur-cli go-code-gen \
  --project my-project --version v3 \
  --config '{"module_name":"acme","root_entity":"6f…uuid","features":["rest"]}' \
  --output ./generated \
  --json
```

Config input (pick one):

- `--config '<json>'` — inline JSON object
- `--config -` — read the JSON object from **stdin**
- `--config-file path.json` — read it from a file

Behavior:

- **Partial configs are merged over `last_used_config`**, so you can override a
  single field without re-specifying everything.
- Supplying any of `--config` / `--config-file` / `--json` (or passing
  `--non-interactive`) turns off all prompts. Missing `--project`, `--version`,
  or (for `run-extension`) `--extension` then becomes an error instead of a prompt.
- The config is **validated before the extension is called**: required fields,
  type coercion (JSON numbers → strings, `"true"` → bool), and uuid/enum
  membership. All problems are reported at once.
- `--output` defaults to `.` in non-interactive mode.

### Success result (`--json`)

Printed to stdout:

```jsonc
{
  "status": "succeeded",
  "execution_uuid": "…",
  "output_path": "./generated",
  "files_written": ["cmd/main.go", "internal/store/user.go"],
  "files_removed": ["internal/store/legacy.go"]
}
```

### Error result (`--json`)

Any failure prints an error envelope to stdout and exits non-zero:

```jsonc
{
  "status": "error",
  "message": "invalid config",
  "errors": [
    { "field": "root_entity", "message": "value \"xyz\" is not one of the allowed options: 6f…uuid, 9a…uuid" },
    { "field": "module_name", "message": "required field is missing" }
  ]
}
```

`errors` is populated only for config-validation failures; other failures carry
just `message`.

## Remote agents: the `nuzur_cc` MCP `describeExtensionConfig` tool

The [`nuzur_cc`](https://github.com/nuzur/nuzur-go/tree/main/ccmcp) MCP server
(used by claude.ai / Claude Desktop, where there's no local shell) exposes a
**`describeExtensionConfig`** tool that returns the *same* schema shape as
`nuzur-cli … describe`, for every extension type. It cannot run the extension —
code generation writes to the local filesystem, which a remote server can't
reach, and importer/synchronizer runs need interactive confirmation — so the
split is:

- **Remote (`nuzur_cc`)** assembles the config schema from backend data it
  already serves (entities, connections, stores, enum options).
- **Local (`nuzur-cli`)** takes that config and does the actual run + file write.

### Tool: `describeExtensionConfig`

Params:

| param | required | meaning |
|---|---|---|
| `projectUuid` | yes | the project to run against |
| `projectVersionUuid` | yes | the version whose entities become the uuid options |
| `extensionIdentifier` | yes | e.g. `"go-code-gen"`. Any extension type; for paired extensions name the member you want (`"sql-push"` or `"sql-push-local"`) — the remote tool has no connection-mode parameter |

Result: the `ConfigSchema` documented above (`extension`, `project`,
`project_version`, `fields`, `last_used_config`) **plus** an `execution` block
that tells the agent how to run it locally, since the remote server can't:

```jsonc
{
  "extension": { "identifier": "go-code-gen", … },
  "fields": [ … ],           // identical shape to `nuzur-cli describe`
  "last_used_config": { … },
  "execution": {
    "runner": "nuzur-cli",
    "note": "This server cannot run the extension — generation writes files to your local machine…",
    "install": "Install the nuzur CLI from https://nuzur.com/cli …, then `nuzur-cli login`.",
    "describe_command": "nuzur-cli run-extension describe --project … --version … --extension go-code-gen",
    "run_command": "nuzur-cli run-extension --project … --version … --extension go-code-gen --config '<json>' --output ./out --json"
  }
}
```

A remote agent's flow: call `describeExtensionConfig` → build the config against
`fields` → hand it to the user's local `nuzur-cli` (the `execution.run_command`)
to execute. If the CLI isn't installed, the agent should surface `execution.install`
to the user.

## Output streams & exit codes

- **stdout** carries only the JSON document (schema, result, or error envelope)
  in `--json`/`describe` mode — safe to pipe into a JSON parser.
- **stderr** carries all human progress/status/warnings.
- Exit code is `0` on success, non-zero on any failure.

## Previewing a deploy: `deploy --plan --json`

`nuzur-cli deploy` is declarative — it reconciles the database to the published
model, so anything the database has and the model does not is surplus and gets
dropped. `--plan` is how an agent finds out which of those two situations it is in
before doing anything:

```bash
nuzur-cli deploy --plan --json --deployment <id> --project <p> --version <v>
```

It provisions nothing, generates nothing, mints no token, and writes nothing to the
box or to nuzur. Target it with `--deployment <id>` (most reliable — the recorded
deployment carries the agent, connection and engine), else `--host` + the derived
identifier, else `--connection <uuid>`; if this machine has no record of the box,
pass `--local-agent <uuid> --local-agent-connection <uuid>`.

Unlike `deploy`, `--plan` accepts a DRAFT version — previewing a not-yet-approved
fix against a drifted database is the point. Every plan reports its version's review
status, and `deploy` still refuses anything unapproved.

### Plan result (`--json`)

```json
{
  "status": "plan",
  "mode": "diff",
  "project":         { "uuid": "…", "name": "acme" },
  "project_version": { "uuid": "…", "identifier": "v_8", "review_status": "DRAFT", "approved": false },
  "target": {
    "source": "deployment acme-3f2a1c", "deployment_id": "acme-3f2a1c",
    "mode": "local", "engine": "postgres", "schema": "public",
    "local_agent_uuid": "…", "local_agent_connection_uuid": "…"
  },
  "changes": true,
  "destructive": true,
  "counts": { "total": 12, "additive": 8, "data_loss": 2, "constraint_loss": 1, "narrowing": 1 },
  "statements": [
    { "index": 4, "sql": "ALTER TABLE \"public\".\"orders\" DROP COLUMN \"legacy_ref\"",
      "kind": "drop_column", "severity": "data_loss", "object": "public.orders.legacy_ref",
      "reason": "drops legacy_ref from public.orders and every value in it" }
  ],
  "apply_sql": "…the exact string the extension would execute…",
  "transactional": false,
  "caveats": ["mysql_phantom_churn"],
  "applied": false,
  "rerun_command": "nuzur-cli deploy --host prod --allow-destructive"
}
```

- `apply_sql` is the load-bearing field — reason about the migration from that, not
  by re-deriving it from `statements`.
- `destructive` is the decision field. `severity` is one of `data_loss`,
  `constraint_loss`, `narrowing`, or absent for additive statements. Only
  `data_loss` blocks a deploy.
- `mode` is `"diff"` against a live database, or `"create"` when there is no
  database yet and this is the script a first deploy would run.
- `applied` is always `false`. `transactional` is always `false` — the statements
  run one at a time, so a failure partway through leaves the earlier ones applied.
- `--plan` exits `0` whether or not there are changes; read `changes` and
  `destructive` rather than the exit code.

### The gate: `--allow-destructive`

A real deploy whose migration deletes data applies **nothing** (the migration goes
to the database as one unit) and exits **non-zero**. `--allow-destructive`
authorizes it, and is flag-only — it cannot come from a `--deploy-config` file.

Do not add that flag and retry. It is the one deploy failure where the suggested
flag is not a missing argument but a question about deleting the user's data. Show
them the `data_loss` statements and get an explicit yes. A plan that wants to drop
tables usually means the **model** is behind — the fix is normally to add the
missing entities/fields to the schema, not to authorize the drop.

### What a plan cannot tell you

- Which statements take an exclusive lock (i.e. take a table offline), or how long
  an index build will take. The differ computes that metadata; it is discarded
  before the CLI sees it.
- How many rows a `DROP TABLE` destroys. Nothing counts them.
- Whether an `ALTER` will actually succeed. Adding a foreign key against orphan
  rows, `SET NOT NULL` against nulls, or a unique index against duplicates are
  flagged `narrowing` ("may fail"), never resolved.
- On MySQL, which statements are real. nuzur cannot read a MySQL schema directly,
  so the "existing" side is reconstructed and re-rendered, and normalized column
  widths/types produce `MODIFY`/`CHANGE COLUMN` statements that change nothing and
  reappear every deploy. That is what `caveats: ["mysql_phantom_churn"]` marks. The
  `CREATE`s and `DROP`s are real.

Anything unrecognized is reported as `kind: "other"` with no severity — never read
"not flagged" as "proven safe".
