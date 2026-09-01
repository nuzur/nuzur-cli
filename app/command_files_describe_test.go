package app

import (
	"encoding/json"
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
)

func decodeDescribe(t *testing.T, out string) filesDescribeResult {
	t.Helper()
	var r filesDescribeResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not the describe document: %v\n%s", err, out)
	}
	return r
}

func describeFlags() filesUploadFlags {
	return filesUploadFlags{version: fakeProjectVersionUUID, jsonOutput: true}
}

// describe is the discovery entry point, so the thing it must get right is
// listing every file field WITHOUT being given a file.
func TestFilesDescribeListsEveryFileField(t *testing.T) {
	env := newUploadTestEnv(t)

	if err := env.imp.runFilesDescribe(describeFlags()); err != nil {
		t.Fatalf("runFilesDescribe: %v\nstderr: %s", err, env.stderr)
	}

	r := decodeDescribe(t, env.stdout.String())
	if r.Status != "describe" {
		t.Errorf("status = %q", r.Status)
	}
	if r.ProjectVersionUUID != fakeProjectVersionUUID {
		t.Errorf("project_version_uuid = %q", r.ProjectVersionUUID)
	}

	got := map[string]describedFileField{}
	for _, f := range r.FileFields {
		got[f.Entity.Identifier+"."+f.Field.Identifier] = f
	}
	// The three file fields, and NOT the varchar.
	for _, want := range []string{"product.spec_sheet", "product.hero_image", "product.manual"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s missing from describe (got %v)", want, keysOf(got))
		}
	}
	if _, ok := got["product.name"]; ok {
		t.Error("a varchar field must not be listed as a file field")
	}
}

func TestFilesDescribeReportsWhatEachFieldAccepts(t *testing.T) {
	env := newUploadTestEnv(t)
	if err := env.imp.runFilesDescribe(describeFlags()); err != nil {
		t.Fatalf("runFilesDescribe: %v", err)
	}
	r := decodeDescribe(t, env.stdout.String())

	var hero describedFileField
	for _, f := range r.FileFields {
		if f.Field.Identifier == "hero_image" {
			hero = f
		}
	}

	if hero.Field.Type != "IMAGE" || hero.StorageType != "OBJECT_STORE" {
		t.Errorf("hero type/storage = %q/%q", hero.Field.Type, hero.StorageType)
	}
	if hero.Path != "/assets/heroes" || hero.KeyPrefix != "assets/heroes" {
		t.Errorf("path/key_prefix = %q/%q", hero.Path, hero.KeyPrefix)
	}
	if len(hero.AllowedExtensions) != 1 || hero.AllowedExtensions[0] != "png" {
		t.Errorf("allowed_extensions = %v", hero.AllowedExtensions)
	}
	if hero.MaxSizeKB != 1 {
		t.Errorf("max_size_kb = %d", hero.MaxSizeKB)
	}
	if !hero.Uploadable {
		t.Errorf("hero_image should be uploadable, reason: %q", hero.Reason)
	}
	// The command has to be usable verbatim.
	for _, frag := range []string{"files upload", "--entity product", "--field hero_image", fakeProjectVersionUUID} {
		if !strings.Contains(hero.UploadCommand, frag) {
			t.Errorf("upload_command missing %q: %s", frag, hero.UploadCommand)
		}
	}
}

// A BINARY field has no object store, so it must report none rather than an
// empty-string store uuid that looks like a real one.
func TestFilesDescribeOmitsObjectStoreForBinary(t *testing.T) {
	env := newUploadTestEnv(t)
	if err := env.imp.runFilesDescribe(describeFlags()); err != nil {
		t.Fatalf("runFilesDescribe: %v", err)
	}
	r := decodeDescribe(t, env.stdout.String())

	for _, f := range r.FileFields {
		if f.Field.Identifier != "manual" {
			continue
		}
		if f.StorageType != "BINARY" {
			t.Errorf("storage_type = %q, want BINARY", f.StorageType)
		}
		if f.ObjectStoreUUID != "" || f.Path != "" || f.KeyPrefix != "" {
			t.Errorf("BINARY field should carry no object-store detail, got %q/%q/%q",
				f.ObjectStoreUUID, f.Path, f.KeyPrefix)
		}
		if !f.Uploadable {
			t.Errorf("a BINARY field is uploadable, reason: %q", f.Reason)
		}
		return
	}
	t.Fatal("manual not found")
}

// The case this exists for: a misconfigured field is still LISTED, with a
// reason. An agent must be able to tell "no such field" from "field is broken"
// — the real chorus project has four fields in exactly this state.
func TestFilesDescribeListsUnuploadableFieldsWithAReason(t *testing.T) {
	env := newUploadTestEnv(t)
	env.runner.SchemaEntities = append(env.runner.SchemaEntities, &nemgen.Entity{
		Uuid:       "f8888e33-0000-0000-0000-0000000000e2",
		Identifier: "recording",
		Fields: []*nemgen.Field{{
			Uuid: "f8888e33-0000-0000-0000-0000000000f9", Identifier: "spectrogram",
			Type: nemgen.FieldType_FIELD_TYPE_IMAGE,
			// storage_type left at its zero value, as chorus does.
			TypeConfig: &nemgen.FieldTypeConfig{Image: &nemgen.FieldTypeFileConfig{}},
		}},
	})

	if err := env.imp.runFilesDescribe(describeFlags()); err != nil {
		t.Fatalf("runFilesDescribe: %v", err)
	}
	r := decodeDescribe(t, env.stdout.String())

	for _, f := range r.FileFields {
		if f.Field.Identifier != "spectrogram" {
			continue
		}
		if f.Uploadable {
			t.Error("a field with no storage type must not be reported uploadable")
		}
		if !strings.Contains(f.Reason, "storage type") {
			t.Errorf("reason should explain the misconfiguration, got %q", f.Reason)
		}
		return
	}
	t.Fatal("spectrogram was dropped from describe — a broken field must still be listed")
}

// describe must never invent an empty list into a null, and must stay pipeable.
func TestFilesDescribeEmitsAnArrayEvenWithNoFileFields(t *testing.T) {
	env := newUploadTestEnv(t)
	env.runner.SchemaEntities = []*nemgen.Entity{{
		Uuid: "f8888e33-0000-0000-0000-0000000000e3", Identifier: "tag",
		Fields: []*nemgen.Field{{Uuid: "x", Identifier: "label", Type: nemgen.FieldType_FIELD_TYPE_VARCHAR}},
	}}

	if err := env.imp.runFilesDescribe(describeFlags()); err != nil {
		t.Fatalf("runFilesDescribe: %v", err)
	}
	if !strings.Contains(env.stdout.String(), `"file_fields": []`) {
		t.Errorf("want an empty array, not null:\n%s", env.stdout)
	}
}

// describe reports on configuration only, so it must not need a project database
// or upload anything.
func TestFilesDescribeUploadsNothing(t *testing.T) {
	env := newUploadTestEnv(t)
	if err := env.imp.runFilesDescribe(describeFlags()); err != nil {
		t.Fatalf("runFilesDescribe: %v", err)
	}
	if got := len(env.client.CallsTo("UploadRecordFieldFile")); got != 0 {
		t.Errorf("describe made %d uploads", got)
	}
}

func keysOf(m map[string]describedFileField) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
