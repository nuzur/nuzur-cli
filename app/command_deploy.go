package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/urfave/cli"
)

func (i *Implementation) DeployCommand() cli.Command {
	return cli.Command{
		Name:  "deploy",
		Usage: i.localize.Localize("deploy_desc", "Deploy a project to a server: self-host its database and pair it back to nuzur"),
		Flags: []cli.Flag{
			cli.StringFlag{Name: "provider", Value: "ssh", Usage: "Where to deploy: ssh (bring-your-own-server), or a managed provider that creates the VM for you — digitalocean | hetzner (more coming). Managed providers shell out to your already-authenticated provider CLI."},
			cli.StringFlag{Name: "host", Usage: "Target server IP/hostname (ssh provider)"},
			cli.StringFlag{Name: "region", Usage: "Managed providers: region/location to create the VM in (e.g. nyc3, fra1 for DigitalOcean; nbg1, fsn1 for Hetzner)"},
			cli.StringFlag{Name: "size", Usage: "Managed providers: instance size/type (default: a small instance per provider)"},
			cli.StringFlag{Name: "image", Usage: "Managed providers: OS image (default: Ubuntu 22.04)"},
			cli.StringFlag{Name: "ssh-key-name", Usage: "Managed providers: name/id of an SSH key already registered with the provider. Omit to upload the public half of --ssh-key (or your default ~/.ssh key)."},
			cli.StringFlag{Name: "user", Value: "root", Usage: "SSH user"},
			cli.StringFlag{Name: "ssh-key", Usage: "Path to an SSH private key (default: ssh-agent / ~/.ssh/config)"},
			cli.IntFlag{Name: "port", Value: 22, Usage: "SSH port"},
			cli.StringFlag{Name: "domain", Usage: "Domain pointing at the server — Caddy provisions a real HTTPS cert for it. Omit for IP-only (a self-signed cert is used)."},
			cli.StringFlag{Name: "project, p", Usage: "Project name or UUID"},
			cli.StringFlag{Name: "version", Usage: "Project version identifier or UUID (default: latest)"},
			cli.StringFlag{Name: "identifier", Usage: "Deployment identifier (names the DB/service/config on the box; default: from the go-code-gen config, or the project name for --db-only)"},
			cli.BoolFlag{Name: "db-only", Usage: "Database-only: install the DB engine (--db), pair the agent, register the connection, and apply the schema — but do NOT generate/build/run the app or Caddy. Manage the DB entirely through nuzur."},
			cli.StringFlag{Name: "db-dsn", Usage: "Use an EXISTING database instead of self-hosting one. MySQL DSN (user:pass@tcp(host:port)/db?params) or Postgres URL (postgres://user:pass@host:port/db?sslmode=require). The app + agent connect to it; MySQL install/creation is skipped."},
			cli.StringFlag{Name: "connection", Usage: "Deploy against an EXISTING nuzur team connection (by UUID) instead of --db-dsn. The DSN is resolved server-side from the connection's stored credentials — no plaintext secret on the command line. Mutually exclusive with --db-dsn."},
			cli.BoolFlag{Name: "save-connection", Usage: "After an external (--db-dsn) deploy, register the database as a team connection so your team can use the data manager on it. Requires a team admin. (Non-interactive opt-in; a TTY otherwise prompts.)"},
			cli.BoolFlag{Name: "no-save-connection", Usage: "Never prompt to save the deployed external database as a team connection."},
			cli.StringFlag{Name: "db-schema", Usage: "Postgres schema/namespace to target (default: public). Ignored for MySQL, where the database IS the schema."},
			cli.StringFlag{Name: "db", Value: "mysql", Usage: "Self-hosted database engine: mysql | postgres"},
			cli.StringFlag{Name: "api", Usage: "API surface to generate: rest | grpc | both. Pick by the consumer — REST for JS/web/browser clients, gRPC for Go/backend clients (leave unset to use the project's last/provided config)"},
			cli.StringFlag{Name: "auth", Usage: "Auth middleware: disabled | jwt | keycloak (leave unset to use the project's last/provided config)"},
			cli.BoolFlag{Name: "custom", Usage: "Generate the custom application layer (app package for custom endpoints)"},
			cli.StringFlag{Name: "source-dir", Usage: "Directory for the app's source (the workspace deploy generates + builds from; you edit custom endpoints here). Default: ./nuzur-<identifier>. Re-deploys reuse it and preserve your edits."},
			cli.StringFlag{Name: "deploy-config", Usage: "Path to a JSON deploy spec describing the whole deploy (topology + a nested `codegen` block); use '-' to read from stdin. Explicit flags override values in the file. Build or generate one from nuzur web."},
			cli.BoolFlag{Name: "print-config", Usage: "Print the effective deploy config (as JSON) resolved from flags + --deploy-config, then exit without deploying. Use it to snapshot an invocation into a reusable deploy-config file."},
			cli.StringFlag{Name: "gen-config", Usage: "Path to a JSON go-code-gen config (overrides the deploy-config's `codegen` block; else the last-used config for this project is reused)"},
			cli.StringFlag{Name: "cli-install-cmd", Usage: "Command to install the nuzur CLI on the box (must leave `nuzur` on PATH)"},
			cli.BoolFlag{Name: "sudo", Usage: "Run the bootstrap via sudo (auto-enabled for non-root SSH users; the box needs passwordless sudo)"},
			cli.StringFlag{Name: "web-url", Value: constants.WEB_PROD_URL, Usage: "nuzur web app base URL (for the data-manager deep link)"},
		},
		Subcommands: []cli.Command{i.DeployListCommand()},
		Action: func(c *cli.Context) error {
			return i.runDeploy(c)
		},
	}
}

func (i *Implementation) runDeploy(c *cli.Context) (rerr error) {
	// Set once the deploy is recorded in nuzur (right after the box exists). If
	// anything fails after that, mark the revision FAILED with the error — a broken
	// deploy should be visible in nuzur, not look like it never happened.
	var deployRevUUID string
	defer func() {
		if rerr != nil && deployRevUUID != "" {
			i.updateDeployRevision(context.Background(), deployRevUUID,
				nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_FAILED, rerr.Error())
		}
	}()

	// Resolve the effective settings from the --deploy-config file merged with the
	// CLI flags (explicit flags win). Everything below reads from `s`.
	s, err := resolveDeploySettings(c)
	if err != nil {
		return err
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
	if provider != deploy.ProviderSSH && strings.TrimSpace(s.Region) == "" {
		return fmt.Errorf("--region is required for the %s provider", provider)
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
	}, resolveOptions{
		extensionIdentifier: goCodeGenExtensionIdentifier,
		interactive:         false,
		checkAccess:         true,
		checkLimit:          true,
	})
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
		provided["custom_enabled"] = s.Custom
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
		configValues, err = targets.er.BuildConfigFromJSON(targets.project, targets.projectVersion.Uuid, targets.configEntity, provided, targets.lastConfig)
		if err != nil {
			return fmt.Errorf("building generator config (pass --config-file, or run `nuzur go-code-gen` once): %w", err)
		}
		// Generation happens below (step 2), once the identifier + any prior
		// deployment are known — so it targets the persistent workspace.
	}

	// Identifier: --identifier override, else the go-code-gen config's identifier,
	// else (db-only) the sanitized project name.
	identifier := firstNonEmpty(s.Identifier, stringValue(configValues, "identifier", ""), sanitizeDBName(targets.project.Name))

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
	schema := dbName
	dbSchema := "" // agent-connection default schema; empty for MySQL (chosen per query)
	if dbEngine == deploy.DBPostgres {
		schema = firstNonEmpty(s.DBSchema, "public")
		dbSchema = schema
	}
	connName := identifier + "-db"
	host := s.Host

	// Multi-project on one box: the box has ONE shared agent (reused for every
	// project on it — box-level), while the connection UUID + deployment record
	// are per-project (host+identifier).
	prior := findPriorDeployment(host, identifier)
	// Guard: refuse if this identifier on this host maps to a DIFFERENT project —
	// they'd share the derived DB name/user and collide. Require a distinct id.
	if prior != nil && prior.ProjectUUID != "" && prior.ProjectUUID != targets.project.Uuid {
		return fmt.Errorf("host %s already runs a different project under identifier %q (project %s) — deploy the new project under a distinct identifier", host, identifier, prior.ProjectUUID)
	}
	reuseAgentUUID := findBoxAgent(host)
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
	// so an interrupted deploy still leaves something `nuzur destroy` can clean up.
	depID := identifier + "-" + shortID()
	if prior != nil {
		depID = prior.ID
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
	}
	if provider != deploy.ProviderSSH {
		outputtools.PrintlnColoredErr("Creating the server on "+string(provider)+" (this can take a minute)...", outputtools.Blue)
	}
	prov, err := provisioner.Provision(ctx, spec)
	if err != nil {
		return err
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
	// `nuzur destroy <id>` can tear the VM down instead of orphaning a billing
	// server nuzur has no memory of. Step 12 fills in the rest (agent, URLs).
	dep := &deploy.Deployment{
		ID:                 depID,
		Provider:           provider,
		ProviderInstanceID: prov.InstanceID,
		Region:             prov.Region,
		Host:               target.Host, User: target.User, Port: target.Port,
		Identifier:         identifier,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		ConnUUID:           connUUID,
		DBEngine:           dbEngine,
		ExternalDB:         externalDB,
		SourceDir:          workspaceDir,
		Domain:             s.Domain,
		CreatedAt:          time.Now(),
	}
	if prior != nil {
		dep.CreatedAt = prior.CreatedAt
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
		Provider:       provider,
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
		Custom:         s.Custom,
		SourceDir:      workspaceDir,
		Status:         nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS,
		StatusMessage:  "server ready — bootstrapping",
	}
	if rev, err := i.reportDeployment(ctx, reportIn); err != nil {
		outputtools.PrintlnColoredErr("Deploy not reported to nuzur (continuing): "+err.Error(), outputtools.Yellow)
	} else {
		deployRevUUID = rev
	}

	// Restrict inbound at the provider level to mirror the box's ufw (SSH + the
	// Caddy front doors). Best-effort — the on-box ufw is the authoritative gate,
	// so a firewall hiccup must not fail an otherwise-good deploy. No-op for BYO-SSH.
	if provider != deploy.ProviderSSH {
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
	// releases itself.
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
		ConnUUID:          connUUID,
		ConnName:          connName,
		Domain:            s.Domain,
		Host:              host,
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
	i.updateDeployRevision(ctx, deployRevUUID,
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, bootMsg)
	if err := runner.RunScript(ctx, script); err != nil {
		return err
	}

	// 9. Verify the agent connected. First deploy → a new agent UUID appears;
	// re-deploy → the existing (reused) agent should come back ONLINE.
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_verifying", "Waiting for the agent to connect..."), outputtools.Blue)
	i.updateDeployRevision(ctx, deployRevUUID,
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

	// 10. Publish the connection catalog (needs the user token — the box can't)
	// and auto-apply the schema to the empty DB.
	schemaApplied := true
	i.updateDeployRevision(ctx, deployRevUUID,
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, "applying the schema to the database")
	if err := i.publishAndApplySchema(targets, agentUUID, connUUID, connName, dbEngine, schema, connFlag, connStore); err != nil {
		schemaApplied = false
		outputtools.PrintlnColoredErr("Schema auto-apply skipped: "+err.Error(), outputtools.Yellow)
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
	reportIn.RevisionUUID = deployRevUUID
	reportIn.ImageName = imageName // built by now — safe to pin in the history
	reportIn.Status = nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_ACTIVE
	reportIn.StatusMessage = ""
	if _, err := i.reportDeployment(ctx, reportIn); err != nil {
		outputtools.PrintlnColoredErr("Deployment recorded locally but not reported to nuzur: "+err.Error(), outputtools.Yellow)
	}

	// 13. Report.
	outputtools.PrintlnColored("\nDeployment complete.", outputtools.Green)
	fmt.Printf("  deployment id: %s\n", dep.ID)
	fmt.Printf("  agent uuid:    %s\n", agentUUID)
	fmt.Printf("  connection:    %s (%s)\n", connName, connUUID)
	if externalDB {
		fmt.Printf("  database:      external %s at %s:%s/%s (not self-hosted; kept on destroy)\n", dbEngine, extHost, extPort, dbName)
	}
	fmt.Printf("  teardown:      nuzur destroy %s\n", dep.ID)

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
		fmt.Printf("  No app/API or Caddy was installed. Add one later with a normal deploy.\n")
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
	if !schemaApplied {
		// Auto-apply is supported for both engines over the agent; if it was
		// skipped it's because the diff step errored (see the message above).
		// The DB + agent connection are live either way — retry, or apply the
		// schema from nuzur (SQL Push / change request).
		outputtools.PrintlnColoredErr("\nSchema auto-apply was skipped (see the error above). The database + agent connection are live — re-run the deploy to retry, or apply the schema from nuzur (SQL Push / change request).", outputtools.Yellow)
	}

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
		if s.Custom {
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
	return nil
}

// publishAndApplySchema publishes the localhost DB as a connection on the paired
// agent (using the user's token, which the headless box lacks), then runs the
// SQL-push extension to create the schema on the empty database.
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
// deployment's topology, never configured — see publishAndApplySchema.
const (
	sqlPushLocalExtensionIdentifier = "sql-push-local" // via the box's agent
	sqlPushExtensionIdentifier      = "sql-push"       // direct from nuzur to a team connection
)

// publishAndApplySchema publishes the box's DB as a named agent connection (so
// nuzur can serve it in the data manager) and then applies the project's schema to
// the freshly-provisioned, empty database.
//
// teamConnUUID/teamConnStore are set only for a --connection deploy (an existing
// team connection).
func (i *Implementation) publishAndApplySchema(targets *runTargets, agentUUID, connUUID, connName string, dbEngine deploy.DBEngine, schema, teamConnUUID, teamConnStore string) error {
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

	// Pick the push path from the topology, not from a flag. A self-hosted (or raw
	// --db-dsn) database lives behind the box, so it's only reachable through the
	// agent → sql-push-local. An existing team connection is reachable from nuzur
	// directly, so push remotely → sql-push, which keeps the shared connection in
	// sync the same way any other schema change to it would.
	sqlPushExtID := sqlPushLocalExtensionIdentifier
	configValues := map[string]interface{}{
		"local_agent":            agentUUID,
		"local_agent_connection": connUUID,
		"local_agent_schema":     schema,
	}
	if teamConnUUID != "" {
		sqlPushExtID = sqlPushExtensionIdentifier
		configValues = map[string]interface{}{
			"store":      teamConnStore,
			"connection": teamConnUUID,
			"schema":     schema,
		}
	}

	ext, err := targets.er.FindExtensionByIdentifier(sqlPushExtID)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", sqlPushExtID, err)
	}
	ver, err := targets.er.GetLatestExtensionVersion(ext.Uuid)
	if err != nil {
		return err
	}
	outDir, err := os.MkdirTemp("", "nuzur-sqlpush-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(outDir)

	outputtools.PrintlnColoredErr("Applying schema to the new database...", outputtools.Blue)
	_, err = targets.er.Run(extensionrun.RunParams{
		Extension:          ext,
		ExtensionVersion:   ver,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		ConfigValues:       configValues,
		AutoConfirmSteps:   true,
		OutputPath:         outDir,
	})
	return err
}

func (i *Implementation) DeployListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: i.localize.Localize("deploy_list_desc", "List deployments created on this machine"),
		Action: func(c *cli.Context) error {
			deps, err := deploy.ListDeployments()
			if err != nil {
				return err
			}
			if len(deps) == 0 {
				fmt.Println("No deployments.")
				return nil
			}
			for _, d := range deps {
				fmt.Printf("%s  %-10s  %s@%s  agent=%s  %s\n",
					d.ID, d.Provider, d.User, d.Host, d.LocalAgentUUID, d.CreatedAt.Format("2006-01-02 15:04"))
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
			cli.BoolFlag{Name: "skip-server", Usage: "Only revoke the agent + remove local state; leave the server untouched"},
			cli.BoolFlag{Name: "keep-vm", Usage: "For a managed-provider deployment, keep the created VM instead of deleting it (default: the VM nuzur created is deleted when the last project on it is destroyed)"},
		},
		Action: func(c *cli.Context) error {
			if !c.Args().Present() {
				return fmt.Errorf("missing deployment-id (see `nuzur deploy list`)")
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

			// A box can host multiple projects on one shared agent. This is the
			// LAST project on the box iff no other deployment record shares its
			// host. Only then do we tear down the shared agent + revoke it — while
			// other projects are live, the agent must survive.
			isLast := true
			if deps, e := deploy.ListDeployments(); e == nil {
				for _, d := range deps {
					if d.ID != id && d.Host == dep.Host {
						isLast = false
						break
					}
				}
			}

			// 1. Server teardown: remove THIS project's artifacts (its service,
			// container, image, /etc/nuzur/{id}, Caddy snippet, cron, connection);
			// the shared agent + Caddy root go only when isLast. Best-effort — a
			// gone/unreachable box still lets the cloud-side cleanup proceed.
			if !c.Bool("skip-server") {
				dbName := sanitizeDBName(dep.Identifier)
				// Never drop an EXTERNAL (--db-dsn) database — it's the user's own
				// managed/remote DB, not something we provisioned.
				purge := c.Bool("purge")
				if purge && dep.ExternalDB {
					purge = false
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
					outputtools.PrintlnColoredErr(fmt.Sprintf("warning: server teardown failed (%v) — cleaning up nuzur state anyway. Re-run `nuzur destroy %s` once the box is reachable, or use --skip-server.", err, id), outputtools.Yellow)
				}
			}

			// 2. Cloud-side agent cleanup.
			if dep.LocalAgentUUID != "" {
				authCtx, err := productclient.ClientContext()
				if err != nil {
					return err
				}
				if isLast {
					// Last project on the box → revoke the shared agent.
					if _, err := i.productClient.ProductClient.RevokeLocalAgent(authCtx, &pb.RevokeLocalAgentRequest{
						LocalAgentUuid: dep.LocalAgentUUID,
					}); err != nil {
						outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not revoke agent %s: %v", dep.LocalAgentUUID, err), outputtools.Yellow)
					}
				} else {
					// Other projects survive → keep the agent, but re-publish the
					// remaining connections so this project's drops out of the catalog.
					conns := []*nemgen.LocalAgentConnection{}
					if deps, e := deploy.ListDeployments(); e == nil {
						for _, d := range deps {
							if d.ID != id && d.LocalAgentUUID == dep.LocalAgentUUID && d.ConnUUID != "" {
								conns = append(conns, &nemgen.LocalAgentConnection{
									Uuid:   d.ConnUUID,
									Name:   d.Identifier + "-db",
									DbType: agentConnDbType(d.DBEngine),
								})
							}
						}
					}
					if _, err := i.productClient.ProductClient.UpdateLocalAgentConnections(authCtx, &pb.UpdateLocalAgentConnectionsRequest{
						LocalAgentUuid: dep.LocalAgentUUID,
						Connections:    conns,
					}); err != nil {
						outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not refresh agent connections: %v", err), outputtools.Yellow)
					}
				}
			}

			// 2b. Mark the cloud-side deployment record DESTROYED (kept as
			// history). Best-effort — a stale row is preferable to failing the
			// destroy; the local state removal below is what matters.
			if authCtx, err := productclient.ClientContext(); err == nil {
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
			vmDeleted := false
			if isLast && !c.Bool("keep-vm") && !c.Bool("skip-server") &&
				dep.Provider != deploy.ProviderSSH && dep.Provider != "" && dep.ProviderInstanceID != "" {
				if provisioner, perr := deploy.NewProvisioner(dep.Provider); perr != nil {
					outputtools.PrintlnColoredErr(fmt.Sprintf("warning: cannot delete the %s VM (%v) — delete instance %s manually.", dep.Provider, perr, dep.ProviderInstanceID), outputtools.Yellow)
				} else {
					outputtools.PrintlnColoredErr(fmt.Sprintf("Deleting the %s VM (instance %s)...", dep.Provider, dep.ProviderInstanceID), outputtools.Blue)
					prov := deploy.Provisioned{
						Target:     deploy.Target{Host: dep.Host, User: dep.User, Port: dep.Port, KeyPath: c.String("ssh-key")},
						InstanceID: dep.ProviderInstanceID,
						Region:     dep.Region,
					}
					if err := provisioner.Destroy(ctx, prov); err != nil {
						outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not delete the %s VM %s (%v) — delete it manually to avoid charges.", dep.Provider, dep.ProviderInstanceID, err), outputtools.Yellow)
					} else {
						vmDeleted = true
					}
				}
			}

			// 3. Remove local deployment state.
			if err := deploy.DeleteDeployment(id); err != nil {
				return err
			}
			if isLast {
				if vmDeleted {
					fmt.Printf("Destroyed deployment %s (VM deleted, shared agent revoked — last project on the box).\n", id)
				} else {
					fmt.Printf("Destroyed deployment %s (server cleaned up, shared agent revoked — last project on the box).\n", id)
				}
			} else {
				fmt.Printf("Destroyed deployment %s (this project removed; the box's shared agent stays for its other projects).\n", id)
			}
			if !c.Bool("purge") && !c.Bool("skip-server") {
				fmt.Printf("  The database was kept — pass --purge to drop it.\n")
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

// findPriorDeployment returns the most recent recorded deployment for this
// host+identifier, or nil. Used to detect a re-deploy so the existing agent and
// connection are reused instead of pairing a fresh agent.
func findPriorDeployment(host, identifier string) *deploy.Deployment {
	deps, err := deploy.ListDeployments()
	if err != nil {
		return nil
	}
	var match *deploy.Deployment
	for idx := range deps {
		d := deps[idx]
		if d.Host == host && d.Identifier == identifier && d.LocalAgentUUID != "" {
			if match == nil || d.CreatedAt.After(match.CreatedAt) {
				m := d
				match = &m
			}
		}
	}
	return match
}

// findBoxAgent returns the local-agent UUID already paired on this host (any
// project's deployment), or "". A box has ONE shared agent serving all its
// projects, so a second project reuses it rather than pairing a new one.
func findBoxAgent(host string) string {
	deps, err := deploy.ListDeployments()
	if err != nil {
		return ""
	}
	var latest *deploy.Deployment
	for idx := range deps {
		d := deps[idx]
		if d.Host == host && d.LocalAgentUUID != "" {
			if latest == nil || d.CreatedAt.After(latest.CreatedAt) {
				m := d
				latest = &m
			}
		}
	}
	if latest == nil {
		return ""
	}
	return latest.LocalAgentUUID
}

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
		params = u.RawQuery
		if params == "" {
			params = "sslmode=require"
		}
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
	params = "parseTime=true"
	if q := strings.LastIndex(dsn, "?"); q >= 0 {
		params = dsn[q+1:]
	}
	return deploy.DBMySQL, host, port, cfg.User, cfg.Passwd, cfg.DBName, params, nil
}

// resolveWorkspace picks the persistent app-source directory deploy generates
// into and builds from: --source-dir if given, else the prior deployment's
// recorded dir (so a re-deploy reuses it without re-passing the flag), else the
// default ./nuzur-<identifier>. The path is returned absolute.
func resolveWorkspace(flagDir string, prior *deploy.Deployment, identifier string) (string, error) {
	dir := strings.TrimSpace(flagDir)
	if dir == "" && prior != nil {
		dir = prior.SourceDir
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
