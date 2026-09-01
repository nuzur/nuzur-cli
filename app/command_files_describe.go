package app

import (
	"fmt"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/fieldfile"
	"github.com/urfave/cli"
)

// `files describe` is the discovery half of the files command group.
//
// It exists because an agent cannot use `files upload` without already knowing
// an entity and a field, and nothing else in the CLI would tell it. The
// interactive pickers are useless to a script, and the "not found" errors only
// list candidates AFTER a failed call that already needed a real file on disk —
// discovery should not require a payload.
//
// This mirrors `run-extension describe` (app/command_extension_agent.go): print
// the machine contract for what the sibling command needs, always as JSON.

func (i *Implementation) FilesDescribeCommand() cli.Command {
	return cli.Command{
		Name:  "describe",
		Usage: i.localize.Localize("files_describe_desc", "Print, as JSON, every field in a project version that can hold a file, and what each one accepts"),
		Flags: []cli.Flag{
			cli.StringFlag{Name: "project, p", Usage: "Project name or UUID (optional when --version is a UUID)"},
			cli.StringFlag{Name: "version", Usage: "Project version identifier or UUID"},
			cli.BoolFlag{Name: "non-interactive", Usage: "Never prompt; fail if required input is missing"},
		},
		Action: func(c *cli.Context) error {
			if err := requireNoArgs(c, "files describe"); err != nil {
				return err
			}
			flags := filesUploadFlags{
				project: c.String("project"),
				version: c.String("version"),
				// A schema is a machine contract, so this is always JSON —
				// which also means always non-interactive.
				jsonOutput:     true,
				nonInteractive: c.Bool("non-interactive"),
			}
			return i.runFilesDescribe(flags)
		},
	}
}

// filesDescribeResult is the `files describe` document. Field names are part of
// the stable contract in docs/agent-usage.md.
type filesDescribeResult struct {
	Status                   string `json:"status"` // always "describe"
	ProjectUUID              string `json:"project_uuid"`
	ProjectVersionUUID       string `json:"project_version_uuid"`
	ProjectVersionIdentifier string `json:"project_version_identifier,omitempty"`

	// FileFields is every field that can hold a file, uploadable or not. A field
	// that cannot currently be uploaded to is still listed, with Uploadable
	// false and Reason saying why — an agent needs to be able to tell "this
	// field does not exist" from "this field is misconfigured".
	FileFields []describedFileField `json:"file_fields"`
}

type describedFileField struct {
	Entity uploadEntityRef `json:"entity"`
	Field  uploadFieldRef  `json:"field"`

	StorageType string `json:"storage_type"`
	// ObjectStoreUUID and Path are set only for OBJECT_STORE fields.
	ObjectStoreUUID string `json:"object_store_uuid,omitempty"`
	Path            string `json:"path,omitempty"`
	// KeyPrefix is Path normalized the way the server normalizes it, i.e. the
	// literal prefix an uploaded object's key will start with.
	KeyPrefix string `json:"key_prefix,omitempty"`

	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
	MaxSizeKB         int64    `json:"max_size_kb,omitempty"`
	AllowMultiple     bool     `json:"allow_multiple"`

	Uploadable bool   `json:"uploadable"`
	Reason     string `json:"reason,omitempty"`

	// UploadCommand is the exact command to upload into this field, so an agent
	// can copy it rather than assemble it and get a flag wrong.
	UploadCommand string `json:"upload_command"`
}

func (i *Implementation) runFilesDescribe(flags filesUploadFlags) error {
	fail := func(err error) error {
		return failWith(flags.jsonOutput,
			i.localize.Localize("files_describe_error", "Could not describe the project version's file fields"),
			jsonError{Status: "error", Message: err.Error()})
	}

	if err := i.login(); err != nil {
		return fail(err)
	}

	pv, err := i.resolveFilesProjectVersion(flags)
	if err != nil {
		return fail(err)
	}

	result := filesDescribeResult{
		Status:                   "describe",
		ProjectUUID:              pv.GetProjectUuid(),
		ProjectVersionUUID:       pv.GetUuid(),
		ProjectVersionIdentifier: pv.GetIdentifier(),
		FileFields:               []describedFileField{},
	}

	for _, e := range pv.GetEntities() {
		for _, f := range e.GetFields() {
			if !fieldfile.IsFileField(f) {
				continue
			}
			result.FileFields = append(result.FileFields, describeFileField(pv, e, f))
		}
	}

	return printJSONValue(result)
}

func describeFileField(pv *nemgen.ProjectVersion, e *nemgen.Entity, f *nemgen.Field) describedFileField {
	out := describedFileField{
		Entity: uploadEntityRef{UUID: e.GetUuid(), Identifier: e.GetIdentifier()},
		Field: uploadFieldRef{
			UUID:       f.GetUuid(),
			Identifier: f.GetIdentifier(),
			Type:       shortEnum(f.GetType().String(), "FIELD_TYPE_"),
		},
		UploadCommand: fmt.Sprintf(
			"nuzur-cli files upload <path> --version %s --entity %s --field %s --json",
			pv.GetUuid(), e.GetIdentifier(), f.GetIdentifier()),
	}

	cfg, err := fieldfile.FileConfig(f)
	if err != nil {
		out.Reason = err.Error()
		return out
	}

	out.StorageType = shortEnum(cfg.GetStorageType().String(), "FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_")
	out.AllowedExtensions = cfg.GetAllowedExtensions()
	out.MaxSizeKB = cfg.GetMaxSize()
	out.AllowMultiple = cfg.GetAllowMultiple()

	if fieldfile.UsesObjectStore(cfg) {
		os := cfg.GetStorageConfig().GetObjectStore()
		out.ObjectStoreUUID = os.GetObjectStoreUuid()
		out.Path = os.GetPath()
		out.KeyPrefix = fieldfile.NormalizeKey(os.GetPath())
	}

	// The same check the upload path runs, so describe and upload cannot
	// disagree about whether a field is usable.
	if err := fieldfile.ValidateFieldTarget(f, cfg); err != nil {
		out.Reason = err.Error()
		return out
	}

	out.Uploadable = true
	return out
}
