package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
)

func (i *Implementation) DeployCommand() cli.Command {
	return cli.Command{
		Name:  "deploy",
		Usage: i.localize.Localize("deploy_desc", "Deploy a project to a server: self-host its database and pair it back to nuzur"),
		Flags: []cli.Flag{
			cli.StringFlag{Name: "provider", Value: "ssh", Usage: "Where to deploy: ssh (bring-your-own-server), k8s (an existing Kubernetes cluster, as a Helm release — reached over SSH, so no kubeconfig is needed locally), or a managed provider that creates the VM for you — digitalocean | hetzner | linode | gcp | azure | vultr | scaleway (aws coming). Managed providers shell out to your already-authenticated provider CLI."},
			cli.StringFlag{Name: "host", Usage: "Target server IP/hostname (ssh and k8s providers — for k8s, the machine that can reach your cluster)"},
			cli.StringFlag{Name: "region", Usage: "Managed providers: region/location to create the VM in — OPTIONAL, each provider has a default (nyc3 DigitalOcean; nbg1 Hetzner; us-east Linode; ewr Vultr; eastus Azure; us-central1-a GCP; fr-par-1 Scaleway). For GCP and Scaleway this takes a ZONE."},
			cli.StringFlag{Name: "size", Usage: "Managed providers: instance size/type (default: a ~2GB instance per provider — the app image is built on the box, which OOMs on 1GB)"},
			cli.StringFlag{Name: "image", Usage: "Managed providers: OS image (default: Ubuntu 24.04 LTS). Vultr addresses images by numeric id — see `vultr-cli os list`"},
			cli.BoolFlag{Name: "new-vm", Usage: "Managed providers: create a FRESH VM even though this project + --identifier already has one recorded. By default a re-deploy reuses the server it created last time (same box, same agent, same database) instead of billing for a second one. Flag-only — never read from a --deploy-config file."},
			cli.StringFlag{Name: "ssh-key-name", Usage: "Managed providers: name/id of an SSH key already registered with the provider (DigitalOcean/Hetzner/Vultr only — Linode passes the key inline, GCP injects it via metadata, Scaleway uses your account keys). Omit to upload the public half of --ssh-key (or your default ~/.ssh key)."},
			cli.StringFlag{Name: "user", Value: "root", Usage: "SSH user"},
			cli.StringFlag{Name: "ssh-key", Usage: "Path to an SSH private key (default: ssh-agent / ~/.ssh/config)"},
			cli.IntFlag{Name: "port", Value: 22, Usage: "SSH port"},
			cli.StringFlag{Name: "domain", Usage: "Domain pointing at the server — Caddy provisions a real Let's Encrypt cert and serves HTTPS on 443. Omit for IP-only: plain HTTP (no cert of any kind) on a port assigned per project on the box, starting at 8443. Despite the number, that port is NOT TLS. The deploy prints the exact URL and writes it to /etc/nuzur/<identifier>/url — use that rather than assembling one."},
			cli.StringFlag{Name: "project, p", Usage: "Project name or UUID"},
			cli.StringFlag{Name: "version", Usage: "Project version identifier or UUID (default: latest)"},
			cli.StringFlag{Name: "identifier", Usage: "Deployment identifier (names the DB/service/config on the box, the workspace, and the generated root folder/go module — when passed it overrides the identifier in the project's saved go-code-gen config; default: from that saved config, else the project name)"},
			cli.BoolFlag{Name: "db-only", Usage: "Database-only: install the DB engine (--db), pair the agent, register the connection, and apply the schema — but do NOT generate/build/run the app or Caddy. Manage the DB entirely through nuzur."},
			cli.StringFlag{Name: "db-dsn", Usage: "Use an EXISTING database instead of self-hosting one. MySQL DSN (user:pass@tcp(host:port)/db?params) or Postgres URL (postgres://user:pass@host:port/db?sslmode=require). The app + agent connect to it; MySQL install/creation is skipped."},
			cli.StringFlag{Name: "connection", Usage: "Deploy against an EXISTING nuzur team connection (by UUID) instead of --db-dsn. The DSN is resolved server-side from the connection's stored credentials — no plaintext secret on the command line. Mutually exclusive with --db-dsn."},
			cli.BoolFlag{Name: "save-connection", Usage: "After an external (--db-dsn) deploy, register the database as a team connection so your team can use the data manager on it. Requires a team admin. (Non-interactive opt-in; a TTY otherwise prompts.)"},
			cli.BoolFlag{Name: "no-save-connection", Usage: "Never prompt to save the deployed external database as a team connection."},
			cli.StringFlag{Name: "db-schema", Usage: "Postgres schema/namespace to target (default: public). Ignored for MySQL, where the database IS the schema."},
			cli.StringFlag{Name: "db", Value: "mysql", Usage: "Self-hosted database engine: mysql | postgres (postgresql is accepted as an alias). Anything else is rejected."},
			cli.BoolFlag{Name: "storage-enabled", Usage: "Generate the S3 file-upload endpoints (/upload, /sign) even without a team object store — provide credentials via the --s3-* flags below or by editing prod.yaml. Implied by --storage or any --s3-* flag."},
			cli.StringFlag{Name: "storage", Usage: "Enable S3 file uploads using a nuzur team ObjectStore (by UUID): its S3 credentials are resolved server-side and written into the app's config, so /upload and /sign can reach the bucket. Defaults to the object store saved in the project's go-code-gen config. For credentials NOT stored in nuzur, use --s3-* instead."},
			cli.StringFlag{Name: "s3-bucket", Usage: "Manual S3 credentials (not stored in nuzur): bucket name. Enables storage and is written into prod.yaml. Alternative to --storage."},
			cli.StringFlag{Name: "s3-region", Usage: "Manual S3 credentials: region (used with --s3-bucket)."},
			cli.StringFlag{Name: "s3-access-key", Usage: "Manual S3 credentials: access key id (used with --s3-bucket)."},
			cli.StringFlag{Name: "s3-secret", Usage: "Manual S3 credentials: secret access key (used with --s3-bucket). CLI-only — never read from a deploy-config file."},
			cli.StringFlag{Name: "api", Usage: "API surface to generate: rest | grpc | both. Pick by the consumer — REST for JS/web/browser clients, gRPC for Go/backend clients (leave unset to use the project's last/provided config)"},
			cli.StringFlag{Name: "auth", Usage: "Auth middleware: disabled | jwt | keycloak. Anything else is rejected (leave unset to use the project's last/provided config)"},
			cli.BoolFlag{Name: "custom", Usage: "Generate the custom application layer (app package for custom endpoints). Sticky: once a deploy sets it, later deploys of the same project keep it without re-passing the flag — pass --custom=false to turn it back off."},
			cli.StringFlag{Name: "source-dir", Usage: "Directory for the app's source (the workspace deploy generates + builds from; you edit custom endpoints here). Default: ./nuzur-<identifier>. Re-deploys reuse it and preserve your edits."},
			cli.StringFlag{Name: "deploy-config", Usage: "Path to a JSON deploy spec describing the whole deploy (topology + a nested `codegen` block); use '-' to read from stdin. Explicit flags override values in the file. Build or generate one from nuzur web."},
			cli.BoolFlag{Name: "print-config", Usage: "Print the effective deploy config (as JSON) resolved from flags + --deploy-config, then exit without deploying. Use it to snapshot an invocation into a reusable deploy-config file."},
			cli.BoolFlag{Name: "plan", Usage: "Dry run: print the exact SQL this deploy would apply to the target database, then exit. Provisions nothing, generates nothing, issues no token, and writes nothing to the box or to nuzur. Use it to see what a deploy would change — including anything it would DROP — before running one."},
			cli.BoolFlag{Name: "json", Usage: "With --plan: emit the plan as JSON on stdout (for agents and CI)."},
			cli.StringFlag{Name: "deployment", Usage: "Target a recorded deployment by id (see 'nuzur-cli deploy list'). The most reliable selector — the record carries the project, the provider, the box, the identifier, the agent, the connection and the engine, so nothing has to be re-derived from flags. With --plan it selects the database to diff; on a real deploy it selects the box to deploy to. Every other flag overrides what the record says."},
			cli.StringFlag{Name: "local-agent", Usage: "With --plan: the local agent uuid to plan through, for when this machine has no record of the deployment (requires --local-agent-connection)."},
			cli.StringFlag{Name: "local-agent-connection", Usage: "With --plan: the agent connection uuid to plan against (requires --local-agent)."},
			cli.BoolFlag{Name: "allow-destructive", Usage: "Authorize a schema apply that DELETES DATA (DROP TABLE/SCHEMA, DROP COLUMN, TRUNCATE). Without it, a deploy whose schema plan deletes data applies nothing and exits non-zero. Flag-only — never read from a --deploy-config file. Run --plan first to see what would be dropped."},
			cli.StringFlag{Name: "gen-config", Usage: "Path to a JSON go-code-gen config (overrides the deploy-config's `codegen` block; else the last-used config for this project is reused). A project that has never run the generator needs none: deploy fills the required fields from these flags (REST API, no auth, identifier/module from --identifier or the project name). Whatever it resolves is saved as the project's go-code-gen config once the code generates"},
			cli.StringFlag{Name: "cli-install-cmd", Usage: "Command to install the nuzur CLI on the box (must leave `nuzur` on PATH). By default the box downloads the GitHub release PINNED to this CLI's own version and verifies it against the release checksums — use this for boxes that can't reach GitHub, or to pin a different version on purpose. To install the LATEST published release instead: --cli-install-cmd 'curl -fsSL https://nuzur.com/install.sh | NUZUR_INSTALL_DIR=/usr/local/bin sh'"},
			cli.BoolFlag{Name: "sudo", Usage: "Run the bootstrap via sudo (auto-enabled for non-root SSH users; the box needs passwordless sudo)"},
			cli.StringFlag{Name: "web-url", Value: constants.WEB_PROD_URL, Usage: "nuzur web app base URL (for the data-manager deep link)"},

			// ── k8s provider ──────────────────────────────────────────────
			cli.StringFlag{Name: "namespace", Usage: "k8s: namespace for the Helm release (default: the identifier). Created if missing."},
			cli.StringFlag{Name: "release", Usage: "k8s: Helm release name (default: the identifier)"},
			cli.StringFlag{Name: "helm-cmd", Usage: "k8s: helm command to run ON THE HOST, overriding detection. Detection probes `microk8s helm3`, `microk8s helm`, then `helm`, preferring microk8s — use this when the box runs microk8s but you want a different cluster."},
			cli.StringFlag{Name: "kubectl-cmd", Usage: "k8s: kubectl command to run ON THE HOST, overriding detection (see --helm-cmd)"},
			cli.StringFlag{Name: "image-repo", Usage: "k8s: container image repository CI pushes to, e.g. ghcr.io/<owner>/<repo>. Defaults to what the generated workflow publishes."},
			cli.StringFlag{Name: "image-tag", Usage: "k8s: deploy this exact tag instead of the tag built from the pushed commit"},
			cli.BoolFlag{Name: "pin-digest", Usage: "k8s: resolve the image to an immutable sha256 digest and pin that, so the release can be rolled back exactly. Flag-only."},
			cli.BoolFlag{Name: "skip-schema", Usage: "Do not apply the schema this run — deploy the app and leave the database untouched. Use when the schema is already applied, or when a plan shows only no-op churn you do not want re-run. Pairs with --connection, which deploy still uses to write the host's credentials file. Flag-only."},
			cli.StringFlag{Name: "write-config", Usage: "k8s: whether deploy may create the host's credentials file (/etc/config/<identifier>/prod.yaml) from the --connection you passed — full | no-password | skip. Writing it means this CLI reads your database password and sends it to the host over SSH; by default it asks, and skips when non-interactive. An existing file is never overwritten. Flag-only."},
			cli.StringFlag{Name: "auth-domain", Usage: "Hostname for the JWT auth server (e.g. auth.example.com, against --domain api.example.com). On k8s it runs as a second deployment of the same image exposing only its HTTP endpoints; on a VM it is a second Caddy site pointing at the same process. Only meaningful for a project with JWT auth. Recorded, and re-adopted by a re-deploy that omits it."},
			cli.StringFlag{Name: "grpc-domain", Usage: "Hostname for the gRPC API (e.g. grpc.example.com, against --domain api.example.com). It needs its own hostname because HTTP/2-to-the-backend is configured per host: on k8s that is a second Ingress carrying the h2c annotations, on a VM a second Caddy site proxying h2c to the app's gRPC port, each with its own certificate. Only meaningful for a project that serves gRPC. Recorded, and re-adopted by a re-deploy that omits it."},
			cli.StringFlag{Name: "chart-values", Usage: "k8s: path to an extra Helm values file, applied last (after the values deploy generates)"},
			cli.BoolFlag{Name: "no-commit", Usage: "k8s: do not commit or push the generated code — use the repo as it stands. Flag-only."},
			cli.BoolFlag{Name: "no-wait", Usage: "k8s: do not wait for the CI build; release whatever image is already published. Flag-only."},
			cli.BoolFlag{Name: "release-only", Usage: "k8s: skip generation, commit and CI entirely and just run the Helm release, reusing the image and chart version from the last deploy. Flag-only."},
		},
		Subcommands: []cli.Command{i.DeployListCommand()},
		Action: func(c *cli.Context) error {
			// Checked before anything is resolved, generated or provisioned:
			// `deploy --custom false --allow-destructive` used to run a real deploy
			// with the authorization gate silently dropped. See command_args.go.
			if err := requireNoArgs(c, "deploy"); err != nil {
				return err
			}
			return i.runDeploy(c)
		},
	}
}

// deployResolveOptions is how deploy resolves its project/version/extension. It
// is a function rather than an inline literal so a test can assert on the policy
// bits — particularly requireApprovedVersion, which is the only thing standing
// between a draft schema and a production box.
func deployResolveOptions() resolveOptions {
	return resolveOptions{
		extensionIdentifier: goCodeGenExtensionIdentifier,
		interactive:         false,
		checkAccess:         true,
		checkLimit:          true,
		// Production runs reviewed schemas only. Checked here, before the CLI
		// provisions or bills anything.
		requireApprovedVersion: true,
	}
}

func (i *Implementation) runDeploy(c *cli.Context) (rerr error) {
	// Everything this deploy knows — the resolved settings, the names derived from
	// them, the box it chose and what the run learns as it goes. See
	// deploy_pipeline.go, including the mutex-guarded interrupt cell (the revision
	// uuid, the destroy id and the pending VM name) that the deferred hook and the
	// signal handler below read from another goroutine.
	//
	// The revision is set once the deploy is recorded in nuzur (right after the box
	// exists). If anything fails after that, mark the revision FAILED with the
	// error — a broken deploy should be visible in nuzur, not look like it never
	// happened.
	st := &deployState{}
	defer func() {
		// revisionShouldFail keeps a blocked destructive schema from being relabelled
		// a failed deploy: it returns a bare exit error so CI notices, but the box is
		// provisioned and serving and the revision already says what was skipped.
		if rev := st.deployRev(); rev != "" && revisionShouldFail(rerr) {
			i.updateDeployRevision(context.Background(), rev,
				nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_FAILED, rerr.Error())
			// The same fact, on the record the NEXT run reads. Which step the
			// record stopped at says how far the deploy got; this says why it
			// stopped, and the pair is what turns "this record looks unfinished"
			// into a diagnosis.
			//
			// Gated on there being a revision, which means the box exists and the
			// record is this run's (6b wrote it, 6c opened the revision). Before
			// that point the record on disk may be a PREVIOUS deploy's — a
			// re-deploy refused because its recorded box did not answer must not
			// annotate the record it refused to touch.
			//
			// Best-effort and existence-checked: never create a record from the
			// error path, and never turn a failed deploy into a differently
			// failed one.
			if id := st.deployUserIDVal(); id != "" {
				if _, lerr := deploy.LoadDeployment(id); lerr == nil {
					_, _ = deploy.MutateDeployment(id, func(rec *deploy.Deployment) {
						rec.LastError = rerr.Error()
					})
				}
			}
		}
	}()

	// Ctrl-C / SIGTERM kills the process WITHOUT running the deferred hook above,
	// which would strand the revision as "Deploying…" in nuzur forever. Catch the
	// signal, mark it failed, then exit. (SIGKILL can't be caught — the destroy
	// path finalizes any revision still in flight as the backstop.)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	// Stop first, then close: after Stop nothing can send, so the close is safe and
	// it releases the goroutine below instead of leaking it.
	defer func() {
		signal.Stop(sigCh)
		close(sigCh)
	}()
	go func() {
		sig, ok := <-sigCh
		if !ok {
			return // normal return path closed the channel
		}
		// Only claim to have recorded it if there is in fact a revision to record
		// against — an interrupt before the box exists has nothing to mark.
		if rev := st.deployRev(); rev != "" {
			i.updateDeployRevision(context.Background(), rev,
				nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_FAILED,
				fmt.Sprintf("deploy interrupted (%s) before it finished", sig))
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"\nInterrupted (%s) — marked this deploy failed in nuzur.", sig), outputtools.Yellow)
		} else {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"\nInterrupted (%s) before the deploy was recorded in nuzur.", sig), outputtools.Yellow)
		}
		// How far it got, from the checkpoint the record already carries. An
		// interrupt is the case the checkpoints were added for — the deferred hook
		// does not run here, so this line and the record are the whole account of
		// what the next run will find. Silent when nothing has been checkpointed:
		// there is then nothing on disk this could be describing.
		if cp := st.checkpoint(); cp != "" {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"The deploy had reached '%s'; its record says the same, and the next run of it reads that.", cp),
				outputtools.Yellow)
		}
		// The part that costs money if unsaid. The VM was written to local state
		// before it was created, so destroy can find it either way.
		if vm := st.pendingVM(); vm != "" {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"A server (%s) may have been created and is billing.\nRun `nuzur-cli destroy %s` to remove it.",
				vm, st.deployUserIDVal()), outputtools.Yellow)
		}
		os.Exit(130)
	}()

	// Resolve the effective settings from the --deploy-config file merged with the
	// CLI flags (explicit flags win). Everything below reads from `s`.
	settings, err := resolveDeploySettings(c)
	if err != nil {
		return err
	}
	st.s = settings
	// --deployment <id>: take the targeting from a recorded deployment, the same
	// selector --plan uses. Skipped for --plan, which resolves the record itself (it
	// targets a DATABASE, and can legitimately plan a record this project's flags
	// would not reach). See applyDeploymentSelector for what is adopted and why the
	// selector had to work here at all.
	if depID := strings.TrimSpace(c.String("deployment")); depID != "" && !c.Bool("plan") {
		deps, _ := deploy.ListDeployments()
		rec := findDeploymentByID(deps, depID)
		if rec == nil {
			return fmt.Errorf("no deployment %q on this machine (see `nuzur-cli deploy list`)", depID)
		}
		adopted, err := applyDeploymentSelector(st.s, rec, c.IsSet)
		if err != nil {
			return err
		}
		if len(adopted) > 0 {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"Deploying to deployment %s, taken from its record: %s. Any flag you passed overrides it.",
				rec.ID, strings.Join(adopted, ", ")), outputtools.Blue)
		}
	}
	// --print-config: emit the resolved deploy spec and exit without deploying, so
	// a user can snapshot an invocation into a reusable deploy-config file.
	if c.Bool("print-config") {
		out, err := json.MarshalIndent(st.s.toDeployConfig(), "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(outputtools.Stdout, string(out))
		return nil
	}

	// --plan: a dry run against the database this deploy WOULD push to.
	//
	// It deliberately does not share the body below. Applying the schema is step 10
	// of 12 — by then the deploy has run the code generator, mutated the project's
	// saved config, issued a provisioning token, created the VM, opened a deployment
	// revision in nuzur and run the whole bootstrap on the box. There is no point in
	// that sequence from which "stop before applying" is a dry run, so --plan exits
	// early, like --print-config.
	if c.Bool("plan") {
		return i.runDeployPlan(c, st.s)
	}
	if c.Bool("json") {
		return fmt.Errorf("--json only applies to --plan; a deploy has no JSON output")
	}

	ctx := context.Background()

	// The deploy itself: deploySteps() in order, each step's precondition asked
	// immediately before it. The last step is the report, whose error IS this
	// run's result (a schema that never reached the database exits non-zero), so
	// the loop returns what a step returns rather than translating it.
	for _, step := range deploySteps() {
		if step.skip != nil && step.skip(st) {
			continue
		}
		if err := step.run(i, ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// goCodeGenDBValue maps a deploy engine to go-code-gen's `db` config option value.
// NB: go-code-gen uses "postgresql" (its DatabaseType enum), NOT the runtime driver
// name "postgres" that prod.yaml + the agent connection use.
func goCodeGenDBValue(engine deploy.DBEngine) string {
	if engine == deploy.DBPostgres {
		return "postgresql"
	}
	return "mysql"
}

// agentConnDbType maps a deploy engine to the nem local-agent connection DbType.
// Defaults to MySQL for the empty/unknown engine (older records predate the field).
func agentConnDbType(engine deploy.DBEngine) nemgen.LocalAgentConnectionDbType {
	if engine == deploy.DBPostgres {
		return nemgen.LocalAgentConnectionDbType_LOCAL_AGENT_CONNECTION_DB_TYPE_POSTGRES
	}
	return nemgen.LocalAgentConnectionDbType_LOCAL_AGENT_CONNECTION_DB_TYPE_MYSQL
}

// SQL-push extensions. Which one applies the schema is derived from the
// deployment's topology, never configured — see applySchema.
var sqlPushPair = mustPairForFront("sql-push")

// publishConnectionCatalog publishes the box's DB as a named agent connection, so
// nuzur can serve it in the data manager under "Via agent". The box itself registers
// the connection locally with --no-publish (it has no user token), so this call is
// the ONLY thing that puts the connection in nuzur's catalog.
//
// Deliberately separate from applySchema: the two are independent (the schema push
// routes through the agent by uuid, not through the published catalog), and folding
// them together meant a publish failure silently cost you the schema too — and got
// reported as "Schema auto-apply skipped", which is how the catalog bug stayed hidden.
func (i *Implementation) publishConnectionCatalog(agentUUID, connUUID, connName string, dbEngine deploy.DBEngine) error {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	// UpdateLocalAgentConnections REPLACES the agent's cloud catalog, and one box
	// shares one agent across N projects — so publish the UNION of every project's
	// connection on this agent, not just the current one, or a second project's
	// deploy would wipe the first's connection from nuzur.
	conns := []*nemgen.LocalAgentConnection{}
	seen := map[string]bool{}
	addConn := func(uuid, name string, engine deploy.DBEngine) {
		if uuid == "" || seen[uuid] {
			return
		}
		seen[uuid] = true
		conns = append(conns, &nemgen.LocalAgentConnection{
			Uuid:   uuid,
			Name:   name,
			DbType: agentConnDbType(engine),
		})
	}
	addConn(connUUID, connName, dbEngine) // the project being deployed (its record isn't saved yet)
	if deps, e := deploy.ListDeployments(); e == nil {
		for _, d := range deps {
			if d.LocalAgentUUID == agentUUID && d.ConnUUID != "" {
				addConn(d.ConnUUID, d.Identifier+"-db", d.DBEngine)
			}
		}
	}
	if _, err := i.productClient.ProductClient.UpdateLocalAgentConnections(authCtx, &pb.UpdateLocalAgentConnectionsRequest{
		LocalAgentUuid: agentUUID,
		Connections:    conns,
	}); err != nil {
		return fmt.Errorf("publishing connection catalog: %w", err)
	}
	return nil
}

// deployPushTarget describes the database this deploy pushes its schema to. The
// topology decides which shape it takes: teamConnUUID is set only for a
// --connection deploy (an existing team connection), everything else goes through
// the box's agent.
func deployPushTarget(agentUUID, connUUID, schema, teamConnUUID, teamConnStore string, engine deploy.DBEngine) planTarget {
	if teamConnUUID != "" {
		return planTarget{
			Mode:         connModeRemote,
			TeamConnUUID: teamConnUUID,
			TeamStore:    teamConnStore,
			Schema:       schema,
			Engine:       engine,
		}
	}
	return planTarget{
		Mode:      connModeLocal,
		AgentUUID: agentUUID,
		ConnUUID:  connUUID,
		Schema:    schema,
		Engine:    engine,
	}
}

// applySchema applies the project's schema to the deployed database via the
// SQL-push extension, subject to the destructive-change gate.
//
// Note that the database is not necessarily new or empty: a re-deploy, a --db-dsn
// deploy and a --connection deploy all land here against a database that already has
// tables and rows in it. That is what the gate is for.
func (i *Implementation) applySchema(targets *runTargets, t planTarget, allowDestructive bool, gate *schemaGateResult) error {
	outputtools.PrintlnColoredErr("Applying schema to the database...", outputtools.Blue)
	var progress sqlPushProgress
	_, err := i.sqlPushRun(targets, t, i.schemaApplyDecider(allowDestructive, gate), &progress)
	// Whether anything was actually sent. Recorded even on success, so the caller
	// never has to infer it from the error.
	gate.sqlIssued = progress.SQLIssued
	if gate.blocked {
		// The extension cancelled because we rejected it, which is the intended
		// outcome — report it as a decision, not as the cancellation it looks like.
		return errSchemaBlocked
	}
	return err
}

func (i *Implementation) DeployListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: i.localize.Localize("deploy_list_desc", "List deployments created on this machine"),
		Action: func(c *cli.Context) error {
			if err := requireNoArgs(c, "deploy list"); err != nil {
				return err
			}
			deps, err := deploy.ListDeployments()
			if err != nil {
				return err
			}
			if len(deps) == 0 {
				fmt.Fprintln(outputtools.Stdout, "No deployments.")
				return nil
			}
			for _, d := range deps {
				// .Local() on every row: records are persisted in UTC (see
				// SaveDeployment), so the conversion belongs here — once, uniformly —
				// rather than being inherited from whatever zone each record was built
				// in. Without it two records minutes apart could list hours apart.
				fmt.Fprintf(outputtools.Stdout, "%s  %-10s  %s@%s  agent=%s  %s\n",
					d.ID, d.Provider, d.User, d.Host, d.LocalAgentUUID, d.CreatedAt.Local().Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
}

func (i *Implementation) DestroyCommand() cli.Command {
	return cli.Command{
		Name:      "destroy",
		Usage:     i.localize.Localize("destroy_desc", "Tear down a deployment: clean up the server, revoke its agent, remove local state"),
		ArgsUsage: "<deployment-id>",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "ssh-key", Usage: "Path to the SSH private key for the server teardown (default: ssh-agent / ~/.ssh/config)"},
			cli.StringFlag{Name: "user", Usage: "SSH user (default: the deployment's recorded user)"},
			cli.IntFlag{Name: "port", Usage: "SSH port (default: the deployment's recorded port)"},
			cli.BoolFlag{Name: "sudo", Usage: "Run the teardown with sudo (auto-enabled for non-root users)"},
			cli.BoolFlag{Name: "purge", Usage: "Also DROP the database and app user on the box (irreversible; default keeps the data)"},
			cli.BoolFlag{Name: "skip-server", Usage: "Don't SSH to the box to clean it up — use when it's unreachable, half-built, or already gone. A VM nuzur created is still deleted (that cleans up everything the on-box teardown would have); pass --keep-vm to preserve it."},
			cli.BoolFlag{Name: "keep-vm", Usage: "For a managed-provider deployment, keep the created VM instead of deleting it. This is the only flag that preserves the VM (default: the VM nuzur created is deleted when the last project on it is destroyed)"},
		},
		Action: func(c *cli.Context) error {
			if !c.Args().Present() {
				return fmt.Errorf("missing deployment-id (see `nuzur-cli deploy list`)")
			}
			// Exactly one. A second positional means a flag was written in a form the
			// parser stopped at, and destroy is not a command to run with flags
			// silently dropped — --keep-vm and --purge are both in that set.
			if err := requireOneArg(c, "destroy", "the deployment id"); err != nil {
				return err
			}
			id := c.Args().First()
			dep, err := deploy.LoadDeployment(id)
			if err != nil {
				return err
			}
			if err := i.login(); err != nil {
				return err
			}
			ctx := context.Background()

			// What actually happened, filled in as it happens and rendered at the end.
			// The summary used to be assembled from what destroy set out to do, which is
			// how a teardown against a box that no longer existed reported "server
			// cleaned up" and "The database was kept".
			outcome := destroyOutcome{
				ID:           id,
				Provider:     dep.Provider,
				Provisioning: dep.Provisioning,
				Purge:        c.Bool("purge"),
				ExternalDB:   dep.ExternalDB,
			}

			// Read once and share: isLast, the agent resolution below and the
			// connection re-publish all ask questions of the same list, and they must
			// not disagree about what is on this box.
			deps, _ := deploy.ListDeployments()

			// A box can host multiple projects on one shared agent. This is the
			// LAST project on the box iff no other deployment record shares its
			// host. Only then do we tear down the shared agent + revoke it — while
			// other projects are live, the agent must survive.
			isLast := true
			for _, d := range deps {
				if d.ID != id && d.Host == dep.Host {
					isLast = false
					break
				}
			}
			outcome.IsLast = isLast

			// The agent to act on is the BOX's agent, not necessarily this record's.
			// A deploy that died before pairing leaves a record with no agent uuid
			// (pickPriorDeployment skips exactly those), so the retry creates a second
			// record for the same host — and destroying the empty one last used to
			// revoke nothing while still printing "shared agent revoked". Fall back to
			// whatever agent the host is known to be running.
			//
			// Deliberately NOT written back onto the sibling records: an empty
			// LocalAgentUUID is load-bearing elsewhere — pickPriorDeployment reads it as
			// "this deploy died in flight, do not reuse it" — so filling it in would make
			// a half-built record look like a working prior deployment. The fallback is
			// a read, not a repair.
			agentUUID := firstNonEmpty(dep.LocalAgentUUID, pickBoxAgent(deps, dep.Host))

			// 1. Server teardown: remove THIS project's artifacts (its service,
			// container, image, /etc/nuzur/{id}, Caddy snippet, cron, connection);
			// the shared agent + Caddy root go only when isLast. Best-effort — a
			// gone/unreachable box still lets the cloud-side cleanup proceed.
			// A record still marked Provisioning is a deploy that died before the box
			// was ever bootstrapped: there is no service, container, database or agent
			// on it to tear down, and its Host may not even be known. Skip straight to
			// deleting the VM.
			// A k8s deployment installed no runtime on the box, so there is no
			// service, container, database or agent for the teardown script to
			// remove — only a Helm release, which comes off with helm.
			//
			// The namespace is deliberately left in place: this chart may be one
			// of several things the user runs in it, and deleting it would take
			// their other releases with it.
			if !c.Bool("skip-server") && !dep.Provisioning && dep.Provider == deploy.ProviderK8s {
				if err := i.uninstallK8sRelease(ctx, c, dep); err != nil {
					outcome.Server = teardownFailed
					outputtools.PrintlnColoredErr(fmt.Sprintf(
						"warning: %v — cleaning up nuzur state anyway. Re-run `nuzur-cli destroy %s` once the cluster is reachable, or use --skip-server.",
						err, id), outputtools.Yellow)
				} else {
					outcome.Server = teardownDone
				}
			} else if !c.Bool("skip-server") && !dep.Provisioning {
				dbName := sanitizeDBName(dep.Identifier)
				// Never drop an EXTERNAL (--db-dsn) database — it's the user's own
				// managed/remote DB, not something we provisioned.
				purge := c.Bool("purge")
				if purge && dep.ExternalDB {
					purge = false
					outcome.Purge = false
					outputtools.PrintlnColoredErr("Note: this deployment uses an external database (--db-dsn); --purge is ignored (managed elsewhere).", outputtools.Yellow)
				}
				script, rerr := deploy.RenderTeardown(deploy.TeardownParams{
					Identifier:    dep.Identifier,
					DBEngine:      dep.DBEngine,
					DBName:        dbName,
					DBUser:        dbName + "_app",
					ConnUUID:      dep.ConnUUID,
					Purge:         purge,
					IsLastProject: isLast,
				})
				if rerr != nil {
					return rerr
				}
				port := c.Int("port")
				if port == 0 {
					port = dep.Port
				}
				target := deploy.Target{
					Host:    dep.Host,
					User:    firstNonEmpty(c.String("user"), dep.User),
					Port:    port,
					KeyPath: c.String("ssh-key"),
				}
				runner := i.sshRunner(target)
				runner.SetSudo(c.Bool("sudo") || target.User != "root")
				outputtools.PrintlnColoredErr("Cleaning up the server (this project's service, container, config"+purgeSuffix(c.Bool("purge"))+")...", outputtools.Blue)
				// The error already names the teardown (deploy.ScriptTeardown) and
				// carries ssh's own diagnosis, so it leads the warning rather than
				// sitting in a parenthetical behind a second "teardown failed".
				if err := runner.RunScript(ctx, deploy.ScriptTeardown, script); err != nil {
					outcome.Server = teardownFailed
					outputtools.PrintlnColoredErr(fmt.Sprintf("warning: %v — cleaning up nuzur state anyway. Re-run `nuzur-cli destroy %s` once the box is reachable, or use --skip-server.", err, id), outputtools.Yellow)
				} else {
					outcome.Server = teardownDone
				}
			}

			// 2. Cloud-side agent cleanup. `revoked` records that a revoke was actually
			// attempted AND succeeded, so the closing message can only claim it when it
			// happened — the "shared agent revoked" line used to print unconditionally,
			// including in the one case where the revoke call was skipped entirely.
			revoked := false
			if agentUUID != "" {
				authCtx, err := productclient.ClientContext()
				if err != nil {
					return err
				}
				if isLast {
					// Last project on the box → revoke the shared agent.
					if _, err := i.productClient.ProductClient.RevokeLocalAgent(authCtx, &pb.RevokeLocalAgentRequest{
						LocalAgentUuid: agentUUID,
					}); err != nil {
						outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not revoke agent %s: %v", agentUUID, err), outputtools.Yellow)
					} else {
						revoked = true
					}
				} else {
					// Other projects survive → keep the agent, but re-publish the
					// remaining connections so this project's drops out of the catalog.
					conns := []*nemgen.LocalAgentConnection{}
					for _, d := range deps {
						if d.ID != id && d.LocalAgentUUID == agentUUID && d.ConnUUID != "" {
							conns = append(conns, &nemgen.LocalAgentConnection{
								Uuid:   d.ConnUUID,
								Name:   d.Identifier + "-db",
								DbType: agentConnDbType(d.DBEngine),
							})
						}
					}
					if _, err := i.productClient.ProductClient.UpdateLocalAgentConnections(authCtx, &pb.UpdateLocalAgentConnectionsRequest{
						LocalAgentUuid: agentUUID,
						Connections:    conns,
					}); err != nil {
						outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not refresh agent connections: %v", err), outputtools.Yellow)
					}
				}
			} else if isLast && !dep.Provisioning {
				// Nothing left on this machine knows which agent the box paired, so no
				// revoke is possible. Say so: an agent left ACTIVE in nuzur pointing at a
				// box that is about to disappear is exactly the state the user needs to
				// hear about, and it is one command away from being cleaned up.
				outputtools.PrintlnColoredErr(
					"warning: no local agent uuid is recorded for this box, so nothing was revoked in nuzur. "+
						"If it paired an agent, find it with `nuzur-cli agent list` and remove it with "+
						"`nuzur-cli agent revoke <uuid>`.", outputtools.Yellow)
			}

			// 2b. Mark the cloud-side deployment record DESTROYED (kept as
			// history). Best-effort — a stale row is preferable to failing the
			// destroy; the local state removal below is what matters.
			// Nothing to mark for a deploy that died while provisioning: the cloud-side
			// record is only written once the box exists.
			if authCtx, err := productclient.ClientContext(); err == nil && !dep.Provisioning {
				if _, err := i.productClient.ProductClient.MarkDeploymentDestroyed(authCtx, &pb.MarkDeploymentDestroyedRequest{
					Host:       dep.Host,
					Identifier: dep.Identifier,
				}); err != nil {
					outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not mark deployment destroyed in nuzur: %v", err), outputtools.Yellow)
				}
			}

			// 2c. Delete the managed-provider VM nuzur created — only when this is the
			// last project on the box (others still need it) and the user didn't ask
			// to keep it. Runs before local-state removal so the instance id is still
			// available. BYO-SSH has no VM to delete. Best-effort.
			//
			// NB: --skip-server deliberately does NOT suppress this. It means "don't
			// SSH in" (the box is unreachable or half-built) — and that's exactly when
			// you still want the VM gone, since deleting it cleans up everything the
			// on-box teardown would have. Gating the delete on it made the flag you'd
			// reach for on a broken box the one that silently leaks it. --keep-vm is
			// the way to preserve a VM.
			if isLast && c.Bool("keep-vm") && dep.Provider.CreatesInfrastructure() {
				outcome.VM = vmKept
			}
			if isLast && !c.Bool("keep-vm") &&
				dep.Provider.CreatesInfrastructure() &&
				(dep.ProviderInstanceID != "" || dep.ProviderResourceName != "") {
				if provisioner, perr := i.provisioner(dep.Provider); perr != nil {
					outcome.VM = vmDeleteFailed
					outputtools.PrintlnColoredErr(fmt.Sprintf("warning: cannot delete the %s VM (%v) — delete %s manually.", dep.Provider, perr, firstNonEmpty(dep.ProviderInstanceID, dep.ProviderResourceName)), outputtools.Yellow)
				} else {
					// A deploy killed during the create call never learned the instance
					// id, so fall back to the name minted before the call. Resolving it
					// is the difference between deleting the VM and leaking it.
					instanceID := dep.ProviderInstanceID
					if instanceID == "" {
						outputtools.PrintlnColoredErr(fmt.Sprintf("Looking for a %s server named %s (this deploy was interrupted while creating it)...", dep.Provider, dep.ProviderResourceName), outputtools.Blue)
						found, ferr := provisioner.FindInstanceByName(ctx, dep.ProviderResourceName, dep.Region)
						if ferr != nil {
							outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not look up %s on %s (%v) — check for it manually to avoid charges.", dep.ProviderResourceName, dep.Provider, ferr), outputtools.Yellow)
						}
						instanceID = found
					}
					switch {
					case instanceID == "" && dep.ProviderInstanceID == "":
						// The create never took effect — there is nothing to delete, and
						// saying "VM deleted" here would be a lie.
						outcome.VM = vmNeverCreated
						fmt.Fprintf(outputtools.Stdout, "No %s server was created for this deployment — nothing to delete.\n", dep.Provider)
					case instanceID == "":
						outcome.VM = vmAlreadyGone
						outputtools.PrintlnColoredErr(fmt.Sprintf("warning: %s instance %s not found — it may already be gone.", dep.Provider, dep.ProviderInstanceID), outputtools.Yellow)
					default:
						outputtools.PrintlnColoredErr(fmt.Sprintf("Deleting the %s VM (instance %s)...", dep.Provider, instanceID), outputtools.Blue)
						prov := deploy.Provisioned{
							Target:     deploy.Target{Host: dep.Host, User: dep.User, Port: dep.Port, KeyPath: c.String("ssh-key")},
							InstanceID: instanceID,
							Region:     dep.Region,
						}
						err := provisioner.Destroy(ctx, prov)
						switch {
						case err == nil:
							outcome.VM = vmDeleted
						case deploy.InstanceAlreadyGone(err):
							// The recognisable "already gone" case, which used to land in the
							// generic arm below and send the user to their provider console
							// to hunt for a server that does not exist. A 404 from the delete
							// means the same thing as a 404 from the lookup: nothing to do,
							// and nothing billing.
							outcome.VM = vmAlreadyGone
							outputtools.PrintlnColoredErr(fmt.Sprintf("The %s VM %s no longer exists — it was already deleted. Nothing to remove.", dep.Provider, instanceID), outputtools.Blue)
						default:
							outcome.VM = vmDeleteFailed
							outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not delete the %s VM %s (%v) — delete it manually to avoid charges.", dep.Provider, instanceID, err), outputtools.Yellow)
						}
					}
				}
			}

			// 3. Remove local deployment state.
			if err := deploy.DeleteDeployment(id); err != nil {
				return err
			}
			outcome.Revoked = revoked
			for _, line := range outcome.summary() {
				fmt.Fprintln(outputtools.Stdout, line)
			}
			return nil
		},
	}
}

func purgeSuffix(purge bool) string {
	if purge {
		return ", database"
	}
	return ""
}

// ── helpers ──────────────────────────────────────────────────────────────────

func loadDeployConfig(path string) (map[string]interface{}, error) {
	m := map[string]interface{}{}
	if path == "" {
		return m, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config-file: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing config-file JSON: %w", err)
	}
	return m, nil
}

// findSourceRoot locates the generated module (the dir containing a Dockerfile)
// under the extracted output.
func findSourceRoot(root string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "Dockerfile" {
			found = filepath.Dir(p)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no Dockerfile found in generated output — enable the Dockerfile option in the generator config")
	}
	return found, nil
}

// carryForwardProvisioning keeps a re-deploy from erasing the handles destroy
// needs to delete a managed-provider VM.
//
// `--provider ssh --host <ip>` is what nuzur itself suggests for retrying against
// a box that already exists — but the SSH path creates nothing, so it has no
// instance id, resource name or region to record, and the re-deploy rewrites the
// record wholesale. Left alone, a box originally created with
// `--provider digitalocean` ends up recorded as plain ssh, and destroy's VM-delete
// gate (a managed provider AND an id or name) silently skips: the box is torn down,
// the droplet keeps running and billing, and nothing warns you.
//
// Only the ssh-over-managed direction is carried forward. A re-deploy naming a
// managed provider provisioned its own VM, and those values are authoritative.
// prior is matched on host+identifier, so it is the same box by construction.
func carryForwardProvisioning(dep, prior *deploy.Deployment, provider deploy.Provider) {
	// Only the given-host-over-managed direction. A re-deploy naming a managed
	// provider provisioned its own VM, and those values are authoritative.
	if prior == nil || provider.CreatesInfrastructure() {
		return
	}
	if !prior.Provider.CreatesInfrastructure() {
		return
	}
	if prior.ProviderInstanceID == "" && prior.ProviderResourceName == "" {
		return
	}
	dep.Provider = prior.Provider
	dep.ProviderInstanceID = prior.ProviderInstanceID
	dep.ProviderResourceName = prior.ProviderResourceName
	if dep.Region == "" {
		dep.Region = prior.Region
	}
}

// The findPriorDeployment / findBoxAgent wrappers that used to live here are
// gone: runDeploy now reads the deployment records ONCE and passes that one list
// to decideDeployBox, pickPriorDeployment and pickBoxAgent. Three questions about
// the same box answered from three separate reads is how a re-deploy ended up
// reusing one record's agent while provisioning past another's VM.
// waitForAgentOnline polls until the given agent uuid reaches ONLINE. Returns
// (true) when observed online, (false) if the timeout passes while it exists but
// stays not-online (the caller warns rather than hard-fails, matching the
// new-agent path).
func (i *Implementation) waitForAgentOnline(uuid string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		authCtx, err := productclient.ClientContext()
		if err != nil {
			return false, err
		}
		res, err := i.productClient.ProductClient.ListLocalAgents(authCtx, &pb.ListLocalAgentsRequest{})
		if err == nil {
			for _, a := range res.GetLocalAgents() {
				if a.GetUuid() == uuid && a.GetStatus() == nemgen.LocalAgentStatus_LOCAL_AGENT_STATUS_ONLINE {
					return true, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(3 * time.Second)
	}
}

func (i *Implementation) listAgentUUIDs() (map[string]bool, error) {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}
	res, err := i.productClient.ProductClient.ListLocalAgents(authCtx, &pb.ListLocalAgentsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing local agents: %w", err)
	}
	set := map[string]bool{}
	for _, a := range res.GetLocalAgents() {
		set[a.GetUuid()] = true
	}
	return set, nil
}

// waitForNewOnlineAgent polls until an agent uuid not in `existing` appears and
// reaches ONLINE status. Returns the new uuid and whether it was observed
// ONLINE. If it registers but doesn't go ONLINE within the timeout, returns
// (uuid, false, nil) so the caller can warn rather than hard-fail (the schema
// apply may still work via the live session, and status can lag).
func (i *Implementation) waitForNewOnlineAgent(existing map[string]bool, timeout time.Duration) (string, bool, error) {
	deadline := time.Now().Add(timeout)
	newUUID := ""
	for {
		authCtx, err := productclient.ClientContext()
		if err != nil {
			return "", false, err
		}
		res, err := i.productClient.ProductClient.ListLocalAgents(authCtx, &pb.ListLocalAgentsRequest{})
		if err == nil {
			for _, a := range res.GetLocalAgents() {
				if existing[a.GetUuid()] {
					continue
				}
				newUUID = a.GetUuid()
				if a.GetStatus() == nemgen.LocalAgentStatus_LOCAL_AGENT_STATUS_ONLINE {
					return newUUID, true, nil
				}
			}
		}
		if time.Now().After(deadline) {
			if newUUID != "" {
				return newUUID, false, nil // registered but not observed ONLINE
			}
			return "", false, fmt.Errorf("timed out waiting for the agent to register (check the server bootstrap output)")
		}
		time.Sleep(3 * time.Second)
	}
}

func stringValue(m map[string]interface{}, key, def string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

// deployFirewallRules is the inbound-TCP allowlist a managed provider's firewall
// should open, mirroring the box's own ufw: always SSH (22); for a full app also
// 80 + 443, plus the IP-only auto-assigned public-port range (8443+, one per
// project on the box) when there's no --domain. A --db-only box exposes only SSH.
func deployFirewallRules(dbOnly bool, domain string) []deploy.FirewallRule {
	rules := []deploy.FirewallRule{{Port: 22}}
	if !dbOnly {
		rules = append(rules, deploy.FirewallRule{Port: 80}, deploy.FirewallRule{Port: 443})
		if strings.TrimSpace(domain) == "" {
			rules = append(rules, deploy.FirewallRule{Port: 8443, PortEnd: 8542})
		}
	}
	return rules
}

// parseDeployDSN parses a MySQL DSN (user:pass@tcp(host:port)/db?params) or a
// Postgres URL (postgres://user:pass@host:port/db?params) into the pieces the
// bootstrap needs. The engine is inferred from a postgres:// / postgresql://
// scheme; everything else is treated as a MySQL DSN.
func parseDeployDSN(dsn string) (engine deploy.DBEngine, host, port, user, pass, name, params string, err error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, e := url.Parse(dsn)
		if e != nil {
			return "", "", "", "", "", "", "", e
		}
		host = u.Hostname()
		port = u.Port()
		if port == "" {
			port = "5432"
		}
		user = u.User.Username()
		pass, _ = u.User.Password()
		name = strings.TrimPrefix(u.Path, "/")
		// Merged onto the default, not conditional on there being no parameters at
		// all: `?connect_timeout=10` used to silently drop sslmode=require, which a
		// managed Postgres refuses the connection without. An explicit sslmode in the
		// DSN still wins.
		params = mergeDSNParams([]string{"sslmode=require"}, u.RawQuery)
		return deploy.DBPostgres, host, port, user, pass, name, params, nil
	}
	cfg, e := mysqldriver.ParseDSN(dsn)
	if e != nil {
		return "", "", "", "", "", "", "", e
	}
	host, port, e = net.SplitHostPort(cfg.Addr)
	if e != nil { // Addr without a port
		host, port = cfg.Addr, "3306"
	}
	// The DSN's own query string, MERGED onto parseTime=true rather than replacing
	// it. Overwriting it meant that any parameter at all — and reaching a managed
	// MySQL requires one, since TLS is a query parameter — left the generated app
	// unable to scan a single DATE/DATETIME column.
	userParams := ""
	if q := strings.LastIndex(dsn, "?"); q >= 0 {
		userParams = dsn[q+1:]
	}
	params = mergeDSNParams([]string{"parseTime=true"}, userParams)
	return deploy.DBMySQL, host, port, cfg.User, cfg.Passwd, cfg.DBName, params, nil
}

// resolveWorkspace picks the persistent app-source directory deploy generates
// into and builds from: --source-dir if given, else the prior deployment's
// recorded dir (so a re-deploy reuses it without re-passing the flag), else the
// default ./nuzur-<identifier>. The path is returned absolute.
func resolveWorkspace(flagDir string, prior *deploy.Deployment, identifier string) (string, error) {
	dir := strings.TrimSpace(flagDir)
	if dir == "" && prior != nil {
		dir = prior.WorkspaceDir
	}
	if dir == "" {
		dir = "nuzur-" + identifier
	}
	return filepath.Abs(dir)
}

// workspaceGitignore keeps secrets and build artifacts out of git so the
// workspace is safe to commit/push. It excludes the box-only prod config,
// key/cert material, env files, and build output.
const workspaceGitignore = `# nuzur deploy — keep secrets and build artifacts out of git
config/prod.yaml
*.local.yaml
.env
.env.*
*.key
*.pem
*.p12
/bin/
*.exe
.DS_Store
`

// workspaceDockerignore keeps git history + secrets + build artifacts out of the
// docker build context (and thus the image), and keeps the build lean — the
// generated Dockerfile builds from this dir, so docker reads this file here.
const workspaceDockerignore = `# nuzur deploy — keep git history, secrets, and artifacts out of the image
.git
.gitignore
config/prod.yaml
*.local.yaml
.env
.env.*
*.key
*.pem
*.p12
/bin/
*.exe
.DS_Store
`

// writeWorkspaceGitignore writes a .gitignore and a .dockerignore into the
// workspace on first creation, so it's safe to commit and its build stays lean +
// secret-free. Neither clobbers an existing file (the user owns them).
func writeWorkspaceGitignore(dir string) error {
	if err := writeIfAbsent(filepath.Join(dir, ".gitignore"), workspaceGitignore); err != nil {
		return err
	}
	return writeIfAbsent(filepath.Join(dir, ".dockerignore"), workspaceDockerignore)
}

// writeIfAbsent writes content to path only when the file doesn't already exist.
func writeIfAbsent(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // present — leave it
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// firstNonEmpty returns the first non-empty string in vals, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// generatedHasJWTAuth reports whether the generated app uses the JWT auth server
// (which needs a signing key injected). Signalled by an `auth:`/`jwt:` block in
// the generated config/base.yaml. Best-effort; false if the file is missing.
func generatedHasJWTAuth(sourceRoot string) bool {
	data, err := os.ReadFile(filepath.Join(sourceRoot, "config", "base.yaml"))
	if err != nil {
		return false
	}
	inAuth := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line == trimmed { // top-level key
			inAuth = trimmed == "auth:"
			continue
		}
		if inAuth && strings.HasPrefix(trimmed, "jwt:") {
			return true
		}
	}
	return false
}

func boolValue(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// sanitizeDBName reduces an identifier to a safe MySQL identifier.
func sanitizeDBName(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "app"
	}
	return out
}

func shortID() string {
	u, err := uuid.NewV4()
	if err != nil {
		return fmt.Sprintf("%d", time.Now().Unix())
	}
	return u.String()[:8]
}
