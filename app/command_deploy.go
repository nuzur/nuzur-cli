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
			cli.StringFlag{Name: "provider", Value: "ssh", Usage: "Deploy target provider (v1: ssh = bring-your-own-server)"},
			cli.StringFlag{Name: "host", Usage: "Target server IP/hostname (ssh provider)"},
			cli.StringFlag{Name: "user", Value: "root", Usage: "SSH user"},
			cli.StringFlag{Name: "ssh-key", Usage: "Path to an SSH private key (default: ssh-agent / ~/.ssh/config)"},
			cli.IntFlag{Name: "port", Value: 22, Usage: "SSH port"},
			cli.StringFlag{Name: "domain", Usage: "Domain pointing at the server — Caddy provisions a real HTTPS cert for it. Omit for IP-only (a self-signed cert is used)."},
			cli.StringFlag{Name: "project, p", Usage: "Project name or UUID"},
			cli.StringFlag{Name: "version", Usage: "Project version identifier or UUID (default: latest)"},
			cli.StringFlag{Name: "identifier", Usage: "Deployment identifier (names the DB/service/config on the box; default: from the go-code-gen config, or the project name for --db-only)"},
			cli.BoolFlag{Name: "db-only", Usage: "Database-only: install the DB engine (--db), pair the agent, register the connection, and apply the schema — but do NOT generate/build/run the app or Caddy. Manage the DB entirely through nuzur."},
			cli.StringFlag{Name: "db-dsn", Usage: "Use an EXISTING database instead of self-hosting one. MySQL DSN (user:pass@tcp(host:port)/db?params) or Postgres URL (postgres://user:pass@host:port/db?sslmode=require). The app + agent connect to it; MySQL install/creation is skipped."},
			cli.StringFlag{Name: "db-schema", Usage: "Postgres schema/namespace to target (default: public). Ignored for MySQL, where the database IS the schema."},
			cli.StringFlag{Name: "db", Value: "mysql", Usage: "Self-hosted database engine: mysql | postgres"},
			cli.StringFlag{Name: "api", Usage: "API surface to generate: rest | grpc | both. Pick by the consumer — REST for JS/web/browser clients, gRPC for Go/backend clients (leave unset to use the project's last/provided config)"},
			cli.StringFlag{Name: "auth", Usage: "Auth middleware: disabled | jwt | keycloak (leave unset to use the project's last/provided config)"},
			cli.BoolFlag{Name: "custom", Usage: "Generate the custom application layer (app package for custom endpoints)"},
			cli.StringFlag{Name: "config-file", Usage: "Path to a JSON go-code-gen config (else the last-used config for this project is reused)"},
			cli.StringFlag{Name: "cli-install-cmd", Usage: "Command to install the nuzur CLI on the box (must leave `nuzur` on PATH)"},
			cli.StringFlag{Name: "schema-push-extension", Value: "sql-push-local", Usage: "Identifier of the SQL-push extension used to auto-apply the schema"},
			cli.BoolFlag{Name: "sudo", Usage: "Run the bootstrap via sudo (auto-enabled for non-root SSH users; the box needs passwordless sudo)"},
			cli.StringFlag{Name: "web-url", Value: constants.WEB_PROD_URL, Usage: "nuzur web app base URL (for the data-manager deep link)"},
		},
		Subcommands: []cli.Command{i.DeployListCommand()},
		Action: func(c *cli.Context) error {
			return i.runDeploy(c)
		},
	}
}

func (i *Implementation) runDeploy(c *cli.Context) error {
	if deploy.Provider(c.String("provider")) != deploy.ProviderSSH {
		return fmt.Errorf("only the 'ssh' provider is supported in v1")
	}
	if strings.TrimSpace(c.String("host")) == "" {
		return fmt.Errorf("--host is required for the ssh provider")
	}
	ctx := context.Background()
	dbOnly := c.Bool("db-only")

	// --db-dsn: connect to an EXISTING database (local or remote, MySQL or
	// Postgres) instead of self-hosting one. Parse it up front so it can drive
	// the generated app's engine, the app config, and the agent connection.
	dbDSN := strings.TrimSpace(c.String("db-dsn"))
	externalDB := dbDSN != ""
	dbEngine := deploy.DBMySQL
	var extHost, extPort, extUser, extPass, extName, extParams string
	if externalDB {
		var perr error
		dbEngine, extHost, extPort, extUser, extPass, extName, extParams, perr = parseDeployDSN(dbDSN)
		if perr != nil {
			return fmt.Errorf("parsing --db-dsn: %w", perr)
		}
		if extName == "" {
			return fmt.Errorf("--db-dsn must include a database name")
		}
	} else if c.String("db") == "postgres" {
		// Self-hosted Postgres: install + provision PG on the box (parallels the
		// MySQL local tier). The engine drives the bootstrap install/create branch,
		// the app config driver, and the agent connection's --driver/--schema.
		dbEngine = deploy.DBPostgres
	}

	// 1. Resolve project/version + the go-code-gen extension (logs in).
	targets, err := i.resolveRunTargets(extRunFlags{
		project:        c.String("project"),
		version:        c.String("version"),
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

	// 2 + 3. Generate the app (skipped entirely for --db-only, which self-hosts
	// only the DB + agent and manages it through nuzur — no app, no code-gen
	// config required, so it works for any project).
	var configValues map[string]interface{}
	var sourceRoot string
	jwtAuth := false
	if !dbOnly {
		provided, err := loadDeployConfig(c.String("config-file"))
		if err != nil {
			return err
		}
		// dbEngine is authoritative (from --db, or inferred from --db-dsn). go-code-gen's
		// `db` config option uses "postgresql" (its DatabaseType enum) — distinct from the
		// runtime driver name "postgres" used in prod.yaml + the agent connection.
		provided["db"] = goCodeGenDBValue(dbEngine)
		provided["custom_enabled"] = c.Bool("custom")
		provided["dockerfile"] = true
		// Transport selection: pick REST for JS/web clients, gRPC for Go/backend
		// clients. Unset leaves the project's last/provided config untouched.
		switch c.String("api") {
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
			// leave to config-file / last-used / generator defaults
		default:
			return fmt.Errorf("--api must be one of: rest, grpc, both")
		}
		if a := c.String("auth"); a != "" {
			provided["auth"] = a
		}
		configValues, err = targets.er.BuildConfigFromJSON(targets.project, targets.projectVersion.Uuid, targets.configEntity, provided, targets.lastConfig)
		if err != nil {
			return fmt.Errorf("building generator config (pass --config-file, or run `nuzur go-code-gen` once): %w", err)
		}

		outDir, err := os.MkdirTemp("", "nuzur-deploy-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(outDir)
		outputtools.PrintlnColoredErr(i.localize.Localize("deploy_generating", "Generating application code..."), outputtools.Blue)
		if _, err := targets.er.Run(extensionrun.RunParams{
			Extension:          targets.extension,
			ExtensionVersion:   targets.extensionVersion,
			ProjectUUID:        targets.project.Uuid,
			ProjectVersionUUID: targets.projectVersion.Uuid,
			ConfigValues:       configValues,
			OutputPath:         outDir,
		}); err != nil {
			return fmt.Errorf("generating code: %w", err)
		}
		sourceRoot, err = findSourceRoot(outDir)
		if err != nil {
			return err
		}
		jwtAuth = generatedHasJWTAuth(sourceRoot)
	}

	// Identifier: --identifier override, else the go-code-gen config's identifier,
	// else (db-only) the sanitized project name.
	identifier := firstNonEmpty(c.String("identifier"), stringValue(configValues, "identifier", ""), sanitizeDBName(targets.project.Name))

	// The DB is registered as a named agent connection with this UUID, then
	// published to nuzur so the schema can be pushed to it. Self-hosted → a DB
	// named after the identifier with a least-priv `{db}_app` user; external
	// (--db-dsn) → the DB name + user from the DSN.
	dbName := sanitizeDBName(identifier)
	dbUser := dbName + "_app"
	if externalDB {
		dbName = extName
		dbUser = extUser
	}
	// Schema vs database: in MySQL the database IS the schema; in Postgres a
	// database contains schemas (default `public`). `schema` is what the diff
	// engine, the data-manager link, and the agent connection's default schema
	// target — the DB name for MySQL, a namespace for Postgres.
	schema := dbName
	dbSchema := "" // agent-connection default schema; empty for MySQL (chosen per query)
	if dbEngine == deploy.DBPostgres {
		schema = firstNonEmpty(c.String("db-schema"), "public")
		dbSchema = schema
	}
	connName := identifier + "-db"
	host := c.String("host")

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

	// 6. Provision (validate host) + preflight SSH.
	spec := deploy.Spec{
		Provider: deploy.ProviderSSH,
		Target: deploy.Target{
			Host: c.String("host"), User: c.String("user"),
			Port: c.Int("port"), KeyPath: c.String("ssh-key"),
		},
		Identifier:         identifier,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		DBEngine:           dbEngine,
		ProvisioningToken:  tokRes.GetProvisioningToken(),
		SourceDir:          sourceRoot,
	}
	provisioner := deploy.NewSSHProvisioner()
	target, err := provisioner.Provision(ctx, spec)
	if err != nil {
		return err
	}
	runner := deploy.NewSSHRunner(target)
	// Non-root SSH users need sudo for the privileged bootstrap steps.
	runner.Sudo = c.Bool("sudo") || target.User != "root"
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
		CLIInstallCmd:     c.String("cli-install-cmd"),
		ConnUUID:          connUUID,
		ConnName:          connName,
		Domain:            c.String("domain"),
		Host:              host,
	}
	if !dbOnly {
		bp.RemoteSrcDir = remoteSrc
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
	if err := runner.RunScript(ctx, script); err != nil {
		return err
	}

	// 9. Verify the agent connected. First deploy → a new agent UUID appears;
	// re-deploy → the existing (reused) agent should come back ONLINE.
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_verifying", "Waiting for the agent to connect..."), outputtools.Blue)
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
	if err := i.publishAndApplySchema(targets, agentUUID, connUUID, connName, dbEngine, schema, c.String("schema-push-extension")); err != nil {
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
			if c.String("domain") != "" {
				publicURL = "https://" + c.String("domain")
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
		strings.TrimRight(c.String("web-url"), "/"),
		targets.project.Uuid, targets.projectVersion.Uuid,
		agentUUID, connUUID, url.QueryEscape(schema),
	)

	// 12. Record state. A re-deploy updates the existing record in place (same
	// ID) rather than accumulating a new one per deploy.
	depID := identifier + "-" + shortID()
	if prior != nil {
		depID = prior.ID
	}
	dep := &deploy.Deployment{
		ID:                 depID,
		Provider:           deploy.ProviderSSH,
		Host:               target.Host, User: target.User, Port: target.Port,
		Identifier:         identifier,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		LocalAgentUUID:     agentUUID,
		ConnUUID:           connUUID,
		DBEngine:           dbEngine,
		ExternalDB:         externalDB,
		Domain:             c.String("domain"),
		APIURL:             publicURL,
		PublicURL:          publicURL,
		DataManagerURL:     dataManagerURL,
		CreatedAt:          time.Now(),
	}
	if err := deploy.SaveDeployment(dep); err != nil {
		return err
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

func (i *Implementation) publishAndApplySchema(targets *runTargets, agentUUID, connUUID, connName string, dbEngine deploy.DBEngine, schema, sqlPushExtID string) error {
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
		ConfigValues: map[string]interface{}{
			"local_agent":            agentUUID,
			"local_agent_connection": connUUID,
			"local_agent_schema":     schema,
		},
		AutoConfirmSteps: true,
		OutputPath:       outDir,
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

			// 3. Remove local deployment state.
			if err := deploy.DeleteDeployment(id); err != nil {
				return err
			}
			if isLast {
				fmt.Printf("Destroyed deployment %s (server cleaned up, shared agent revoked — last project on the box).\n", id)
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
