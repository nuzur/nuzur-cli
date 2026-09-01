package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/manifoldco/promptui"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/fieldfile"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// signedURLTTLSeconds is how long the URL the upload returns stays valid. It is
// a fact about the server (product/server/files_record_object_store.go signs
// for time.Hour*24), reported so a caller does not have to guess.
const signedURLTTLSeconds = 86400

func (i *Implementation) FilesCommand() cli.Command {
	return cli.Command{
		Name:  "files",
		Usage: i.localize.Localize("files_desc", "Work with the files stored in a project's file fields"),
		Subcommands: []cli.Command{
			i.FilesUploadCommand(),
			i.FilesDescribeCommand(),
		},
	}
}

func (i *Implementation) FilesUploadCommand() cli.Command {
	return cli.Command{
		Name:      "upload",
		Usage:     i.localize.Localize("files_upload_desc", "Upload a local file to a project's file field, the same way the data manager does"),
		ArgsUsage: "<path>",
		Description: "Uploads a file to the object store configured on a file/image/video/audio field.\n" +
			"   This talks only to nuzur, so it works for any project — including one with no deployed API.\n" +
			"   It prints the value to store in the record; it does not write records itself.",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "project, p", Usage: "Project name or UUID (optional when --version is a UUID)"},
			cli.StringFlag{Name: "version", Usage: "Project version identifier or UUID"},
			cli.StringFlag{Name: "entity", Usage: "Entity identifier or UUID that holds the field"},
			cli.StringFlag{Name: "field", Usage: "Field identifier or UUID to upload into (must be a file, image, video or audio field)"},
			cli.StringFlag{Name: "name", Usage: "Object file name to use instead of the local file's name. Sanitized the same way either way."},
			cli.BoolFlag{Name: "force", Usage: "Overwrite the existing object at that key. The key is the field's path plus the file name and has no per-record component, so this can replace a file another record points at."},
			cli.BoolFlag{Name: "dry-run", Usage: "Resolve and validate everything, print the destination, and upload nothing"},
			cli.DurationFlag{Name: "timeout", Value: fieldfile.MaxTimeout, Usage: "Deadline for the upload. Capped at 10m, which is where the nuzur ingress cuts a request off anyway."},
			cli.Int64Flag{Name: "max-bytes", Value: fieldfile.DefaultMaxBytes, Usage: "Refuse to upload a file larger than this. 0 disables the check."},
			cli.BoolFlag{Name: "non-interactive", Usage: "Never prompt; fail if required input is missing (for scripts and AI agents)"},
			cli.BoolFlag{Name: "json", Usage: "Emit machine-readable JSON output (implies non-interactive)"},
		},
		Action: func(c *cli.Context) error {
			if err := requireOneArg(c, "files upload", "the path of the file to upload"); err != nil {
				return err
			}
			if !c.Args().Present() {
				return fmt.Errorf("`nuzur-cli files upload` needs the path of a file to upload.\n" +
					"To see which fields you can upload to, run: nuzur-cli files describe --project <p> --version <v> --json")
			}
			flags := filesUploadFlagsFromContext(c)
			if err := i.runFilesUpload(c.Args().First(), flags); err != nil {
				return err
			}
			return nil
		},
	}
}

// filesUploadFlags holds everything `files upload` was asked to do. Flags that
// name a thing (project, entity, field) skip the corresponding prompt.
type filesUploadFlags struct {
	project string
	version string
	entity  string
	field   string
	name    string

	force          bool
	dryRun         bool
	nonInteractive bool
	jsonOutput     bool
	timeout        time.Duration
	maxBytes       int64
}

// isNonInteractive reports whether prompting is off. --json implies it, because
// a prompt on stdout would corrupt the document stdout is supposed to carry.
func (f filesUploadFlags) isNonInteractive() bool {
	return f.nonInteractive || f.jsonOutput
}

func filesUploadFlagsFromContext(c *cli.Context) filesUploadFlags {
	return filesUploadFlags{
		project:        c.String("project"),
		version:        c.String("version"),
		entity:         c.String("entity"),
		field:          c.String("field"),
		name:           c.String("name"),
		force:          c.Bool("force"),
		dryRun:         c.Bool("dry-run"),
		nonInteractive: c.Bool("non-interactive"),
		jsonOutput:     c.Bool("json"),
		timeout:        c.Duration("timeout"),
		maxBytes:       c.Int64("max-bytes"),
	}
}

// filesUploadResult is the --json document. Field names are part of the stable
// contract documented in docs/agent-usage.md.
type filesUploadResult struct {
	Status string `json:"status"` // "uploaded" or "dry_run"

	ProjectUUID              string `json:"project_uuid"`
	ProjectVersionUUID       string `json:"project_version_uuid"`
	ProjectVersionIdentifier string `json:"project_version_identifier,omitempty"`

	Entity uploadEntityRef `json:"entity"`
	Field  uploadFieldRef  `json:"field"`

	StorageType string `json:"storage_type"`
	SourcePath  string `json:"source_path"`
	FileName    string `json:"file_name"`
	SizeBytes   int64  `json:"size_bytes"`

	// ObjectKey is absent for a BINARY field, whose key the server generates.
	ObjectKey string `json:"object_key,omitempty"`

	// RecordValue is the value to write into the record. It is an identifier,
	// not a public link — nuzur re-signs it whenever the record is read.
	RecordValue string `json:"record_value,omitempty"`
	// SignedURL is a temporary, directly-fetchable URL. Do NOT store it.
	SignedURL                 string `json:"signed_url,omitempty"`
	SignedURLExpiresInSeconds int    `json:"signed_url_expires_in_seconds,omitempty"`

	ForceOverride bool `json:"force_override"`
}

type uploadEntityRef struct {
	UUID       string `json:"uuid"`
	Identifier string `json:"identifier"`
}

type uploadFieldRef struct {
	UUID       string `json:"uuid"`
	Identifier string `json:"identifier"`
	Type       string `json:"type"`
}

// uuidRefPattern recognises a reference that is already a UUID, which lets the
// command skip listing the user's projects entirely.
var uuidRefPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func looksLikeUUID(s string) bool { return uuidRefPattern.MatchString(s) }

func (i *Implementation) runFilesUpload(path string, flags filesUploadFlags) error {
	fail := func(err error) error {
		return failWith(flags.jsonOutput,
			i.localize.Localize("files_upload_error", "File upload failed"),
			jsonError{Status: "error", Message: err.Error()})
	}

	// Size is checked before the bytes are read: reading a 2 GB file into memory
	// only to reject it would be a strange way to enforce a size limit.
	info, err := os.Stat(path)
	if err != nil {
		return fail(fmt.Errorf("cannot read %s: %w", path, err))
	}
	if info.IsDir() {
		return fail(fmt.Errorf("%s is a directory — upload takes a single file", path))
	}
	if err := fieldfile.CheckSizeGuard(info.Size(), flags.maxBytes); err != nil {
		return fail(err)
	}

	if err := i.login(); err != nil {
		return fail(err)
	}

	target, err := i.resolveUploadTarget(flags)
	if err != nil {
		return fail(err)
	}

	cfg, err := fieldfile.FileConfig(target.field)
	if err != nil {
		return fail(err)
	}

	fileName, err := uploadFileName(flags.name, path)
	if err != nil {
		return fail(err)
	}
	// A name that cleaned down to nothing but its extension is legal (the data
	// manager does the same) but produces an object key that says nothing about
	// what the file is. Not worth mentioning for a real dotfile, which started
	// that way.
	if fieldfile.IsBareExtension(fileName) && !strings.HasPrefix(filepath.Base(path), ".") {
		i.warn(flags, fmt.Sprintf(
			"The file name cleans down to %q — everything before the extension was dropped. Pass --name for something readable.", fileName))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fail(fmt.Errorf("cannot read %s: %w", path, err))
	}

	// The extension is checked against the name the user actually has on disk,
	// which is what the data manager checks — sanitizing first would change the
	// answer for a name like "report v2.PDF".
	originalName := flags.name
	if originalName == "" {
		originalName = filepath.Base(path)
	}
	if err := fieldfile.Validate(target.field, cfg, originalName, info.Size(), sniffHead(data)); err != nil {
		return fail(err)
	}

	result := filesUploadResult{
		ProjectUUID:              target.projectUUID,
		ProjectVersionUUID:       target.projectVersionUUID,
		ProjectVersionIdentifier: target.projectVersionIdentifier,
		Entity:                   uploadEntityRef{UUID: target.entity.GetUuid(), Identifier: target.entity.GetIdentifier()},
		Field: uploadFieldRef{
			UUID:       target.field.GetUuid(),
			Identifier: target.field.GetIdentifier(),
			Type:       shortEnum(target.field.GetType().String(), "FIELD_TYPE_"),
		},
		StorageType:   shortEnum(cfg.GetStorageType().String(), "FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_"),
		SourcePath:    path,
		FileName:      fileName,
		SizeBytes:     info.Size(),
		ObjectKey:     fieldfile.ObjectKey(cfg, fileName),
		ForceOverride: flags.force,
	}

	i.warnAboutStorage(flags, cfg, result.ObjectKey)

	if flags.dryRun {
		result.Status = "dry_run"
		return i.reportUpload(flags, result)
	}

	i.progress(flags, fmt.Sprintf("Uploading %s (%s) to %s.%s...",
		fileName, fieldfile.HumanSize(info.Size()),
		target.entity.GetIdentifier(), target.field.GetIdentifier()))

	signedURL, err := i.uploadRecordFieldFile(flags, target, fileName, data)
	if err != nil {
		return i.failUpload(flags, err, target, cfg, result.ObjectKey)
	}

	result.Status = "uploaded"
	result.SignedURL = signedURL
	result.RecordValue = fieldfile.RecordValue(signedURL)
	result.SignedURLExpiresInSeconds = signedURLTTLSeconds

	return i.reportUpload(flags, result)
}

// resolveFilesProjectVersion resolves the project version WITH its schema, which
// is all `files describe` needs and the first half of what `files upload` needs.
//
// A --version that is already a UUID short-circuits the project lookup entirely:
// GetProjectVersion returns the version's own project uuid along with the
// schema, so nothing has to list the user's projects to find it.
func (i *Implementation) resolveFilesProjectVersion(flags filesUploadFlags) (*nemgen.ProjectVersion, error) {
	er, err := i.extensionRunner()
	if err != nil {
		return nil, err
	}

	if looksLikeUUID(flags.version) {
		pv, err := er.GetProjectVersion(flags.version)
		if err != nil {
			return nil, fmt.Errorf("could not load project version %s: %w", flags.version, err)
		}
		// Only checkable when --project is itself a uuid; a name would need the
		// lookup this path exists to avoid.
		if looksLikeUUID(flags.project) && !strings.EqualFold(flags.project, pv.GetProjectUuid()) {
			return nil, fmt.Errorf(
				"--project %s does not own project version %s (it belongs to project %s)",
				flags.project, flags.version, pv.GetProjectUuid())
		}
		return pv, nil
	}

	project, err := i.resolveUploadProject(er, flags)
	if err != nil {
		return nil, err
	}
	version, err := i.resolveUploadVersion(er, project.GetUuid(), flags)
	if err != nil {
		return nil, err
	}
	pv, err := er.GetProjectVersion(version.GetUuid())
	if err != nil {
		return nil, fmt.Errorf("could not load the schema of project version %s: %w", version.GetIdentifier(), err)
	}
	return pv, nil
}

// uploadTarget is everything the RPC needs, resolved.
type uploadTarget struct {
	projectUUID              string
	projectVersionUUID       string
	projectVersionIdentifier string
	entity                   *nemgen.Entity
	field                    *nemgen.Field
}

// resolveUploadTarget turns the flags into the four uuids the upload takes.
//
// A --version that is already a UUID short-circuits the whole project lookup:
// GetProjectVersion returns the version's own project uuid along with the
// schema, so nothing has to list the user's projects to find it.
func (i *Implementation) resolveUploadTarget(flags filesUploadFlags) (uploadTarget, error) {
	pv, err := i.resolveFilesProjectVersion(flags)
	if err != nil {
		return uploadTarget{}, err
	}

	entity, field, err := i.resolveUploadField(pv, flags)
	if err != nil {
		return uploadTarget{}, err
	}

	return uploadTarget{
		projectUUID:              pv.GetProjectUuid(),
		projectVersionUUID:       pv.GetUuid(),
		projectVersionIdentifier: pv.GetIdentifier(),
		entity:                   entity,
		field:                    field,
	}, nil
}

func (i *Implementation) resolveUploadProject(er extensionRunner, flags filesUploadFlags) (*nemgen.Project, error) {
	if flags.project != "" {
		return i.resolveProject(er, flags.project)
	}
	if flags.isNonInteractive() {
		return nil, fmt.Errorf("--project is required (or pass a project version uuid to --version)")
	}
	return i.SelectProject(er)
}

func (i *Implementation) resolveUploadVersion(er extensionRunner, projectUUID string, flags filesUploadFlags) (*nemgen.ProjectVersion, error) {
	if flags.version != "" {
		return i.resolveProjectVersion(er, projectUUID, flags.version)
	}
	if flags.isNonInteractive() {
		return nil, fmt.Errorf("--version is required when not prompting")
	}
	return i.SelectProjectVersion(er, projectUUID)
}

// resolveUploadField resolves --entity and --field, prompting for whichever is
// missing when prompting is allowed.
//
// The pickers list only entities that have somewhere to put a file, and only
// the file fields of the chosen entity. An unfiltered field list on a wide
// entity is unusable, and every non-file choice is a guaranteed failure one
// round trip later.
func (i *Implementation) resolveUploadField(pv *nemgen.ProjectVersion, flags filesUploadFlags) (*nemgen.Entity, *nemgen.Field, error) {
	if flags.entity != "" && flags.field != "" {
		return fieldfile.ResolveEntityField(pv, flags.entity, flags.field)
	}
	if flags.isNonInteractive() {
		return nil, nil, fmt.Errorf("--entity and --field are both required when not prompting")
	}

	entity, err := i.selectUploadEntity(pv, flags)
	if err != nil {
		return nil, nil, err
	}
	if flags.field != "" {
		return fieldfile.ResolveEntityField(pv, entity.GetUuid(), flags.field)
	}
	field, err := i.selectUploadField(entity)
	if err != nil {
		return nil, nil, err
	}
	return entity, field, nil
}

func (i *Implementation) selectUploadEntity(pv *nemgen.ProjectVersion, flags filesUploadFlags) (*nemgen.Entity, error) {
	if flags.entity != "" {
		for _, e := range pv.GetEntities() {
			if e.GetUuid() == flags.entity || strings.EqualFold(e.GetIdentifier(), flags.entity) {
				return e, nil
			}
		}
		return nil, fmt.Errorf("entity %q not found in this project version (match by identifier or uuid)", flags.entity)
	}

	var choices []*nemgen.Entity
	for _, e := range pv.GetEntities() {
		if len(fieldfile.FileFieldsOf(e)) > 0 {
			choices = append(choices, e)
		}
	}
	if len(choices) == 0 {
		return nil, fmt.Errorf("no entity in project version %q has a file, image, video or audio field to upload into", pv.GetIdentifier())
	}

	prompt := promptui.Select{
		Label: i.localize.Localize("files_upload_select_entity", "Select the entity"),
		Items: choices,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}?",
			Active:   "↠ {{ .Identifier | cyan }}",
			Inactive: "  {{ .Identifier | cyan }}",
			Selected: "↠ {{ .Identifier | cyan }}",
		},
	}
	index, _, err := prompt.Run()
	if err != nil {
		return nil, err
	}
	return choices[index], nil
}

func (i *Implementation) selectUploadField(entity *nemgen.Entity) (*nemgen.Field, error) {
	var choices []*nemgen.Field
	for _, f := range entity.GetFields() {
		if fieldfile.IsFileField(f) {
			choices = append(choices, f)
		}
	}
	if len(choices) == 0 {
		return nil, fmt.Errorf("entity %q has no file, image, video or audio field", entity.GetIdentifier())
	}

	prompt := promptui.Select{
		Label: i.localize.Localize("files_upload_select_field", "Select the field"),
		Items: choices,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}?",
			Active:   "↠ {{ .Identifier | cyan }}",
			Inactive: "  {{ .Identifier | cyan }}",
			Selected: "↠ {{ .Identifier | cyan }}",
		},
	}
	index, _, err := prompt.Run()
	if err != nil {
		return nil, err
	}
	return choices[index], nil
}

// uploadFileName picks the name the object is stored under. Both an explicit
// --name and a derived one go through the same sanitizer, so the two paths
// cannot disagree about the resulting key.
//
// A name that cleans away entirely ("---") has nothing left to store under and
// is an error. A name that keeps only its extension ("!!!.pdf" -> ".pdf") is
// NOT an error: the data manager produces exactly that and the CLI has to agree
// with it, so the caller warns instead.
func uploadFileName(override, path string) (string, error) {
	source := override
	if source == "" {
		source = filepath.Base(path)
	}
	name := fieldfile.SanitizeFileName(source)
	if name == "" {
		return "", fmt.Errorf("%q leaves nothing usable as a file name after cleaning — pass --name", source)
	}
	return name, nil
}

// sniffHead returns the prefix http.DetectContentType needs, and no more.
func sniffHead(data []byte) []byte {
	if len(data) > 512 {
		return data[:512]
	}
	return data
}

// shortEnum renders a protobuf enum without its type prefix: "IMAGE", not
// "FIELD_TYPE_IMAGE".
func shortEnum(s, prefix string) string { return strings.TrimPrefix(s, prefix) }

func (i *Implementation) uploadRecordFieldFile(flags filesUploadFlags, t uploadTarget, fileName string, data []byte) (string, error) {
	timeout := flags.timeout
	if timeout <= 0 || timeout > fieldfile.MaxTimeout {
		timeout = fieldfile.MaxTimeout
	}

	ctx, cancel, err := productclient.ClientContextWithTimeout(timeout)
	if err != nil {
		return "", fmt.Errorf("error building auth context: %w", err)
	}
	defer cancel()

	res, err := i.productClient.ProductClient.UploadRecordFieldFile(ctx, &pb.UploadRecordFieldFileRequest{
		ProjectUuid:        t.projectUUID,
		ProjectVersionUuid: t.projectVersionUUID,
		EntityUuid:         t.entity.GetUuid(),
		FieldUuid:          t.field.GetUuid(),
		FileName:           fileName,
		FileData:           data,
		ForceOverride:      flags.force,
	})
	if err != nil {
		return "", err
	}
	return res.GetUrl(), nil
}

// warnAboutStorage says the things that are true before the upload and easy to
// get wrong, on stderr so --json stdout stays clean.
func (i *Implementation) warnAboutStorage(flags filesUploadFlags, cfg *nemgen.FieldTypeFileConfig, key string) {
	if !fieldfile.UsesObjectStore(cfg) {
		msg := "This field stores files in nuzur's own storage, not your object store, " +
			"and its key is generated fresh for every upload."
		if flags.force {
			// Silently accepting a flag that does nothing is how people come to
			// believe in behaviour that does not exist.
			msg += " --force has no effect here: nothing can collide."
		}
		i.warn(flags, msg)
		return
	}
	if key != "" {
		i.progress(flags, fmt.Sprintf("Destination key: %s", key))
	}
}

// failUpload maps a gRPC failure onto something the user can act on, using the
// context the CLI already has in hand (the key, the field, the timeout).
func (i *Implementation) failUpload(flags filesUploadFlags, err error, t uploadTarget, cfg *nemgen.FieldTypeFileConfig, key string) error {
	st, ok := status.FromError(err)
	if !ok {
		return failWith(flags.jsonOutput, i.localize.Localize("files_upload_error", "File upload failed"),
			jsonError{Status: "error", Message: err.Error()})
	}

	msg := st.Message()
	switch st.Code() {
	case codes.AlreadyExists:
		msg = fmt.Sprintf("%s\n  re-run with --force to overwrite it, or pass --name to upload under a different file name", msg)
	case codes.NotFound:
		msg = fmt.Sprintf("%s\n  the project version may be a draft you cannot read, or the entity or field may have been removed since", msg)
	case codes.InvalidArgument:
		msg = fmt.Sprintf("%s\n  %s.%s is a %s field with %s storage",
			msg, t.entity.GetIdentifier(), t.field.GetIdentifier(),
			shortEnum(t.field.GetType().String(), "FIELD_TYPE_"),
			shortEnum(cfg.GetStorageType().String(), "FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_"))
	case codes.Unimplemented:
		msg = fmt.Sprintf("%s\n  the object store attached to this field is of a type nuzur cannot upload to yet", msg)
	case codes.Unauthenticated:
		msg = fmt.Sprintf("%s\n  run `nuzur-cli login`", msg)
	case codes.PermissionDenied:
		msg = fmt.Sprintf("%s\n  you may not have access to this project's team", msg)
	case codes.ResourceExhausted:
		msg = fmt.Sprintf("%s\n  the server rejected the request as too large; try a smaller file", msg)
	case codes.DeadlineExceeded:
		msg = fmt.Sprintf("upload did not finish within %s\n  raise --timeout, though nuzur's ingress cuts a single request off at 10m", flags.timeout)
	case codes.Unavailable:
		msg = fmt.Sprintf("%s\n  could not reach nuzur — check your network", msg)
	}

	return failWith(flags.jsonOutput, i.localize.Localize("files_upload_error", "File upload failed"),
		jsonError{Status: "error", Code: st.Code().String(), Message: msg})
}

// reportUpload writes the result: the JSON document on stdout in --json mode,
// otherwise a human summary plus the advisory that earns this command its
// keep — the record value and the signed URL are not interchangeable.
func (i *Implementation) reportUpload(flags filesUploadFlags, r filesUploadResult) error {
	if flags.jsonOutput {
		return printJSONValue(r)
	}

	verb := "Uploaded"
	if r.Status == "dry_run" {
		verb = "Would upload"
	}
	outputtools.PrintlnColored(fmt.Sprintf("%s %s (%s) to %s.%s",
		verb, r.FileName, fieldfile.HumanSize(r.SizeBytes), r.Entity.Identifier, r.Field.Identifier), outputtools.Green)

	// The detail lines are printed uncolored rather than with outputtools.Reset,
	// which would wrap them in a pointless escape sequence — these are values a
	// user copies or a script cuts out of, so they stay plain text.
	if r.ObjectKey != "" {
		fmt.Fprintf(outputtools.Stdout, "Object key:   %s\n", r.ObjectKey)
	}
	if r.Status == "dry_run" {
		return nil
	}
	fmt.Fprintf(outputtools.Stdout, "Record value: %s\n", r.RecordValue)
	fmt.Fprintf(outputtools.Stdout, "Signed URL:   %s\n", r.SignedURL)

	i.warn(flags, "Store the record value in the field, not the signed URL: the signed URL expires in 24h. "+
		"The record value is an identifier, not a public link — nuzur re-signs it when the record is read.")
	return nil
}

// progress and warn keep commentary on stderr, which is what makes --json
// stdout safe to pipe into a parser.
func (i *Implementation) progress(flags filesUploadFlags, msg string) {
	if flags.jsonOutput {
		return
	}
	outputtools.PrintlnColoredErr(msg, outputtools.Blue)
}

func (i *Implementation) warn(flags filesUploadFlags, msg string) {
	if flags.jsonOutput {
		return
	}
	outputtools.PrintlnColoredErr(msg, outputtools.Yellow)
}
