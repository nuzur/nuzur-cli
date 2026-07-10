package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/urfave/cli"
)

// This file contains the non-interactive / agent-facing surface of the
// extension commands: the `describe` subcommand (emits a JSON config schema),
// shared target resolution, config loading, and JSON output helpers. See
// docs/agent-usage.md for the stable JSON contract.

// runTargets bundles everything resolved before an extension can be described or
// run against a specific project version.
type runTargets struct {
	er               *extensionrun.Implementation
	project          *nemgen.Project
	projectVersion   *nemgen.ProjectVersion
	extension        *nemgen.Extension
	extensionVersion *nemgen.ExtensionVersion
	configEntity     *extensiongen.ExtensionConfigurationEntity
	lastConfig       map[string]interface{}
	allLastConfigs   map[string]map[string]interface{}
}

// resolveOptions controls how runTargets are resolved.
type resolveOptions struct {
	extensionIdentifier string // preset extension (e.g. go-code-gen shortcut); empty = resolve from flags/prompt
	interactive         bool   // allow interactive prompts when a value isn't supplied via flags
	checkAccess         bool   // enforce that the user can run extensions on the project
	checkLimit          bool   // enforce the monthly Pro execution limit
}

// describeSubcommand builds the `describe` subcommand shared by run-extension and
// the go-code-gen shortcut. It prints the extension's config schema as JSON.
func (i *Implementation) describeSubcommand(extensionIdentifier string) cli.Command {
	return cli.Command{
		Name:  "describe",
		Usage: "Print the JSON config schema this extension needs (fields, types, and allowed uuid/enum values)",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "project, p", Usage: "Project name or UUID"},
			cli.StringFlag{Name: "version", Usage: "Project version identifier or UUID"},
			cli.StringFlag{Name: "extension, e", Usage: "Extension identifier (run-extension only)"},
		},
		Action: func(c *cli.Context) error {
			flags := extRunFlags{
				project:    c.String("project"),
				version:    c.String("version"),
				extension:  c.String("extension"),
				jsonOutput: true, // describe output is always machine-readable
			}
			return i.describeFlow(extensionIdentifier, flags)
		},
	}
}

// describeFlow resolves the target extension + project version and prints its
// config schema as JSON. Project/version/extension may be supplied via flags
// (for agents) or selected interactively (for humans exploring).
func (i *Implementation) describeFlow(extensionIdentifier string, flags extRunFlags) error {
	targets, err := i.resolveRunTargets(flags, resolveOptions{
		extensionIdentifier: extensionIdentifier,
		interactive:         true,
		checkAccess:         false,
		checkLimit:          false,
	})
	if err != nil {
		return i.failRun(flags, err)
	}

	schema, err := targets.er.DescribeConfig(
		targets.project,
		targets.projectVersion,
		targets.extension,
		targets.extensionVersion,
		targets.configEntity,
		targets.lastConfig,
	)
	if err != nil {
		return i.failRun(flags, err)
	}

	if err := printJSONValue(schema); err != nil {
		return i.failRun(flags, err)
	}
	return nil
}

// resolveRunTargets performs the shared resolution: login, project, (optional
// access check), project version, extension, (optional limit check), extension
// version, config entity, and last-used config. In non-interactive mode a
// missing project/version/extension is an error rather than a prompt.
func (i *Implementation) resolveRunTargets(flags extRunFlags, opts resolveOptions) (*runTargets, error) {
	if err := i.Login(); err != nil {
		return nil, err
	}

	er, err := extensionrun.New(extensionrun.Params{Auth: i.auth})
	if err != nil {
		return nil, err
	}

	// project
	var project *nemgen.Project
	switch {
	case flags.project != "":
		project, err = i.resolveProject(er, flags.project)
	case opts.interactive:
		project, err = i.SelectProject(er)
	default:
		return nil, errors.New("a project is required in non-interactive mode; pass --project <name|uuid>")
	}
	if err != nil {
		return nil, err
	}

	if opts.checkAccess {
		role, err := er.GetUserRoleForProject(project.Uuid)
		if err != nil {
			return nil, err
		}
		if role != nemgen.UserProjectRole_USER_PROJECT_ROLE_ADMIN &&
			role != nemgen.UserProjectRole_USER_PROJECT_ROLE_DEVELOPER {
			return nil, errors.New(i.localize.Localize("extension_run_no_access", "You do not have access to run extensions for this project."))
		}
	}

	// project version
	var projectVersion *nemgen.ProjectVersion
	switch {
	case flags.version != "":
		projectVersion, err = i.resolveProjectVersion(er, project.Uuid, flags.version)
	case opts.interactive:
		projectVersion, err = i.SelectProjectVersion(er, project.Uuid)
	default:
		return nil, errors.New("a project version is required in non-interactive mode; pass --version <identifier|uuid>")
	}
	if err != nil {
		return nil, err
	}

	// extension
	extIdentifier := opts.extensionIdentifier
	if extIdentifier == "" {
		extIdentifier = flags.extension
	}
	var extension *nemgen.Extension
	switch {
	case extIdentifier != "":
		extension, err = i.FindGeneratorExtension(er, extIdentifier)
	case opts.interactive:
		extension, err = i.SelectPublicExtension(er)
	default:
		return nil, errors.New("an extension is required in non-interactive mode; pass --extension <identifier>")
	}
	if err != nil {
		return nil, err
	}

	if opts.checkLimit {
		limitRes, err := er.CheckExtensionExecutionLimit(project.Uuid, extension.Uuid)
		if err != nil {
			return nil, err
		}
		if limitRes.IsLimited {
			return nil, errors.New(i.localize.Localize("pro_execution_limit_reached", "Monthly limit of 5 Pro extension executions reached. Please upgrade to Pro for unlimited executions."))
		}
	}

	extensionVersion, err := er.GetLatestExtensionVersion(extension.Uuid)
	if err != nil {
		return nil, err
	}

	configEntity, err := er.GetConfigEntity(extensionVersion)
	if err != nil {
		return nil, err
	}

	allLastConfigs, err := er.GetLastUsedConfig(projectVersion.Uuid)
	if err != nil {
		allLastConfigs = nil
	}
	var lastConfig map[string]interface{}
	if allLastConfigs != nil {
		lastConfig = allLastConfigs[extension.Identifier]
	}

	return &runTargets{
		er:               er,
		project:          project,
		projectVersion:   projectVersion,
		extension:        extension,
		extensionVersion: extensionVersion,
		configEntity:     configEntity,
		lastConfig:       lastConfig,
		allLastConfigs:   allLastConfigs,
	}, nil
}

// loadProvidedConfig reads the caller-supplied config from --config (inline JSON
// or "-" for stdin) or --config-file. An absent config is treated as an empty
// object so required-field validation still runs.
func loadProvidedConfig(flags extRunFlags) (map[string]interface{}, error) {
	var raw []byte
	var err error
	switch {
	case flags.configFile != "":
		raw, err = os.ReadFile(flags.configFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
	case flags.config == "-":
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("failed to read config from stdin: %w", err)
		}
	case flags.config != "":
		raw = []byte(flags.config)
	default:
		return map[string]interface{}{}, nil
	}

	if strings.TrimSpace(string(raw)) == "" {
		return map[string]interface{}{}, nil
	}

	var provided map[string]interface{}
	if err := json.Unmarshal(raw, &provided); err != nil {
		return nil, fmt.Errorf("invalid config JSON: %w", err)
	}
	return provided, nil
}

// jsonError is the stable JSON error envelope emitted in --json mode.
type jsonError struct {
	Status  string                    `json:"status"` // always "error"
	Message string                    `json:"message"`
	Errors  []extensionrun.FieldError `json:"errors,omitempty"` // populated for config validation failures
}

// failRun reports a failure and returns an error that exits the process with a
// non-zero code. In --json mode it emits a structured error envelope on stdout;
// otherwise it prints a colored message on stderr.
func (i *Implementation) failRun(flags extRunFlags, err error) error {
	if flags.jsonOutput {
		env := jsonError{Status: "error", Message: err.Error()}
		var ve *extensionrun.ConfigValidationError
		if errors.As(err, &ve) {
			env.Message = "invalid config"
			env.Errors = ve.Fields
		}
		_ = printJSONValue(env)
	} else {
		outputtools.PrintlnColoredErr(
			fmt.Sprintf("%s: %v", i.localize.Localize("extension_run_error", "Extension run failed"), err),
			outputtools.Red,
		)
	}
	// empty message: the envelope/colored line above is the user-facing output;
	// this just sets the exit code.
	return cli.NewExitError("", 1)
}

// printJSONValue writes an indented JSON representation of v to stdout.
func printJSONValue(v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
