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
| `--no-branch-point` | false | on a non-default branch, do not also scan the commit it split off at |

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

#### Branches and worktrees

A scan describes the commit checked out where it runs and records its branch. Scans of the
codebase's **default branch** (from nuzur, else `origin/HEAD`, else whichever of `main`/`master`
exists) are what the project shows by default; scans of other branches are kept for comparing.

On any other branch the scan also records its **branch point**: the merge-base with the default
branch (`origin/<default>` when present, else the local branch). After uploading, if that commit
has never been scanned, it is checked out into a temporary `git worktree`, scanned, uploaded
and removed; the checkout you ran in is never touched. That gives "what has this branch
changed" a base: `diffCodeSnapshots` with only `head`, or "What … changed since it split" in
the web Compare menu. A failure there is printed and does not fail the scan.

A git worktree that has no `.nuzur/codebase.json` of its own (it was committed on a branch this
one has not merged yet) uses the main checkout's: they are the same repository.

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

## Templates and scripts (scanner 0.4.0)

For a server-rendered app the scan also maps the frontend, from what the code names literally:

- **Rendered templates.** A Go string literal naming an existing template file (`.html`, `.tmpl`,
  `.gohtml`, `.tpl`) makes its package render it: `ParseFiles("./templates/home.html")`,
  `ParseGlob`, `ParseFS`, or a helper that takes the path. A name joined at run time
  (`"./templates/admin/" + page` with `page = "llm.html"`) resolves to the one template whose path
  ends that way. Every other template in the same top-level directory is kept too.
- **Template includes.** `{{template "x"}}` includes the one file named or `{{define}}`-ing `x`
  (an ambiguous name is skipped). `<script src>` and any quoted absolute `.js` path (`import()`,
  loader helpers) include that script. Query strings are ignored, so `?v={{.Version}}` is fine.
  A path built from a template variable (`/assets/js/{{$f}}.js`) marks its directory: a Go string
  literal naming a file there (`"drag_v3.3.2"`) makes that package load it.
- **Script imports.** `import … from "./b.js"`, `import("./b.js?v=1")`, and a string naming a
  sibling script without path or version (`loadBeat('fsv3_raq')`, names with `_` or `-` only).
- **Routes scripts call.** JS string literals starting with `/` (concatenation and `${…}` become a
  dynamic segment) that match a Go route pattern. A dynamic segment only matches a route
  parameter. This catches requests and links alike.
- **Left out.** Minified files (`*.min.*`, or a few very long lines), bundler output
  (browserify/webpack), `node_modules`, remote scripts, and CSS.

Each template and script is one node. Its **owner** is the component (or container) rendering or
loading it, when exactly one does; otherwise it is shared. Ids drop version suffixes
(`assets/js/fsv3_v3.3.2.js` → `js:assets/js/fsv3.js`, `baq_v3.3.2/` → `baq/`) so a version bump
that renames every file is not every file replaced; churn is tracked the same way. Go metrics and
findings are unaffected: frontend lines are reported beside a component, never inside its LOC.

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
