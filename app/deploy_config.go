package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/urfave/cli"
)

// DeployConfig is the JSON schema for a whole-deploy spec file passed via
// --deploy-config. Its top-level keys mirror the deploy flags 1:1, and the
// generator (go-code-gen) config is nested under `codegen` (the content that
// used to be passed via --gen-config / the old --config-file). Every field is a
// pointer so "absent in the file" is distinguishable from "present but zero" —
// the resolver only falls back to a file value when the flag wasn't set.
//
// A web-authored config never contains db_dsn (a raw secret): external databases
// are expressed as a `connection` (team-connection UUID resolved server-side at
// deploy time). The CLI still accepts db_dsn in a locally-authored file.
type DeployConfig struct {
	Provider   *string `json:"provider,omitempty"`
	Host       *string `json:"host,omitempty"`
	Region     *string `json:"region,omitempty"`
	Size       *string `json:"size,omitempty"`
	Image      *string `json:"image,omitempty"`
	SSHKeyName *string `json:"ssh_key_name,omitempty"`
	User       *string `json:"user,omitempty"`
	SSHKey     *string `json:"ssh_key,omitempty"`
	Port       *int    `json:"port,omitempty"`
	Domain     *string `json:"domain,omitempty"`

	Project    *string `json:"project,omitempty"`
	Version    *string `json:"version,omitempty"`
	Identifier *string `json:"identifier,omitempty"`

	DBOnly           *bool   `json:"db_only,omitempty"`
	DB               *string `json:"db,omitempty"`
	DBSchema         *string `json:"db_schema,omitempty"`
	DBDSN            *string `json:"db_dsn,omitempty"`
	Connection       *string `json:"connection,omitempty"`
	SaveConnection   *bool   `json:"save_connection,omitempty"`
	NoSaveConnection *bool   `json:"no_save_connection,omitempty"`

	// StorageEnabled turns on the generated /upload and /sign endpoints (the S3
	// storage zone) independently of where credentials come from. A web-authored
	// config sets this alongside Storage; the manual --s3-* flags also imply it.
	StorageEnabled *bool `json:"storage_enabled,omitempty"`
	// Storage is the team ObjectStore (S3) UUID whose credentials are resolved
	// server-side at deploy time and written into the app's `aws:` config, so the
	// generated /upload and /sign endpoints can reach the bucket. Like Connection,
	// a web-authored config carries only the UUID, never the plaintext secret.
	// Raw S3 credentials are provided instead via the CLI-only --s3-* flags
	// (mirroring how db_dsn is CLI-only), never in a web-authored config.
	Storage *string `json:"storage,omitempty"`

	API    *string `json:"api,omitempty"`
	Auth   *string `json:"auth,omitempty"`
	Custom *bool   `json:"custom,omitempty"`

	SourceDir     *string `json:"source_dir,omitempty"`
	CLIInstallCmd *string `json:"cli_install_cmd,omitempty"`
	Sudo          *bool   `json:"sudo,omitempty"`
	WebURL        *string `json:"web_url,omitempty"`

	Codegen map[string]interface{} `json:"codegen,omitempty"`
}

// loadDeployConfigFile reads a --deploy-config file (or "-" for stdin). An empty
// path yields an empty config so a pure-flags deploy keeps working. Mirrors
// loadProvidedConfig (command_extension_agent.go).
func loadDeployConfigFile(path string) (*DeployConfig, error) {
	if path == "" {
		return &DeployConfig{}, nil
	}
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading deploy-config from stdin: %w", err)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading deploy-config: %w", err)
		}
	}
	if strings.TrimSpace(string(raw)) == "" {
		return &DeployConfig{}, nil
	}
	// Unknown keys are ignored (not an error): a deploy-config is a portable
	// artifact that an older CLI may run against a config a newer nuzur web
	// produced — forward-compat beats strict typo-catching here.
	var cfg DeployConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing deploy-config JSON: %w", err)
	}
	return &cfg, nil
}

// deploySettings holds the resolved deploy inputs — the single source runDeploy
// reads from, after merging the --deploy-config file with the CLI flags (flags
// win). Concrete values (defaults applied), never pointers.
type deploySettings struct {
	Provider   string
	Host       string
	Region     string
	Size       string
	Image      string
	SSHKeyName string
	User       string
	SSHKey     string
	Port       int
	Domain     string

	Project    string
	Version    string
	Identifier string

	DBOnly           bool
	DB               string
	DBSchema         string
	DBDSN            string
	Connection       string
	SaveConnection   bool
	NoSaveConnection bool
	StorageEnabled   bool
	Storage          string
	// Manual S3 credentials (CLI-only, never from a web-authored config), the
	// alternative to referencing a team ObjectStore via Storage.
	S3Bucket    string
	S3Region    string
	S3AccessKey string
	S3Secret    string

	API  string
	Auth string
	// Custom is tri-state: nil means "the user said nothing about the custom zone
	// this run", which is different from "the user turned it off". A plain bool made
	// those the same thing, so a re-deploy that simply omitted --custom regenerated
	// the app WITHOUT the custom-routes hook — dropping every hand-written endpoint
	// from a deploy that reported success. Only a value the user actually supplied
	// (the flag, or `custom` in a deploy-config file) is written into the generator
	// config; nil leaves the project's saved config to decide, which is what makes
	// the setting sticky.
	Custom *bool

	SourceDir     string
	CLIInstallCmd string
	Sudo          bool
	WebURL        string

	// AllowDestructive authorizes a schema apply that deletes data. Flag-only, and
	// deliberately absent from DeployConfig: authorizing data loss has to be an act
	// at the keyboard for this one run, not a property of a JSON file somebody
	// committed months ago and shares across a team.
	AllowDestructive bool

	// Codegen is the go-code-gen config map: the deploy-config's `codegen` block
	// as the base, overlaid by a --gen-config file when given.
	Codegen map[string]interface{}
}

// resolveDeploySettings merges the --deploy-config file with the CLI flags into
// the effective settings runDeploy uses. Precedence (low→high): deploy-config
// file → default → explicit flag. A flag "wins" only when the user actually
// passed it (c.IsSet), so a config file can carry the base and one-off flags
// tweak it. The `codegen` block is the deploy-config's map overlaid by a
// --gen-config file.
func resolveDeploySettings(c *cli.Context) (*deploySettings, error) {
	cfg, err := loadDeployConfigFile(c.String("deploy-config"))
	if err != nil {
		return nil, err
	}

	s := &deploySettings{
		Provider:   strSetting(c, "provider", cfg.Provider, "ssh"),
		Host:       strSetting(c, "host", cfg.Host, ""),
		Region:     strSetting(c, "region", cfg.Region, ""),
		Size:       strSetting(c, "size", cfg.Size, ""),
		Image:      strSetting(c, "image", cfg.Image, ""),
		SSHKeyName: strSetting(c, "ssh-key-name", cfg.SSHKeyName, ""),
		User:       strSetting(c, "user", cfg.User, "root"),
		SSHKey:     strSetting(c, "ssh-key", cfg.SSHKey, ""),
		Port:       intSetting(c, "port", cfg.Port, 22),
		Domain:     strSetting(c, "domain", cfg.Domain, ""),

		Project:    strSetting(c, "project", cfg.Project, ""),
		Version:    strSetting(c, "version", cfg.Version, ""),
		Identifier: strSetting(c, "identifier", cfg.Identifier, ""),

		DBOnly:           boolSetting(c, "db-only", cfg.DBOnly),
		DB:               strSetting(c, "db", cfg.DB, "mysql"),
		DBSchema:         strSetting(c, "db-schema", cfg.DBSchema, ""),
		DBDSN:            strSetting(c, "db-dsn", cfg.DBDSN, ""),
		Connection:       strSetting(c, "connection", cfg.Connection, ""),
		SaveConnection:   boolSetting(c, "save-connection", cfg.SaveConnection),
		NoSaveConnection: boolSetting(c, "no-save-connection", cfg.NoSaveConnection),
		StorageEnabled:   boolSetting(c, "storage-enabled", cfg.StorageEnabled),
		Storage:          strSetting(c, "storage", cfg.Storage, ""),
		// Manual creds are flag-only (fileVal nil): never read from a config file,
		// so a shared/web-authored config can't carry a plaintext S3 secret.
		S3Bucket:    strSetting(c, "s3-bucket", nil, ""),
		S3Region:    strSetting(c, "s3-region", nil, ""),
		S3AccessKey: strSetting(c, "s3-access-key", nil, ""),
		S3Secret:    strSetting(c, "s3-secret", nil, ""),

		API:    strSetting(c, "api", cfg.API, ""),
		Auth:   strSetting(c, "auth", cfg.Auth, ""),
		Custom: boolPtrSetting(c, "custom", cfg.Custom),

		SourceDir:     strSetting(c, "source-dir", cfg.SourceDir, ""),
		CLIInstallCmd: strSetting(c, "cli-install-cmd", cfg.CLIInstallCmd, ""),
		Sudo:          boolSetting(c, "sudo", cfg.Sudo),
		WebURL:        strSetting(c, "web-url", cfg.WebURL, constants.WEB_PROD_URL),

		// Flag-only (fileVal nil), for the reason on the struct field.
		AllowDestructive: boolSetting(c, "allow-destructive", nil),
	}

	// Codegen: start from the deploy-config's nested `codegen` block, then overlay
	// a standalone --gen-config file (keys in the file win). Either may be nil.
	codegen := map[string]interface{}{}
	for k, v := range cfg.Codegen {
		codegen[k] = v
	}
	if genFile := c.String("gen-config"); genFile != "" {
		m, err := loadDeployConfig(genFile)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			codegen[k] = v
		}
	}
	s.Codegen = codegen

	// Validate the enumerated flags HERE rather than at their use sites, so a typo
	// fails identically for `deploy`, `deploy --plan` and `--print-config`, and always
	// before anything is generated or provisioned.
	if s.DB, err = normalizeDBEngine(s.DB); err != nil {
		return nil, err
	}
	if err := validateAuthValue(s.Auth); err != nil {
		return nil, err
	}

	return s, nil
}

// deployDBEngines maps every accepted --db value onto the engine name the rest of the
// CLI uses.
//
// `postgresql` is accepted and folded to `postgres` because go-code-gen's own config
// calls this same engine `postgresql` — one concept, two vocabularies, and users read
// both. Until this existed --db was not validated at all: only the exact string
// `postgres` selected Postgres and every other value, `postgresql` included, fell
// silently through to MySQL, so a plausible spelling produced the wrong database with
// no diagnostic anywhere.
var deployDBEngines = map[string]string{
	"mysql":      string(deploy.DBMySQL),
	"postgres":   string(deploy.DBPostgres),
	"postgresql": string(deploy.DBPostgres),
}

// normalizeDBEngine validates --db and returns the canonical engine name.
func normalizeDBEngine(v string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(v))
	if name == "" {
		return string(deploy.DBMySQL), nil
	}
	if engine, ok := deployDBEngines[name]; ok {
		return engine, nil
	}
	return "", fmt.Errorf("--db must be one of: mysql, postgres (postgresql is accepted as an alias for postgres); got %q", v)
}

// deployAuthValues is the `auth` generator option's enum. --api has always had a
// default arm rejecting anything unrecognized; --auth was copied straight into the
// codegen config, so a typo produced an app with no auth middleware at all and said
// nothing about it.
var deployAuthValues = []string{"disabled", "jwt", "keycloak"}

// validateAuthValue checks --auth. Empty is valid and means "leave the project's
// last/provided config alone", which is what the flag help promises.
func validateAuthValue(v string) error {
	value := strings.TrimSpace(v)
	if value == "" {
		return nil
	}
	for _, allowed := range deployAuthValues {
		if value == allowed {
			return nil
		}
	}
	return fmt.Errorf("--auth must be one of: %s; got %q", strings.Join(deployAuthValues, ", "), v)
}

// strSetting returns the flag value when the user set it, else the file value,
// else the default.
func strSetting(c *cli.Context, flag string, fileVal *string, def string) string {
	if c.IsSet(flag) {
		return c.String(flag)
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

func boolSetting(c *cli.Context, flag string, fileVal *bool) bool {
	if c.IsSet(flag) {
		return c.Bool(flag)
	}
	if fileVal != nil {
		return *fileVal
	}
	return false
}

// boolPtrSetting is boolSetting for a setting that has to keep its unset state:
// nil when neither the flag nor the config file mentioned it.
//
// Separate from boolSetting rather than a change to it, because the other six bool
// settings are per-run switches with no memory (--db-only, --sudo,
// --allow-destructive, …) and "absent means false" is right for all of them. It is
// wrong only where the value is remembered across deploys — see deploySettings.Custom.
func boolPtrSetting(c *cli.Context, flag string, fileVal *bool) *bool {
	if c.IsSet(flag) {
		v := c.Bool(flag)
		return &v
	}
	return fileVal
}

func intSetting(c *cli.Context, flag string, fileVal *int, def int) int {
	if c.IsSet(flag) {
		return c.Int(flag)
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

// toDeployConfig renders the resolved settings back into a DeployConfig for
// --print-config (a template / snapshot of the effective deploy). Empty values
// are omitted (omitempty), so the output shows only what's actually set.
func (s *deploySettings) toDeployConfig() *DeployConfig {
	sp := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	bp := func(v bool) *bool {
		if !v {
			return nil
		}
		return &v
	}
	ip := func(v int) *int {
		if v == 0 {
			return nil
		}
		return &v
	}
	cfg := &DeployConfig{
		Provider:         sp(s.Provider),
		Host:             sp(s.Host),
		Region:           sp(s.Region),
		Size:             sp(s.Size),
		Image:            sp(s.Image),
		SSHKeyName:       sp(s.SSHKeyName),
		User:             sp(s.User),
		SSHKey:           sp(s.SSHKey),
		Port:             ip(s.Port),
		Domain:           sp(s.Domain),
		Project:          sp(s.Project),
		Version:          sp(s.Version),
		Identifier:       sp(s.Identifier),
		DBOnly:           bp(s.DBOnly),
		DB:               sp(s.DB),
		DBSchema:         sp(s.DBSchema),
		DBDSN:            sp(s.DBDSN),
		Connection:       sp(s.Connection),
		SaveConnection:   bp(s.SaveConnection),
		NoSaveConnection: bp(s.NoSaveConnection),
		StorageEnabled:   bp(s.StorageEnabled),
		Storage:          sp(s.Storage),
		// Manual S3 creds are deliberately NOT round-tripped into a config snapshot
		// (they're flag-only secrets, like db_dsn's password should not be shared).
		API:  sp(s.API),
		Auth: sp(s.Auth),
		// Passed through as-is, not through bp(): the tri-state is the point. Unset
		// stays absent from the snapshot; an explicit `--custom=false` round-trips as
		// `"custom": false` rather than silently becoming "unset" — which would make a
		// snapshot mean something different from the invocation it snapshotted.
		Custom:        s.Custom,
		SourceDir:     sp(s.SourceDir),
		CLIInstallCmd: sp(s.CLIInstallCmd),
		Sudo:          bp(s.Sudo),
		WebURL:        sp(s.WebURL),
	}
	if len(s.Codegen) > 0 {
		cfg.Codegen = s.Codegen
	}
	return cfg
}
