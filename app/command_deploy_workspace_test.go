package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

func TestResolveWorkspace(t *testing.T) {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }

	t.Run("flag wins", func(t *testing.T) {
		got, err := resolveWorkspace("/tmp/ws", &deploy.Deployment{SourceDir: "/other"}, "sfapi")
		if err != nil {
			t.Fatal(err)
		}
		if got != "/tmp/ws" {
			t.Errorf("got %q, want /tmp/ws", got)
		}
	})
	t.Run("prior record reused when no flag", func(t *testing.T) {
		got, err := resolveWorkspace("", &deploy.Deployment{SourceDir: "/recorded/ws"}, "sfapi")
		if err != nil {
			t.Fatal(err)
		}
		if got != "/recorded/ws" {
			t.Errorf("got %q, want /recorded/ws", got)
		}
	})
	t.Run("default from identifier", func(t *testing.T) {
		got, err := resolveWorkspace("", nil, "sfapi")
		if err != nil {
			t.Fatal(err)
		}
		if got != abs("nuzur-sfapi") {
			t.Errorf("got %q, want %q", got, abs("nuzur-sfapi"))
		}
	})
	t.Run("prior with empty SourceDir falls back to default", func(t *testing.T) {
		got, err := resolveWorkspace("", &deploy.Deployment{}, "sfapi")
		if err != nil {
			t.Fatal(err)
		}
		if got != abs("nuzur-sfapi") {
			t.Errorf("got %q, want %q", got, abs("nuzur-sfapi"))
		}
	})
}

func TestWriteWorkspaceGitignore(t *testing.T) {
	dir := t.TempDir()
	if err := writeWorkspaceGitignore(dir); err != nil {
		t.Fatal(err)
	}
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("no .gitignore: %v", err)
	}
	di, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		t.Fatalf("no .dockerignore: %v", err)
	}
	for _, want := range []string{"config/prod.yaml", "*.key", "*.pem", ".env"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf(".gitignore missing %q", want)
		}
	}
	if !strings.Contains(string(di), ".git") {
		t.Errorf(".dockerignore should exclude .git")
	}

	// Never clobber an existing file.
	custom := "my own rules\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkspaceGitignore(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(got) != custom {
		t.Errorf("existing .gitignore was clobbered: %q", got)
	}
}
