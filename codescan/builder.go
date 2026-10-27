package codescan

import (
	"fmt"
	"go/token"
	"path/filepath"
	"sort"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// maxEvidence caps the call/import sites kept per edge. Weight still counts
// every site; evidence is a sample for the inspector's "show me where" links.
const maxEvidence = 5

type edgeKey struct {
	kind pb.CodeEdgeKind
	from string
	to   string
}

type site struct {
	file string
	line int32
}

type edgeAcc struct {
	sites map[site]bool
	ops   map[string]bool
}

// builder accumulates facts while extractors run, then emits them in a
// deterministic order: the same tree must always produce the same bytes so
// snapshots diff cleanly.
type builder struct {
	repoRoot   string
	nodes      map[string]*pb.CodeNode
	edges      map[edgeKey]*edgeAcc
	routes     []*pb.CodeRoute
	background []*pb.CodeBackgroundTask
	unresolved map[string]*pb.CodeUnresolved
	loadErrors map[string]string
}

func newBuilder(repoRoot string) *builder {
	return &builder{
		repoRoot:   repoRoot,
		nodes:      map[string]*pb.CodeNode{},
		edges:      map[edgeKey]*edgeAcc{},
		unresolved: map[string]*pb.CodeUnresolved{},
		loadErrors: map[string]string{},
	}
}

// node returns the node with id, creating it when absent.
func (b *builder) node(id string, kind pb.CodeNodeKind, name string) *pb.CodeNode {
	if n, ok := b.nodes[id]; ok {
		return n
	}
	n := &pb.CodeNode{Id: id, Kind: kind, Name: name}
	b.nodes[id] = n
	return n
}

func (b *builder) edge(kind pb.CodeEdgeKind, from, to string, pos token.Position, op string) {
	if from == "" || to == "" || from == to {
		return
	}
	k := edgeKey{kind, from, to}
	acc, ok := b.edges[k]
	if !ok {
		acc = &edgeAcc{sites: map[site]bool{}, ops: map[string]bool{}}
		b.edges[k] = acc
	}
	acc.sites[b.site(pos)] = true
	if op != "" {
		acc.ops[op] = true
	}
}

func (b *builder) site(pos token.Position) site {
	return site{file: b.rel(pos.Filename), line: int32(pos.Line)}
}

func (b *builder) evidence(pos token.Position) *pb.CodeEvidence {
	s := b.site(pos)
	return &pb.CodeEvidence{File: s.file, Line: s.line}
}

// rel turns an absolute path into a slash-separated, repo-relative one.
func (b *builder) rel(path string) string {
	if path == "" {
		return ""
	}
	r, err := filepath.Rel(b.repoRoot, path)
	if err != nil || strings.HasPrefix(r, "..") {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}

func (b *builder) unresolve(kind, value, pkgID string, pos token.Position) {
	key := kind + "\x00" + value + "\x00" + pkgID
	if _, ok := b.unresolved[key]; ok {
		return
	}
	b.unresolved[key] = &pb.CodeUnresolved{Kind: kind, Value: value, PackageId: pkgID, Evidence: b.evidence(pos)}
}

func (b *builder) facts() ([]*pb.CodeNode, []*pb.CodeEdge, []*pb.CodeRoute, []*pb.CodeBackgroundTask, []*pb.CodeUnresolved, []*pb.CodeLoadError) {
	nodes := make([]*pb.CodeNode, 0, len(b.nodes))
	for _, n := range b.nodes {
		sort.Strings(n.Docs)
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Id < nodes[j].Id })

	edges := make([]*pb.CodeEdge, 0, len(b.edges))
	for k, acc := range b.edges {
		sites := make([]site, 0, len(acc.sites))
		for s := range acc.sites {
			sites = append(sites, s)
		}
		sort.Slice(sites, func(i, j int) bool {
			if sites[i].file != sites[j].file {
				return sites[i].file < sites[j].file
			}
			return sites[i].line < sites[j].line
		})
		e := &pb.CodeEdge{Kind: k.kind, From: k.from, To: k.to, Weight: int32(len(sites))}
		for i, s := range sites {
			if i == maxEvidence {
				break
			}
			e.Evidence = append(e.Evidence, &pb.CodeEvidence{File: s.file, Line: s.line})
		}
		for op := range acc.ops {
			e.Ops = append(e.Ops, op)
		}
		sort.Strings(e.Ops)
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		a, c := edges[i], edges[j]
		if a.Kind != c.Kind {
			return a.Kind < c.Kind
		}
		if a.From != c.From {
			return a.From < c.From
		}
		return a.To < c.To
	})

	sort.Slice(b.routes, func(i, j int) bool {
		a, c := b.routes[i], b.routes[j]
		if a.Pattern != c.Pattern {
			return a.Pattern < c.Pattern
		}
		if a.Method != c.Method {
			return a.Method < c.Method
		}
		return evidenceLess(a.Evidence, c.Evidence)
	})
	sort.Slice(b.background, func(i, j int) bool {
		a, c := b.background[i], b.background[j]
		if a.PackageId != c.PackageId {
			return a.PackageId < c.PackageId
		}
		return evidenceLess(a.Evidence, c.Evidence)
	})

	unresolved := make([]*pb.CodeUnresolved, 0, len(b.unresolved))
	for _, u := range b.unresolved {
		unresolved = append(unresolved, u)
	}
	sort.Slice(unresolved, func(i, j int) bool {
		a, c := unresolved[i], unresolved[j]
		return fmt.Sprint(a.Kind, a.Value, a.PackageId) < fmt.Sprint(c.Kind, c.Value, c.PackageId)
	})

	loadErrors := make([]*pb.CodeLoadError, 0, len(b.loadErrors))
	for id, msg := range b.loadErrors {
		loadErrors = append(loadErrors, &pb.CodeLoadError{PackageId: id, Message: msg})
	}
	sort.Slice(loadErrors, func(i, j int) bool { return loadErrors[i].PackageId < loadErrors[j].PackageId })

	return nodes, edges, b.routes, b.background, unresolved, loadErrors
}

func evidenceLess(a, c *pb.CodeEvidence) bool {
	if a == nil || c == nil {
		return a == nil && c != nil
	}
	if a.File != c.File {
		return a.File < c.File
	}
	return a.Line < c.Line
}
