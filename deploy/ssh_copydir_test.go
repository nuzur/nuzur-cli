package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CopyDir streams a gzipped tar rather than using `scp -r` (which pays an SFTP
// round-trip per file — minutes for a ~650-file app on a WAN link). These tests
// pin the archive's shape without needing a host: they run the same tar command
// CopyDir builds and extract it the way the remote side does.

func tarStream(t *testing.T, dir string) []byte {
	t.Helper()
	cmd := exec.Command("tar", "czf", "-", "-C", dir, ".")
	// Same env CopyDir uses — see the AppleDouble note there.
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("tar: %v", err)
	}
	return out
}

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"Dockerfile", "go.mod", "app/rest.go", "core/db.go"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The archive must carry the directory's CONTENTS (not the directory itself), so
// extracting into remotePath reproduces the old `scp -r` semantics the bootstrap
// depends on — RemoteSrcDir must contain the Dockerfile, not <dir>/Dockerfile.
func TestCopyDirArchivesContentsNotTheDir(t *testing.T) {
	src := fixture(t)
	dest := t.TempDir()

	cmd := exec.Command("tar", "xzf", "-", "-C", dest)
	cmd.Stdin = strings.NewReader(string(tarStream(t, src)))
	if err := cmd.Run(); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// The bootstrap does `docker build <RemoteSrcDir>`, so these must be at the root.
	for _, want := range []string{"Dockerfile", "go.mod", "app/rest.go", "core/db.go"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("%s missing at the destination root: %v", want, err)
		}
	}
	// A nested copy of the source dir's own name would mean docker build sees no Dockerfile.
	if _, err := os.Stat(filepath.Join(dest, filepath.Base(src))); err == nil {
		t.Errorf("archive nested the source directory itself; the build context would be wrong")
	}
}

// macOS tar encodes xattrs as AppleDouble "._*" entries that GNU tar extracts as
// REAL files — 741 of them for a generated app, into the docker build context.
// Go ignores "._"-prefixed files, so this bloats images silently rather than
// failing loudly. COPYFILE_DISABLE must keep them out.
func TestCopyDirEmitsNoAppleDoubleFiles(t *testing.T) {
	src := fixture(t)
	dest := t.TempDir()

	cmd := exec.Command("tar", "xzf", "-", "-C", dest)
	cmd.Stdin = strings.NewReader(string(tarStream(t, src)))
	if err := cmd.Run(); err != nil {
		t.Fatalf("extract: %v", err)
	}

	var junk []string
	err := filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.Base(p), "._") {
			junk = append(junk, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(junk) > 0 {
		t.Errorf("archive carries %d AppleDouble file(s) into the build context: %v", len(junk), junk)
	}
}

// The remote path is interpolated into a shell command, so it must be quoted.
func TestShellSingleQuote(t *testing.T) {
	cases := map[string]string{
		"/tmp/nuzur-src":   "'/tmp/nuzur-src'",
		"/tmp/has space":   "'/tmp/has space'",
		`/tmp/it's`:        `'/tmp/it'\''s'`,
		"/tmp/x; rm -rf /": "'/tmp/x; rm -rf /'",
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
