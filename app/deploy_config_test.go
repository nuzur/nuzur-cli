package app

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/urfave/cli"
)

// deployFlagSet builds a flag set mirroring the deploy command's flags (defaults
// included) so resolveDeploySettings can be exercised with a real *cli.Context.
// Parsing `args` marks the passed flags as "set" (c.IsSet), which is what the
// flags-override-file precedence hinges on.
func deployContext(t *testing.T, args []string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("deploy", flag.ContinueOnError)
	set.String("deploy-config", "", "")
	set.String("gen-config", "", "")
	set.String("provider", "ssh", "")
	set.String("host", "", "")
	set.String("region", "", "")
	set.String("size", "", "")
	set.String("image", "", "")
	set.String("ssh-key-name", "", "")
	set.String("user", "root", "")
	set.String("ssh-key", "", "")
	set.Int("port", 22, "")
	set.String("domain", "", "")
	set.String("project", "", "")
	set.String("version", "", "")
	set.String("identifier", "", "")
	set.Bool("db-only", false, "")
	set.String("db", "mysql", "")
	set.String("db-schema", "", "")
	set.String("db-dsn", "", "")
	set.String("connection", "", "")
	set.Bool("save-connection", false, "")
	set.Bool("no-save-connection", false, "")
	set.String("api", "", "")
	set.String("auth", "", "")
	set.Bool("custom", false, "")
	set.String("source-dir", "", "")
	set.String("cli-install-cmd", "", "")
	set.Bool("sudo", false, "")
	set.String("web-url", "", "")
	if err := set.Parse(args); err != nil {
		t.Fatalf("parsing args: %v", err)
	}
	return cli.NewContext(nil, set, nil)
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "deploy.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return p
}

// A file with no flags resolves entirely from the file (and defaults).
func TestResolveDeploySettings_FileOnly(t *testing.T) {
	path := writeTempConfig(t, `{
		"provider": "digitalocean",
		"region": "nyc3",
		"project": "sfapi",
		"version": "v_21",
		"db": "postgres",
		"db_schema": "public",
		"api": "both",
		"auth": "jwt",
		"custom": true,
		"port": 2222
	}`)
	c := deployContext(t, []string{"--deploy-config", path})
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "digitalocean" || s.Region != "nyc3" || s.Project != "sfapi" || s.Version != "v_21" {
		t.Fatalf("topology not from file: %+v", s)
	}
	if s.DB != "postgres" || s.DBSchema != "public" || s.API != "both" || s.Auth != "jwt" || !s.Custom {
		t.Fatalf("db/api/auth/custom not from file: %+v", s)
	}
	if s.Port != 2222 {
		t.Fatalf("port not from file: %d", s.Port)
	}
	// Unset-in-file fields fall back to defaults.
	if s.User != "root" {
		t.Fatalf("defaults not applied: %+v", s)
	}
}

// An explicit flag overrides the file value; unpassed flags keep the file value.
func TestResolveDeploySettings_FlagOverridesFile(t *testing.T) {
	path := writeTempConfig(t, `{"version": "v_20", "domain": "api.acme.com", "provider": "hetzner", "region": "nbg1"}`)
	c := deployContext(t, []string{"--deploy-config", path, "--version", "v_21"})
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "v_21" {
		t.Fatalf("flag should override file: got version %q", s.Version)
	}
	if s.Domain != "api.acme.com" || s.Provider != "hetzner" || s.Region != "nbg1" {
		t.Fatalf("unpassed flags should keep file values: %+v", s)
	}
}

// No file, no flags → the documented defaults.
func TestResolveDeploySettings_Defaults(t *testing.T) {
	c := deployContext(t, nil)
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "ssh" || s.User != "root" || s.Port != 22 || s.DB != "mysql" {
		t.Fatalf("unexpected defaults: %+v", s)
	}
	if s.Custom || s.DBOnly {
		t.Fatalf("bools should default false: %+v", s)
	}
}

// The codegen block: deploy-config.codegen is the base; a --gen-config file
// overlays it (its keys win).
func TestResolveDeploySettings_CodegenMerge(t *testing.T) {
	deployPath := writeTempConfig(t, `{"codegen": {"rest_base_path": "/v1", "identifier": "sfapi"}}`)
	dir := t.TempDir()
	genPath := filepath.Join(dir, "gen.json")
	if err := os.WriteFile(genPath, []byte(`{"identifier": "override", "extra": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := deployContext(t, []string{"--deploy-config", deployPath, "--gen-config", genPath})
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Codegen["rest_base_path"] != "/v1" {
		t.Fatalf("base codegen key lost: %v", s.Codegen)
	}
	if s.Codegen["identifier"] != "override" {
		t.Fatalf("gen-config should override codegen key: %v", s.Codegen["identifier"])
	}
	if s.Codegen["extra"] != true {
		t.Fatalf("gen-config-only key missing: %v", s.Codegen)
	}
}

// Unknown keys are ignored (forward-compat), not an error.
func TestLoadDeployConfigFile_IgnoresUnknownKeys(t *testing.T) {
	path := writeTempConfig(t, `{"provider": "ssh", "future_field": "whatever"}`)
	cfg, err := loadDeployConfigFile(path)
	if err != nil {
		t.Fatalf("unknown key should be ignored, got error: %v", err)
	}
	if cfg.Provider == nil || *cfg.Provider != "ssh" {
		t.Fatalf("known key not parsed: %+v", cfg)
	}
}

// An external DB via `connection` (no db_dsn) is accepted — the secret-free web
// shape. toDeployConfig round-trips it without ever emitting a db_dsn.
func TestToDeployConfig_SecretFreeExternal(t *testing.T) {
	path := writeTempConfig(t, `{"connection": "conn-uuid-123", "project": "sfapi"}`)
	c := deployContext(t, []string{"--deploy-config", path})
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Connection != "conn-uuid-123" || s.DBDSN != "" {
		t.Fatalf("expected connection set, dsn empty: %+v", s)
	}
	out := s.toDeployConfig()
	if out.Connection == nil || *out.Connection != "conn-uuid-123" {
		t.Fatalf("connection not round-tripped: %+v", out)
	}
	if out.DBDSN != nil {
		t.Fatalf("db_dsn must be omitted when empty (secret-free): %v", *out.DBDSN)
	}
}
