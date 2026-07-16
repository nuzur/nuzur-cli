package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nuzur/nuzur-cli/constants"
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

	API    string
	Auth   string
	Custom bool

	SourceDir     string
	CLIInstallCmd string
	Sudo          bool
	WebURL        string

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

		API:    strSetting(c, "api", cfg.API, ""),
		Auth:   strSetting(c, "auth", cfg.Auth, ""),
		Custom: boolSetting(c, "custom", cfg.Custom),

		SourceDir:     strSetting(c, "source-dir", cfg.SourceDir, ""),
		CLIInstallCmd: strSetting(c, "cli-install-cmd", cfg.CLIInstallCmd, ""),
		Sudo:          boolSetting(c, "sudo", cfg.Sudo),
		WebURL:        strSetting(c, "web-url", cfg.WebURL, constants.WEB_PROD_URL),
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

	return s, nil
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
		API:              sp(s.API),
		Auth:             sp(s.Auth),
		Custom:           bp(s.Custom),
		SourceDir:        sp(s.SourceDir),
		CLIInstallCmd:    sp(s.CLIInstallCmd),
		Sudo:             bp(s.Sudo),
		WebURL:           sp(s.WebURL),
	}
	if len(s.Codegen) > 0 {
		cfg.Codegen = s.Codegen
	}
	return cfg
}
