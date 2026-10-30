package codescan

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

type gitInfo struct {
	root        string
	sha         string
	branch      string
	committedAt string
	dirty       bool
	remote      string
}

func gitOut(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readGitInfo describes the commit being scanned. Outside a git repository
// every field is empty and the scan still works.
func readGitInfo(ctx context.Context, dir string) gitInfo {
	g := gitInfo{root: gitOut(ctx, dir, "rev-parse", "--show-toplevel")}
	if g.root == "" {
		return g
	}
	g.sha = gitOut(ctx, dir, "rev-parse", "HEAD")
	g.branch = gitOut(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	g.committedAt = gitOut(ctx, dir, "show", "-s", "--format=%cI", "HEAD")
	g.dirty = gitOut(ctx, dir, "status", "--porcelain", "--untracked-files=no") != ""
	g.remote = NormalizeRemote(gitOut(ctx, dir, "remote", "get-url", "origin"))
	return g
}

// branchPoint is the commit where HEAD's branch split from defaultBranch: the
// merge-base with origin's copy of it when there is one (it is the shared,
// up-to-date one), else the local branch. Empty when HEAD is on the default
// branch or the default branch is not known here.
func branchPoint(ctx context.Context, dir, branch, defaultBranch string) string {
	if defaultBranch == "" || branch == defaultBranch {
		return ""
	}
	for _, ref := range []string{"refs/remotes/origin/" + defaultBranch, "refs/heads/" + defaultBranch} {
		if gitOut(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}") == "" {
			continue
		}
		return gitOut(ctx, dir, "merge-base", "HEAD", ref)
	}
	return ""
}

// localDefaultBranch is origin's default branch as this clone knows it. A
// clone made by pushing an existing repository has no origin/HEAD; then it is
// whichever one of main and master exists, and empty when both or neither do.
func localDefaultBranch(ctx context.Context, dir string) string {
	if b := strings.TrimPrefix(gitOut(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"), "origin/"); b != "" {
		return b
	}
	var found []string
	for _, b := range []string{"main", "master"} {
		for _, ref := range []string{"refs/remotes/origin/" + b, "refs/heads/" + b} {
			if gitOut(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}") != "" {
				found = append(found, b)
				break
			}
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

var scpRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormalizeRemote turns any git remote into a credential-free https URL:
// git@github.com:org/repo.git and https://token@github.com/org/repo.git both
// become https://github.com/org/repo.
func NormalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if !strings.Contains(remote, "://") {
		if m := scpRemote.FindStringSubmatch(remote); m != nil {
			remote = "https://" + m[1] + "/" + m[2]
		}
	}
	if i := strings.Index(remote, "://"); i >= 0 {
		rest := remote[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 && at < strings.Index(rest+"/", "/") {
			rest = rest[at+1:]
		}
		remote = "https://" + rest
	}
	return strings.TrimSuffix(strings.TrimSuffix(remote, "/"), ".git")
}

type churnAcc struct {
	lines   int
	commits map[string]bool
	authors map[string]bool
}

var braceRename = regexp.MustCompile(`\{[^}]* => ([^}]*)\}`)

// churnData is git activity over the churn window: per repo-relative
// directory for Go files, and per repo-relative file for frontend files
// (templates and scripts are one node each), keyed by normalizeAssetPath.
type churnData struct {
	dirs  map[string]*churnAcc
	files map[string]*churnAcc
}

// churn sums lines changed over the last days, with the commits and authors
// behind them.
func (g gitInfo) churn(ctx context.Context, dir string, days int) churnData {
	data := churnData{dirs: map[string]*churnAcc{}, files: map[string]*churnAcc{}}
	if g.root == "" {
		return data
	}
	cmd := exec.CommandContext(ctx, "git", "log", "--since="+strconv.Itoa(days)+".days", "--numstat",
		"--format=%x00%H%x09%ae", "--", ".")
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		return data
	}
	var sha, author string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\x00") {
			sha, author, _ = strings.Cut(line[1:], "\t")
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[0] == "-" {
			continue
		}
		file := parts[2]
		if strings.Contains(file, " => ") {
			if braceRename.MatchString(file) {
				file = braceRename.ReplaceAllString(file, "$1")
				file = strings.ReplaceAll(file, "//", "/")
			} else {
				file = file[strings.Index(file, " => ")+4:]
			}
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		ext := strings.ToLower(path.Ext(file))
		switch {
		case ext == ".go":
			d := path.Dir(file)
			if d == "." {
				d = ""
			}
			data.dirs[d] = data.dirs[d].add(added+deleted, sha, author)
		case templateExts[ext] || scriptExts[ext] || styleExts[ext]:
			// Keyed without version suffixes, so edits made before a
			// version-bump rename still count for the file.
			key := normalizeAssetPath(file)
			data.files[key] = data.files[key].add(added+deleted, sha, author)
		}
	}
	return data
}

func (a *churnAcc) add(lines int, sha, author string) *churnAcc {
	if a == nil {
		a = &churnAcc{commits: map[string]bool{}, authors: map[string]bool{}}
	}
	a.lines += lines
	a.commits[sha] = true
	a.authors[author] = true
	return a
}

// applyChurn sets churn metrics on package and container nodes, and the
// distinct commit and author counts on their components.
func (s *scanner) applyChurn(byDir map[string]*churnAcc) {
	type compAcc struct{ commits, authors map[string]bool }
	comps := map[string]*compAcc{}
	for _, p := range s.handPkgs() {
		n := s.b.nodes[p.PkgPath]
		acc := byDir[n.Path]
		if acc == nil || n.Metrics == nil || n.Attrs["generated"] == "true" {
			continue
		}
		n.Metrics.Churn = int32(acc.lines)
		n.Metrics.Commits = int32(len(acc.commits))
		n.Metrics.Authors = int32(len(acc.authors))
		comp := s.component[p.PkgPath]
		if comp == "" {
			continue
		}
		c := comps[comp]
		if c == nil {
			c = &compAcc{commits: map[string]bool{}, authors: map[string]bool{}}
			comps[comp] = c
		}
		for k := range acc.commits {
			c.commits[k] = true
		}
		for k := range acc.authors {
			c.authors[k] = true
		}
		s.b.nodes[comp].Metrics.Churn += int32(acc.lines)
	}
	for id, c := range comps {
		s.b.nodes[id].Metrics.Commits = int32(len(c.commits))
		s.b.nodes[id].Metrics.Authors = int32(len(c.authors))
	}
}
