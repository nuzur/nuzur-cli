package fieldfile

import (
	"fmt"
	"net/http"
	"strings"

	nemgen "github.com/nuzur/nem/idl/gen"
)

// bytesPerKB is the unit max_size is expressed in. The web compares
// `file.size / 1024 > maxSize`, so max_size is KILOBYTES, not bytes.
const bytesPerKB = 1024

// Validate applies the checks the data manager applies before it uploads, and
// that the RPC itself does NOT: the field's allowed extensions, its maximum
// size, and — for image/video/audio fields — that the file is of that kind.
//
// Doing this client-side is not belt-and-braces, it is the only enforcement
// there is. The server's upload handler reads storage_type and path out of this
// same config and ignores allowed_extensions and max_size entirely, so a
// scripted upload that skipped these would put an object into the bucket that
// the UI then refuses to accept as a value.
//
// originalName is the file's name as the user has it on disk, BEFORE
// sanitization: that is what the web tests the extension against, and
// sanitizing first would change the answer for a name like "report.v2.PDF".
// head is the first bytes of the file (512 is enough) for content sniffing.
func Validate(f *nemgen.Field, cfg *nemgen.FieldTypeFileConfig, originalName string, sizeBytes int64, head []byte) error {
	if err := ValidateFieldTarget(f, cfg); err != nil {
		return err
	}
	if err := validateKind(f, head); err != nil {
		return err
	}
	if err := validateExtension(cfg.GetAllowedExtensions(), originalName); err != nil {
		return err
	}
	return validateSize(cfg.GetMaxSize(), sizeBytes)
}

// validateStorageType rejects a field whose storage type was never set.
//
// The server accepts only OBJECT_STORE and BINARY and answers anything else
// with a bare InvalidArgument "invalid storage type for field", which does not
// say which field or what to do about it. This is not a hypothetical: a field
// added through the API without a storage type keeps the zero value, and the
// code generator normalizes that to object store when it emits a column — so a
// schema can look entirely healthy and still be un-uploadable.
func validateStorageType(f *nemgen.Field, cfg *nemgen.FieldTypeFileConfig) error {
	switch cfg.GetStorageType() {
	case nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_OBJECT_STORE,
		nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_BINARY:
		return nil
	}
	return fmt.Errorf(
		"field %q has no storage type set, so there is nowhere to put the file — "+
			"open the field in the project editor and choose object store or binary storage",
		f.GetIdentifier())
}

// requiredContentPrefix is the MIME prefix the web demands for the three
// media field types (it reads the browser-supplied file.type). A plain FILE
// field accepts anything.
func requiredContentPrefix(t nemgen.FieldType) string {
	switch t {
	case nemgen.FieldType_FIELD_TYPE_IMAGE:
		return "image/"
	case nemgen.FieldType_FIELD_TYPE_VIDEO:
		return "video/"
	case nemgen.FieldType_FIELD_TYPE_AUDIO:
		return "audio/"
	}
	return ""
}

// validateKind is the CLI's stand-in for the browser's file.type check.
//
// There is no MIME type on a file read off disk, so the content is sniffed
// instead. Go's sniffer is weaker than a browser's, and a false rejection here
// would block a legitimate upload for no reason — so this only fails on a
// CONFIDENT contradiction: a type was positively identified and it is the wrong
// kind. An unrecognised file (http.DetectContentType's octet-stream fallback,
// which covers most video containers) is allowed through, and the server and
// the UI remain the backstop.
func validateKind(f *nemgen.Field, head []byte) error {
	prefix := requiredContentPrefix(f.GetType())
	if prefix == "" || len(head) == 0 {
		return nil
	}

	detected := http.DetectContentType(head)
	if detected == "application/octet-stream" || strings.HasPrefix(detected, prefix) {
		return nil
	}
	return fmt.Errorf("field %q only accepts %s files, but this one looks like %s",
		f.GetIdentifier(), strings.TrimSuffix(prefix, "/"), detected)
}

// validateExtension checks the file's extension against the field's allow-list.
// An empty list means the field accepts anything.
//
// It mirrors the web's check (modal_file_field.tsx), which is
// `allowedExtensions.includes(name.split('.').pop().toLowerCase())` — note two
// consequences that are deliberate rather than oversights:
//
//   - the extension comes from the ORIGINAL name, not the sanitized one;
//   - a name with no dot at all yields the whole name as the "extension",
//     because that is what String.split('.').pop() returns.
//
// Two tolerances are added on top, both of which only ever ACCEPT something the
// web would have rejected, never the reverse: the comparison is
// case-insensitive on both sides (the web lowercases only the file's half, so
// an allow-list authored as "PNG" matches nothing at all there), and a leading
// dot on a configured entry is ignored.
func validateExtension(allowed []string, originalName string) error {
	if len(allowed) == 0 {
		return nil
	}

	ext := strings.ToLower(originalName)
	if dot := strings.LastIndex(ext, "."); dot != -1 {
		ext = ext[dot+1:]
	}

	for _, a := range allowed {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(a), "."), ext) {
			return nil
		}
	}
	return fmt.Errorf("%q is not an accepted file type for this field — it accepts %s",
		originalName, humanList(allowed))
}

// validateSize checks the file against max_size, which is in KB. A max_size of
// zero (or less) means the field sets no limit.
//
// The comparison is done in floating point to match the web's `size / 1024 >
// max` exactly: an integer division would let a file that is over the limit by
// less than a kilobyte through.
func validateSize(maxSizeKB, sizeBytes int64) error {
	if maxSizeKB <= 0 {
		return nil
	}
	if float64(sizeBytes)/bytesPerKB > float64(maxSizeKB) {
		return fmt.Errorf("file is %s, which is over this field's limit of %s",
			HumanSize(sizeBytes), HumanSize(maxSizeKB*bytesPerKB))
	}
	return nil
}

// humanList renders an allow-list as ".png, .jpg, .webp".
func humanList(allowed []string) string {
	out := make([]string, 0, len(allowed))
	for _, a := range allowed {
		out = append(out, "."+strings.TrimPrefix(strings.TrimSpace(a), "."))
	}
	return strings.Join(out, ", ")
}

// HumanSize renders a byte count the way the rest of the CLI talks about sizes.
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ValidateFieldTarget runs the checks that depend only on the field and its
// configuration, not on any particular file.
//
// It is what `files describe` uses to report whether a field is uploadable at
// all, and it is a prefix of what Validate does, so the two cannot disagree: a
// field describe calls uploadable is one upload will not reject on
// configuration grounds.
func ValidateFieldTarget(f *nemgen.Field, cfg *nemgen.FieldTypeFileConfig) error {
	if cfg == nil {
		return fmt.Errorf("no file configuration given")
	}
	return validateStorageType(f, cfg)
}
