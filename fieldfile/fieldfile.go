// Package fieldfile holds the logic behind `nuzur-cli files upload`: resolving a
// file field inside a project version, deciding whether a local file is allowed
// into it, and reproducing the object key the server will write it to.
//
// It exists as a package rather than living in the command because every rule
// here is a rule that already exists somewhere else, and the two have to agree:
//
//  1. The object key is computed server-side in nuzur-go
//     (product/server/files_record_object_store.go) as
//     NormalizeKey(fieldPath + "/" + fileName). The CLI cannot ask for the key
//     back — the upload RPC returns only a presigned URL — so ObjectKey
//     recomputes it purely for display. If the server's rule changes, this is
//     the thing that goes quietly wrong.
//
//  2. The filename is sanitized CLIENT-side, by the web data manager
//     (nuzur-web/src/project-data-manager/modal_file_field.tsx). The server
//     accepts whatever it is given. So a file uploaded through the CLI only
//     lands on the same key as the same file uploaded through the data manager
//     if SanitizeFileName reproduces that rule exactly — see its doc comment.
//
//  3. allowed_extensions and max_size are likewise enforced only client-side.
//     A field config that the web would reject is accepted by the RPC without
//     complaint, so Validate is the only thing standing between a scripted
//     upload and an object the UI considers invalid.
//
// Everything here is pure: no network, no filesystem, no globals.
package fieldfile

import (
	"fmt"
	"strings"

	nemgen "github.com/nuzur/nem/idl/gen"
)

// fileFieldTypes are the field types that can hold a file. It mirrors the
// server's check in validateFileFieldForRecord, which rejects anything else
// with "field is not of type file".
var fileFieldTypes = map[nemgen.FieldType]bool{
	nemgen.FieldType_FIELD_TYPE_FILE:  true,
	nemgen.FieldType_FIELD_TYPE_IMAGE: true,
	nemgen.FieldType_FIELD_TYPE_VIDEO: true,
	nemgen.FieldType_FIELD_TYPE_AUDIO: true,
}

// IsFileField reports whether a field can hold a file at all. Used both to
// validate an explicit --field and to narrow the interactive picker.
func IsFileField(f *nemgen.Field) bool {
	return f != nil && fileFieldTypes[f.GetType()]
}

// FileConfig returns the field's file configuration — the block that names the
// object store, the path, and the client-side limits.
//
// Which of the four type-config slots holds it depends on the field's type;
// the server does the same switch before reading StorageType.
func FileConfig(f *nemgen.Field) (*nemgen.FieldTypeFileConfig, error) {
	if f == nil {
		return nil, fmt.Errorf("no field given")
	}
	if !IsFileField(f) {
		return nil, fmt.Errorf(
			"field %q is of type %s, which cannot hold a file — only file, image, video and audio fields can",
			f.GetIdentifier(), friendlyFieldType(f.GetType()))
	}

	var cfg *nemgen.FieldTypeFileConfig
	switch f.GetType() {
	case nemgen.FieldType_FIELD_TYPE_FILE:
		cfg = f.GetTypeConfig().GetFile()
	case nemgen.FieldType_FIELD_TYPE_IMAGE:
		cfg = f.GetTypeConfig().GetImage()
	case nemgen.FieldType_FIELD_TYPE_VIDEO:
		cfg = f.GetTypeConfig().GetVideo()
	case nemgen.FieldType_FIELD_TYPE_AUDIO:
		cfg = f.GetTypeConfig().GetAudio()
	}
	if cfg == nil {
		// The server turns this into a bare codes.Internal "field type config is
		// nil"; say something the user can act on instead.
		return nil, fmt.Errorf(
			"field %q is a %s field but has no file configuration — set its storage type and path in the project editor",
			f.GetIdentifier(), friendlyFieldType(f.GetType()))
	}
	return cfg, nil
}

// friendlyFieldType renders a FieldType the way a user wrote it, not the way
// protobuf spells it: "varchar", not "FIELD_TYPE_VARCHAR".
func friendlyFieldType(t nemgen.FieldType) string {
	return strings.ToLower(strings.TrimPrefix(t.String(), "FIELD_TYPE_"))
}

// ResolveEntityField finds an entity and one of its fields in a project version,
// matching either on identifier (case-insensitively, as resolveProject and
// resolveProjectVersion do for their own references) or on uuid.
//
// The field is looked up only within the resolved entity: the server pairs the
// two the same way, and a field identifier is only unique inside its entity.
func ResolveEntityField(pv *nemgen.ProjectVersion, entityRef, fieldRef string) (*nemgen.Entity, *nemgen.Field, error) {
	if pv == nil {
		return nil, nil, fmt.Errorf("no project version given")
	}
	if entityRef == "" {
		return nil, nil, fmt.Errorf("an entity is required (--entity, by identifier or uuid)")
	}
	if fieldRef == "" {
		return nil, nil, fmt.Errorf("a field is required (--field, by identifier or uuid)")
	}

	var entity *nemgen.Entity
	for _, e := range pv.GetEntities() {
		if e.GetUuid() == entityRef || strings.EqualFold(e.GetIdentifier(), entityRef) {
			entity = e
			break
		}
	}
	if entity == nil {
		return nil, nil, fmt.Errorf("entity %q not found in this project version (match by identifier or uuid)%s",
			entityRef, suggestion("entities with file fields", entitiesWithFileFields(pv)))
	}

	for _, f := range entity.GetFields() {
		if f.GetUuid() == fieldRef || strings.EqualFold(f.GetIdentifier(), fieldRef) {
			return entity, f, nil
		}
	}
	return nil, nil, fmt.Errorf("field %q not found on entity %q (match by identifier or uuid)%s",
		fieldRef, entity.GetIdentifier(), suggestion("file fields on this entity", FileFieldsOf(entity)))
}

// FileFieldsOf names the subset of an entity's fields that can hold a file.
func FileFieldsOf(e *nemgen.Entity) []string {
	var out []string
	for _, f := range e.GetFields() {
		if IsFileField(f) {
			out = append(out, f.GetIdentifier())
		}
	}
	return out
}

// entitiesWithFileFields names the entities worth pointing a user at when their
// --entity did not match: the ones that have somewhere to put a file.
func entitiesWithFileFields(pv *nemgen.ProjectVersion) []string {
	var out []string
	for _, e := range pv.GetEntities() {
		if len(FileFieldsOf(e)) > 0 {
			out = append(out, e.GetIdentifier())
		}
	}
	return out
}

// suggestion appends a "did you mean" tail, or nothing when there is nothing
// useful to suggest. Capped so a wide schema does not bury the actual error.
func suggestion(label string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	const max = 10
	shown := candidates
	tail := ""
	if len(shown) > max {
		shown, tail = shown[:max], fmt.Sprintf(", and %d more", len(candidates)-max)
	}
	return fmt.Sprintf("\n%s: %s%s", label, strings.Join(shown, ", "), tail)
}
