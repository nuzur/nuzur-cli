package fieldfile

import "testing"

// TestSanitizeFileName pins the CLI's filename cleaning to the web's.
//
// The source of truth is nuzur-web/src/project-data-manager/modal_file_field.tsx
// (the `customRequest` handler). Every case below is what that code produces.
// If one of these looks wrong to you — the dropped trailing dot, the leading-dot
// name, the uncleaned extension — it is still what the data manager does, and
// "fixing" it here would mean the same file uploaded from the CLI and from the
// UI lands on two different object keys. Change the web first.
func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases and replaces spaces", "Invoice 2024.PDF", "invoice_2024.pdf"},
		{"collapses runs of replacements", "my--file!!.png", "my_file.png"},
		{"splits on the last dot only", "archive.tar.gz", "archive_tar.gz"},
		{"trims leading and trailing underscores", "__lead and trail__.png", "lead_and_trail.png"},
		{"keeps a dotfile's leading dot", ".gitignore", ".gitignore"},
		{"drops a trailing dot", "foo.", "foo"},
		{"allows a base that cleans away entirely", "!!!.pdf", ".pdf"},
		// The replacement underscore is then trimmed as trailing, so this loses the
		// accented character entirely rather than keeping a "_" for it — same as JS.
		{"replaces non-ascii", "café.JPG", "caf.jpg"},
		{"keeps an interior replacement", "cafés bar.JPG", "caf_s_bar.jpg"},
		{"leaves a name with no extension alone", "no_extension_here", "no_extension_here"},
		{"does not clean the extension itself", "a b.T X T", "a_b.t x t"},
		{"handles an empty string", "", ""},
		{"handles a name that is only separators", "___", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeFileName(tt.in); got != tt.want {
				t.Errorf("SanitizeFileName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDeriveFileName(t *testing.T) {
	if got, want := DeriveFileName("/tmp/some dir/My Report.PDF"), "my_report.pdf"; got != want {
		t.Errorf("DeriveFileName = %q, want %q", got, want)
	}
	// A path whose base cleans away to nothing is how the caller learns it must
	// ask for --name.
	if got := DeriveFileName("/tmp/---"); got != "" {
		t.Errorf("DeriveFileName of an unusable name = %q, want empty", got)
	}
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"uploads/invoices/a.pdf", "uploads/invoices/a.pdf"},
		{"/uploads/a.pdf", "uploads/a.pdf"},
		{"uploads//invoices///a.pdf", "uploads/invoices/a.pdf"},
		{"/x//y/", "x/y"},
		{"/", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeKey(tt.in); got != tt.want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
