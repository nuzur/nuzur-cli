package codescan

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// ScanCommit scans another commit of the repository holding opts.Dir without
// touching that checkout: the commit is checked out into a temporary git
// worktree, scanned at the same module path, and the worktree is removed.
// opts.Branch should name a branch the commit is on (it is checked out
// detached, so git cannot say).
func ScanCommit(ctx context.Context, opts Options, sha string) (*pb.CodeFacts, error) {
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if gitOut(ctx, dir, "rev-parse", "--show-toplevel") == "" {
		return nil, fmt.Errorf("%s is not inside a git repository", dir)
	}
	prefix := gitOut(ctx, dir, "rev-parse", "--show-prefix")

	tmp, err := os.MkdirTemp("", "nuzur-code-scan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "checkout")
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "worktree", "add", "--detach", "--quiet", wt, sha).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("checking out %s into a temporary worktree: %v: %s", sha, err, strings.TrimSpace(string(out)))
	}
	// A background context: the worktree must be unregistered even when ctx
	// was cancelled mid-scan.
	defer exec.Command("git", "-C", dir, "worktree", "remove", "--force", wt).Run() //nolint:errcheck

	opts.Dir = filepath.Join(wt, prefix)
	return Scan(ctx, opts)
}
