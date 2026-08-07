package deploy

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nuzur/nuzur-cli/constants"
)

func TestRenderBootstrap(t *testing.T) {
	script, err := RenderBootstrap(BootstrapParams{
		Identifier:        "shop",
		DBEngine:          DBMySQL,
		DBName:            "shop",
		DBUser:            "shop_app",
		GRPCEnabled:       true,
		JWTAuth:           true,
		Host:              "1.2.3.4",
		RemoteSrcDir:      "/opt/nuzur/shop/src",
		ProvisioningToken: "nzpt_test",
		CLIInstallCmd:     "curl -fsSL https://example/install | sh",
		ConnUUID:          "conn-uuid-1",
		ConnName:          "shop-db",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}

	// Spot-check the key steps are present and per-project namespaced.
	for _, want := range []string{
		"set -euo pipefail",
		"docker build -t nuzur/shop:latest \"/opt/nuzur/shop/src\"",
		"CREATE DATABASE IF NOT EXISTS",
		"'shop_app'@'localhost'",
		// per-project config + secrets under /etc/nuzur/{id}
		"PROD_YAML=/etc/nuzur/shop/config/prod.yaml",
		"DB_PASSWORD_FILE=/etc/nuzur/shop/db_password",
		"JWT_KEY_FILE=/etc/nuzur/shop/jwt_key",
		"key: ${JWT_KEY}",
		// on-box port allocation → prod.yaml
		"GRPC_PORT=\"$(alloc_port 6009)\"",
		"HTTP_PORT=\"$(alloc_port 8080)\"",
		"grpc: ${GRPC_PORT}",
		"http: ${HTTP_PORT}",
		// idempotent pairing guard (shared agent)
		"AGENT_STATUS=\"$(/usr/local/bin/nuzur-cli agent status 2>/dev/null || true)\"",
		"printf '%s' \"$AGENT_STATUS\" | grep -q \"uuid:\"",
		"agent pair --provisioning-token 'nzpt_test'",
		"agent connection add 'shop-db' --uuid 'conn-uuid-1' --driver mysql",
		"--no-publish --non-interactive",
		"curl -fsSL https://example/install | sh",
		// Caddy import-dir + per-project snippet
		"import /etc/caddy/conf.d/*.caddy",
		"/etc/caddy/conf.d/shop.caddy",
		"reverse_proxy h2c://127.0.0.1:${GRPC_PORT}",
		"reverse_proxy 127.0.0.1:${HTTP_PORT}",
		// IP-only: auto public port + URL uses the host
		"PUBLIC_PORT=8443",
		"PUBLIC_URL=\"http://1.2.3.4:${PUBLIC_PORT}\"",
		"> /etc/nuzur/shop/url",
		// box-allocated ports recorded for the deployment-record read-back
		"> /etc/nuzur/shop/ports",
		"http=${HTTP_PORT}",
		"grpc=${GRPC_PORT}",
		"ufw allow 22/tcp",
		"ufw allow ${PUBLIC_PORT}/tcp",
		// per-project backup cron
		"/etc/cron.d/nuzur-backup-shop",
		"mysqldump shop",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap script missing %q", want)
		}
	}
	// The main Caddyfile must NOT be overwritten wholesale (import-dir only).
	if strings.Contains(script, "cat > /etc/caddy/Caddyfile") {
		t.Error("bootstrap must not overwrite the shared main Caddyfile")
	}
}

func TestRenderBootstrap_DomainAndNoGRPC(t *testing.T) {
	script, err := RenderBootstrap(BootstrapParams{
		Identifier: "api", DBEngine: DBMySQL, DBName: "api", DBUser: "api_app",
		GRPCEnabled:  false,
		RemoteSrcDir: "/opt/nuzur/api/src", ProvisioningToken: "t",
		Domain: "api.example.com",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	if strings.Contains(script, "h2c://") {
		t.Error("expected no gRPC route in the snippet when GRPCEnabled is false")
	}
	for _, want := range []string{
		"/etc/caddy/conf.d/api.caddy",
		"SITE_ADDR=\"api.example.com\"",
		"PUBLIC_URL=\"https://api.example.com\"",
		"reverse_proxy 127.0.0.1:${HTTP_PORT}",
		"ufw allow 443/tcp",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Domain mode must not allocate an IP-only public port.
	if strings.Contains(script, "PUBLIC_PORT=8443") {
		t.Error("domain mode should not allocate an IP-only public port")
	}
}

// ── the optional extra Caddy sites ───────────────────────────────────────────

// caddySnippet returns the shell that writes this project's Caddy snippet: from
// the first `cat` that opens it through the heredoc terminator of that first
// block. That block is the site every existing deployment is served by, and it
// is what must not move.
func caddySnippet(t *testing.T, script, identifier string) string {
	t.Helper()
	open := "cat > /etc/caddy/conf.d/" + identifier + ".caddy <<CADDY\n"
	start := strings.Index(script, open)
	if start < 0 {
		t.Fatalf("no Caddy snippet for %q in the rendered script", identifier)
	}
	end := strings.Index(script[start+len(open):], "\nCADDY\n")
	if end < 0 {
		t.Fatalf("the Caddy snippet for %q is never terminated", identifier)
	}
	return script[start : start+len(open)+end+len("\nCADDY\n")]
}

// TestRenderBootstrapExtraCaddySites covers the two OPTIONAL hostnames a VM can
// serve alongside the one --domain names.
//
// The existing arrangement is the thing being protected, not the thing being
// replaced: one site block routing gRPC and HTTP apart by Content-Type, which is
// what every deployed box runs and what ingress-nginx cannot do. The extra
// blocks are appended to the same snippet, so each gets its own Let's Encrypt
// cert, and everything stays on 80/443 — no firewall change.
func TestRenderBootstrapExtraCaddySites(t *testing.T) {
	base := BootstrapParams{
		Identifier: "shop", DBEngine: DBMySQL, DBName: "shop", DBUser: "shop_app",
		GRPCEnabled: true, RemoteSrcDir: "/opt/nuzur/shop/src", ProvisioningToken: "t",
		Domain: "api.example.com",
	}
	render := func(t *testing.T, p BootstrapParams) string {
		t.Helper()
		script, err := RenderBootstrap(p)
		if err != nil {
			t.Fatalf("RenderBootstrap: %v", err)
		}
		return script
	}

	plain := render(t, base)
	withHosts := base
	withHosts.GRPCDomain = "grpc.example.com"
	withHosts.AuthDomain = "auth.example.com"
	extra := render(t, withHosts)

	t.Run("the existing site block is byte-identical", func(t *testing.T) {
		before, after := caddySnippet(t, plain, "shop"), caddySnippet(t, extra, "shop")
		if before != after {
			t.Errorf("the existing site block changed.\nwas:\n%s\nnow:\n%s", before, after)
		}
		// And so is everything on either side of the insertion point: the script
		// up to the end of that block, and everything from the Caddy reload on
		// (the url/ports readback, the ufw rules, the backup cron).
		if idx := strings.Index(plain, before) + len(before); plain[:idx] != extra[:idx] {
			t.Error("the script BEFORE the extra sites changed")
		}
		const tail = "systemctl enable --now caddy"
		if plain[strings.Index(plain, tail):] != extra[strings.Index(extra, tail):] {
			t.Error("the script AFTER the extra sites changed — the firewall and the readbacks must be untouched")
		}
	})

	t.Run("nothing is appended when no host is set", func(t *testing.T) {
		if strings.Contains(plain, "cat >> /etc/caddy/conf.d/") {
			t.Error("a deploy with neither host must append nothing to the snippet")
		}
	})

	t.Run("each host becomes its own site in the same snippet", func(t *testing.T) {
		for _, want := range []string{
			// Appended (>>), not written (>): the Content-Type site stays.
			"cat >> /etc/caddy/conf.d/shop.caddy",
			"grpc.example.com {",
			"reverse_proxy h2c://127.0.0.1:${GRPC_PORT}",
			"auth.example.com {",
			"reverse_proxy 127.0.0.1:${HTTP_PORT}",
		} {
			if !strings.Contains(extra, want) {
				t.Errorf("the rendered snippet is missing %q", want)
			}
		}
		if got := strings.Count(extra, "cat >> /etc/caddy/conf.d/shop.caddy"); got != 2 {
			t.Errorf("expected exactly two appended site blocks, got %d", got)
		}
	})

	t.Run("each host is independent of the other", func(t *testing.T) {
		onlyAuth := base
		onlyAuth.AuthDomain = "auth.example.com"
		script := render(t, onlyAuth)
		if !strings.Contains(script, "auth.example.com {") {
			t.Error("the auth site must render on its own")
		}
		if strings.Contains(script, "grpc.example.com") {
			t.Error("an unset gRPC host must render nothing")
		}

		onlyGRPC := base
		onlyGRPC.GRPCDomain = "grpc.example.com"
		script = render(t, onlyGRPC)
		if !strings.Contains(script, "grpc.example.com {") {
			t.Error("the gRPC site must render on its own")
		}
		if strings.Contains(script, "auth.example.com") {
			t.Error("an unset auth host must render nothing")
		}
	})

	t.Run("a gRPC host on an app that serves no gRPC renders nothing", func(t *testing.T) {
		// The site would proxy h2c to a port nothing listens on, and Caddy would
		// still take out a certificate for it — a hostname that answers only with
		// failures is worse than one that does not resolve.
		noGRPC := base
		noGRPC.GRPCEnabled = false
		noGRPC.GRPCDomain = "grpc.example.com"
		if script := render(t, noGRPC); strings.Contains(script, "grpc.example.com") {
			t.Error("no gRPC server means no gRPC site")
		}
	})

	t.Run("a database-only deploy has no Caddy at all", func(t *testing.T) {
		dbOnly := BootstrapParams{
			Identifier: "shop", DBEngine: DBMySQL, DBName: "shop", DBUser: "shop_app",
			DBOnly: true, ProvisioningToken: "t",
			GRPCEnabled: true, GRPCDomain: "grpc.example.com", AuthDomain: "auth.example.com",
		}
		script := render(t, dbOnly)
		for _, unwanted := range []string{"grpc.example.com", "auth.example.com", "/etc/caddy/conf.d/"} {
			if strings.Contains(script, unwanted) {
				t.Errorf("--db-only installs no Caddy, so %q must not appear", unwanted)
			}
		}
	})
}

func TestRenderBootstrap_DBOnly(t *testing.T) {
	script, err := RenderBootstrap(BootstrapParams{
		Identifier: "dbonly", DBEngine: DBMySQL, DBName: "dbonly", DBUser: "dbonly_app",
		DBOnly: true, ProvisioningToken: "t", ConnUUID: "c1", ConnName: "dbonly-db",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap db-only: %v", err)
	}
	// Keeps: MySQL, DB user, persisted password, agent pair + connection, backup.
	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS",
		"'dbonly_app'@'localhost'",
		"DB_PASSWORD_FILE=/etc/nuzur/dbonly/db_password",
		"agent pair --provisioning-token 't'",
		"agent connection add 'dbonly-db' --uuid 'c1'",
		"/etc/cron.d/nuzur-backup-dbonly",
		"ufw allow 22/tcp",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("db-only bootstrap missing %q", want)
		}
	}
	// Skips: app build, app service, Caddy, app-config ports, extra firewall.
	for _, unwanted := range []string{
		"docker build", "-api.service", "import /etc/caddy", "reverse_proxy",
		"alloc_port", "ufw allow 80/tcp", "ufw allow 443/tcp",
	} {
		if strings.Contains(script, unwanted) {
			t.Errorf("db-only bootstrap should not contain %q", unwanted)
		}
	}
	// RemoteSrcDir is not required for db-only.
	if _, err := RenderBootstrap(BootstrapParams{Identifier: "x", DBName: "x", DBUser: "x_app", DBOnly: true}); err != nil {
		t.Errorf("db-only should not require RemoteSrcDir: %v", err)
	}
}

func TestRenderBootstrap_ExternalDB(t *testing.T) {
	// External MySQL (remote): no install/create, app config points at the remote.
	my, err := RenderBootstrap(BootstrapParams{
		Identifier: "ext", DBEngine: DBMySQL, DBName: "shopdb", DBUser: "app",
		ExternalDB: true, DBHost: "db.example.com", DBPort: "3306", DBPassword: "secret",
		DBParams: "parseTime=true&tls=true", DBDSN: "app:secret@tcp(db.example.com:3306)/shopdb?parseTime=true&tls=true",
		GRPCEnabled: true, RemoteSrcDir: "/src", ProvisioningToken: "t", ConnUUID: "c", ConnName: "ext-db",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap external mysql: %v", err)
	}
	for _, want := range []string{
		"using external mysql database at ${DB_HOST}:${DB_PORT}",
		"DB_HOST='db.example.com'",
		"DB_PORT='3306'",
		"host: ${DB_HOST}",
		"params: parseTime=true&tls=true",
		"agent connection add 'ext-db' --uuid 'c' --driver mysql --dsn \"app:secret@tcp(db.example.com:3306)/shopdb?parseTime=true&tls=true\"",
	} {
		if !strings.Contains(my, want) {
			t.Errorf("external mysql bootstrap missing %q", want)
		}
	}
	for _, unwanted := range []string{"installing mysql-server", "CREATE DATABASE", "nuzur-backup-ext"} {
		if strings.Contains(my, unwanted) {
			t.Errorf("external db bootstrap should not contain %q", unwanted)
		}
	}

	// External Postgres.
	pg, err := RenderBootstrap(BootstrapParams{
		Identifier: "pg", DBEngine: DBPostgres, DBName: "shop", DBUser: "app",
		ExternalDB: true, DBHost: "pg.example.com", DBPort: "5432", DBPassword: "s",
		DBParams: "sslmode=require", DBDSN: "postgres://app:s@pg.example.com:5432/shop?sslmode=require",
		DBSchema:     "public", // Postgres schema/namespace, distinct from the database
		RemoteSrcDir: "/src", ProvisioningToken: "t", ConnUUID: "c", ConnName: "pg-db",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap external postgres: %v", err)
	}
	for _, want := range []string{
		"using external postgres database",
		"driver: \"postgres\"",
		// Postgres passes a default schema to the connection (database ≠ schema).
		"--driver postgres --schema 'public' --dsn \"postgres://app:s@pg.example.com:5432/shop?sslmode=require\"",
	} {
		if !strings.Contains(pg, want) {
			t.Errorf("external postgres bootstrap missing %q", want)
		}
	}
}

func TestRenderBootstrap_SelfHostedPostgres(t *testing.T) {
	// Self-hosted Postgres: install PG, create the role+database, wire the app
	// config + agent connection with the lib/pq keyword DSN, and back it up.
	script, err := RenderBootstrap(BootstrapParams{
		Identifier: "shop", DBEngine: DBPostgres, DBName: "shop", DBUser: "shop_app",
		DBSchema:    "public", // engine-set default schema (database ≠ schema in PG)
		GRPCEnabled: true, RemoteSrcDir: "/src", ProvisioningToken: "t",
		ConnUUID: "c", ConnName: "shop-db", Host: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap self-hosted postgres: %v", err)
	}
	for _, want := range []string{
		"apt-get install -y postgresql",
		"systemctl enable --now postgresql",
		"DB_PORT=5432",
		`CREATE ROLE \"shop_app\" LOGIN CREATEDB PASSWORD`,
		`ALTER ROLE \"shop_app\" LOGIN CREATEDB PASSWORD`,
		`createdb -O "shop_app" "shop"`,
		`CREATE SCHEMA IF NOT EXISTS \"public\" AUTHORIZATION \"shop_app\"`,
		`driver: "postgres"`,
		"params: sslmode=disable",
		"schema: public",
		// lib/pq keyword DSN (not the MySQL @tcp form) for both the agent conn + env.
		`--driver postgres --schema 'public' --dsn "host=${DB_HOST} port=${DB_PORT} user=shop_app password=${DB_PASSWORD} dbname=shop sslmode=disable"`,
		"pg_dump shop",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("self-hosted postgres bootstrap missing %q", want)
		}
	}
	for _, unwanted := range []string{"installing mysql-server", "mysqldump", "@tcp("} {
		if strings.Contains(script, unwanted) {
			t.Errorf("self-hosted postgres bootstrap should not contain %q", unwanted)
		}
	}
}

func TestRenderTeardown_Postgres(t *testing.T) {
	// Purge on a Postgres deployment drops the database then the role via psql.
	purged, err := RenderTeardown(TeardownParams{
		Identifier: "shop", DBEngine: DBPostgres, DBName: "shop", DBUser: "shop_app", Purge: true,
	})
	if err != nil {
		t.Fatalf("RenderTeardown postgres purge: %v", err)
	}
	for _, want := range []string{
		`pg_terminate_backend(pid)`, // close open connections first, else DROP DATABASE is refused
		`WHERE datname='shop'`,
		`DROP DATABASE IF EXISTS \"shop\"`,
		`DROP ROLE IF EXISTS \"shop_app\"`,
	} {
		if !strings.Contains(purged, want) {
			t.Errorf("postgres purge teardown missing %q", want)
		}
	}
	for _, unwanted := range []string{"mysql <<SQL", "FLUSH PRIVILEGES"} {
		if strings.Contains(purged, unwanted) {
			t.Errorf("postgres purge teardown should not contain %q", unwanted)
		}
	}
}

func TestRenderTeardown(t *testing.T) {
	// Default (keep data, not last project): tears down THIS project only.
	script, err := RenderTeardown(TeardownParams{Identifier: "shop", DBName: "shop", DBUser: "shop_app", ConnUUID: "conn-1"})
	if err != nil {
		t.Fatalf("RenderTeardown: %v", err)
	}
	for _, want := range []string{
		"systemctl stop shop-api.service",
		"docker rm -f shop-api",
		"docker rmi -f nuzur/shop:latest",
		"rm -f /etc/caddy/conf.d/shop.caddy",
		"rm -rf /etc/nuzur/shop",
		"rm -f /etc/cron.d/nuzur-backup-shop",
		"agent connection remove 'conn-1'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("teardown missing %q", want)
		}
	}
	// Not the last project → the shared agent + Caddy root must be left alone.
	for _, unwanted := range []string{"nuzur-agent.service", "rm -rf /root/.config/nuzur", "rm -f /etc/caddy/Caddyfile", "DROP DATABASE"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("non-last teardown should not contain %q", unwanted)
		}
	}

	// Last project → also removes the shared agent + Caddy root.
	last, err := RenderTeardown(TeardownParams{Identifier: "shop", DBName: "shop", DBUser: "shop_app", ConnUUID: "conn-1", IsLastProject: true})
	if err != nil {
		t.Fatalf("RenderTeardown last: %v", err)
	}
	for _, want := range []string{"nuzur-agent.service", "rm -rf /root/.config/nuzur", "rm -f /etc/caddy/Caddyfile"} {
		if !strings.Contains(last, want) {
			t.Errorf("last-project teardown missing %q", want)
		}
	}

	// Purge: also drops the DB + user.
	purged, err := RenderTeardown(TeardownParams{Identifier: "shop", DBName: "shop", DBUser: "shop_app", Purge: true})
	if err != nil {
		t.Fatalf("RenderTeardown purge: %v", err)
	}
	for _, want := range []string{"DROP DATABASE IF EXISTS", "shop", "DROP USER IF EXISTS 'shop_app'@'localhost'"} {
		if !strings.Contains(purged, want) {
			t.Errorf("purge teardown missing %q", want)
		}
	}
}

func TestRenderTeardown_PurgeRequiresDBFields(t *testing.T) {
	if _, err := RenderTeardown(TeardownParams{Identifier: "shop", Purge: true}); err == nil {
		t.Fatal("expected error: purge needs DBName/DBUser")
	}
}

func TestRenderBootstrap_RequiresFields(t *testing.T) {
	if _, err := RenderBootstrap(BootstrapParams{Identifier: "x"}); err == nil {
		t.Fatal("expected error when required DB fields are missing")
	}
}

func TestDeploymentStateRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	// Redirect the per-user config dir on both macOS ($HOME/Library) and Linux
	// ($XDG_CONFIG_HOME) so the test never touches the real user directory.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))

	d := &Deployment{
		ID:             "shop-abc123",
		Provider:       ProviderSSH,
		Host:           "1.2.3.4",
		User:           "root",
		Port:           22,
		Identifier:     "shop",
		ProjectUUID:    "p-uuid",
		LocalAgentUUID: "a-uuid",
		DBEngine:       DBMySQL,
		CreatedAt:      time.Now(),
	}
	if err := SaveDeployment(d); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	got, err := LoadDeployment(d.ID)
	if err != nil {
		t.Fatalf("LoadDeployment: %v", err)
	}
	if got.Host != d.Host || got.LocalAgentUUID != d.LocalAgentUUID || got.Provider != ProviderSSH {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}

	list, err := ListDeployments()
	if err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("ListDeployments: got %d entries, err=%v", len(list), err)
	}

	if err := DeleteDeployment(d.ID); err != nil {
		t.Fatalf("DeleteDeployment: %v", err)
	}
	if _, err := LoadDeployment(d.ID); err == nil {
		t.Fatal("expected LoadDeployment to fail after delete")
	}
	// Deleting a missing deployment is not an error.
	if err := DeleteDeployment(d.ID); err != nil {
		t.Fatalf("DeleteDeployment (missing) should be nil: %v", err)
	}
}

// The box installs the CLI VERSION THAT IS DRIVING THE DEPLOY, not
// `releases/latest`. A managed deploy once died at exit 1 with `curl: (22) 404`
// after the droplet, Docker, MySQL and the app image had all been built, because a
// Release had been created 79 seconds earlier carrying zero assets and `latest`
// resolved to it. Pinning makes a release-in-progress unable to reach a running
// deploy, and keeps the box's CLI equal to the one that paired its agent.
func TestRenderBootstrapPinsTheCLIVersion(t *testing.T) {
	script, err := RenderBootstrap(BootstrapParams{
		Identifier: "shop", DBEngine: DBMySQL, DBName: "shop", DBUser: "shop_app",
		RemoteSrcDir: "/opt/nuzur/shop/src", ProvisioningToken: "t",
		CLIVersion: "1.5.1",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	if strings.Contains(script, "releases/latest/download") {
		t.Error("the bootstrap must not resolve the CLI through the releases/latest alias — that is the unpinned URL a release-in-progress breaks")
	}
	for _, want := range []string{
		`NUZUR_CLI_URL="https://github.com/nuzur/nuzur-cli/releases/download/v1.5.1/nuzur-cli_Linux_${NUZUR_ARCH}.tar.gz"`,
		// The failure must name the version AND the URL it tried, so a dev build
		// with no published release reads as exactly that in the deploy log.
		`could not download nuzur-cli v1.5.1 for Linux`,
		`tried: $NUZUR_CLI_URL`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap script missing %q", want)
		}
	}
}

// Unset means "the CLI running this deploy" — no caller can accidentally render
// an unpinned script.
func TestRenderBootstrapDefaultsToThisCLIVersion(t *testing.T) {
	for _, tc := range []struct{ name, given, want string }{
		{name: "unset uses the constant", given: "", want: constants.CLI_VERSION},
		{name: "explicit is honored", given: "1.4.0", want: "1.4.0"},
		// The constant is bare but the git tag and the URL carry a `v`; absorb the
		// obvious mistake rather than rendering `.../vv1.4.0/...`.
		{name: "a leading v is tolerated", given: "v1.4.0", want: "1.4.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, err := RenderBootstrap(BootstrapParams{
				Identifier: "shop", DBEngine: DBMySQL, DBName: "shop", DBUser: "shop_app",
				RemoteSrcDir: "/src", ProvisioningToken: "t", CLIVersion: tc.given,
			})
			if err != nil {
				t.Fatalf("RenderBootstrap: %v", err)
			}
			want := "releases/download/v" + tc.want + "/nuzur-cli_Linux_"
			if !strings.Contains(script, want) {
				t.Errorf("bootstrap script missing %q", want)
			}
		})
	}
}

// --cli-install-cmd stays the escape hatch for boxes that can't reach GitHub: it
// replaces the download entirely, pinned URL included.
func TestRenderBootstrapCLIInstallCmdStillOverrides(t *testing.T) {
	script, err := RenderBootstrap(BootstrapParams{
		Identifier: "shop", DBEngine: DBMySQL, DBName: "shop", DBUser: "shop_app",
		RemoteSrcDir: "/src", ProvisioningToken: "t",
		CLIInstallCmd: "curl -fsSL https://example/install | sh",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	if !strings.Contains(script, "curl -fsSL https://example/install | sh") {
		t.Error("expected the --cli-install-cmd override in the script")
	}
	if strings.Contains(script, "NUZUR_CLI_URL=") {
		t.Error("--cli-install-cmd must replace the GitHub download entirely")
	}
}
