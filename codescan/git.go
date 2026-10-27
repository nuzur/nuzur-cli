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

// churn sums lines changed in .go files per repo-relative directory over the
// last days, with the commits and authors that touched each directory.
func (g gitInfo) churn(ctx context.Context, dir string, days int) map[string]*churnAcc {
	out := map[string]*churnAcc{}
	if g.root == "" {
		return out
	}
	cmd := exec.CommandContext(ctx, "git", "log", "--since="+strconv.Itoa(days)+".days", "--numstat",
		"--format=%x00%H%x09%ae", "--", ".")
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		return out
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
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		d := path.Dir(file)
		if d == "." {
			d = ""
		}
		acc, ok := out[d]
		if !ok {
			acc = &churnAcc{commits: map[string]bool{}, authors: map[string]bool{}}
			out[d] = acc
		}
		acc.lines += added + deleted
		acc.commits[sha] = true
		acc.authors[author] = true
	}
	return out
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
