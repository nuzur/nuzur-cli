package fieldfile

import (
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
)

func testProjectVersion() *nemgen.ProjectVersion {
	return &nemgen.ProjectVersion{
		Uuid:       "pv-uuid",
		Identifier: "v3",
		Entities: []*nemgen.Entity{
			{
				Uuid:       "entity-invoice",
				Identifier: "invoice",
				Fields: []*nemgen.Field{
					{Uuid: "field-number", Identifier: "number", Type: nemgen.FieldType_FIELD_TYPE_VARCHAR},
					{
						Uuid: "field-attachment", Identifier: "attachment", Type: nemgen.FieldType_FIELD_TYPE_FILE,
						TypeConfig: &nemgen.FieldTypeConfig{File: objectStoreConfig("/uploads/invoices")},
					},
				},
			},
			{
				Uuid:       "entity-tag",
				Identifier: "tag",
				Fields: []*nemgen.Field{
					{Uuid: "field-label", Identifier: "label", Type: nemgen.FieldType_FIELD_TYPE_VARCHAR},
				},
			},
			{
				Uuid:       "entity-user",
				Identifier: "user_profile",
				Fields: []*nemgen.Field{
					{
						Uuid: "field-avatar", Identifier: "avatar", Type: nemgen.FieldType_FIELD_TYPE_IMAGE,
						TypeConfig: &nemgen.FieldTypeConfig{Image: objectStoreConfig("/avatars")},
					},
				},
			},
		},
	}
}

func objectStoreConfig(path string) *nemgen.FieldTypeFileConfig {
	return &nemgen.FieldTypeFileConfig{
		StorageType: nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_OBJECT_STORE,
		StorageConfig: &nemgen.FileStorageConfig{
			ObjectStore: &nemgen.FileObjectStorageConfig{ObjectStoreUuid: "store-uuid", Path: path},
		},
	}
}

func TestResolveEntityField(t *testing.T) {
	pv := testProjectVersion()

	t.Run("by identifier", func(t *testing.T) {
		e, f, err := ResolveEntityField(pv, "invoice", "attachment")
		if err != nil {
			t.Fatal(err)
		}
		if e.GetUuid() != "entity-invoice" || f.GetUuid() != "field-attachment" {
			t.Errorf("resolved %s/%s", e.GetUuid(), f.GetUuid())
		}
	})

	t.Run("by uuid", func(t *testing.T) {
		e, f, err := ResolveEntityField(pv, "entity-invoice", "field-attachment")
		if err != nil {
			t.Fatal(err)
		}
		if e.GetIdentifier() != "invoice" || f.GetIdentifier() != "attachment" {
			t.Errorf("resolved %s/%s", e.GetIdentifier(), f.GetIdentifier())
		}
	})

	t.Run("identifier match is case-insensitive", func(t *testing.T) {
		if _, _, err := ResolveEntityField(pv, "INVOICE", "Attachment"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a non-file field still resolves", func(t *testing.T) {
		// Resolution and suitability are separate steps: resolving succeeds so
		// FileConfig can produce the error that names the field's actual type.
		if _, _, err := ResolveEntityField(pv, "invoice", "number"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown entity suggests entities that have file fields", func(t *testing.T) {
		_, _, err := ResolveEntityField(pv, "invoce", "attachment")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "invoice") || !strings.Contains(err.Error(), "user_profile") {
			t.Errorf("want suggestions naming invoice and user_profile, got %q", err)
		}
		if strings.Contains(err.Error(), "tag") {
			t.Errorf("tag has no file field and should not be suggested: %q", err)
		}
	})

	t.Run("unknown field suggests the entity's file fields", func(t *testing.T) {
		_, _, err := ResolveEntityField(pv, "invoice", "attachmnt")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "attachment") {
			t.Errorf("want a suggestion naming attachment, got %q", err)
		}
	})

	t.Run("a field on another entity does not match", func(t *testing.T) {
		if _, _, err := ResolveEntityField(pv, "invoice", "avatar"); err == nil {
			t.Fatal("a field belonging to a different entity must not resolve")
		}
	})

	t.Run("missing references are named", func(t *testing.T) {
		if _, _, err := ResolveEntityField(pv, "", "attachment"); err == nil {
			t.Error("expected an error for an empty entity reference")
		}
		if _, _, err := ResolveEntityField(pv, "invoice", ""); err == nil {
			t.Error("expected an error for an empty field reference")
		}
	})
}

func TestFileConfig(t *testing.T) {
	pv := testProjectVersion()

	t.Run("each media type reads its own config slot", func(t *testing.T) {
		for _, tc := range []struct {
			typ  nemgen.FieldType
			conf *nemgen.FieldTypeConfig
		}{
			{nemgen.FieldType_FIELD_TYPE_FILE, &nemgen.FieldTypeConfig{File: objectStoreConfig("/f")}},
			{nemgen.FieldType_FIELD_TYPE_IMAGE, &nemgen.FieldTypeConfig{Image: objectStoreConfig("/i")}},
			{nemgen.FieldType_FIELD_TYPE_VIDEO, &nemgen.FieldTypeConfig{Video: objectStoreConfig("/v")}},
			{nemgen.FieldType_FIELD_TYPE_AUDIO, &nemgen.FieldTypeConfig{Audio: objectStoreConfig("/a")}},
		} {
			f := &nemgen.Field{Identifier: "x", Type: tc.typ, TypeConfig: tc.conf}
			cfg, err := FileConfig(f)
			if err != nil {
				t.Fatalf("%v: %v", tc.typ, err)
			}
			if cfg == nil {
				t.Fatalf("%v: nil config", tc.typ)
			}
		}
	})

	t.Run("a non-file field names its actual type", func(t *testing.T) {
		_, f, _ := ResolveEntityField(pv, "invoice", "number")
		_, err := FileConfig(f)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "varchar") {
			t.Errorf("error should name the field's real type, got %q", err)
		}
	})

	t.Run("a file field with no config is actionable", func(t *testing.T) {
		f := &nemgen.Field{Identifier: "attachment", Type: nemgen.FieldType_FIELD_TYPE_FILE}
		_, err := FileConfig(f)
		if err == nil || !strings.Contains(err.Error(), "no file configuration") {
			t.Errorf("got %v", err)
		}
	})
}

func TestObjectKeyAndRecordValue(t *testing.T) {
	objectStore := objectStoreConfig("/uploads/invoices/")
	if got, want := ObjectKey(objectStore, "a.pdf"), "uploads/invoices/a.pdf"; got != want {
		t.Errorf("ObjectKey = %q, want %q", got, want)
	}
	if got, want := ObjectKey(objectStoreConfig(""), "a.pdf"), "a.pdf"; got != want {
		t.Errorf("ObjectKey with an empty path = %q, want %q", got, want)
	}

	// A BINARY field's key embeds a server-generated uuid, so there is nothing
	// truthful to display.
	binary := &nemgen.FieldTypeFileConfig{
		StorageType: nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_BINARY,
	}
	if got := ObjectKey(binary, "a.pdf"); got != "" {
		t.Errorf("ObjectKey for a BINARY field = %q, want empty", got)
	}
	if UsesObjectStore(binary) {
		t.Error("a BINARY field does not use the team object store")
	}

	if got, want := RecordValue("https://h/k.pdf?X-Amz-Signature=abc&e=1"), "https://h/k.pdf"; got != want {
		t.Errorf("RecordValue = %q, want %q", got, want)
	}
	if got, want := RecordValue("https://h/k.pdf"), "https://h/k.pdf"; got != want {
		t.Errorf("RecordValue without a query = %q, want %q", got, want)
	}
}
