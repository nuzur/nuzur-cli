package deploy

import (
	"context"
	"fmt"
	"strings"
)

// GitRepo is the repository a workspace lives in.
//
// Root and the workspace are often the same directory (the generated app IS the
// repo, as in a fresh `nuzur-<identifier>`), but need not be: the workspace can
// sit inside a larger repo. Both layouts are supported, and the difference
// matters — a commit must be scoped to the workspace path so a deploy never
// sweeps up unrelated work in progress elsewhere in the tree.
type GitRepo struct {
	Root   string // absolute path to the repository root
	Branch string // currently checked-out branch
	// RelPath is the workspace's path relative to Root, or "." when they are the
	// same directory. It is what `git add` is scoped to.
	RelPath string
}

// DiscoverGitRepo resolves the repository containing dir.
func DiscoverGitRepo(ctx context.Context, dir string) (GitRepo, error) {
	if err := RequireLocalTool("git", "commit the generated code", "install git, or pass --no-commit to deploy the repo as it stands"); err != nil {
		return GitRepo{}, err
	}

	root, err := LocalCommand(ctx, dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return GitRepo{}, fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}

	branch, err := LocalCommand(ctx, dir, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return GitRepo{}, fmt.Errorf("cannot determine the current branch in %s: %w", root, err)
	}
	if branch == "HEAD" {
		return GitRepo{}, fmt.Errorf("%s has a detached HEAD — check out a branch, or pass --no-commit", root)
	}

	// Ask git rather than computing it with filepath.Rel: on macOS the repo may
	// be reached through /tmp while git reports /private/tmp, and symlinked
	// checkouts differ the same way, so string arithmetic on the two paths
	// produces a path that exists in neither.
	rel, err := LocalCommand(ctx, dir, "git", "rev-parse", "--show-prefix")
	if err != nil {
		return GitRepo{}, fmt.Errorf("cannot locate %s within %s: %w", dir, root, err)
	}
	rel = strings.TrimSuffix(strings.TrimSpace(rel), "/")
	if rel == "" {
		rel = "."
	}

	return GitRepo{Root: root, Branch: branch, RelPath: rel}, nil
}

// HasChanges reports whether the workspace path has anything to commit,
// including untracked files.
func (r GitRepo) HasChanges(ctx context.Context) (bool, error) {
	out, err := LocalCommand(ctx, r.Root, "git", "status", "--porcelain", "--", r.RelPath)
	if err != nil {
		return false, fmt.Errorf("checking for changes in %s: %w", r.RelPath, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// CommitAndPush stages the workspace path, commits it and pushes the branch.
//
// Staging is scoped to RelPath: in a repo where the generated app is one
// directory among many, `git add -A` would sweep unrelated work into a deploy
// commit. Returns the SHA that is now on the remote.
func (r GitRepo) CommitAndPush(ctx context.Context, message string) (string, error) {
	if _, err := LocalCommand(ctx, r.Root, "git", "add", "--", r.RelPath); err != nil {
		return "", fmt.Errorf("staging %s: %w", r.RelPath, err)
	}
	if _, err := LocalCommand(ctx, r.Root, "git", "commit", "-m", message); err != nil {
		return "", fmt.Errorf("committing: %w", err)
	}
	if _, err := LocalCommand(ctx, r.Root, "git", "push", "origin", r.Branch); err != nil {
		return "", fmt.Errorf("pushing %s: %w", r.Branch, err)
	}
	return r.HeadSHA(ctx)
}

// HeadSHA returns the full commit SHA at HEAD. Full rather than abbreviated
// because it is matched against GitHub Actions runs, and the image tag the
// generated workflow publishes is type=sha,format=long.
func (r GitRepo) HeadSHA(ctx context.Context) (string, error) {
	sha, err := LocalCommand(ctx, r.Root, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("reading HEAD: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// PushedSHAExistsOnRemote reports whether HEAD is present on the tracking
// remote.
//
// Checked before waiting on CI: with --no-commit the local HEAD may never have
// been pushed, and waiting for a workflow run on a commit GitHub has never seen
// would otherwise block until the timeout with nothing to show for it.
func (r GitRepo) PushedSHAExistsOnRemote(ctx context.Context, sha string) bool {
	out, err := LocalCommand(ctx, r.Root, "git", "branch", "-r", "--contains", sha)
	return err == nil && strings.TrimSpace(out) != ""
}
