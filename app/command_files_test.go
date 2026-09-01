package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/fieldfile"
	"github.com/nuzur/nuzur-cli/localize"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pngBytes is a minimal but genuine PNG, so http.DetectContentType agrees it is
// an image and the media-kind check exercises its real path.
var pngBytes = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0x00}, 64)...)

// uploadSchema is the project version the upload tests resolve against: one
// entity with a file field, an image field, and a plain varchar that must not
// be uploadable.
func uploadSchema() []*nemgen.Entity {
	objectStore := func(path string, maxKB int64, exts []string) *nemgen.FieldTypeFileConfig {
		return &nemgen.FieldTypeFileConfig{
			StorageType:       nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_OBJECT_STORE,
			AllowedExtensions: exts,
			MaxSize:           maxKB,
			StorageConfig: &nemgen.FileStorageConfig{
				ObjectStore: &nemgen.FileObjectStorageConfig{
					ObjectStoreUuid: "f8888e33-0000-0000-0000-0000000000os",
					Path:            path,
				},
			},
		}
	}
	return []*nemgen.Entity{
		{
			Uuid:       "f8888e33-0000-0000-0000-0000000000e1",
			Identifier: "product",
			Fields: []*nemgen.Field{
				{Uuid: "f8888e33-0000-0000-0000-0000000000f0", Identifier: "name", Type: nemgen.FieldType_FIELD_TYPE_VARCHAR},
				{
					Uuid: "f8888e33-0000-0000-0000-0000000000f1", Identifier: "spec_sheet",
					Type:       nemgen.FieldType_FIELD_TYPE_FILE,
					TypeConfig: &nemgen.FieldTypeConfig{File: objectStore("/assets/specs", 0, nil)},
				},
				{
					Uuid: "f8888e33-0000-0000-0000-0000000000f2", Identifier: "hero_image",
					Type:       nemgen.FieldType_FIELD_TYPE_IMAGE,
					TypeConfig: &nemgen.FieldTypeConfig{Image: objectStore("/assets/heroes", 1, []string{"png"})},
				},
				{
					Uuid: "f8888e33-0000-0000-0000-0000000000f3", Identifier: "manual",
					Type: nemgen.FieldType_FIELD_TYPE_FILE,
					TypeConfig: &nemgen.FieldTypeConfig{File: &nemgen.FieldTypeFileConfig{
						StorageType: nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_BINARY,
					}},
				},
			},
		},
	}
}

// uploadTestEnv wires an Implementation with every seam filled and both output
// streams captured, and returns the two buffers.
type uploadTestEnv struct {
	imp    *Implementation
	client *fakeProductClient
	runner *fakeExtensionRunner
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newUploadTestEnv(t *testing.T) *uploadTestEnv {
	t.Helper()
	isolateHome(t) // for productclient.ClientContextWithTimeout's token read

	client := newFakeProductClient()
	runner := newFakeExtensionRunner()
	runner.SchemaEntities = uploadSchema()

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	prevOut, prevErr := outputtools.Stdout, outputtools.Stderr
	outputtools.Stdout, outputtools.Stderr = stdout, stderr
	t.Cleanup(func() { outputtools.Stdout, outputtools.Stderr = prevOut, prevErr })

	imp := &Implementation{
		// localize must be non-nil: the command's messages all go through it.
		localize:      localize.New(),
		productClient: &productclient.Client{ProductClient: client},
		loginFn:       func() error { return nil },
		newExtensionRunner: func() (extensionRunner, error) {
			return runner, nil
		},
	}
	return &uploadTestEnv{imp: imp, client: client, runner: runner, stdout: stdout, stderr: stderr}
}

// writeTempFile puts content on disk and returns its path.
func writeTempFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// baseFlags is a fully non-interactive upload of the file field.
func baseFlags() filesUploadFlags {
	return filesUploadFlags{
		version:    fakeProjectVersionUUID,
		entity:     "product",
		field:      "spec_sheet",
		jsonOutput: true,
		maxBytes:   fieldfile.DefaultMaxBytes,
	}
}

func decodeResult(t *testing.T, out string) filesUploadResult {
	t.Helper()
	var r filesUploadResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not the JSON document: %v\n%s", err, out)
	}
	return r
}

func TestFilesUploadHappyPath(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "Spec Sheet v2.PDF", []byte("%PDF-1.4 fake"))

	if err := env.imp.runFilesUpload(path, baseFlags()); err != nil {
		t.Fatalf("runFilesUpload: %v\nstderr: %s", err, env.stderr)
	}

	calls := env.client.CallsTo("UploadRecordFieldFile")
	if len(calls) != 1 {
		t.Fatalf("want exactly one upload, got %d (%v)", len(calls), env.client.MethodSequence())
	}
	p := calls[0].Params
	// The name is sanitized the way the data manager sanitizes it — this is the
	// assertion that keeps CLI and UI uploads on the same object key.
	if p["file_name"] != "spec_sheet_v2.pdf" {
		t.Errorf("file_name = %q, want %q", p["file_name"], "spec_sheet_v2.pdf")
	}
	if p["entity_uuid"] != "f8888e33-0000-0000-0000-0000000000e1" {
		t.Errorf("entity_uuid = %q", p["entity_uuid"])
	}
	if p["field_uuid"] != "f8888e33-0000-0000-0000-0000000000f1" {
		t.Errorf("field_uuid = %q", p["field_uuid"])
	}
	if p["project_version_uuid"] != fakeProjectVersionUUID {
		t.Errorf("project_version_uuid = %q", p["project_version_uuid"])
	}
	if p["file_bytes"] != "13" {
		t.Errorf("file_bytes = %q, want 13", p["file_bytes"])
	}
	if p["force_override"] != "false" {
		t.Errorf("force_override = %q, want false", p["force_override"])
	}

	r := decodeResult(t, env.stdout.String())
	if r.Status != "uploaded" {
		t.Errorf("status = %q", r.Status)
	}
	if r.ObjectKey != "assets/specs/spec_sheet_v2.pdf" {
		t.Errorf("object_key = %q", r.ObjectKey)
	}
	// The record value is the signed URL with its query removed. Storing the
	// signed URL instead is the mistake this whole command is shaped to prevent.
	if r.RecordValue != "https://bucket.s3.us-east-1.amazonaws.com/uploads/f.png" {
		t.Errorf("record_value = %q", r.RecordValue)
	}
	if !strings.Contains(r.SignedURL, "X-Amz-Signature") {
		t.Errorf("signed_url should be the signed one, got %q", r.SignedURL)
	}
	if r.SignedURLExpiresInSeconds != signedURLTTLSeconds {
		t.Errorf("signed_url_expires_in_seconds = %d", r.SignedURLExpiresInSeconds)
	}
	if r.Field.Type != "FILE" || r.StorageType != "OBJECT_STORE" {
		t.Errorf("field type / storage = %q / %q", r.Field.Type, r.StorageType)
	}
}

// A --version that is a uuid must not send the command looking for projects.
func TestFilesUploadWithVersionUUIDSkipsProjectLookup(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "a.pdf", []byte("x"))

	if err := env.imp.runFilesUpload(path, baseFlags()); err != nil {
		t.Fatalf("runFilesUpload: %v", err)
	}
	for _, m := range env.runner.calls {
		if m == "ListUserProjects" || m == "ListProjectVersions" {
			t.Errorf("resolved via %s despite a uuid --version (%v)", m, env.runner.calls)
		}
	}
}

// Pre-flight has to be pre-flight: a bad target must cost zero round trips.
func TestFilesUploadValidationHappensBeforeAnyUpload(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content []byte
		mutate  func(*filesUploadFlags)
		wantErr string
	}{
		{
			name: "a varchar field cannot hold a file",
			file: "a.pdf", content: []byte("x"),
			mutate:  func(f *filesUploadFlags) { f.field = "name" },
			wantErr: "varchar",
		},
		{
			// Real PNG bytes so the content check passes and the allow-list is
			// unambiguously what rejects this.
			name: "an extension outside the allow-list",
			file: "a.pdf", content: pngBytes,
			mutate:  func(f *filesUploadFlags) { f.field = "hero_image" },
			wantErr: "not an accepted file type",
		},
		{
			name: "a file over the field's max size",
			file: "big.png", content: append(pngBytes, bytes.Repeat([]byte{0x00}, 4096)...),
			mutate:  func(f *filesUploadFlags) { f.field = "hero_image" },
			wantErr: "over this field's limit",
		},
		{
			name: "a file over the client guard",
			file: "a.pdf", content: bytes.Repeat([]byte{0x01}, 4096),
			mutate:  func(f *filesUploadFlags) { f.maxBytes = 1024 },
			wantErr: "above the",
		},
		{
			name: "an unknown entity",
			file: "a.pdf", content: []byte("x"),
			mutate:  func(f *filesUploadFlags) { f.entity = "prodcut" },
			wantErr: "not found",
		},
		{
			name: "an unknown field",
			file: "a.pdf", content: []byte("x"),
			mutate:  func(f *filesUploadFlags) { f.field = "spec_shet" },
			wantErr: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newUploadTestEnv(t)
			path := writeTempFile(t, tt.file, tt.content)
			flags := baseFlags()
			tt.mutate(&flags)

			err := env.imp.runFilesUpload(path, flags)
			if err == nil {
				t.Fatal("expected a failure")
			}
			if got := len(env.client.CallsTo("UploadRecordFieldFile")); got != 0 {
				t.Errorf("uploaded %d times despite failing validation", got)
			}
			if !strings.Contains(env.stdout.String(), tt.wantErr) {
				t.Errorf("error envelope does not mention %q:\n%s", tt.wantErr, env.stdout.String())
			}
		})
	}
}

// An image field accepts a real PNG within its limits — the positive half of
// the media-kind and extension checks above.
func TestFilesUploadImageFieldAcceptsAPNG(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "Hero.PNG", pngBytes)
	flags := baseFlags()
	flags.field = "hero_image"

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v\nstdout: %s", err, env.stdout)
	}
	r := decodeResult(t, env.stdout.String())
	if r.ObjectKey != "assets/heroes/hero.png" {
		t.Errorf("object_key = %q", r.ObjectKey)
	}
}

// A PDF into an image field is caught by content sniffing even when the
// extension allow-list would not have stopped it.
func TestFilesUploadImageFieldRejectsNonImageContent(t *testing.T) {
	env := newUploadTestEnv(t)
	// Named .png so the extension check passes and the kind check is what fires.
	path := writeTempFile(t, "not_really.png", []byte("%PDF-1.4 this is a pdf, not an image"))
	flags := baseFlags()
	flags.field = "hero_image"

	if err := env.imp.runFilesUpload(path, flags); err == nil {
		t.Fatal("expected a PDF into an image field to be refused")
	}
	if len(env.client.CallsTo("UploadRecordFieldFile")) != 0 {
		t.Error("uploaded despite failing the content check")
	}
}

func TestFilesUploadNameOverrideIsStillSanitized(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "a.pdf", []byte("x"))
	flags := baseFlags()
	flags.name = "Weird Name (final).PDF"

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v", err)
	}
	if got := env.client.CallsTo("UploadRecordFieldFile")[0].Params["file_name"]; got != "weird_name_final.pdf" {
		t.Errorf("file_name = %q, want %q", got, "weird_name_final.pdf")
	}
}

func TestFilesUploadAlreadyExistsPointsAtForce(t *testing.T) {
	env := newUploadTestEnv(t)
	env.client.UploadRecordFieldFileErr = status.Error(codes.AlreadyExists, "file already exists: assets/specs/a.pdf")
	path := writeTempFile(t, "a.pdf", []byte("x"))

	err := env.imp.runFilesUpload(path, baseFlags())
	if err == nil {
		t.Fatal("expected the AlreadyExists failure to surface")
	}

	var env2 jsonError
	if uErr := json.Unmarshal(env.stdout.Bytes(), &env2); uErr != nil {
		t.Fatalf("stdout is not the error envelope: %v\n%s", uErr, env.stdout)
	}
	if env2.Status != "error" || env2.Code != codes.AlreadyExists.String() {
		t.Errorf("envelope status/code = %q/%q", env2.Status, env2.Code)
	}
	if !strings.Contains(env2.Message, "--force") {
		t.Errorf("message should point at --force, got %q", env2.Message)
	}
}

func TestFilesUploadForcePropagates(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "a.pdf", []byte("x"))
	flags := baseFlags()
	flags.force = true

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v", err)
	}
	if got := env.client.CallsTo("UploadRecordFieldFile")[0].Params["force_override"]; got != "true" {
		t.Errorf("force_override = %q, want true", got)
	}
}

func TestFilesUploadDryRunUploadsNothing(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "Spec Sheet.PDF", []byte("x"))
	flags := baseFlags()
	flags.dryRun = true

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v", err)
	}
	if got := len(env.client.CallsTo("UploadRecordFieldFile")); got != 0 {
		t.Fatalf("--dry-run uploaded %d times", got)
	}

	r := decodeResult(t, env.stdout.String())
	if r.Status != "dry_run" {
		t.Errorf("status = %q, want dry_run", r.Status)
	}
	if r.ObjectKey != "assets/specs/spec_sheet.pdf" {
		t.Errorf("object_key = %q", r.ObjectKey)
	}
	if r.RecordValue != "" || r.SignedURL != "" {
		t.Errorf("--dry-run must not invent a value or a url, got %q / %q", r.RecordValue, r.SignedURL)
	}
}

// The stream contract: in --json mode stdout is the document and nothing else,
// so it stays safe to pipe into a parser.
func TestFilesUploadJSONKeepsStdoutClean(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scriptErr bool
	}{
		{"success", false},
		{"failure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newUploadTestEnv(t)
			if tc.scriptErr {
				env.client.UploadRecordFieldFileErr = status.Error(codes.Unavailable, "nope")
			}
			path := writeTempFile(t, "a.pdf", []byte("x"))
			_ = env.imp.runFilesUpload(path, baseFlags())

			out := strings.TrimSpace(env.stdout.String())
			var any map[string]interface{}
			if err := json.Unmarshal([]byte(out), &any); err != nil {
				t.Fatalf("stdout is not exactly one JSON document: %v\n%s", err, out)
			}
		})
	}
}

// Without --json the human summary goes to stdout and the advisory to stderr.
func TestFilesUploadHumanOutputSeparatesStreams(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "a.pdf", []byte("x"))
	flags := baseFlags()
	flags.jsonOutput = false
	flags.nonInteractive = true

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v", err)
	}
	if !strings.Contains(env.stdout.String(), "Record value:") {
		t.Errorf("stdout should carry the record value:\n%s", env.stdout)
	}
	if !strings.Contains(env.stderr.String(), "expires") {
		t.Errorf("stderr should carry the expiry advisory:\n%s", env.stderr)
	}
}

// Non-interactive mode names what is missing instead of trying to prompt.
func TestFilesUploadNonInteractiveRequiresTargets(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "a.pdf", []byte("x"))
	flags := baseFlags()
	flags.entity = ""

	if err := env.imp.runFilesUpload(path, flags); err == nil {
		t.Fatal("expected a missing --entity to fail rather than prompt")
	}
	if !strings.Contains(env.stdout.String(), "--entity") {
		t.Errorf("error should name --entity:\n%s", env.stdout)
	}
}

func TestFilesUploadRejectsADirectory(t *testing.T) {
	env := newUploadTestEnv(t)
	if err := env.imp.runFilesUpload(t.TempDir(), baseFlags()); err == nil {
		t.Fatal("expected a directory to be refused")
	}
	if len(env.client.CallsTo("UploadRecordFieldFile")) != 0 {
		t.Error("uploaded a directory")
	}
}

func TestUploadFileName(t *testing.T) {
	if _, err := uploadFileName("", "/tmp/---"); err == nil {
		t.Error("a name that cleans away entirely should be an error")
	}
	// Parity with the data manager: a base that cleans away but keeps an
	// extension is accepted, not rejected.
	got, err := uploadFileName("", "/tmp/!!!.pdf")
	if err != nil {
		t.Fatalf("uploadFileName: %v", err)
	}
	if got != ".pdf" {
		t.Errorf("uploadFileName = %q, want %q", got, ".pdf")
	}
}

// A BINARY field stores into nuzur's own bucket under a server-generated key,
// so there is no key to predict and --force cannot mean anything.
func TestFilesUploadBinaryFieldHasNoPredictableKey(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "manual.pdf", []byte("%PDF-1.4"))
	flags := baseFlags()
	flags.field = "manual"

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v\nstdout: %s", err, env.stdout)
	}

	r := decodeResult(t, env.stdout.String())
	if r.StorageType != "BINARY" {
		t.Errorf("storage_type = %q, want BINARY", r.StorageType)
	}
	if r.ObjectKey != "" {
		t.Errorf("object_key should be omitted for a BINARY field, got %q", r.ObjectKey)
	}
	if r.RecordValue == "" {
		t.Error("a BINARY upload still yields a record value")
	}
}

// --force against a BINARY field does nothing, and saying so beats letting the
// user believe it did something.
func TestFilesUploadForceOnBinaryFieldWarns(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "manual.pdf", []byte("%PDF-1.4"))
	flags := baseFlags()
	flags.field = "manual"
	flags.force = true
	flags.jsonOutput = false
	flags.nonInteractive = true

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v", err)
	}
	if !strings.Contains(env.stderr.String(), "--force has no effect") {
		t.Errorf("stderr should say --force does nothing here:\n%s", env.stderr)
	}
}

// The same warning must NOT appear when --force was not passed.
func TestFilesUploadBinaryFieldWithoutForceDoesNotMentionIt(t *testing.T) {
	env := newUploadTestEnv(t)
	path := writeTempFile(t, "manual.pdf", []byte("%PDF-1.4"))
	flags := baseFlags()
	flags.field = "manual"
	flags.jsonOutput = false
	flags.nonInteractive = true

	if err := env.imp.runFilesUpload(path, flags); err != nil {
		t.Fatalf("runFilesUpload: %v", err)
	}
	if strings.Contains(env.stderr.String(), "--force") {
		t.Errorf("stderr should not mention a flag the user did not pass:\n%s", env.stderr)
	}
}
