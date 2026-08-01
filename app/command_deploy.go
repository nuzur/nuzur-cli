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
	"sync"
	"syscall"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/nuzur/nuzur-cli/sqlplan"
	"github.com/urfave/cli"
)

func (i *Implementation) DeployCommand() cli.Command {
	return cli.Command{
		Name:  "deploy",
		Usage: i.localize.Localize("deploy_desc", "Deploy a project to a server: self-host its database and pair it back to nuzur"),
		Flags: []cli.Flag{
			cli.StringFlag{Name: "provider", Value: "ssh", Usage: "Where to deploy: ssh (bring-your-own-server), or a managed provider that creates the VM for you — digitalocean | hetzner | linode | gcp | azure | vultr | scaleway (aws coming). Managed providers shell out to your already-authenticated provider CLI."},
			cli.StringFlag{Name: "host", Usage: "Target server IP/hostname (ssh provider)"},
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
			cli.StringFlag{Name: "cli-install-cmd", Usage: "Command to install the nuzur CLI on the box (must leave `nuzur` on PATH). By default the box downloads the GitHub release PINNED to this CLI's own version — use this for boxes that can't reach GitHub, or to pin a different version on purpose."},
			cli.BoolFlag{Name: "sudo", Usage: "Run the bootstrap via sudo (auto-enabled for non-root SSH users; the box needs passwordless sudo)"},
			cli.StringFlag{Name: "web-url", Value: constants.WEB_PROD_URL, Usage: "nuzur web app base URL (for the data-manager deep link)"},
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
	// Set once the deploy is recorded in nuzur (right after the box exists). If
	// anything fails after that, mark the revision FAILED with the error — a broken
	// deploy should be visible in nuzur, not look like it never happened.
	//
	// Guarded because the signal handler below reads them from another goroutine.
	var revMu sync.Mutex
	var deployRevUUID, deployUserID string
	deployRev := func() string {
		revMu.Lock()
		defer revMu.Unlock()
		return deployRevUUID
	}
	setDeployRev := func(v string) {
		revMu.Lock()
		deployRevUUID = v
		revMu.Unlock()
	}
	// The id the user types into `nuzur-cli destroy` — known before the revision is,
	// so the interrupt path can name it.
	setDeployUserID := func(v string) {
		revMu.Lock()
		deployUserID = v
		revMu.Unlock()
	}
	deployUserIDVal := func() string {
		revMu.Lock()
		defer revMu.Unlock()
		return deployUserID
	}
	// Set once a managed VM may exist. From that instant an interrupt has to tell the
	// user a server is running and how to remove it — the one thing that costs real
	// money if they don't hear it.
	var pendingVMName string
	setPendingVM := func(v string) {
		revMu.Lock()
		pendingVMName = v
		revMu.Unlock()
	}
	pendingVM := func() string {
		revMu.Lock()
		defer revMu.Unlock()
		return pendingVMName
	}
	defer func() {
		// revisionShouldFail keeps a blocked destructive schema from being relabelled
		// a failed deploy: it returns a bare exit error so CI notices, but the box is
		// provisioned and serving and the revision already says what was skipped.
		if rev := deployRev(); rev != "" && revisionShouldFail(rerr) {
			i.updateDeployRevision(context.Background(), rev,
				nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_FAILED, rerr.Error())
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
		if rev := deployRev(); rev != "" {
			i.updateDeployRevision(context.Background(), rev,
				nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_FAILED,
				fmt.Sprintf("deploy interrupted (%s) before it finished", sig))
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"\nInterrupted (%s) — marked this deploy failed in nuzur.", sig), outputtools.Yellow)
		} else {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"\nInterrupted (%s) before the deploy was recorded in nuzur.", sig), outputtools.Yellow)
		}
		// The part that costs money if unsaid. The VM was written to local state
		// before it was created, so destroy can find it either way.
		if vm := pendingVM(); vm != "" {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"A server (%s) may have been created and is billing.\nRun `nuzur-cli destroy %s` to remove it.",
				vm, deployUserIDVal()), outputtools.Yellow)
		}
		os.Exit(130)
	}()

	// Resolve the effective settings from the --deploy-config file merged with the
	// CLI flags (explicit flags win). Everything below reads from `s`.
	s, err := resolveDeploySettings(c)
	if err != nil {
		return err
	}
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
		adopted, err := applyDeploymentSelector(s, rec, c.IsSet)
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
		out, err := json.MarshalIndent(s.toDeployConfig(), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
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
		return i.runDeployPlan(c, s)
	}
	if c.Bool("json") {
		return fmt.Errorf("--json only applies to --plan; a deploy has no JSON output")
	}

	provider := deploy.Provider(strings.TrimSpace(s.Provider))
	if provider == "" {
		provider = deploy.ProviderSSH
	}
	provisioner, err := deploy.NewProvisioner(provider)
	if err != nil {
		return err
	}
	if provider == deploy.ProviderSSH && strings.TrimSpace(s.Host) == "" {
		return fmt.Errorf("--host is required for the ssh provider")
	}
	ctx := context.Background()
	dbOnly := s.DBOnly

	// --db-dsn / --connection: connect to an EXISTING database instead of
	// self-hosting one. --db-dsn takes a raw DSN; --connection resolves the DSN
	// server-side from a stored team connection (no plaintext secret on the CLI).
	// Both feed the same external-DB path below.
	dbDSN := strings.TrimSpace(s.DBDSN)
	connFlag := strings.TrimSpace(s.Connection)
	if connFlag != "" && dbDSN != "" {
		return fmt.Errorf("--connection and --db-dsn are mutually exclusive")
	}
	fromConnection := connFlag != ""
	externalDB := dbDSN != "" || fromConnection
	dbEngine := deploy.DBMySQL
	var extHost, extPort, extUser, extPass, extName, extParams string
	// connStore is the team connection's store uuid (only set for --connection);
	// the remote sql-push extension needs it to target the connection.
	var connStore string
	if dbDSN != "" {
		var perr error
		dbEngine, extHost, extPort, extUser, extPass, extName, extParams, perr = parseDeployDSN(dbDSN)
		if perr != nil {
			return fmt.Errorf("parsing --db-dsn: %w", perr)
		}
		if extName == "" {
			return fmt.Errorf("--db-dsn must include a database name")
		}
	} else if !fromConnection && s.DB == "postgres" {
		// Self-hosted Postgres: install + provision PG on the box (parallels the
		// MySQL local tier). The engine drives the bootstrap install/create branch,
		// the app config driver, and the agent connection's --driver/--schema.
		dbEngine = deploy.DBPostgres
	}

	// 1. Resolve project/version + the go-code-gen extension (logs in).
	targets, err := i.resolveRunTargets(extRunFlags{
		project:        s.Project,
		version:        s.Version,
		nonInteractive: true,
	}, deployResolveOptions())
	if err != nil {
		return err
	}

	// --connection: resolve the DSN parts from the stored team connection now that
	// the project's team is known. Drives the same external-DB path as --db-dsn.
	if fromConnection {
		dbEngine, extHost, extPort, extUser, extPass, extName, extParams, connStore, err = i.resolveConnectionForDeploy(connFlag, targets.project.TeamUuid)
		if err != nil {
			return err
		}
	}

	// 2 + 3. Generate the app (skipped entirely for --db-only, which self-hosts
	// only the DB + agent and manages it through nuzur — no app, no code-gen
	// config required, so it works for any project).
	var configValues map[string]interface{}
	var sourceRoot string
	var workspaceDir string // persistent app-source workspace (full-app deploys)
	jwtAuth := false
	// S3 storage: resolved from the team ObjectStore referenced by --storage (or
	// the object_store saved in the project's go-code-gen config). Enables the
	// generated /upload + /sign endpoints and is written into the box's prod.yaml.
	var s3Enabled bool
	var s3Region, s3Bucket, s3Key, s3Secret string
	if !dbOnly {
		// The go-code-gen config: the deploy-config's `codegen` block overlaid by a
		// --gen-config file (resolved in s.Codegen), then the deploy-level knobs
		// (db/custom/api/auth) applied on top.
		provided := map[string]interface{}{}
		for k, v := range s.Codegen {
			provided[k] = v
		}
		// dbEngine is authoritative (from --db, or inferred from --db-dsn). go-code-gen's
		// `db` config option uses "postgresql" (its DatabaseType enum) — distinct from the
		// runtime driver name "postgres" used in prod.yaml + the agent connection.
		provided["db"] = goCodeGenDBValue(dbEngine)
		// Written ONLY when the user said something about it this run. `provided`
		// always beats the project's saved config, so writing it unconditionally made
		// --custom a flag you had to re-pass on every single deploy: omitting it
		// regenerated the app with the custom zone off, which drops the custom-routes
		// hook from the generated server and `app.ProvideCustomRoutes` from main.go.
		// The user's own app/ package survives on disk but is no longer imported, so
		// `go build .` never compiles it — the deploy succeeds and every custom
		// endpoint is simply gone. Leaving the key out lets the saved config carry the
		// setting forward, which is what "re-run the same deploy" has to mean.
		if s.Custom != nil {
			provided["custom_enabled"] = *s.Custom
		}
		provided["dockerfile"] = true
		// Transport selection: pick REST for JS/web clients, gRPC for Go/backend
		// clients. Unset leaves the project's last/provided config untouched.
		switch s.API {
		case "rest":
			provided["proto_enabled"] = false
			provided["grpc_server_enabled"] = false
			provided["rest_enabled"] = true
		case "grpc":
			provided["proto_enabled"] = true
			provided["grpc_server_enabled"] = true
			provided["rest_enabled"] = false
		case "both":
			provided["proto_enabled"] = true
			provided["grpc_server_enabled"] = true
			provided["rest_enabled"] = true
		case "":
			// leave to codegen / last-used / generator defaults
		default:
			return fmt.Errorf("--api must be one of: rest, grpc, both")
		}
		if a := s.Auth; a != "" {
			provided["auth"] = a
		}
		// Storage: --storage-enabled, --storage <uuid>, or any --s3-* flag turns the
		// generation switch on (the saved config's storage_enabled/object_store flow
		// through lastConfig otherwise). --storage overrides the saved object store.
		manualS3 := strings.TrimSpace(s.S3Bucket) != ""
		if s.StorageEnabled || strings.TrimSpace(s.Storage) != "" || manualS3 {
			provided["storage_enabled"] = true
		}
		if ref := strings.TrimSpace(s.Storage); ref != "" {
			provided["object_store"] = ref
		}
		// Fill the required generator fields nothing else supplies. A project that
		// has never had go-code-gen run against it (created via the API/MCP, or
		// straight from the web) has no last-used config, and the deploy flags cover
		// only part of the generator's required surface — so without this the very
		// first deploy of a new project failed validation on `identifier`,
		// `go_module`, `events`, … before anything was provisioned. Missing fields
		// only: explicit values and the saved config always win.
		codegenIdentifier := sanitizeDBName(firstNonEmpty(s.Identifier, targets.project.Name))
		// An explicitly passed --identifier names the generated root folder and go
		// module too, per the flag's own help — which it did not do on a project that
		// already had a saved go-code-gen config, because the identifier only ever
		// reached the generator as a default for a MISSING field. Nothing about which
		// deployment record this run matches changes; see applyCodegenIdentity.
		if renamed := applyCodegenIdentity(provided, targets.lastConfig, s.Identifier); len(renamed) > 0 {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"Naming the generated app from --identifier: %s (the project's saved go-code-gen config said identifier=%s).",
				strings.Join(renamed, ", "), stringValue(targets.lastConfig, "identifier", "")), outputtools.Blue)
		}
		if applied := applyCodegenDefaults(targets.configEntity, provided, targets.lastConfig, codegenIdentifier); len(applied) > 0 {
			lead := "No saved go-code-gen config for this project (first deploy)"
			if len(targets.lastConfig) > 0 {
				lead = "The project's saved go-code-gen config is missing required fields"
			}
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"%s — deploying with derived defaults: %s.\nOverride with the deploy flags (--identifier/--api/--auth/--db), a `codegen` block in --deploy-config, or --gen-config <file>.\nThe resolved config is saved as this project's go-code-gen config, so later deploys reuse it.",
				lead, strings.Join(applied, ", ")), outputtools.Yellow)
		}

		configValues, err = targets.er.BuildConfigFromJSON(targets.project, targets.projectVersion.Uuid, targets.configEntity, provided, targets.lastConfig)
		if err != nil {
			return fmt.Errorf("building generator config — supply the missing fields via --gen-config <file> or a `codegen` block in --deploy-config, or run `nuzur-cli go-code-gen` once to save a config for this project: %w", err)
		}

		// The custom zone is sticky when the flag is omitted (see deploySettings.Custom).
		// Say so at the point it is resolved: a setting that carries itself forward in
		// silence is undiscoverable, and the user who wants it off has to be told how.
		if notice := customStickinessNotice(s.Custom, provided, targets.lastConfig); notice != "" {
			outputtools.PrintlnColoredErr(notice, outputtools.Blue)
		}

		// Catch an unsupportable JWT config here: left alone it generates fine and
		// only fails on the remote host, during the docker build, after the VPS has
		// already been provisioned.
		configErr, jwtWarnings, checkErr := targets.er.ValidateJWTAuthRequirements(targets.projectVersion.Uuid, configValues)
		if checkErr != nil {
			return checkErr
		}
		if configErr != nil {
			return configErr
		}
		for _, w := range jwtWarnings {
			outputtools.PrintlnColoredErr(fmt.Sprintf("warning: %s", w), outputtools.Yellow)
		}

		// If storage generation is on, resolve credentials for prod.yaml — from a
		// team ObjectStore (nuzur-stored) or the manual --s3-* flags. Storage may be
		// enabled with no creds yet (endpoints return 503 until prod.yaml is set).
		if boolValue(configValues, "storage_enabled") || strings.TrimSpace(stringValue(configValues, "object_store", "")) != "" {
			storeUUID := strings.TrimSpace(stringValue(configValues, "object_store", ""))
			switch {
			case storeUUID != "":
				s3Region, s3Bucket, s3Key, s3Secret, err = i.resolveObjectStoreForDeploy(storeUUID, targets.project.TeamUuid)
				if err != nil {
					return err
				}
				s3Enabled = true
			case manualS3:
				s3Region, s3Bucket, s3Key, s3Secret = strings.TrimSpace(s.S3Region), strings.TrimSpace(s.S3Bucket), strings.TrimSpace(s.S3AccessKey), s.S3Secret
				s3Enabled = true
			default:
				outputtools.PrintlnColoredErr("S3 storage is enabled but no credentials were provided (no --storage / --s3-* and none saved) — /upload and /sign will return 503 until you set the aws: block in the app's prod.yaml.", outputtools.Yellow)
			}
		}
		// Generation happens below (step 2), once the identifier + any prior
		// deployment are known — so it targets the persistent workspace.
	}

	// Identifier: --identifier override, else the go-code-gen config's identifier,
	// else (db-only) the sanitized project name. Shared with --plan so a plan and
	// the deploy it previews always name the same database — see deploy_targeting.go.
	identifier := planIdentifier(s.Identifier, configValues, targets.project.Name)

	// Per-revision image tag: each deploy builds + runs a uniquely-tagged image
	// (not :latest) so the deployment revision history pins the exact artifact
	// that shipped — the basis for auditing and a future rollback.
	imageName := fmt.Sprintf("nuzur/%s:%s", identifier, time.Now().UTC().Format("20060102-150405")+"-"+shortID()[:6])

	// The DB is registered as a named agent connection with this UUID, then
	// published to nuzur so the schema can be pushed to it. Self-hosted → a DB
	// named after the identifier with a least-priv `{db}_app` user; external
	// (--db-dsn) → the DB name + user from the DSN.
	dbName := sanitizeDBName(identifier)
	dbUser := dbName + "_app"
	if externalDB {
		// external DB name/user come from the DSN/connection. A MySQL connection is
		// server-level (no database name), so fall back to the identifier-derived
		// name — the app targets that database on the connection's server.
		if extName != "" {
			dbName = extName
		}
		dbUser = extUser
	}
	if externalDB && extName == "" {
		extName = dbName
	}
	// --connection has no raw DSN yet: assemble one from the resolved parts so the
	// external-DB bootstrap can inject it into the on-box agent connection.
	if fromConnection {
		dbDSN = assembleDeployDSN(dbEngine, extHost, extPort, extUser, extPass, extName, extParams)
	}
	// Schema vs database: in MySQL the database IS the schema; in Postgres a
	// database contains schemas (default `public`). `schema` is what the diff
	// engine, the data-manager link, and the agent connection's default schema
	// target — the DB name for MySQL, a namespace for Postgres.
	schema := deploySchemaName(dbEngine, dbName, s.DBSchema)
	dbSchema := "" // agent-connection default schema; empty for MySQL (chosen per query)
	if dbEngine == deploy.DBPostgres {
		dbSchema = schema
	}
	connName := identifier + "-db"
	host := s.Host

	// WHICH BOX. For --provider ssh this is just --host. For a managed provider it
	// is the decision that used to not exist: a re-deploy of the same project +
	// identifier reuses the VM the provider already created for it instead of
	// creating (and billing for) another one. See decideDeployBox for the matrix.
	//
	// Taken HERE, before the prior-deployment lookup, because everything downstream
	// — the shared agent, the connection uuid, the deployment id, the destructive
	// pre-flight gate, the workspace — is keyed on the host. Resolving the host
	// first is what makes managed re-deploys preserve exactly what SSH re-deploys
	// already preserved.
	allDeployments, _ := deploy.ListDeployments()
	box, err := decideDeployBox(boxDecisionInput{
		Provider:    provider,
		HostFlag:    s.Host,
		NewVM:       s.NewVM,
		Identifier:  identifier,
		ProjectUUID: targets.project.Uuid,
		Deployments: allDeployments,
	})
	if err != nil {
		return err
	}
	if box.Message != "" {
		colour := outputtools.Blue
		if box.Action == boxProvision && box.Record != nil {
			// A fresh VM alongside one that already exists is the billing case; it
			// should not read like routine progress.
			colour = outputtools.Yellow
		}
		outputtools.PrintlnColoredErr(box.Message, colour)
	}
	reuseBox := box.Action == boxReuseRecorded
	if reuseBox {
		// From here the run is indistinguishable from `--provider ssh --host <recorded>`:
		// the recorded SSH parameters, no provisioning, the same idempotent bootstrap.
		host = box.Host
		s.Host = box.Host
		if box.User != "" {
			s.User = box.User
		}
		if box.Port != 0 {
			s.Port = box.Port
		}
		// A managed re-deploy passes no --region (the box already exists), so keep
		// reporting the one the VM actually lives in rather than blanking it.
		if strings.TrimSpace(s.Region) == "" {
			s.Region = box.Record.Region
		}
	}

	// Multi-project on one box: the box has ONE shared agent (reused for every
	// project on it — box-level), while the connection UUID + deployment record
	// are per-project (host+identifier).
	prior := pickPriorDeployment(allDeployments, host, identifier)
	// Guard: refuse if this identifier on this host maps to a DIFFERENT project —
	// they'd share the derived DB name/user and collide. Require a distinct id.
	if prior != nil && prior.ProjectUUID != "" && prior.ProjectUUID != targets.project.Uuid {
		return fmt.Errorf("host %s already runs a different project under identifier %q (project %s) — deploy the new project under a distinct identifier", host, identifier, prior.ProjectUUID)
	}
	reuseAgentUUID := pickBoxAgent(allDeployments, host)
	connUUID := ""
	if prior != nil {
		connUUID = prior.ConnUUID
	}
	if connUUID == "" {
		connU, err := uuid.NewV4()
		if err != nil {
			return err
		}
		connUUID = connU.String()
	}
	if reuseAgentUUID != "" {
		outputtools.PrintlnColoredErr("Reusing the box's existing agent ("+reuseAgentUUID+") — no new pairing.", outputtools.Blue)
	}

	// Deployment id: reuse the prior record on a re-deploy, else mint one now. The
	// record is written as soon as the box exists (step 6b) rather than at the end,
	// so an interrupted deploy still leaves something `nuzur-cli destroy` can clean up.
	depID := identifier + "-" + shortID()
	switch {
	case prior != nil:
		depID = prior.ID
	case reuseBox && box.Record != nil:
		// Adopting a box whose record has no agent — the deploy that created it died
		// before pairing, which is exactly what pickPriorDeployment skips. Write back
		// onto THAT record rather than minting a second one for the same VM: two
		// records pointing at one box is how the orphan was created in the first
		// place, and this run is finishing the job the dead one started.
		depID = box.Record.ID
	}
	setDeployUserID(depID)

	// A reused box has to answer before anything is generated or reported. If it
	// doesn't, the deploy stops: silently provisioning a replacement is the exact
	// behaviour this reuse exists to remove.
	if reuseBox {
		outputtools.PrintlnColoredErr("Checking the reused server "+host+" is reachable...", outputtools.Blue)
		probe := deploy.NewSSHRunner(deploy.Target{Host: host, User: s.User, Port: s.Port, KeyPath: s.SSHKey})
		if err := probe.Ping(ctx); err != nil {
			return reusedBoxUnreachableError(box.Record, provider, identifier, err)
		}
	}

	// 1b. RE-DEPLOY ONLY: run the destructive gate before anything is shipped.
	//
	// The apply is step 10, after the bootstrap has rebuilt the image and restarted
	// the container — so a gate that refuses there refuses too late, leaving new code
	// serving the old schema. Here the box, its agent and its database already exist
	// (that is what `prior` means), so the migration is computable while nothing has
	// changed yet, and a blocked deploy ships nothing.
	//
	// Skipped when --allow-destructive is set: the gate would let it through anyway,
	// and the pre-flight costs a round trip to the box's agent. Skipped on a first
	// deploy, which has no agent yet and no old schema to mismatch. Non-blocking on
	// any error — see preflightSchemaGate.
	if !s.AllowDestructive && prior != nil && prior.ConnUUID != "" {
		if preflightAgent := firstNonEmpty(prior.LocalAgentUUID, reuseAgentUUID); preflightAgent != "" {
			preflightTarget := deployPushTarget(preflightAgent, prior.ConnUUID, schema, connFlag, connStore, dbEngine)
			if err := i.preflightSchemaGate(targets, preflightTarget); err != nil {
				return err
			}
		}
	}

	// 2. Generate the app into the PERSISTENT workspace (full-app deploys only) —
	// the editable source of truth deploy builds from. Re-deploys regenerate in
	// place, refreshing generated code while preserving the user's custom
	// endpoints (see extensionrun's user-file-preserving extraction).
	if !dbOnly {
		workspaceDir, err = resolveWorkspace(s.SourceDir, prior, identifier)
		if err != nil {
			return err
		}
		outputtools.PrintlnColoredErr("Generating application code into "+workspaceDir+" ...", outputtools.Blue)
		if _, err := targets.er.Run(extensionrun.RunParams{
			Extension:          targets.extension,
			ExtensionVersion:   targets.extensionVersion,
			ProjectUUID:        targets.project.Uuid,
			ProjectVersionUUID: targets.projectVersion.Uuid,
			ConfigValues:       configValues,
			OutputPath:         workspaceDir,
		}); err != nil {
			return fmt.Errorf("generating code: %w", err)
		}
		// Remember the config that just generated as the project's last-used
		// go-code-gen config — the same record `nuzur-cli go-code-gen` writes and the
		// web app reads. Without this a deploy-derived config was invisible and
		// unreusable: the next deploy re-derived it, a later `go-code-gen` run still
		// prompted from scratch, and one-off flags (--api/--auth) silently reverted.
		// Saved AFTER generation succeeded, so a config that cannot even generate
		// never becomes what the project remembers. Non-fatal: the deploy already has
		// what it needs.
		if saveErr := targets.er.SaveLastUsedConfigEntry(targets.projectVersion.Uuid, targets.extension.Identifier, configValues); saveErr != nil {
			outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not save this deploy's generator config for reuse: %v", saveErr), outputtools.Yellow)
		}
		sourceRoot, err = findSourceRoot(workspaceDir)
		if err != nil {
			return err
		}
		jwtAuth = generatedHasJWTAuth(sourceRoot)
		// Ignore files go at the project root (where the Dockerfile + go.mod live,
		// which the generator nests under <identifier>) — that's the docker build
		// context root and the natural `git init` root.
		if gerr := writeWorkspaceGitignore(sourceRoot); gerr != nil {
			outputtools.PrintlnColoredErr("warning: could not write .gitignore in the workspace: "+gerr.Error(), outputtools.Yellow)
		}
	}

	// 4. Mint a single-use provisioning token for headless pairing.
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return fmt.Errorf("building auth context: %w", err)
	}
	tokRes, err := i.productClient.ProductClient.IssueProvisioningToken(authCtx, &pb.IssueProvisioningTokenRequest{
		ProjectUuid: targets.project.Uuid,
	})
	if err != nil {
		return fmt.Errorf("issuing provisioning token: %w", err)
	}

	// 5. Snapshot existing agents so we can identify the new one after pairing.
	existing, err := i.listAgentUUIDs()
	if err != nil {
		return err
	}

	// 6. Provision: BYO-SSH validates the host; a managed provider creates the VM
	// (over its own CLI) and waits for SSH. Everything after the returned Target is
	// provider-agnostic.
	// Mint the provider-side resource name HERE rather than inside the adapter, and
	// write it to local state before the create call. Creating a VM is a side effect
	// we cannot make atomic with recording it, so the record goes first: if this
	// process dies any time after the call starts, `nuzur-cli destroy <id>` can still
	// find the VM — by id once we have it, by name until then. Without this a killed
	// deploy left a running, billing VM that nothing on disk pointed at.
	var resourceName string
	if provider != deploy.ProviderSSH && !reuseBox {
		resourceName, err = deploy.ProviderResourceName(identifier)
		if err != nil {
			return err
		}
		pending := &deploy.Deployment{
			ID:                   depID,
			Provider:             provider,
			ProviderResourceName: resourceName,
			Provisioning:         true,
			Region:               s.Region,
			Identifier:           identifier,
			ProjectUUID:          targets.project.Uuid,
			ProjectVersionUUID:   targets.projectVersion.Uuid,
			DBEngine:             dbEngine,
			// The workspace ROOT, not sourceRoot: resolveWorkspace reads this
			// back on the next run, and a deploy that died after this write
			// used to hand it the app dir — nesting the retry's generated
			// workspace inside the previous app.
			WorkspaceDir: workspaceDir,
			CreatedAt:    time.Now().UTC(),
		}
		if err := deploy.SaveDeployment(pending); err != nil {
			return fmt.Errorf("recording the deploy before creating the server: %w", err)
		}
		setPendingVM(resourceName)
	}

	spec := deploy.Spec{
		Provider: provider,
		Target: deploy.Target{
			Host: s.Host, User: s.User,
			Port: s.Port, KeyPath: s.SSHKey,
		},
		ProviderConfig: deploy.ProviderConfig{
			Region:     s.Region,
			Size:       s.Size,
			Image:      s.Image,
			SSHKeyName: s.SSHKeyName,
		},
		Identifier:         identifier,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		DBEngine:           dbEngine,
		ProvisioningToken:  tokRes.GetProvisioningToken(),
		SourceDir:          sourceRoot,
		ResourceName:       resourceName,
		// Fires the moment the provider acknowledges the VM — minutes before
		// Provision returns, since it still has to wait for SSH. Persist the id now
		// so the box is deletable for that whole wait.
		OnInstanceCreated: func(ref deploy.InstanceRef) {
			rec, err := deploy.LoadDeployment(depID)
			if err != nil {
				return
			}
			rec.ProviderInstanceID = ref.InstanceID
			rec.Region = ref.Region
			if ref.Host != "" {
				rec.Host = ref.Host
			}
			if err := deploy.SaveDeployment(rec); err != nil {
				outputtools.PrintlnColoredErr(fmt.Sprintf(
					"warning: created %s instance %s but could not record it locally (%v) — delete it manually if this deploy fails",
					provider, ref.InstanceID, err), outputtools.Yellow)
			}
		},
	}
	var prov deploy.Provisioned
	switch {
	case reuseBox:
		// The box already exists and was reached above, so there is nothing to
		// provision and nothing to wait for. Rebuild the Provisioned the rest of the
		// deploy expects from the RECORD, so the provider ids survive: they are the
		// only handle `nuzur-cli destroy` has on the VM, and re-recording this
		// deployment without them would leave the droplet running with nothing on
		// disk pointing at it.
		prov = deploy.Provisioned{
			Target:     deploy.Target{Host: host, User: s.User, Port: s.Port, KeyPath: s.SSHKey},
			InstanceID: box.Record.ProviderInstanceID,
			Region:     box.Record.Region,
		}
		resourceName = box.Record.ProviderResourceName
	default:
		if provider != deploy.ProviderSSH {
			outputtools.PrintlnColoredErr("Creating the server on "+string(provider)+" (this can take a minute)...", outputtools.Blue)
		}
		prov, err = provisioner.Provision(ctx, spec)
		if err != nil {
			return err
		}
	}
	target := prov.Target
	// Managed providers create the host, so --host (and thus `host`) was empty.
	// Adopt the provisioned address so the bootstrap URL, ports readback, public
	// URL, and deployment record all use the real VM IP.
	if strings.TrimSpace(host) == "" {
		host = target.Host
	}

	// 6b. Record the deployment AS SOON AS THE BOX EXISTS — before the long
	// bootstrap/build/pairing steps. If anything after this fails (or the run is
	// interrupted), the record still carries the provider instance id, so
	// `nuzur-cli destroy <id>` can tear the VM down instead of orphaning a billing
	// server nuzur has no memory of. Step 12 fills in the rest (agent, URLs).
	//
	// The record a decision was taken from is only this deployment's when the box is
	// being REUSED. A --new-vm run also carries one — the box it is billing alongside
	// — and that box's agent belongs to that box, not to the fresh VM being created
	// here.
	var adoptedRecord *deploy.Deployment
	if reuseBox {
		adoptedRecord = box.Record
	}
	dep := &deploy.Deployment{
		ID:                 depID,
		Provider:           provider,
		ProviderInstanceID: prov.InstanceID,
		// Carried forward from the pre-provision record: this struct overwrites that
		// file wholesale, and dropping the name would lose the only handle on a VM
		// whose id never came back. Provisioning is left false — the box exists now.
		ProviderResourceName: resourceName,
		Region:               prov.Region,
		Host:                 target.Host, User: target.User, Port: target.Port,
		Identifier:         identifier,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		ConnUUID:           connUUID,
		DBEngine:           dbEngine,
		ExternalDB:         externalDB,
		WorkspaceDir:       workspaceDir,
		Domain:             s.Domain,
		// Carried forward for the same reason as ProviderResourceName above: this
		// struct overwrites the record wholesale, and blanking the agent uuid for the
		// ~20 minutes between here and step 12 is not a cosmetic loss. It makes
		// `--plan --deployment <id>` fail with a false diagnosis ("the deploy that
		// created it did not finish pairing" — it had), and if the re-deploy is
		// interrupted in that window the loss is permanent: pickPriorDeployment skips
		// agentless records, so the next deploy mints a SECOND record for the same
		// host+identifier, which is what makes destroy's isLast refuse to delete the VM.
		// Empty on a genuine first deploy, where no agent is known yet — that record
		// correctly reads as "died before pairing" until step 12 fills it in.
		LocalAgentUUID: knownAgentUUID(prior, reuseAgentUUID, adoptedRecord),
		CreatedAt:      time.Now().UTC(),
	}
	switch {
	case prior != nil:
		dep.CreatedAt = prior.CreatedAt
		carryForwardProvisioning(dep, prior, provider)
	case adoptedRecord != nil:
		// Adopting a died-in-flight record (no agent, so `prior` skipped it): its
		// creation time is when this deployment really started.
		dep.CreatedAt = adoptedRecord.CreatedAt
	}
	if err := deploy.SaveDeployment(dep); err != nil {
		return err
	}

	// 6c. Report the deploy to nuzur as IN_PROGRESS — same reasoning as the local
	// record above, for the cloud side: the box exists, so it should be visible
	// (and watchable, and seen failing) while the slow bootstrap/build/pair steps
	// run. Everything except the box-allocated ports, URLs and agent is already
	// known; step 12b finalizes THIS revision with the rest. Best-effort: progress
	// reporting must never fail an otherwise-good deploy.
	reportIn := deploymentReportInput{
		// dep.Provider, not provider: on an SSH re-deploy onto a managed box this is
		// the carried-forward original, so the cloud record keeps saying digitalocean
		// instead of flipping to ssh.
		Provider:       dep.Provider,
		Identifier:     identifier,
		ProjectUUID:    targets.project.Uuid,
		ProjectVersion: targets.projectVersion.Uuid,
		ConnUUID:       connUUID,
		Host:           target.Host,
		DBEngine:       dbEngine,
		ExternalDB:     externalDB,
		DBOnly:         dbOnly,
		Domain:         s.Domain,
		ExtDBPort:      extPort,
		RESTEnabled:    boolValue(configValues, "rest_enabled"),
		GRPCEnabled:    boolValue(configValues, "grpc_server_enabled"),
		JWTAuth:        jwtAuth,
		AuthConfig:     stringValue(configValues, "auth", ""),
		Region:         s.Region,
		Size:           s.Size,
		Image:          s.Image,
		SSHKeyName:     s.SSHKeyName,
		SSHUser:        target.User,
		SSHPort:        target.Port,
		DBSchema:       schema,
		// The RESOLVED value, not the flag: with --custom sticky the flag may well be
		// absent on a deploy that does generate the custom zone, and the deployment
		// history should record what shipped rather than what was typed.
		Custom:        boolValue(configValues, "custom_enabled"),
		SourceDir:     workspaceDir,
		Status:        nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS,
		StatusMessage: "server ready — bootstrapping",
	}
	if rev, err := i.reportDeployment(ctx, reportIn); err != nil {
		outputtools.PrintlnColoredErr("Deploy not reported to nuzur (continuing): "+err.Error(), outputtools.Yellow)
	} else {
		setDeployRev(rev)
	}

	// Restrict inbound at the provider level to mirror the box's ufw (SSH + the
	// Caddy front doors). Best-effort — the on-box ufw is the authoritative gate,
	// so a firewall hiccup must not fail an otherwise-good deploy. No-op for BYO-SSH.
	// Re-run on a reused box too (a re-deploy can open a new project's port), except
	// when the record never learned the instance id and there is nothing to address.
	if provider != deploy.ProviderSSH && prov.InstanceID != "" {
		if err := provisioner.ConfigureFirewall(ctx, prov, deployFirewallRules(dbOnly, s.Domain)); err != nil {
			outputtools.PrintlnColoredErr("Provider firewall not fully configured (the box's own ufw still applies): "+err.Error(), outputtools.Yellow)
		}
	}

	runner := deploy.NewSSHRunner(target)
	// Non-root SSH users need sudo for the privileged bootstrap steps.
	runner.Sudo = s.Sudo || target.User != "root"
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_preflight", "Checking SSH connectivity..."), outputtools.Blue)
	if err := runner.Ping(ctx); err != nil {
		return err
	}

	// 7. Copy generated source to a user-writable path (scp runs as the SSH
	// user, which may be non-root; the sudo bootstrap builds from here). Skipped
	// for --db-only (no app to build).
	const remoteSrc = "/tmp/nuzur-src"
	if !dbOnly {
		if err := runner.RunCommand(ctx, "rm -rf "+remoteSrc); err != nil {
			return err
		}
		outputtools.PrintlnColoredErr(i.localize.Localize("deploy_copying", "Copying source to the server..."), outputtools.Blue)
		if err := runner.CopyDir(ctx, sourceRoot, remoteSrc); err != nil {
			return err
		}
	}

	// 8. Render + run the bootstrap.
	// Empty cli-install-cmd → the bootstrap installs the nuzur CLI from GitHub
	// releases itself, PINNED to this CLI's own version (see
	// BootstrapParams.CLIVersion): the box then runs exactly the CLI that is driving
	// the deploy, and a release published while the deploy runs cannot change what
	// the box downloads out from under it.
	bp := deploy.BootstrapParams{
		Identifier:        identifier,
		DBEngine:          dbEngine,
		DBName:            dbName,
		DBUser:            dbUser,
		DBOnly:            dbOnly,
		ExternalDB:        externalDB,
		DBHost:            extHost,
		DBPort:            extPort,
		DBPassword:        extPass,
		DBParams:          extParams,
		DBDSN:             dbDSN,
		DBSchema:          dbSchema,
		GRPCEnabled:       boolValue(configValues, "grpc_server_enabled"),
		JWTAuth:           jwtAuth,
		ProvisioningToken: tokRes.GetProvisioningToken(),
		CLIInstallCmd:     s.CLIInstallCmd,
		CLIVersion:        constants.CLI_VERSION,
		ConnUUID:          connUUID,
		ConnName:          connName,
		Domain:            s.Domain,
		Host:              host,
		S3Enabled:         s3Enabled,
		S3Region:          s3Region,
		S3Bucket:          s3Bucket,
		S3Key:             s3Key,
		S3Secret:          s3Secret,
	}
	if !dbOnly {
		bp.RemoteSrcDir = remoteSrc
		bp.ImageName = imageName
	}
	script, err := deploy.RenderBootstrap(bp)
	if err != nil {
		return err
	}
	dbLabel := "MySQL"
	if dbEngine == deploy.DBPostgres {
		dbLabel = "Postgres"
	}
	bootMsg := "Bootstrapping the server (Docker, " + dbLabel + ", build, pairing)..."
	if dbOnly {
		bootMsg = "Bootstrapping the server (" + dbLabel + " + agent, database-only)..."
	}
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_bootstrapping", bootMsg), outputtools.Blue)
	i.updateDeployRevision(ctx, deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, bootMsg)
	if err := runner.RunScript(ctx, script); err != nil {
		return err
	}

	// 9. Verify the agent connected. First deploy → a new agent UUID appears;
	// re-deploy → the existing (reused) agent should come back ONLINE.
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_verifying", "Waiting for the agent to connect..."), outputtools.Blue)
	i.updateDeployRevision(ctx, deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, "waiting for the agent to connect")
	var agentUUID string
	var online bool
	if reuseAgentUUID != "" {
		agentUUID = reuseAgentUUID
		online, err = i.waitForAgentOnline(reuseAgentUUID, 150*time.Second)
	} else {
		agentUUID, online, err = i.waitForNewOnlineAgent(existing, 150*time.Second)
	}
	if err != nil {
		return err
	}
	if !online {
		outputtools.PrintlnColoredErr("Agent registered but not observed online yet; schema auto-apply may fail until it connects.", outputtools.Yellow)
	}

	// 10. Publish the connection catalog (needs the user token — the box can't) and
	// auto-apply the schema to the empty DB. Two independent steps, tracked and
	// reported separately: a failure in one must neither skip nor be mistaken for
	// the other.
	// appShipped: the bootstrap above has already rebuilt the image and restarted the
	// container, so from here on an unapplied schema means the running app no longer
	// matches its database. --db-only has no app, so it has nothing to mismatch.
	outcome := deployOutcome{catalogPublished: true, schemaApplied: true, appShipped: !dbOnly}
	i.updateDeployRevision(ctx, deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, "publishing the connection to nuzur")
	if err := i.publishConnectionCatalog(agentUUID, connUUID, connName, dbEngine); err != nil {
		outcome.catalogPublished = false
		outputtools.PrintlnColoredErr("Connection not published to nuzur: "+err.Error(), outputtools.Yellow)
	}

	i.updateDeployRevision(ctx, deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, "applying the schema to the database")
	pushTarget := deployPushTarget(agentUUID, connUUID, schema, connFlag, connStore, dbEngine)
	var gate schemaGateResult
	if err := i.applySchema(targets, pushTarget, s.AllowDestructive, &gate); err != nil {
		outcome.schemaApplied = false
		if gate.blocked {
			outcome.schemaBlocked = true
			outcome.destructiveCount = len(gate.plan.Destructive())
			outcome.rerunCommand = rerunCommand(os.Args, true)
		} else {
			// Not "skipped": skipping is what the gate does, and calling both the same
			// thing is how the louder problem ended up sounding like the quieter one.
			outputtools.PrintlnColoredErr("Schema apply FAILED: "+err.Error(), outputtools.Red)
			// Whether a statement ever reached the database. A failure while resolving
			// the extension, reaching the agent or computing the diff sent nothing, and
			// used to be reported as a migration that had died partway through.
			outcome.schemaNeverStarted = !gate.sqlIssued
			// Whether that failure took the rest of the migration back with it. Only
			// claimed when the plan that was attempted is in hand — an error before the
			// confirmation step leaves gate.plan empty, and an empty plan must not be
			// read as "nothing could have landed".
			outcome.schemaRolledBack = !gate.plan.Empty() &&
				gate.plan.Transactional(sqlplan.Engine(dbEngine))
		}
	} else if gate.destructiveApplied {
		outcome.destructiveApplied = true
		outcome.destructiveCount = len(gate.plan.Destructive())
	}

	// Read back the resolved front-door URL the bootstrap wrote: a domain project
	// → https://{domain}; an IP-only project → http://{host}:{auto-assigned port}
	// (the public port is allocated on the box so N projects can coexist). Falls
	// back to a best-effort compose if the readback fails. --db-only has no front
	// door.
	publicURL, useHTTPS, grpcTarget := "", false, ""
	if !dbOnly {
		publicURL, _ = runner.Capture(ctx, "cat /etc/nuzur/"+identifier+"/url 2>/dev/null")
		publicURL = strings.TrimSpace(publicURL)
		if publicURL == "" {
			if s.Domain != "" {
				publicURL = "https://" + s.Domain
			} else {
				publicURL = "http://" + target.Host
			}
		}
		useHTTPS = strings.HasPrefix(publicURL, "https://")
		// gRPC dial target host:port (grpcurl needs an explicit port).
		grpcTarget = strings.TrimPrefix(strings.TrimPrefix(publicURL, "https://"), "http://")
		if !strings.Contains(grpcTarget, ":") {
			if useHTTPS {
				grpcTarget += ":443"
			} else {
				grpcTarget += ":80"
			}
		}
	}

	// 11. Build the data-manager deep link (opens the deployed DB directly,
	// with the local-agent connection preselected).
	dataManagerURL := fmt.Sprintf(
		"%s/project/data-manager/%s/%s?mode=local&localAgent=%s&localAgentConn=%s&schema=%s",
		strings.TrimRight(s.WebURL, "/"),
		targets.project.Uuid, targets.projectVersion.Uuid,
		agentUUID, connUUID, url.QueryEscape(schema),
	)

	// 12. Finalize the record: the row was written right after provisioning (6b)
	// so the box was never un-destroyable; fill in what only exists now that
	// pairing + the front door are up. A re-deploy updates the same ID in place.
	dep.LocalAgentUUID = agentUUID
	dep.APIURL = publicURL
	dep.PublicURL = publicURL
	dep.DataManagerURL = dataManagerURL
	if err := deploy.SaveDeployment(dep); err != nil {
		return err
	}

	// 12b. Finalize the nuzur-side revision: fill in what only exists now (the
	// box-allocated ports, the front-door URL, the agent) and flip it ACTIVE, which
	// supersedes the previously-current revision. Updates the SAME revision opened
	// at 6c rather than stacking a duplicate. Best-effort: the local record is
	// authoritative for destroy, so a cloud hiccup must not fail a good deploy.
	reportIn.Runner = runner
	reportIn.LocalAgentUUID = agentUUID
	reportIn.PublicURL = publicURL
	reportIn.DataManagerURL = dataManagerURL
	reportIn.UseHTTPS = useHTTPS
	reportIn.RevisionUUID = deployRev()
	reportIn.ImageName = imageName // built by now — safe to pin in the history
	// ACTIVE even when step 10 was partial: the box, the front door and the app are
	// genuinely serving, so FAILED would mislabel a working deployment, and nem has
	// no DEGRADED value. The shortfall is recorded in the status message instead, so
	// the deployment history can tell a schema-less deploy from a clean one.
	reportIn.Status = nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_ACTIVE
	reportIn.StatusMessage = outcome.revisionMessage()
	if _, err := i.reportDeployment(ctx, reportIn); err != nil {
		outputtools.PrintlnColoredErr("Deployment recorded locally but not reported to nuzur: "+err.Error(), outputtools.Yellow)
	}

	// 13. Report.
	//
	// The banner is qualified rather than unconditional. It used to print "Deployment
	// complete." in green after a schema apply that had errored — so the last loud
	// thing on screen contradicted the one yellow line above it, and a failed
	// migration read as a clean deploy. Both halves are worth saying; saying only the
	// good half is what misleads.
	if outcome.schemaApplied {
		outputtools.PrintlnColored("\nDeployment complete.", outputtools.Green)
	} else {
		outputtools.PrintlnColoredErr("\nDeployment finished, but the schema was NOT applied — see the summary below.", outputtools.Red)
	}
	fmt.Printf("  deployment id: %s\n", dep.ID)
	fmt.Printf("  agent uuid:    %s\n", agentUUID)
	fmt.Printf("  connection:    %s (%s)\n", connName, connUUID)
	if externalDB {
		fmt.Printf("  database:      external %s at %s:%s/%s (not self-hosted; kept on destroy)\n", dbEngine, extHost, extPort, dbName)
	}
	fmt.Printf("  teardown:      nuzur-cli destroy %s\n", dep.ID)

	if dbOnly {
		// Database-only: no app, no front door — just the database managed
		// through nuzur via the agent connection.
		outputtools.PrintlnColored("\nWhat's deployed (database-only):", outputtools.Green)
		if externalDB {
			fmt.Printf("  Database:  external %s (%s:%s), schema applied via the agent.\n", dbEngine, extHost, extPort)
		} else {
			fmt.Printf("  Database:  self-hosted %s on the box (localhost), schema applied.\n", dbEngine)
		}
		fmt.Printf("  Managed:   through nuzur — data manager, SQL Push, and queries via the agent.\n")

		// Loud, actionable notice: db-only is a materially different outcome from
		// a normal deploy (no HTTP API at all), and users who just said "deploy my
		// database" often still expect the generated API. Make the consequence
		// impossible to miss and give the one-step way to add it.
		outputtools.PrintlnColoredErr("\n  This was a DATABASE-ONLY deploy — no REST/gRPC API or app was created.", outputtools.Yellow)
		fmt.Printf("  Nothing serves this data over HTTP; it is reachable only through nuzur (data manager, SQL Push, queries).\n")
		fmt.Printf("  If you also want nuzur's generated API in front of this database, re-run the same deploy\n")
		fmt.Printf("  WITHOUT --db-only (the database, agent, schema and data are reused) — for example:\n")
		rerun := "nuzur-cli deploy"
		if p := strings.TrimSpace(s.Provider); p != "" {
			rerun += " --provider " + p
		}
		if h := strings.TrimSpace(s.Host); h != "" {
			rerun += " --host " + h
		}
		if pr := strings.TrimSpace(s.Project); pr != "" {
			rerun += " --project " + pr
		}
		rerun += " --version " + targets.projectVersion.Uuid + " --api both"
		fmt.Printf("    %s\n", rerun)
		fmt.Printf("  (add your original --ssh-key / --auth / --domain flags as needed).\n")
	} else {
		// What's deployed: this project's own Caddy front door (HTTPS via a domain,
		// otherwise plain HTTP on its auto-assigned public port).
		if useHTTPS {
			outputtools.PrintlnColored("\nWhat's deployed (HTTPS via Caddy):", outputtools.Green)
		} else {
			outputtools.PrintlnColored("\nWhat's deployed (HTTP via Caddy):", outputtools.Green)
		}
		if boolValue(configValues, "grpc_server_enabled") {
			if useHTTPS {
				fmt.Printf("  gRPC API:  %s (TLS)\n", grpcTarget)
				fmt.Printf("             grpcurl %s list\n", grpcTarget)
			} else {
				fmt.Printf("  gRPC API:  %s (plaintext)\n", grpcTarget)
				fmt.Printf("             grpcurl -plaintext %s list\n", grpcTarget)
			}
		}
		if boolValue(configValues, "rest_enabled") {
			base := stringValue(configValues, "rest_base_path", "/v1")
			fmt.Printf("  REST API:  %s%s\n", publicURL, base)
			fmt.Printf("             curl %s%s/<entity>\n", publicURL, base)
		}
		if jwtAuth {
			fmt.Printf("  Auth:      jwt — data endpoints need a Bearer token.\n")
			fmt.Printf("             sign in: POST %s/signin {\"email\",\"password\"} (then /refresh, /validate)\n", publicURL)
			fmt.Printf("             a signing key was generated on the box; sign-in needs a user row in your user entity.\n")
		}
		fmt.Printf("  Info page: %s/\n", publicURL)
		if !useHTTPS {
			outputtools.PrintlnColoredErr("  (IP-only deploy over plain HTTP — pass --domain <name> for automatic HTTPS with a trusted cert.)", outputtools.Yellow)
		}
	}

	outputtools.PrintlnColored("\nManage your data:", outputtools.Green)
	fmt.Printf("  %s\n", dataManagerURL)
	if outcome.catalogPublished {
		fmt.Printf("  The connection is listed under \"Via agent\" — nuzur reaches it through the agent on this box,\n")
		fmt.Printf("  which dials out to nuzur. The database stays private; nothing is exposed to the internet.\n")
	}
	// Say only what actually failed. This block used to assert a cause ("the diff
	// step errored") that was wrong whenever the publish was what broke, and to
	// claim the agent connection was live in exactly the case where it wasn't.
	if s := outcome.summary(); s != "" {
		outputtools.PrintlnColoredErr("\n"+s, outcome.summaryColor())
	}
	printGateFollowUp(outcome)

	// Point the user at their editable app source (the workspace) — this is the
	// code that was deployed. Re-running deploy regenerates it in place, refreshing
	// generated code while keeping their custom endpoints, then ships it.
	if workspaceDir != "" {
		appDir := sourceRoot // the project dir (go.mod/Dockerfile); may be nested under the workspace
		if appDir == "" {
			appDir = workspaceDir
		}
		outputtools.PrintlnColored("\nYour app source:", outputtools.Green)
		fmt.Printf("  %s\n", appDir)
		fmt.Printf("  Re-run the same deploy to ship changes from here.\n")
		// Resolved, not the flag: the tip is about the code that was just generated.
		if boolValue(configValues, "custom_enabled") {
			fmt.Printf("  Add custom endpoints: edit app/grpc.go (override/extend gRPC) or app/rest.go\n")
			fmt.Printf("  (custom REST routes); add RPCs in app/idl/proto/custom.proto then run app/idl/proto/gen.sh.\n")
		}
		fmt.Printf("  Tip: run `git init` here (or commit) to track your changes and see what codegen\n")
		fmt.Printf("  refreshes each deploy — secrets are already covered by the generated .gitignore.\n")
	}

	// Optionally register a raw --db-dsn database as a team connection so the whole
	// team can use the data manager on it. Opt-in only (flag or TTY prompt), and
	// skipped for --connection (already a team connection) and self-hosted DBs
	// (unreachable from nuzur cloud). Best-effort — never fails the deploy.
	if s.SaveConnection && (!externalDB || fromConnection) {
		outputtools.PrintlnColoredErr("--save-connection applies only to an external --db-dsn deploy; ignoring.", outputtools.Yellow)
	}
	if externalDB && !fromConnection && shouldSaveTeamConnection(s.NoSaveConnection, s.SaveConnection) {
		i.saveTeamConnection(saveConnectionInput{
			TeamUUID:    targets.project.TeamUuid,
			ProjectName: targets.project.Name,
			Identifier:  identifier,
			Engine:      dbEngine,
			Host:        extHost,
			Port:        extPort,
			User:        extUser,
			Pass:        extPass,
			Name:        extName,
			Params:      extParams,
		})
	}

	// Last: a schema that did not reach the database exits non-zero — blocked or
	// failed — so CI does not go green on a box that is serving against a schema its
	// generated code no longer matches. Everything above has already printed, because
	// the deploy itself did happen.
	return exitCodeForOutcome(outcome)
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
				fmt.Println("No deployments.")
				return nil
			}
			for _, d := range deps {
				// .Local() on every row: records are persisted in UTC (see
				// SaveDeployment), so the conversion belongs here — once, uniformly —
				// rather than being inherited from whatever zone each record was built
				// in. Without it two records minutes apart could list hours apart.
				fmt.Printf("%s  %-10s  %s@%s  agent=%s  %s\n",
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
			if err := i.Login(); err != nil {
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
			if !c.Bool("skip-server") && !dep.Provisioning {
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
				runner := deploy.NewSSHRunner(target)
				runner.Sudo = c.Bool("sudo") || target.User != "root"
				outputtools.PrintlnColoredErr("Cleaning up the server (this project's service, container, config"+purgeSuffix(c.Bool("purge"))+")...", outputtools.Blue)
				if err := runner.RunScript(ctx, script); err != nil {
					outcome.Server = teardownFailed
					outputtools.PrintlnColoredErr(fmt.Sprintf("warning: server teardown failed (%v) — cleaning up nuzur state anyway. Re-run `nuzur-cli destroy %s` once the box is reachable, or use --skip-server.", err, id), outputtools.Yellow)
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
			if isLast && c.Bool("keep-vm") && dep.Provider != deploy.ProviderSSH && dep.Provider != "" {
				outcome.VM = vmKept
			}
			if isLast && !c.Bool("keep-vm") &&
				dep.Provider != deploy.ProviderSSH && dep.Provider != "" &&
				(dep.ProviderInstanceID != "" || dep.ProviderResourceName != "") {
				if provisioner, perr := deploy.NewProvisioner(dep.Provider); perr != nil {
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
						fmt.Printf("No %s server was created for this deployment — nothing to delete.\n", dep.Provider)
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
				fmt.Println(line)
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
	if prior == nil || provider != deploy.ProviderSSH {
		return
	}
	if prior.Provider == "" || prior.Provider == deploy.ProviderSSH {
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
