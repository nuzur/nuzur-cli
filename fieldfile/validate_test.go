package fieldfile

import (
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
)

// pngHeader is a real PNG signature, enough for http.DetectContentType.
var pngHeader = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

func fileField(t nemgen.FieldType, identifier string) *nemgen.Field {
	return &nemgen.Field{Identifier: identifier, Type: t}
}

func TestValidateExtension(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		file    string
		wantErr bool
	}{
		{"empty allow-list accepts anything", nil, "whatever.xyz", false},
		{"exact match", []string{"pdf", "png"}, "report.pdf", false},
		{"file extension is lowercased", []string{"pdf"}, "REPORT.PDF", false},
		{"disallowed extension", []string{"pdf"}, "report.png", true},
		// The web tests the ORIGINAL name, so a name whose base would sanitize
		// differently must still match on its real extension.
		{"uses the original name", []string{"pdf"}, "Report v2 (final).PDF", false},
		// String.split('.').pop() on a dotless name returns the whole name.
		{"no dot means the whole name is the extension", []string{"readme"}, "README", false},
		{"no dot and no match", []string{"pdf"}, "README", true},
		// Tolerances the web lacks; both only ever accept more.
		{"tolerates a dot in the configured entry", []string{".pdf"}, "report.pdf", false},
		{"tolerates an uppercase configured entry", []string{"PDF"}, "report.pdf", false},
		{"tolerates whitespace in the configured entry", []string{" pdf "}, "report.pdf", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExtension(tt.allowed, tt.file)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateExtension(%v, %q) error = %v, wantErr %v", tt.allowed, tt.file, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSize(t *testing.T) {
	tests := []struct {
		name      string
		maxKB     int64
		sizeBytes int64
		wantErr   bool
	}{
		{"no limit configured", 0, 1 << 30, false},
		{"negative limit is no limit", -1, 1 << 30, false},
		{"exactly at the limit is allowed", 100, 100 * 1024, false},
		// The web compares size/1024 > max in floating point, so a single byte
		// over the limit fails. An integer division would have let this pass.
		{"one byte over the limit fails", 100, 100*1024 + 1, true},
		{"well under the limit", 100, 4096, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSize(tt.maxKB, tt.sizeBytes)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSize(%d, %d) error = %v, wantErr %v", tt.maxKB, tt.sizeBytes, err, tt.wantErr)
			}
		})
	}
}

func TestValidateKind(t *testing.T) {
	tests := []struct {
		name    string
		typ     nemgen.FieldType
		head    []byte
		wantErr bool
	}{
		{"png into an image field", nemgen.FieldType_FIELD_TYPE_IMAGE, pngHeader, false},
		{"png into a plain file field", nemgen.FieldType_FIELD_TYPE_FILE, pngHeader, false},
		{"png into an audio field", nemgen.FieldType_FIELD_TYPE_AUDIO, pngHeader, true},
		{"png into a video field", nemgen.FieldType_FIELD_TYPE_VIDEO, pngHeader, true},
		// Anything the sniffer cannot place is allowed through rather than
		// blocking a legitimate upload Go simply cannot recognise.
		{"unrecognised bytes are allowed", nemgen.FieldType_FIELD_TYPE_IMAGE, []byte{0x00, 0x01, 0x02, 0x03}, false},
		{"no bytes to sniff", nemgen.FieldType_FIELD_TYPE_IMAGE, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKind(fileField(tt.typ, "attachment"), tt.head)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateKind(%v) error = %v, wantErr %v", tt.typ, err, tt.wantErr)
			}
		})
	}
}

func TestValidateRejectsTextSniffedAsPlainIntoImage(t *testing.T) {
	f := fileField(nemgen.FieldType_FIELD_TYPE_IMAGE, "avatar")
	cfg := &nemgen.FieldTypeFileConfig{
		StorageType: nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_OBJECT_STORE,
	}
	err := Validate(f, cfg, "notes.txt", 10, []byte("hello, this is plainly text"))
	if err == nil {
		t.Fatal("expected a text file to be rejected by an image field")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error should name the expected kind, got %q", err)
	}
}

func TestCheckSizeGuard(t *testing.T) {
	if err := CheckSizeGuard(DefaultMaxBytes, DefaultMaxBytes); err != nil {
		t.Errorf("a file exactly at the guard should pass, got %v", err)
	}
	if err := CheckSizeGuard(DefaultMaxBytes+1, DefaultMaxBytes); err == nil {
		t.Error("a file over the guard should fail")
	}
	if err := CheckSizeGuard(1<<40, 0); err != nil {
		t.Errorf("a zero guard disables the check, got %v", err)
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{64 * 1024 * 1024, "64.0 MB"},
	}
	for _, tt := range tests {
		if got := HumanSize(tt.in); got != tt.want {
			t.Errorf("HumanSize(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A field whose storage type was never set is caught here rather than by the
// server's bare "invalid storage type for field". Real schemas have these: the
// chorus project's audio and spectrogram fields both leave it unset.
func TestValidateStorageType(t *testing.T) {
	f := fileField(nemgen.FieldType_FIELD_TYPE_FILE, "audio")

	for _, tc := range []struct {
		name    string
		storage nemgen.FieldTypeFileConfigStorageType
		wantErr bool
	}{
		{"object store", nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_OBJECT_STORE, false},
		{"binary", nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_BINARY, false},
		{"unset", nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_INVALID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStorageType(f, &nemgen.FieldTypeFileConfig{StorageType: tc.storage})
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateStorageType(%v) error = %v, wantErr %v", tc.storage, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "audio") {
				t.Errorf("error should name the field, got %q", err)
			}
		})
	}
}
