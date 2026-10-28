# Mapping a codebase: `nuzur-cli code link`, `scan`, `status`

`code scan` extracts the architecture of a Go module and writes it as **CodeFacts**: its
deployables, components, packages and their dependencies, its HTTP routes, the nuzur entities
it reads and writes, the external systems and Redis keyspaces it touches, and its background
work. nuzur's **Code view** is built from these facts, shown next to the DB model, along with
the findings and commit-to-commit diffs the server derives from them.

The design and roadmap are in
[`nuzur-go/docs/code-architecture-view-plan.md`](../../nuzur-go/docs/code-architecture-view-plan.md).
The facts contract is
[`nuzur-go/product/idl/proto/code_facts.proto`](../../nuzur-go/product/idl/proto/code_facts.proto).

## Usage

```bash
cd path/to/repo            # a Go module root (go.mod), inside a git repo
nuzur-cli code link        # once: ties the module to a nuzur project
git add .nuzur/codebase.json && git commit -m "link to nuzur"
nuzur-cli code scan        # extracts and uploads this commit
nuzur-cli code status      # what is linked, and whether HEAD is scanned
```

### `code link`

| flag | default | meaning |
|---|---|---|
| `--dir` | `.` | module root to link |
| `--project` | detected | project name or uuid. Default: the project stamped into the module's nuzur-generated code, else a picker |
| `--identifier` | repo name | codebase identifier (lowercase slug); a module below the repo root gets the module dir appended |
| `--yes` | false | skip the confirmation (required outside a terminal) |

It creates the project's codebase, or finds it, since linking is idempotent per (project,
identifier). It refreshes the repo URL, module dir and default branch, then writes
`.nuzur/codebase.json`:

```json
{ "project_uuid": "…", "codebase_uuid": "…", "identifier": "dragium-web" }
```

Commit it: agents and CI read it to know where snapshots go. Linking only adds a codebase to the
project. It never touches the project's schema or data. It needs an admin or developer role on
the project.

### `code scan`

| flag | default | meaning |
|---|---|---|
| `--dir` | `.` | module root to scan |
| `--out` | (none) | write the facts as indented protojson to this file |
| `--churn-days` | `90` | git history window for churn, commits and authors; `0` disables it |
| `--allow-dirty` | false | scan a tree with uncommitted changes; the snapshot is marked `dirty` |
| `--no-upload` | false | scan only, even when the module is linked |
| `--yes` | false | do not ask before the first upload (CI, agents) |

A clean tree is required by default, because the facts describe one commit.

When the module is linked, the facts are uploaded as gzip protojson (at most 8 MiB inflated).
Re-scanning a commit replaces its snapshot. The **first** upload of a codebase prints a preview
of what leaves the machine (sample routes, hosts, keyspaces, entities) and asks for
confirmation. Outside a terminal it stops and asks for `--yes`. After the upload, the server's
findings are printed. The command prints a summary:

```
Scanned github.com/maykel/dragium-web at aef8d2d in 1.8s
  containers 15 · components 50 · packages 61 · generated 1
  entities 17 · externals 17 · keyspaces 29
  routes 233 · background tasks 54 · import edges 362 · injects 134
```

### `code status`

Prints the linked codebase, its latest snapshot (commit, branch, date), whether HEAD has been
scanned, and the top findings.

### `code archive`

Hides the linked codebase from its project: it disappears from the Code view and MCP, and
uploads are refused. Nothing is deleted. Running `nuzur-cli code link` in the repository
restores it. Outside a terminal it needs `--yes`. It needs an admin or developer role, and the
web Code view offers the same action in the menu next to the codebase picker.

### Reading it back

Agents read snapshots through the nuzur MCP server:
- `listCodebases`
- `getCodeArchitecture`
- `getCodeComponent`
- `getCodeFindings`
- `diffCodeSnapshots`
- `describeComponents`, which writes what each component is for.

The `onboard-codebase` prompt, also available as `readReference` topic `code-architecture`,
walks an agent through linking, scanning and describing a repository.

## What is (and isn't) collected

Only identifiers, repo-relative paths, route patterns, hostnames, key prefixes and metrics are
collected. **No source code** leaves the machine. Hosts have credentials and query strings
stripped, and the git remote is normalized to a credential-free https URL.

| fact | how it is found |
|---|---|
| containers | every `package main` |
| components | the nearest fx-provided package dir; otherwise the top-level dir (two levels under a namespace dir like `modules/`) |
| dependencies | imports between in-module packages; fx `In` structs (`injects`) |
| routes | go-chi and `net/http` ServeMux registrations, with `Route()` prefixes and wrapping middleware |
| entities | calls on a nuzur-generated gRPC client (`ListSeason` → `season`, list) or a go-code-gen core module (`core/module/season`) |
| externals | known SDK imports (Redis, AWS, Stripe, Spotify, Anthropic, OpenAI, Postgres/MySQL, …), http(s) URL literals, gRPC dial targets |
| keyspaces | the static prefix of keys passed to Redis, directly or through a wrapper package. This is a heuristic: keys built only from variables are not seen |
| background | `go` statements, `time.NewTicker`/`Tick`/`AfterFunc`, `fx.Hook{OnStart}` |
| docs | markdown in a package dir; `docs/**` files whose name mentions a component |
| deploy hints | Kubernetes autoscalers / `replicas > 1`, honoring helm `{{ if .Values.x.enabled }}` guards |

Packages that fail to type-check are still emitted and listed under `load_errors`. The scan
does not abort on them.

## Generated code

- **nuzur-generated trees** are found through their `.nuzur-codegen-manifest.json` and collapsed
  into a single node: a generated client like `sfapi/`, a whole generated app, or a dependency
  module like `github.com/nuzur/nem`.
- A generated tree that contains its own `package main` (a go-code-gen server) counts as a
  **deployable**. Its imports of hand-written packages are kept as edges, so a custom zone that
  the generated server wires in is reachable rather than reported as dead code (scanner 0.2.0).
- **Other generated files**, anything with the standard `// Code generated … DO NOT EDIT.` header
  (protoc, sqlc, stringer), are left out of size and churn metrics.

As a result, hand-written code (including a go-code-gen custom zone) is what the view and its
findings are about.

## Output stability

The output is deterministic: node ids are import paths, and everything is sorted. Two scans of
the same commit produce identical bytes, so snapshots diff cleanly.

Bump `codescan.ScannerVersion` whenever extraction changes, and `codescan.SchemaVersion` only
when the meaning of `CodeFacts` changes (the server rejects versions it doesn't know).

## Developing the scanner

```bash
go test ./codescan/
```

The tests scan `codescan/testdata/app`, a self-contained module whose `go.mod` replaces fx,
go-chi and go-redis with minimal stubs in `_stubs/`. They need no network. Add a case to the
fixture whenever you teach the scanner a new pattern.
