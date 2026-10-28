package codescan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo builds a repository whose module lives in app/: one commit on
// master, then a "feature" branch with one more commit checked out in a
// linked worktree. It returns the main checkout's module dir, the worktree's
// module dir and the master commit.
func gitRepo(t *testing.T) (mainDir, wtDir, masterSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mainDir = filepath.Join(root, "repo", "app")
	write(filepath.Join(mainDir, "go.mod"), "module example.com/app\n\ngo 1.22\n")
	write(filepath.Join(mainDir, "main.go"), "package main\n\nfunc main() {}\n")
	run(filepath.Join(root, "repo"), "init", "--quiet", "-b", "master")
	run(filepath.Join(root, "repo"), "add", ".")
	run(filepath.Join(root, "repo"), "commit", "--quiet", "-m", "first")
	masterSHA = run(mainDir, "rev-parse", "HEAD")

	wt := filepath.Join(root, "wt")
	run(mainDir, "worktree", "add", "--quiet", "-b", "feature", wt)
	wtDir = filepath.Join(wt, "app")
	write(filepath.Join(wtDir, "lib", "lib.go"), "package lib\n\nfunc F() {}\n")
	run(wt, "add", ".")
	run(wt, "commit", "--quiet", "-m", "feature work")
	return mainDir, wtDir, masterSHA
}

func TestReadLinkFallsBackToMainCheckout(t *testing.T) {
	mainDir, wtDir, _ := gitRepo(t)
	if _, err := ReadLink(wtDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nothing is linked yet, got %v", err)
	}
	want := Link{ProjectUUID: "p", CodebaseUUID: "cb", Identifier: "app"}
	if err := WriteLink(mainDir, want); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadLink(wtDir); err != nil || got != want {
		t.Fatalf("a worktree should use the main checkout's link, got %+v, %v", got, err)
	}

	// A worktree's own link wins.
	own := Link{ProjectUUID: "p", CodebaseUUID: "other", Identifier: "app"}
	if err := WriteLink(wtDir, own); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadLink(wtDir); got != own {
		t.Fatalf("the worktree's own link should win, got %+v", got)
	}
}

func TestScanRecordsBranchPoint(t *testing.T) {
	mainDir, wtDir, masterSHA := gitRepo(t)
	ctx := context.Background()

	f, err := Scan(ctx, Options{Dir: wtDir, DefaultBranch: "master"})
	if err != nil {
		t.Fatal(err)
	}
	g := f.GetGit()
	if g.GetBranch() != "feature" || g.GetBranchPointSha() != masterSHA || g.GetDefaultBranch() != "master" {
		t.Fatalf("feature scan git = %+v, want branch point %s on master", g, masterSHA)
	}

	f, err = Scan(ctx, Options{Dir: mainDir, DefaultBranch: "master"})
	if err != nil {
		t.Fatal(err)
	}
	if g := f.GetGit(); g.GetBranchPointSha() != "" || g.GetDefaultBranch() != "" {
		t.Fatalf("a default-branch scan has no branch point, got %+v", g)
	}
}

func TestScanCommitLeavesCheckoutAlone(t *testing.T) {
	_, wtDir, masterSHA := gitRepo(t)
	ctx := context.Background()
	worktrees := func() string {
		out, _ := exec.Command("git", "-C", wtDir, "worktree", "list", "--porcelain").Output()
		return string(out)
	}
	before := worktrees()

	f, err := ScanCommit(ctx, Options{Dir: wtDir, DefaultBranch: "master", Branch: "master"}, masterSHA)
	if err != nil {
		t.Fatal(err)
	}
	g := f.GetGit()
	if g.GetCommitSha() != masterSHA || g.GetBranch() != "master" || g.GetBranchPointSha() != "" || g.GetDirty() {
		t.Fatalf("ScanCommit git = %+v", g)
	}
	for _, n := range f.GetNodes() {
		if strings.Contains(n.GetId(), "/lib") {
			t.Fatalf("the master commit has no lib package, but the scan saw %s", n.GetId())
		}
	}
	if after := worktrees(); after != before {
		t.Fatalf("the temporary worktree was left registered:\n%s", after)
	}
	if head, _ := exec.Command("git", "-C", wtDir, "rev-parse", "--abbrev-ref", "HEAD").Output(); strings.TrimSpace(string(head)) != "feature" {
		t.Fatalf("the user's checkout moved to %s", head)
	}
}

func TestLocalDefaultBranchWithoutOriginHead(t *testing.T) {
	_, wtDir, _ := gitRepo(t)
	// No remote at all: master is the only candidate, even from the worktree
	// that has a different branch checked out.
	if got := localDefaultBranch(context.Background(), wtDir); got != "master" {
		t.Fatalf("localDefaultBranch = %q, want master", got)
	}
	f, err := Scan(context.Background(), Options{Dir: wtDir})
	if err != nil {
		t.Fatal(err)
	}
	if f.GetGit().GetBranchPointSha() == "" {
		t.Fatal("without a default branch from nuzur, the scan should still find the branch point")
	}
	r, err := DescribeRepo(context.Background(), wtDir)
	if err != nil || r.DefaultBranch != "master" {
		t.Fatalf("linking from a worktree must not take its branch as the default, got %q, %v", r.DefaultBranch, err)
	}
}
