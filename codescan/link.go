package codescan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LinkFile ties a module to its nuzur codebase. It lives next to go.mod and is
// meant to be committed: agents and CI read it to know where snapshots go.
const LinkFile = ".nuzur/codebase.json"

// Link is the content of LinkFile.
type Link struct {
	ProjectUUID  string `json:"project_uuid"`
	CodebaseUUID string `json:"codebase_uuid"`
	Identifier   string `json:"identifier"`
}

// ReadLink loads the link of the module in dir. A module that was never
// linked returns os.ErrNotExist (test with errors.Is).
func ReadLink(dir string) (Link, error) {
	raw, err := os.ReadFile(filepath.Join(dir, LinkFile))
	if err != nil {
		return Link{}, err
	}
	var l Link
	if err := json.Unmarshal(raw, &l); err != nil {
		return Link{}, fmt.Errorf("%s is not valid JSON: %w", LinkFile, err)
	}
	if l.CodebaseUUID == "" {
		return Link{}, fmt.Errorf("%s has no codebase_uuid; run `nuzur-cli code link` again", LinkFile)
	}
	return l, nil
}

// WriteLink writes the link of the module in dir.
func WriteLink(dir string, l Link) error {
	path := filepath.Join(dir, LinkFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Repo describes where a module sits in its git repository.
type Repo struct {
	Root          string // absolute repository root
	ModuleDir     string // module dir relative to Root; "" at the root
	RemoteURL     string // origin, normalized to credential-free https
	Head          string // HEAD commit sha; empty in a repository with no commits
	Branch        string // checked-out branch ("HEAD" when detached)
	DefaultBranch string // origin's default branch, or the current one
}

// DescribeRepo reads the git facts `code link` needs about the module in dir.
func DescribeRepo(ctx context.Context, dir string) (Repo, error) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return Repo{}, fmt.Errorf("%s has no go.mod: run this from a Go module root, or pass --dir", dir)
	}
	r := Repo{Root: gitOut(ctx, dir, "rev-parse", "--show-toplevel")}
	if r.Root == "" {
		return Repo{}, fmt.Errorf("%s is not inside a git repository", dir)
	}
	r.ModuleDir = strings.TrimSuffix(gitOut(ctx, dir, "rev-parse", "--show-prefix"), "/")
	r.RemoteURL = NormalizeRemote(gitOut(ctx, dir, "remote", "get-url", "origin"))
	r.Head = gitOut(ctx, dir, "rev-parse", "HEAD")
	r.Branch = gitOut(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	r.DefaultBranch = strings.TrimPrefix(gitOut(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"), "origin/")
	if r.DefaultBranch == "" && r.Branch != "HEAD" {
		r.DefaultBranch = r.Branch
	}
	return r, nil
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// DefaultIdentifier names a codebase after its remote (or root dir), plus the
// module dir when the module is not at the repository root, so two modules of
// one monorepo get distinct identifiers.
func (r Repo) DefaultIdentifier() string {
	base := filepath.Base(r.Root)
	if r.RemoteURL != "" {
		base = r.RemoteURL[strings.LastIndex(r.RemoteURL, "/")+1:]
	}
	if r.ModuleDir != "" {
		base += "-" + strings.ReplaceAll(r.ModuleDir, "/", "-")
	}
	slug := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(base), "-"), "-._")
	if len(slug) > 128 {
		slug = slug[:128]
	}
	return slug
}

// StampedProjects lists the distinct nuzur project uuids stamped into the
// codegen manifests under dir: a module that contains nuzur-generated code
// (a client like sfapi/, or a whole generated app) names its own project.
func StampedProjects(dir string) ([]string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	trees, err := findManifests(abs, abs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	seen := map[string]bool{}
	for _, t := range trees {
		if id := t.manifest.ProjectUUID; id != "" {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
