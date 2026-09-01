package fieldfile

import (
	"path/filepath"
	"regexp"
	"strings"
)

// nonNameChar is every character the web's sanitizer replaces with "_". The
// class is deliberately narrow — a-z, 0-9 and underscore — and is applied
// AFTER lowercasing, which is why it has no A-Z.
var nonNameChar = regexp.MustCompile(`[^a-z0-9_]`)

// underscoreRun collapses the runs the replacement above tends to produce.
var underscoreRun = regexp.MustCompile(`_+`)

// SanitizeFileName reproduces, exactly, the cleaning the web data manager
// applies before it uploads (nuzur-web/src/project-data-manager/modal_file_field.tsx):
//
//	const rawName = file.name.toLowerCase();
//	const dotIndex = rawName.lastIndexOf('.');
//	const namePart = dotIndex !== -1 ? rawName.slice(0, dotIndex) : rawName;
//	const extPart  = dotIndex !== -1 ? rawName.slice(dotIndex + 1) : '';
//	const cleanedName = namePart.replace(/[^a-z0-9_]/g, '_').replace(/_+/g, '_').replace(/^_+|_+$/g, '');
//	const cleanedFileName = extPart ? `${cleanedName}.${extPart}` : cleanedName;
//
// Three details in there are easy to get wrong and matter, because the object
// key is the field path plus this name and nothing else — get it wrong and a
// CLI upload silently becomes a SECOND object rather than the same one the data
// manager would have written:
//
//   - the WHOLE name is lowercased first, so the extension is lowercased too;
//   - only the name part is character-cleaned, the extension is left alone;
//   - the split is on the LAST dot, so "archive.tar.gz" keeps "gz" as the
//     extension and cleans "archive.tar" into "archive_tar".
//
// A name that cleans away to nothing ("...") yields "" for the name part, which
// the caller must treat as a reason to ask for an explicit --name rather than
// upload a file called ".jpg".
func SanitizeFileName(original string) string {
	raw := strings.ToLower(original)

	namePart, extPart := raw, ""
	if dot := strings.LastIndex(raw, "."); dot != -1 {
		namePart, extPart = raw[:dot], raw[dot+1:]
	}

	cleaned := nonNameChar.ReplaceAllString(namePart, "_")
	cleaned = underscoreRun.ReplaceAllString(cleaned, "_")
	cleaned = strings.Trim(cleaned, "_")

	if extPart != "" {
		return cleaned + "." + extPart
	}
	return cleaned
}

// DeriveFileName picks the object name for a local file path: the sanitized
// base name. Returns "" when nothing survives sanitization, which the caller
// reports as "pass --name".
func DeriveFileName(path string) string {
	return SanitizeFileName(filepath.Base(path))
}

// NormalizeKey drops empty path segments, collapsing "//" and trimming a
// leading or trailing "/".
//
// This is a deliberate copy of objectstore.NormalizeKey in nuzur-go
// (product/module/objectstore/objectstore.go), which is in a different Go
// module under an internal path and cannot be imported. It exists here only so
// the CLI can PRINT the key the server is about to write; the server does its
// own normalization regardless, so a drift between the two costs a wrong
// display, not a wrong upload.
func NormalizeKey(key string) string {
	var cleaned []string
	for _, p := range strings.Split(key, "/") {
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return strings.Join(cleaned, "/")
}

// IsBareExtension reports whether sanitization left nothing but an extension —
// "!!!.pdf" cleans to ".pdf".
//
// That is what the data manager produces too, so it is not an error; it is
// worth telling the user about, because the resulting object key carries no
// trace of what the file was.
//
// A genuine dotfile (".gitignore") also looks like this, which is why the
// caller has to compare against the original name rather than rely on this
// alone.
func IsBareExtension(name string) bool {
	return strings.HasPrefix(name, ".") && !strings.Contains(strings.TrimPrefix(name, "."), ".")
}
