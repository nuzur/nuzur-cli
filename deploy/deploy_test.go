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
		GRPCPort:          "50051",
		HTTPPort:          "8080",
		JWTAuth:           true,
		RemoteSrcDir:      "/opt/nuzur/shop/src",
		ProvisioningToken: "nzpt_test",
		CLIInstallCmd:     "curl -fsSL https://example/install | sh",
		ConnUUID:          "conn-uuid-1",
		ConnName:          "shop-db",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}

	// Spot-check the key steps are present and templated.
	for _, want := range []string{
		"set -euo pipefail",
		"docker build -t nuzur/shop:latest \"/opt/nuzur/shop/src\"",
		"bind-address = 127.0.0.1",
		"innodb_buffer_pool_size = 256M",
		"CREATE DATABASE IF NOT EXISTS",
		"'shop_app'@'localhost'",
		"/etc/nuzur/config/prod.yaml",
		"DB_PASSWORD_FILE=/etc/nuzur/db_password", // password persisted + reused across runs
		"JWT_KEY_FILE=/etc/nuzur/jwt_key",         // signing key persisted + reused across runs
		"key: ${JWT_KEY}",
		"AGENT_STATUS=\"$(/usr/local/bin/nuzur-cli agent status 2>/dev/null || true)\"", // idempotent guard (captured to dodge pipefail+SIGPIPE)
		"printf '%s' \"$AGENT_STATUS\" | grep -q \"uuid:\"",
		"agent pair --provisioning-token 'nzpt_test'",
		"agent connection add 'shop-db' --uuid 'conn-uuid-1' --driver mysql",
		"--no-publish --non-interactive",
		"curl -fsSL https://example/install | sh",
		"/etc/caddy/Caddyfile",
		":80 {", // IP-only (no domain) → plain HTTP, no cert
		"reverse_proxy h2c://127.0.0.1:50051",
		"reverse_proxy 127.0.0.1:8080",
		"ufw allow 22/tcp",
		"ufw allow 80/tcp",
		"nuzur-agent.service",
		"mysqldump shop",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap script missing %q", want)
		}
	}

	// IP-only deploy serves plain HTTP: no TLS in the Caddyfile and no 443
	// firewall rule (legit https:// download URLs elsewhere are fine).
	for _, unwanted := range []string{"tls internal", "ufw allow 443/tcp"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("IP-only bootstrap should not contain %q", unwanted)
		}
	}
}

func TestRenderBootstrap_DomainAndNoGRPC(t *testing.T) {
	script, err := RenderBootstrap(BootstrapParams{
		Identifier: "api", DBEngine: DBMySQL, DBName: "api", DBUser: "api_app",
		HTTPPort: "8080", GRPCEnabled: false,
		RemoteSrcDir: "/opt/nuzur/api/src", ProvisioningToken: "t",
		Domain: "api.example.com",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	if strings.Contains(script, "h2c://") {
		t.Error("expected no gRPC route in the Caddyfile when GRPCEnabled is false")
	}
	if strings.Contains(script, "tls internal") {
		t.Error("expected a real (non-self-signed) cert when a domain is set")
	}
	for _, want := range []string{"api.example.com {", "reverse_proxy 127.0.0.1:8080"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRenderTeardown(t *testing.T) {
	// Default (keep data): tears down infra, no DB drop.
	script, err := RenderTeardown(TeardownParams{Identifier: "shop", DBName: "shop", DBUser: "shop_app"})
	if err != nil {
		t.Fatalf("RenderTeardown: %v", err)
	}
	for _, want := range []string{
		"systemctl stop \"$unit\"",
		"shop-api.service nuzur-agent.service",
		"docker rm -f shop-api",
		"docker rmi -f nuzur/shop:latest",
		"rm -f /etc/caddy/Caddyfile",
		"rm -rf /etc/nuzur",
		"rm -rf /root/.config/nuzur",
		"rm -f /etc/cron.d/nuzur-backup",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("teardown missing %q", want)
		}
	}
	if strings.Contains(script, "DROP DATABASE") {
		t.Error("default teardown must not drop the database")
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
