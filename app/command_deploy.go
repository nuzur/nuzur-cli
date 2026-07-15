package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

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
			cli.StringFlag{Name: "db", Value: "mysql", Usage: "Self-hosted database engine (v1: mysql)"},
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

	// 2. Build the go-code-gen config (reuse last/provided; force deploy-friendly values).
	provided, err := loadDeployConfig(c.String("config-file"))
	if err != nil {
		return err
	}
	provided["db"] = c.String("db")
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
	configValues, err := targets.er.BuildConfigFromJSON(targets.project, targets.projectVersion.Uuid, targets.configEntity, provided, targets.lastConfig)
	if err != nil {
		return fmt.Errorf("building generator config (pass --config-file, or run `nuzur go-code-gen` once): %w", err)
	}

	// 3. Generate the app source into a temp dir.
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
	sourceRoot, err := findSourceRoot(outDir)
	if err != nil {
		return err
	}

	identifier := stringValue(configValues, "identifier", targets.project.Name)
	// The generated app binds whatever the generated config/base.yaml says, and
	// the generator's default (6009) has drifted from any hardcoded fallback
	// here — so read the ports back from the generated source. Caddy's routes
	// MUST match these exactly or the gRPC/HTTP proxy hits a dead port. Fall
	// back to the extension config, then to the generator's own defaults.
	genGRPCPort, genHTTPPort := readGeneratedPorts(sourceRoot)
	jwtAuth := generatedHasJWTAuth(sourceRoot)
	grpcPort := firstNonEmpty(genGRPCPort, stringValue(configValues, "grpc_port", ""), "6009")
	// The HTTP port is always bound by the generated app (REST server, or the
	// httpServer fallback that also serves JWT auth + the info page), so always
	// expose it — not only when REST is enabled.
	httpPort := firstNonEmpty(genHTTPPort, stringValue(configValues, "http_port", ""), "8080")

	// The localhost DB is registered as a named agent connection with this
	// UUID (locally on the box), then published to nuzur from here so the
	// schema can be pushed to it.
	dbName := sanitizeDBName(identifier)
	connName := identifier + "-db"
	connU, err := uuid.NewV4()
	if err != nil {
		return err
	}
	connUUID := connU.String()

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
		DBEngine:           deploy.DBMySQL,
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
	// user, which may be non-root; the sudo bootstrap builds from here).
	const remoteSrc = "/tmp/nuzur-src"
	if err := runner.RunCommand(ctx, "rm -rf "+remoteSrc); err != nil {
		return err
	}
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_copying", "Copying source to the server..."), outputtools.Blue)
	if err := runner.CopyDir(ctx, sourceRoot, remoteSrc); err != nil {
		return err
	}

	// 8. Render + run the bootstrap.
	// Empty cli-install-cmd → the bootstrap installs the nuzur CLI from GitHub
	// releases itself.
	script, err := deploy.RenderBootstrap(deploy.BootstrapParams{
		Identifier:        identifier,
		DBEngine:          deploy.DBMySQL,
		DBName:            dbName,
		DBUser:            dbName + "_app",
		GRPCPort:          grpcPort,
		HTTPPort:          httpPort,
		GRPCEnabled:       boolValue(configValues, "grpc_server_enabled"),
		JWTAuth:           jwtAuth,
		RemoteSrcDir:      remoteSrc,
		ProvisioningToken: tokRes.GetProvisioningToken(),
		CLIInstallCmd:     c.String("cli-install-cmd"),
		ConnUUID:          connUUID,
		ConnName:          connName,
		Domain:            c.String("domain"),
	})
	if err != nil {
		return err
	}
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_bootstrapping", "Bootstrapping the server (Docker, MySQL, build, pairing)..."), outputtools.Blue)
	if err := runner.RunScript(ctx, script); err != nil {
		return err
	}

	// 9. Verify the agent paired + connected (a new agent UUID appears).
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_verifying", "Waiting for the agent to connect..."), outputtools.Blue)
	agentUUID, online, err := i.waitForNewOnlineAgent(existing, 150*time.Second)
	if err != nil {
		return err
	}
	if !online {
		outputtools.PrintlnColoredErr("Agent registered but not observed online yet; schema auto-apply may fail until it connects.", outputtools.Yellow)
	}

	// 10. Publish the connection catalog (needs the user token — the box can't)
	// and auto-apply the schema to the empty DB.
	schemaApplied := true
	if err := i.publishAndApplySchema(targets, agentUUID, connUUID, connName, dbName, c.String("schema-push-extension")); err != nil {
		schemaApplied = false
		outputtools.PrintlnColoredErr("Schema auto-apply skipped: "+err.Error(), outputtools.Yellow)
	}

	// Everything is fronted by Caddy. With a --domain, Caddy serves HTTPS on 443
	// with a real (Let's Encrypt) cert; without one, an IP-only box serves plain
	// HTTP on 80 — no self-signed cert, so no browser/tooling TLS warnings.
	pubHost := target.Host
	useHTTPS := c.String("domain") != ""
	if useHTTPS {
		pubHost = c.String("domain")
	}
	scheme := "http"
	if useHTTPS {
		scheme = "https"
	}

	// 11. Build the data-manager deep link (opens the deployed DB directly,
	// with the local-agent connection preselected).
	dataManagerURL := fmt.Sprintf(
		"%s/project/data-manager/%s/%s?mode=local&localAgent=%s&localAgentConn=%s&schema=%s",
		strings.TrimRight(c.String("web-url"), "/"),
		targets.project.Uuid, targets.projectVersion.Uuid,
		agentUUID, connUUID, url.QueryEscape(dbName),
	)

	// 12. Record state.
	dep := &deploy.Deployment{
		ID:                 identifier + "-" + shortID(),
		Provider:           deploy.ProviderSSH,
		Host:               target.Host, User: target.User, Port: target.Port,
		Identifier:         identifier,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		LocalAgentUUID:     agentUUID,
		DBEngine:           deploy.DBMySQL,
		APIURL:             fmt.Sprintf("%s://%s", scheme, pubHost),
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
	fmt.Printf("  teardown:      nuzur destroy %s\n", dep.ID)

	// What's deployed: everything is fronted by Caddy (HTTPS/443 with a domain,
	// otherwise plain HTTP/80).
	if useHTTPS {
		outputtools.PrintlnColored("\nWhat's deployed (HTTPS via Caddy on 443):", outputtools.Green)
	} else {
		outputtools.PrintlnColored("\nWhat's deployed (HTTP via Caddy on 80):", outputtools.Green)
	}
	if boolValue(configValues, "grpc_server_enabled") {
		if useHTTPS {
			fmt.Printf("  gRPC API:  %s:443 (TLS)\n", pubHost)
			fmt.Printf("             grpcurl %s:443 list\n", pubHost)
		} else {
			fmt.Printf("  gRPC API:  %s:80 (plaintext)\n", pubHost)
			fmt.Printf("             grpcurl -plaintext %s:80 list\n", pubHost)
		}
	}
	if boolValue(configValues, "rest_enabled") {
		base := stringValue(configValues, "rest_base_path", "/v1")
		fmt.Printf("  REST API:  %s://%s%s\n", scheme, pubHost, base)
		fmt.Printf("             curl %s://%s%s/<entity>\n", scheme, pubHost, base)
	}
	if jwtAuth {
		fmt.Printf("  Auth:      jwt — data endpoints need a Bearer token.\n")
		fmt.Printf("             sign in: POST %s://%s/signin {\"email\",\"password\"} (then /refresh, /validate)\n", scheme, pubHost)
		fmt.Printf("             a signing key was generated on the box; sign-in needs a user row in your user entity.\n")
	}
	fmt.Printf("  Info page: %s://%s/\n", scheme, pubHost)
	if !useHTTPS {
		outputtools.PrintlnColoredErr("  (IP-only deploy over plain HTTP — pass --domain <name> for automatic HTTPS with a trusted cert.)", outputtools.Yellow)
	}

	outputtools.PrintlnColored("\nManage your data:", outputtools.Green)
	fmt.Printf("  %s\n", dataManagerURL)
	if !schemaApplied {
		outputtools.PrintlnColoredErr("\nApply the schema manually in nuzur (SQL Push / change request) to create the tables.", outputtools.Yellow)
	}
	return nil
}

// publishAndApplySchema publishes the localhost DB as a connection on the paired
// agent (using the user's token, which the headless box lacks), then runs the
// SQL-push extension to create the schema on the empty database.
func (i *Implementation) publishAndApplySchema(targets *runTargets, agentUUID, connUUID, connName, schema, sqlPushExtID string) error {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	if _, err := i.productClient.ProductClient.UpdateLocalAgentConnections(authCtx, &pb.UpdateLocalAgentConnectionsRequest{
		LocalAgentUuid: agentUUID,
		Connections: []*nemgen.LocalAgentConnection{{
			Uuid:   connUUID,
			Name:   connName,
			DbType: nemgen.LocalAgentConnectionDbType_LOCAL_AGENT_CONNECTION_DB_TYPE_MYSQL,
		}},
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
		Usage:     i.localize.Localize("destroy_desc", "Tear down a deployment: revoke its agent and remove local state"),
		ArgsUsage: "<deployment-id>",
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
			// Revoke the agent so no ghost row remains.
			if dep.LocalAgentUUID != "" {
				authCtx, err := productclient.ClientContext()
				if err != nil {
					return err
				}
				if _, err := i.productClient.ProductClient.RevokeLocalAgent(authCtx, &pb.RevokeLocalAgentRequest{
					LocalAgentUuid: dep.LocalAgentUUID,
				}); err != nil {
					outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not revoke agent %s: %v", dep.LocalAgentUUID, err), outputtools.Yellow)
				}
			}
			// Provider teardown (no-op for BYO-SSH — the user owns the box).
			_ = deploy.NewSSHProvisioner().Destroy(context.Background(), deploy.Target{Host: dep.Host})
			if err := deploy.DeleteDeployment(id); err != nil {
				return err
			}
			fmt.Printf("Destroyed deployment %s (agent revoked; the server itself was not deleted for BYO-SSH).\n", id)
			return nil
		},
	}
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

// firstNonEmpty returns the first non-empty string in vals, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// readGeneratedPorts reads ports.grpc / ports.http from the generated
// config/base.yaml — the source of truth for what the app actually binds, so
// the Caddy routes match. Best-effort: returns ("","") if the file is missing
// or unparseable and the caller falls back to defaults. The generated block is:
//
//	ports:
//	  grpc: 6009
//	  http: 8080
func readGeneratedPorts(sourceRoot string) (grpc, http string) {
	data, err := os.ReadFile(filepath.Join(sourceRoot, "config", "base.yaml"))
	if err != nil {
		return "", ""
	}
	inPorts := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A top-level key (no leading indentation) other than a ports child ends
		// the ports block.
		indented := line != trimmed
		if !indented {
			inPorts = trimmed == "ports:"
			continue
		}
		if !inPorts {
			continue
		}
		if v, ok := strings.CutPrefix(trimmed, "grpc:"); ok {
			grpc = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(trimmed, "http:"); ok {
			http = strings.TrimSpace(v)
		}
	}
	return grpc, http
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
