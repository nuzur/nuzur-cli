package app

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
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
	set.Bool("allow-destructive", false, "")
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
	if s.DB != "postgres" || s.DBSchema != "public" || s.API != "both" || s.Auth != "jwt" {
		t.Fatalf("db/api/auth not from file: %+v", s)
	}
	if s.Custom == nil || !*s.Custom {
		t.Fatalf("custom not from file: %+v", s.Custom)
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
	if s.DBOnly {
		t.Fatalf("bools should default false: %+v", s)
	}
	// --custom is tri-state: unset stays unset, so the project's saved generator
	// config keeps deciding. It must NOT resolve to false, which is what silently
	// deleted every custom endpoint on a re-deploy that just omitted the flag.
	if s.Custom != nil {
		t.Fatalf("--custom should be unset when neither flag nor file mentions it: %v", *s.Custom)
	}
}

// The three states of --custom, and what each means downstream.
func TestResolveDeploySettings_CustomIsTriState(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		file string
		want *bool
	}{
		{name: "omitted stays unset", want: nil},
		{name: "flag passed is on", args: []string{"--custom"}, want: boolp(true)},
		{name: "file says true", file: `{"custom": true}`, want: boolp(true)},
		{
			// An explicit false in a config file is a decision, not an absence: it has
			// to reach the generator so the zone can be turned back OFF.
			name: "file says false", file: `{"custom": false}`, want: boolp(false),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if tc.file != "" {
				args = append([]string{"--deploy-config", writeTempConfig(t, tc.file)}, args...)
			}
			s, err := resolveDeploySettings(deployContext(t, args))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil && s.Custom != nil:
				t.Fatalf("Custom = %v, want unset", *s.Custom)
			case tc.want != nil && s.Custom == nil:
				t.Fatalf("Custom is unset, want %v", *tc.want)
			case tc.want != nil && *s.Custom != *tc.want:
				t.Fatalf("Custom = %v, want %v", *s.Custom, *tc.want)
			}
			// --print-config has to round-trip the same three states, or a snapshot
			// means something different from the invocation it snapshotted.
			got := s.toDeployConfig().Custom
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("round-tripped Custom = %v, want absent", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("round-tripped Custom = %v, want %v", got, *tc.want)
			}
		})
	}
}

func boolp(v bool) *bool { return &v }

// --db used to be unvalidated: only the exact string "postgres" selected Postgres and
// everything else — "postgresql" included, which is what go-code-gen's own config
// calls the same engine — fell through to MySQL with no diagnostic at all.
func TestResolveDeploySettings_ValidatesDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "mysql", value: "mysql", want: "mysql"},
		{name: "postgres", value: "postgres", want: "postgres"},
		{name: "postgresql is folded, not silently mysql", value: "postgresql", want: "postgres"},
		{name: "case insensitive", value: "Postgres", want: "postgres"},
		{name: "a typo is rejected", value: "postgress", wantErr: true},
		{name: "an unsupported engine is rejected", value: "sqlite", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := resolveDeploySettings(deployContext(t, []string{"--db", tc.value}))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("--db %q was accepted, resolved to %q", tc.value, s.DB)
				}
				if !strings.Contains(err.Error(), "--db") {
					t.Errorf("error does not name the flag: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("--db %q rejected: %v", tc.value, err)
			}
			if s.DB != tc.want {
				t.Fatalf("DB = %q, want %q", s.DB, tc.want)
			}
		})
	}
	// The default when nothing says otherwise.
	s, err := resolveDeploySettings(deployContext(t, nil))
	if err != nil || s.DB != "mysql" {
		t.Fatalf("default DB = %q (err %v), want mysql", s.DB, err)
	}
}

// --api has always had a default arm rejecting unknown values; --auth was copied
// straight into the codegen config, so a typo produced an app with no auth at all.
func TestResolveDeploySettings_ValidatesAuth(t *testing.T) {
	for _, ok := range []string{"disabled", "jwt", "keycloak"} {
		if _, err := resolveDeploySettings(deployContext(t, []string{"--auth", ok})); err != nil {
			t.Errorf("--auth %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"jwtt", "none", "oauth"} {
		if _, err := resolveDeploySettings(deployContext(t, []string{"--auth", bad})); err == nil {
			t.Errorf("--auth %q was accepted", bad)
		}
	}
	// Unset stays unset: the project's saved config decides.
	s, err := resolveDeploySettings(deployContext(t, nil))
	if err != nil || s.Auth != "" {
		t.Fatalf("Auth = %q (err %v), want empty", s.Auth, err)
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

// --allow-destructive is flag-only and never round-tripped.
//
// Authorizing data loss has to be an act at the keyboard for one run. If a config
// file could carry it, a team would share a file that silently pre-authorizes every
// deploy made with it to drop tables — which is exactly the state the gate exists to
// prevent, reintroduced as a default.
func TestAllowDestructiveIsFlagOnly(t *testing.T) {
	// A config file asking for it is ignored...
	path := writeTempConfig(t, `{"allow_destructive": true, "project": "sfapi"}`)
	c := deployContext(t, []string{"--deploy-config", path})
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.AllowDestructive {
		t.Fatal("--allow-destructive was read from a deploy-config file")
	}

	// ...the flag is honored...
	c = deployContext(t, []string{"--allow-destructive"})
	s, err = resolveDeploySettings(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.AllowDestructive {
		t.Fatal("--allow-destructive flag was ignored")
	}

	// ...and --print-config never emits it, so snapshotting an authorized run into a
	// reusable config cannot smuggle the authorization along with it.
	out, err := json.Marshal(s.toDeployConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "allow_destructive") {
		t.Fatalf("toDeployConfig round-tripped the authorization: %s", out)
	}
}
