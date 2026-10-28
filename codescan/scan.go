// Package codescan extracts the code architecture facts of a Go module: its
// deployables, components, packages and their dependencies, the HTTP routes it
// serves, the nuzur entities it reads and writes, the external systems and
// key-value keyspaces it touches, and its background work. Facts only — no
// source is carried — and the output is deterministic so snapshots diff cleanly.
package codescan

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"golang.org/x/tools/go/packages"
)

// SchemaVersion is the CodeFacts schema this scanner writes.
const SchemaVersion = 1

// ScannerVersion identifies the extraction logic; bump it when the same tree
// would produce different facts.
const ScannerVersion = "0.4.0"

type Options struct {
	// Dir is the module root to scan (the directory holding go.mod).
	Dir string
	// ChurnDays is the git history window for churn metrics; 0 disables churn.
	ChurnDays int
	// KnownEntities, when set, are the project's entity identifiers. Entity
	// calls that resolve to anything else are reported as unresolved.
	KnownEntities []string
	// DefaultBranch is the codebase's default branch, against which a scan on
	// any other branch records its branch point. Empty: origin's default
	// branch as this clone knows it.
	DefaultBranch string
	// Branch, when set, is recorded instead of the checked-out branch: a
	// detached checkout of a commit known to be on that branch.
	Branch string
}

// Scan extracts the facts of the module at opts.Dir.
func Scan(ctx context.Context, opts Options) (*pb.CodeFacts, error) {
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil, fmt.Errorf("no go.mod in %s: run the scan from a Go module root", dir)
	}

	git := readGitInfo(ctx, dir)
	if opts.Branch != "" {
		git.branch = opts.Branch
	}
	defaultBranch := opts.DefaultBranch
	if defaultBranch == "" && git.root != "" {
		defaultBranch = localDefaultBranch(ctx, dir)
	}
	var branchPointSHA string
	if git.root != "" {
		branchPointSHA = branchPoint(ctx, dir, git.branch, defaultBranch)
	}
	repoRoot := git.root
	if repoRoot == "" {
		repoRoot = dir
	}

	s := &scanner{
		ctx:      ctx,
		opts:     opts,
		dir:      dir,
		b:        newBuilder(repoRoot),
		fset:     token.NewFileSet(),
		known:    map[string]bool{},
		hand:     map[string]*packages.Package{},
		genOfPkg: map[string]string{},
		fe:       frontend{renders: map[string]map[string]token.Position{}, names: map[string]map[string]token.Position{}, partial: map[string]map[string]token.Position{}},
	}
	for _, e := range opts.KnownEntities {
		s.known[e] = true
	}

	s.trees, err = findManifests(dir, repoRoot)
	if err != nil {
		return nil, err
	}

	pkgs, err := packages.Load(&packages.Config{
		Context: ctx,
		Dir:     dir,
		Fset:    s.fset,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedModule,
	}, "./...")
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, errors.New("no Go packages found")
	}
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Main && s.modulePath == "" {
			s.modulePath = p.Module.Path
		}
	}
	s.deps = findGeneratedDeps(ctx, dir)

	s.classify(pkgs)
	s.extractPackages()
	s.extractImports()
	s.extractGeneratedImports()
	s.extractFx()
	for _, p := range s.handPkgs() {
		s.walkPackage(p)
	}
	var churn churnData
	if opts.ChurnDays > 0 {
		churn = git.churn(ctx, dir, opts.ChurnDays)
		s.applyChurn(churn.dirs)
	}
	s.extractFrontend(churn.files)
	s.attachDocs(repoRoot)

	generated := s.generatedFacts() // adds the generated nodes, so it runs first
	nodes, edges, routes, bg, unresolved, loadErrors := s.b.facts()
	facts := &pb.CodeFacts{
		SchemaVersion:  SchemaVersion,
		ScannerVersion: ScannerVersion,
		Language:       "go",
		ModulePath:     s.modulePath,
		Git: &pb.CodeGitInfo{
			CommitSha:   git.sha,
			Branch:      git.branch,
			CommittedAt: git.committedAt,
			Dirty:       git.dirty,
			RemoteUrl:   git.remote,
			ChurnDays:   int32(opts.ChurnDays),
			// Recorded only with the branch point it was taken against.
			BranchPointSha: branchPointSHA,
			DefaultBranch:  valueIf(branchPointSHA != "", defaultBranch),
		},
		Nodes:       nodes,
		Edges:       edges,
		Routes:      routes,
		Background:  bg,
		Unresolved:  unresolved,
		LoadErrors:  loadErrors,
		Generated:   generated,
		DeployHints: deployHints(repoRoot, s.b.rel),
	}
	return facts, nil
}

type scanner struct {
	ctx        context.Context
	opts       Options
	dir        string
	modulePath string
	b          *builder
	fset       *token.FileSet
	known      map[string]bool
	trees      []*generatedTree
	deps       []*depModule

	// hand holds the hand-written packages by import path. Generated packages
	// are collapsed: genOfPkg maps their import path to the generated node id.
	hand      map[string]*packages.Package
	genOfPkg  map[string]string
	genPkgs   map[string][]string // generated node id -> collapsed import paths
	genLoaded []*packages.Package // the collapsed local packages themselves
	provided  map[string]bool     // packages exporting an fx-provided constructor
	fxRoots   map[string]bool     // packages calling fx.Provide (composition roots)
	component map[string]string   // package import path -> component id
	fe        frontend
}

func (s *scanner) handPkgs() []*packages.Package {
	out := make([]*packages.Package, 0, len(s.hand))
	for _, p := range s.hand {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PkgPath < out[j].PkgPath })
	return out
}

// classify splits loaded packages into hand-written and generated ones. A
// package is generated when every one of its Go files is listed in a manifest.
func (s *scanner) classify(pkgs []*packages.Package) {
	s.genPkgs = map[string][]string{}
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			s.b.loadErrors[p.PkgPath] = p.Errors[0].Msg
		}
		if tree := s.treeOf(p); tree != nil {
			s.genOfPkg[p.PkgPath] = tree.nodeID
			s.genPkgs[tree.nodeID] = append(s.genPkgs[tree.nodeID], p.PkgPath)
			s.genLoaded = append(s.genLoaded, p)
			continue
		}
		s.hand[p.PkgPath] = p
	}
}

func (s *scanner) treeOf(p *packages.Package) *generatedTree {
	if len(p.GoFiles) == 0 {
		return nil
	}
	for _, t := range s.trees {
		all := true
		for _, f := range p.GoFiles {
			if !t.files[f] {
				all = false
				break
			}
		}
		if all {
			return t
		}
	}
	return nil
}

// generatedNodeOf returns the generated node an import path belongs to: a
// collapsed local package, or a package inside a generated dependency module.
func (s *scanner) generatedNodeOf(pkgPath string) string {
	if id, ok := s.genOfPkg[pkgPath]; ok {
		return id
	}
	for _, d := range s.deps {
		if pkgPath == d.path || strings.HasPrefix(pkgPath, d.path+"/") {
			return d.nodeID
		}
	}
	return ""
}

func (s *scanner) generatedFacts() []*pb.CodeGenerated {
	var out []*pb.CodeGenerated
	for _, t := range s.trees {
		pkgs := append([]string(nil), s.genPkgs[t.nodeID]...)
		sort.Strings(pkgs)
		out = append(out, &pb.CodeGenerated{
			NodeId:             t.nodeID,
			Dir:                s.b.rel(t.dir),
			Generator:          t.manifest.Generator,
			GeneratedAt:        t.manifest.GeneratedAt,
			ProjectUuid:        t.manifest.ProjectUUID,
			ProjectVersionUuid: t.manifest.ProjectVersionUUID,
			ProjectIdentifier:  t.manifest.ProjectIdentifier,
			Files:              int32(len(t.manifest.Files)),
			Packages:           pkgs,
		})
		n := s.b.node(t.nodeID, pb.CodeNodeKind_CODE_NODE_KIND_GENERATED, t.name)
		n.Path = s.b.rel(t.dir)
		// A generated app's own main (go-code-gen's server) is a deployable:
		// it is what wires a custom zone in, so reachability starts there too.
		for _, p := range s.genLoaded {
			if p.Name == "main" && s.genOfPkg[p.PkgPath] == t.nodeID {
				if n.Attrs == nil {
					n.Attrs = map[string]string{}
				}
				n.Attrs["main"] = "true"
			}
		}
	}
	for _, d := range s.deps {
		if _, used := s.b.nodes[d.nodeID]; !used {
			continue // only dependencies the module actually imports
		}
		out = append(out, &pb.CodeGenerated{
			NodeId:             d.nodeID,
			Generator:          d.manifest.Generator,
			GeneratedAt:        d.manifest.GeneratedAt,
			ProjectUuid:        d.manifest.ProjectUUID,
			ProjectVersionUuid: d.manifest.ProjectVersionUUID,
			ProjectIdentifier:  d.manifest.ProjectIdentifier,
			Files:              int32(len(d.manifest.Files)),
		})
	}
	return out
}

// extractPackages emits a node per hand-written package with its metrics, and
// groups packages into containers (package main) and components.
func (s *scanner) extractPackages() {
	s.provided = s.fxProvidedPackages()
	s.component = map[string]string{}
	for _, p := range s.handPkgs() {
		rel := s.relDir(p)
		var n *pb.CodeNode
		if p.Name == "main" {
			n = s.b.node(p.PkgPath, pb.CodeNodeKind_CODE_NODE_KIND_CONTAINER, containerName(rel, s.modulePath))
		} else {
			n = s.b.node(p.PkgPath, pb.CodeNodeKind_CODE_NODE_KIND_PACKAGE, p.Name)
			comp := s.componentOf(p)
			s.component[p.PkgPath] = comp
			n.ParentId = comp
		}
		n.Path = s.repoPath(rel)
		n.Doc = packageDoc(p)
		n.Metrics = s.metrics(p)
		n.Attrs = map[string]string{}
		if s.provided[p.PkgPath] {
			n.Attrs["fx_provided"] = "true"
		}
		if s.fxRoots[p.PkgPath] {
			n.Attrs["composition_root"] = "true"
		}
		if len(p.GoFiles) > 0 && n.Metrics.Files == 0 {
			n.Attrs["generated"] = "true" // every file is tool output
		}
		if len(n.Attrs) == 0 {
			n.Attrs = nil
		}
	}
	// Component nodes aggregate their packages' metrics.
	for _, p := range s.handPkgs() {
		comp, ok := s.component[p.PkgPath]
		if !ok {
			continue
		}
		dir := strings.TrimPrefix(comp, "component:")
		c := s.b.node(comp, pb.CodeNodeKind_CODE_NODE_KIND_COMPONENT, filepath.Base(dir))
		c.Path = s.repoPath(strings.TrimPrefix(dir, "."))
		if c.Metrics == nil {
			c.Metrics = &pb.CodeMetrics{}
		}
		addMetrics(c.Metrics, s.b.nodes[p.PkgPath].Metrics)
		for _, attr := range []string{"fx_provided", "composition_root"} {
			if s.b.nodes[p.PkgPath].Attrs[attr] == "true" {
				if c.Attrs == nil {
					c.Attrs = map[string]string{}
				}
				c.Attrs[attr] = "true"
			}
		}
		if s.relDir(p) == dir {
			c.Doc = s.b.nodes[p.PkgPath].Doc
		}
	}
	for _, c := range s.b.nodes {
		if c.Kind == pb.CodeNodeKind_CODE_NODE_KIND_COMPONENT && c.Metrics != nil && c.Metrics.Files == 0 {
			if c.Attrs == nil {
				c.Attrs = map[string]string{}
			}
			c.Attrs["generated"] = "true"
		}
	}
}

// repoPath turns a module-relative dir into a repo-relative one, for modules
// that live below the repository root.
func (s *scanner) repoPath(moduleRel string) string {
	base := s.b.rel(s.dir)
	if base == "." || base == "" {
		return moduleRel
	}
	if moduleRel == "" {
		return base
	}
	return base + "/" + moduleRel
}

func containerName(rel, modulePath string) string {
	if rel == "" || rel == "." {
		return filepath.Base(modulePath)
	}
	return rel
}

// componentOf picks the package's component: the nearest ancestor dir (or the
// package itself) that provides an fx constructor; otherwise the top-level dir,
// or the first two levels when the top level is only a namespace (modules/,
// internal/, pkg/) holding several feature packages.
func (s *scanner) componentOf(p *packages.Package) string {
	rel := s.relDir(p)
	parts := strings.Split(rel, "/")
	for i := len(parts); i > 0; i-- {
		dir := strings.Join(parts[:i], "/")
		if s.providedDirs()[dir] {
			return "component:" + dir
		}
	}
	if rel == "" || rel == "." {
		return "component:."
	}
	top := parts[0]
	if len(parts) > 1 && s.isNamespace(top) {
		return "component:" + top + "/" + parts[1]
	}
	return "component:" + top
}

func (s *scanner) providedDirs() map[string]bool {
	out := map[string]bool{}
	for path := range s.provided {
		if p, ok := s.hand[path]; ok {
			out[s.relDir(p)] = true
		}
	}
	return out
}

// isNamespace reports whether a top-level dir mostly groups other packages:
// it has at least two child dirs holding packages.
func (s *scanner) isNamespace(top string) bool {
	children := map[string]bool{}
	for _, p := range s.hand {
		parts := strings.Split(s.relDir(p), "/")
		if len(parts) > 1 && parts[0] == top {
			children[parts[1]] = true
		}
	}
	return len(children) >= 2
}

func (s *scanner) relDir(p *packages.Package) string {
	if len(p.GoFiles) == 0 {
		return strings.TrimPrefix(strings.TrimPrefix(p.PkgPath, s.modulePath), "/")
	}
	rel, err := filepath.Rel(s.dir, filepath.Dir(p.GoFiles[0]))
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ""
	}
	return rel
}

func packageDoc(p *packages.Package) string {
	for _, f := range p.Syntax {
		if f.Doc == nil {
			continue
		}
		text := strings.TrimSpace(f.Doc.Text())
		if i := strings.Index(text, "\n\n"); i >= 0 {
			text = text[:i]
		}
		text = strings.Join(strings.Fields(text), " ")
		if len(text) > 500 {
			text = text[:500]
		}
		return text
	}
	return ""
}

// generatedFiles returns the package's files carrying Go's standard
// "Code generated ... DO NOT EDIT." marker (protoc, stringer, sqlc output).
func (s *scanner) generatedFiles(p *packages.Package) map[string]bool {
	out := map[string]bool{}
	for _, f := range p.Syntax {
		if ast.IsGenerated(f) {
			out[s.fset.Position(f.Pos()).Filename] = true
		}
	}
	return out
}

// metrics measures the hand-written part of a package: files carrying a
// generated marker are left out of every count, so tool output never shows
// up as a size outlier or a hotspot.
func (s *scanner) metrics(p *packages.Package) *pb.CodeMetrics {
	gen := s.generatedFiles(p)
	m := &pb.CodeMetrics{}
	for _, f := range p.GoFiles {
		if gen[f] {
			continue
		}
		m.Files++
		m.Loc += int32(countLOC(f))
	}
	if len(p.GoFiles) > 0 {
		tests, _ := filepath.Glob(filepath.Join(filepath.Dir(p.GoFiles[0]), "*_test.go"))
		m.TestFiles = int32(len(tests))
		for _, f := range tests {
			m.TestLoc += int32(countLOC(f))
		}
	}
	if p.Types != nil && m.Files > 0 {
		for _, name := range p.Types.Scope().Names() {
			if token.IsExported(name) {
				m.ExportedSymbols++
			}
		}
	}
	return m
}

func countLOC(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func addMetrics(dst, src *pb.CodeMetrics) {
	if src == nil {
		return
	}
	dst.Files += src.Files
	dst.Loc += src.Loc
	dst.TestFiles += src.TestFiles
	dst.TestLoc += src.TestLoc
	dst.ExportedSymbols += src.ExportedSymbols
	dst.Churn += src.Churn
	dst.Commits += src.Commits
	// Authors don't add up across packages; applyChurn sets component authors.
}

// extractImports emits package->package imports among hand-written packages,
// package->generated edges for imports of generated code, and package->external
// edges for imports of known SDKs.
func (s *scanner) extractImports() {
	for _, p := range s.handPkgs() {
		for _, f := range p.Syntax {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				pos := s.fset.Position(imp.Pos())
				switch {
				case s.hand[path] != nil:
					s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_IMPORTS, p.PkgPath, path, pos, "")
				case s.generatedNodeOf(path) != "":
					gen := s.generatedNodeOf(path)
					s.ensureGeneratedNode(gen)
					s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_USES_GENERATED, p.PkgPath, gen, pos, "")
				default:
					if sys, ok := externalForImport(path); ok {
						s.b.node("ext:"+sys, pb.CodeNodeKind_CODE_NODE_KIND_EXTERNAL_SYSTEM, sys).Attrs = map[string]string{"via": "sdk"}
						s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_USES_EXTERNAL, p.PkgPath, "ext:"+sys, pos, "")
					}
				}
			}
		}
	}
}

// extractGeneratedImports keeps the one thing a collapsed generated tree says
// about hand-written code: which of it the tree imports. go-code-gen's main and
// hooks import the custom zone, and without these edges that zone looks
// unreachable — dead code — when the generated server is what runs it.
func (s *scanner) extractGeneratedImports() {
	sort.Slice(s.genLoaded, func(i, j int) bool { return s.genLoaded[i].PkgPath < s.genLoaded[j].PkgPath })
	for _, p := range s.genLoaded {
		gen := s.genOfPkg[p.PkgPath]
		for _, f := range p.Syntax {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if s.hand[path] != nil {
					s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_IMPORTS, gen, path, s.fset.Position(imp.Pos()), "")
				}
			}
		}
	}
}

func (s *scanner) ensureGeneratedNode(id string) *pb.CodeNode {
	if n, ok := s.b.nodes[id]; ok {
		return n
	}
	name := id
	for _, t := range s.trees {
		if t.nodeID == id {
			name = t.name
		}
	}
	for _, d := range s.deps {
		if d.nodeID == id {
			name = d.name
		}
	}
	return s.b.node(id, pb.CodeNodeKind_CODE_NODE_KIND_GENERATED, name)
}

// walkPackage runs the per-call-site extractors over one package's syntax.
func (s *scanner) walkPackage(p *packages.Package) {
	if p.TypesInfo == nil {
		return
	}
	for _, f := range p.Syntax {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			s.walkRoutes(p, fn.Body, "")
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					s.entityCall(p, x)
					s.keyspaceCall(p, x)
					s.dialCall(p, x)
					s.tickerCall(p, fn, x)
				case *ast.GoStmt:
					s.addBackground(p, fn, pb.CodeBackgroundKind_CODE_BACKGROUND_KIND_GOROUTINE, x.Pos())
				case *ast.CompositeLit:
					s.lifecycleHook(p, fn, x)
				case *ast.BasicLit:
					s.hostLiteral(p, x)
					s.templateLiteral(p, x)
				}
				return true
			})
		}
		// Hosts in package-level consts and vars (base URLs, endpoints).
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				ast.Inspect(gd, func(n ast.Node) bool {
					if lit, ok := n.(*ast.BasicLit); ok {
						s.hostLiteral(p, lit)
						s.templateLiteral(p, lit)
					}
					return true
				})
			}
		}
	}
}

// calledFunc resolves the function or method a call invokes, if static.
func calledFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	var id *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	case *ast.IndexExpr:
		if sel, ok := fun.X.(*ast.SelectorExpr); ok {
			id = sel.Sel
		} else if i, ok := fun.X.(*ast.Ident); ok {
			id = i
		}
	}
	if id == nil {
		return nil
	}
	f, _ := info.Uses[id].(*types.Func)
	return f
}

// recvNamed returns the named type of a method's receiver, through pointers.
func recvNamed(f *types.Func) *types.Named {
	sig, ok := f.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil
	}
	t := sig.Recv().Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, _ := t.(*types.Named)
	return named
}

func funcPkgPath(f *types.Func) string {
	if f == nil || f.Pkg() == nil {
		return ""
	}
	return f.Pkg().Path()
}

// stringConst evaluates an expression to a compile-time string constant.
func stringConst(info *types.Info, e ast.Expr) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

func valueIf(cond bool, v string) string {
	if cond {
		return v
	}
	return ""
}
