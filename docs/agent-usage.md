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
# or, for any generator:
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
`nuzur-cli … describe`. It cannot run the extension — code generation writes to
the local filesystem, which a remote server can't reach — so the split is:

- **Remote (`nuzur_cc`)** assembles the config schema from backend data it
  already serves (entities, connections, stores, enum options).
- **Local (`nuzur-cli`)** takes that config and does the actual run + file write.

### Tool: `describeExtensionConfig`

Params:

| param | required | meaning |
|---|---|---|
| `projectUuid` | yes | the project to run against |
| `projectVersionUuid` | yes | the version whose entities become the uuid options |
| `extensionIdentifier` | yes | e.g. `"go-code-gen"` |

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
