package codescan

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// attachDocs links existing prose to nodes, by path only (content is never
// read into the facts): markdown files inside a package or component dir, and
// files under a top-level docs/ dir whose name mentions a component.
func (s *scanner) attachDocs(repoRoot string) {
	var docFiles []string
	for _, docsDir := range []string{"docs", "doc"} {
		root := filepath.Join(repoRoot, docsDir)
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if rel, _ := filepath.Rel(root, path); strings.Count(rel, string(filepath.Separator)) >= 2 {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				docFiles = append(docFiles, path)
			}
			return nil
		})
	}

	for _, n := range s.b.nodes {
		switch n.Kind {
		case pb.CodeNodeKind_CODE_NODE_KIND_PACKAGE, pb.CodeNodeKind_CODE_NODE_KIND_COMPONENT, pb.CodeNodeKind_CODE_NODE_KIND_CONTAINER:
		default:
			continue
		}
		seen := map[string]bool{}
		add := func(abs string) {
			rel := s.b.rel(abs)
			if !seen[rel] {
				seen[rel] = true
				n.Docs = append(n.Docs, rel)
			}
		}
		if n.Path != "" {
			entries, _ := os.ReadDir(filepath.Join(repoRoot, filepath.FromSlash(n.Path)))
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
					add(filepath.Join(repoRoot, filepath.FromSlash(n.Path), e.Name()))
				}
			}
		}
		if n.Kind != pb.CodeNodeKind_CODE_NODE_KIND_COMPONENT || len(n.Name) < 3 {
			continue
		}
		name := strings.ToLower(n.Name)
		for _, f := range docFiles {
			if mentions(s.b.rel(f), name) {
				add(f)
			}
		}
	}
}

// mentions reports whether a doc path names the component: as a whole token
// ("pass" in pass_gift_comp.md) or as two adjacent tokens run together
// ("fantasy_v4" for fantasyv4).
func mentions(docPath, name string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(strings.TrimSuffix(docPath, filepath.Ext(docPath))), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, t := range tokens {
		if i == 0 && (t == "docs" || t == "doc") {
			continue
		}
		if t == name || (i+1 < len(tokens) && t+tokens[i+1] == name) {
			return true
		}
	}
	return false
}
