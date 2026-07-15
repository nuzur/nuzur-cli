package deploy

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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
