package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo makes a real git repository with one commit and returns its path.
// Real git rather than a stub: the behaviour under test IS git's (how it
// reports the root, the prefix and the branch), and a stub would only assert
// that the code matches my beliefs about git rather than git itself.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "init")
	return root
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDiscoverGitRepoBothLayouts covers the "detect either" requirement: the
// workspace may BE the repository, or sit inside a larger one.
func TestDiscoverGitRepoBothLayouts(t *testing.T) {
	t.Run("workspace is the repo", func(t *testing.T) {
		root := initRepo(t)
		repo, err := DiscoverGitRepo(context.Background(), root)
		if err != nil {
			t.Fatalf("DiscoverGitRepo: %v", err)
		}
		if repo.RelPath != "." {
			t.Errorf("RelPath = %q, want %q", repo.RelPath, ".")
		}
		if repo.Branch != "main" {
			t.Errorf("Branch = %q, want main", repo.Branch)
		}
	})

	t.Run("workspace inside a repo", func(t *testing.T) {
		root := initRepo(t)
		ws := filepath.Join(root, "services", "myapp")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		repo, err := DiscoverGitRepo(context.Background(), ws)
		if err != nil {
			t.Fatalf("DiscoverGitRepo: %v", err)
		}
		if repo.RelPath != "services/myapp" {
			t.Errorf("RelPath = %q, want services/myapp", repo.RelPath)
		}
	})
}

// TestDiscoverGitRepoRejectsNonRepo: failing here, before anything is
// generated or copied, is much better than failing at the commit.
func TestDiscoverGitRepoRejectsNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := DiscoverGitRepo(context.Background(), t.TempDir()); err == nil {
		t.Fatal("expected an error outside a git repository")
	}
}

// TestDiscoverGitRepoRejectsDetachedHead: committing onto a detached HEAD
// produces a commit no branch points at, which is then never pushed and never
// built — a deploy that appears to work and releases nothing.
func TestDiscoverGitRepoRejectsDetachedHead(t *testing.T) {
	root := initRepo(t)
	gitIn(t, root, "checkout", "-q", "--detach", "HEAD")
	_, err := DiscoverGitRepo(context.Background(), root)
	if err == nil {
		t.Fatal("expected an error on a detached HEAD")
	}
	if !strings.Contains(err.Error(), "detached HEAD") {
		t.Errorf("error should name the problem, got: %v", err)
	}
}

// TestHasChangesIsScopedToTheWorkspace is the property that keeps a deploy from
// committing unrelated work: in a shared repo, edits OUTSIDE the workspace must
// not register as something this deploy should commit.
func TestHasChangesIsScopedToTheWorkspace(t *testing.T) {
	root := initRepo(t)
	ws := filepath.Join(root, "services", "myapp")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "add workspace")

	repo, err := DiscoverGitRepo(context.Background(), ws)
	if err != nil {
		t.Fatalf("DiscoverGitRepo: %v", err)
	}

	// Clean to start with.
	changed, err := repo.HasChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("workspace should be clean")
	}

	// A change ELSEWHERE in the repo is not this deploy's business.
	if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = repo.HasChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a change outside the workspace must not count as a workspace change")
	}

	// A change INSIDE the workspace does, including an untracked file.
	if err := os.WriteFile(filepath.Join(ws, "new.txt"), []byte("z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = repo.HasChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("an untracked file inside the workspace is a change to commit")
	}
}

// TestCommitAndPushStagesOnlyTheWorkspace: the commit a deploy makes must
// contain the generated code and nothing else, or a routine deploy silently
// publishes whatever the user had in progress.
func TestCommitAndPushStagesOnlyTheWorkspace(t *testing.T) {
	origin := initRepo(t)
	gitIn(t, origin, "config", "receive.denyCurrentBranch", "ignore")

	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", origin, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	gitIn(t, clone, "config", "user.email", "test@example.com")
	gitIn(t, clone, "config", "user.name", "test")

	ws := filepath.Join(clone, "services", "myapp")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "generated.txt"), []byte("gen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unrelated work in progress, which must be left alone.
	if err := os.WriteFile(filepath.Join(clone, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := DiscoverGitRepo(context.Background(), ws)
	if err != nil {
		t.Fatalf("DiscoverGitRepo: %v", err)
	}
	sha, err := repo.CommitAndPush(context.Background(), "nuzur deploy: generated app")
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("expected a full 40-char SHA (the image tag is type=sha,format=long), got %q", sha)
	}

	files := gitIn(t, clone, "show", "--name-only", "--pretty=format:", "HEAD")
	if !strings.Contains(files, "services/myapp/generated.txt") {
		t.Errorf("commit is missing the generated file:\n%s", files)
	}
	if strings.Contains(files, "wip.txt") {
		t.Errorf("commit swept up unrelated work in progress:\n%s", files)
	}

	if !repo.PushedSHAExistsOnRemote(context.Background(), sha) {
		t.Error("pushed SHA should be reachable from a remote branch")
	}
}
