package codescan

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"golang.org/x/tools/go/packages"
)

// The frontend of a server-rendered Go app: the HTML templates its handlers
// render, the scripts those templates load, and the routes those scripts call
// back into. Everything is found from what the code names literally:
//
//   - a Go string literal that names an existing template file (ParseFiles,
//     ParseGlob, ParseFS, or a helper that receives the path) makes the package
//     render it;
//   - {{template "x"}} includes the one file named or defining x;
//   - <script src="/…/a.js">, or any quoted absolute .js path (import(),
//     loader helpers), includes the file whose path ends that way;
//   - a script path built from a template variable (src="/js/{{$f}}.js")
//     marks its directory dynamic: a name in a Go []string literal naming a
//     file there ([]string{"drag_v3.3.2"}) makes the package include it; the
//     same holds for stylesheets (href="/css/{{$c}}.css");
//   - import … from "./b.js" and import("./b.js?v=1") include b.js, and so
//     does a string naming a sibling script without its version or extension
//     ("fsv3_raq" for fsv3_raq_v3.3.2.js) when the name has a _ or -;
//   - a JS string starting with "/" (concatenations and ${} become a dynamic
//     segment) that matches a Go route pattern calls that route.
//
// Templates and scripts in the same top-level directories that nothing names
// are kept too, so the server can say nothing uses them.

var (
	templateExts = map[string]bool{".html": true, ".tmpl": true, ".gohtml": true, ".tpl": true}
	scriptExts   = map[string]bool{".js": true, ".mjs": true}

	tplIncludeRe = regexp.MustCompile(`\{\{-?\s*(?:template|block)\s+"([^"]+)"`)
	tplDefineRe  = regexp.MustCompile(`\{\{-?\s*(?:define|block)\s+"([^"]+)"`)
	scriptSrcRe  = regexp.MustCompile(`(?is)<script\b[^>]*?\bsrc\s*=\s*["']([^"']+)["']`)
	styleExts    = map[string]bool{".css": true}
	// A quoted absolute path to a script or stylesheet anywhere in a template
	// or script.
	assetPathLitRe  = regexp.MustCompile("[\"'`](/[^\"'`\\s?#{}]+\\.(?:m?js|css))(?:[?#][^\"'`]*)?[\"'`]")
	linkTagRe       = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	stylesheetRelRe = regexp.MustCompile(`(?i)\brel\s*=\s*["']?[^"'>]*\bstylesheet\b`)
	hrefRe          = regexp.MustCompile(`(?i)\bhref\s*=\s*["']([^"']+)["']`)
	cssImportRe     = regexp.MustCompile(`@import\s+(?:url\(\s*)?["']?([^"')\s;]+)`)
	// An attribute in an HTML string a script builds: <a href="/tdr/' + id.
	attrPathRe  = regexp.MustCompile(`(?i)\b(?:href|action|src|formaction)\s*=\s*\\?["']?(/[^"'\s>]*)`)
	semverRe    = regexp.MustCompile(`v?\d+\.\d+\.\d+`)
	goNameLitRe = regexp.MustCompile(`^[\w.-]{1,80}$`)
	jsImportRe  = regexp.MustCompile(`(?m)(?:\bimport\s*(?:[\w*{}\s,]+\s*from\s*)?|\bexport\s+[\w*{}\s,]+\s*from\s*|\bimport\s*\(\s*)["'](\.{1,2}/[^"'?#]+)`)
	// A loader naming a sibling script without path or version
	// (loadBeat('fsv3_raq')): separators keep plain words like "search" out.
	jsNameLitRe = regexp.MustCompile(`^[A-Za-z0-9]+(?:[_-][A-Za-z0-9]+)+$`)
	// A version suffix on a file or directory name: fsv3_v3.3.2.js, baq_v3.3.2/.
	versionSuffixRe = regexp.MustCompile(`^(.+?)[_.-]v?\d+(?:\.\d+){1,3}(\.[A-Za-z0-9]+)?$`)
	// Bundler output (browserify, webpack) is a build artifact, not source.
	bundlerPreludeRe = regexp.MustCompile(`^\s*\(function\(\)\{function r\(e,n,t\)|__webpack_require__`)
)

// frontend collects what the Go walk finds; extractFrontend resolves it.
type frontend struct {
	// renders: package or container id -> template file (abs) -> first site.
	renders map[string]map[string]token.Position
	// names: short Go string literals ("drag_v3.3.2") -> package -> first
	// site, for scripts a template loads by a name the handler passes in.
	names map[string]map[string]token.Position
	// partial: template-named literals that are not a path on their own
	// ("./templates/admin/"+page with page = "llm.html"): package -> literal
	// -> site, resolved later to the one template whose path ends that way.
	partial map[string]map[string]token.Position
}

// templateLiteral records a Go string literal naming template files that
// exist. Paths resolve against the module root (the process's usual working
// directory) and the package's own dir (go:embed + ParseFS).
func (s *scanner) templateLiteral(p *packages.Package, lit *ast.BasicLit) {
	if lit.Kind != token.STRING {
		return
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil || len(v) > 512 || strings.ContainsAny(v, "\n\x00") {
		return
	}
	unit := p.PkgPath
	if p.Name != "main" && s.component[p.PkgPath] == "" {
		return
	}
	if !templateExts[strings.ToLower(path.Ext(v))] {
		return
	}
	bases := []string{s.dir}
	if len(p.GoFiles) > 0 {
		bases = append(bases, filepath.Dir(p.GoFiles[0]))
	}
	for _, base := range bases {
		pattern := filepath.Join(base, filepath.FromSlash(v))
		matches := []string{pattern}
		if strings.ContainsAny(v, "*?[") {
			matches, _ = filepath.Glob(pattern)
		}
		found := false
		for _, m := range matches {
			if st, err := os.Stat(m); err != nil || st.IsDir() || !s.inModule(m) {
				continue
			}
			if s.fe.renders[unit] == nil {
				s.fe.renders[unit] = map[string]token.Position{}
			}
			if _, ok := s.fe.renders[unit][m]; !ok {
				s.fe.renders[unit][m] = s.fset.Position(lit.Pos())
			}
			found = true
		}
		if found {
			return
		}
	}
	if !strings.ContainsAny(v, "*?[") {
		if s.fe.partial[unit] == nil {
			s.fe.partial[unit] = map[string]token.Position{}
		}
		if _, ok := s.fe.partial[unit][v]; !ok {
			s.fe.partial[unit][v] = s.fset.Position(lit.Pos())
		}
	}
}

// nameList records the names in a []string literal (CSSFiles: []string{"tdr"},
// []string{"drag_v3.3.2"}): the lists a handler passes for a template to load
// scripts or stylesheets by name. Only list elements count; the same word
// elsewhere in Go code ("tdr" as a key or a log field) says nothing.
func (s *scanner) nameList(p *packages.Package, lit *ast.CompositeLit) {
	if p.TypesInfo == nil || (p.Name != "main" && s.component[p.PkgPath] == "") {
		return
	}
	sl, ok := p.TypesInfo.TypeOf(lit).(*types.Slice)
	if !ok {
		return
	}
	if b, ok := sl.Elem().Underlying().(*types.Basic); !ok || b.Kind() != types.String {
		return
	}
	for _, e := range lit.Elts {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			continue
		}
		v, err := strconv.Unquote(bl.Value)
		if err != nil || !goNameLitRe.MatchString(v) {
			continue
		}
		if s.fe.names[v] == nil {
			s.fe.names[v] = map[string]token.Position{}
		}
		if _, ok := s.fe.names[v][p.PkgPath]; !ok {
			s.fe.names[v][p.PkgPath] = s.fset.Position(bl.Pos())
		}
	}
}

func (s *scanner) inModule(abs string) bool {
	r, err := filepath.Rel(s.dir, abs)
	return err == nil && !strings.HasPrefix(r, "..") && !strings.Contains(filepath.ToSlash(r), "node_modules/")
}

type feKind int

const (
	feTemplate feKind = iota
	feScript
	feStylesheet
)

var feKinds = map[feKind]struct {
	prefix string
	kind   pb.CodeNodeKind
}{
	feTemplate:   {"tpl:", pb.CodeNodeKind_CODE_NODE_KIND_TEMPLATE},
	feScript:     {"js:", pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT},
	feStylesheet: {"css:", pb.CodeNodeKind_CODE_NODE_KIND_STYLESHEET},
}

type feFile struct {
	abs, rel, id string
	kind         feKind
	text         string
	owners       map[string]bool // packages whose pages load it, directly or not
}

// feSource is what includes a frontend file: a Go package (rendering a
// template, or naming a script) or another frontend file.
type feSource struct {
	pkg  string
	file *feFile
}

type feInclude struct {
	from *feFile
	to   *feFile
	line int
}

type routeCall struct {
	route *pb.CodeRoute
	line  int
}

// extractFrontend turns the recorded renders into template, script and
// stylesheet nodes, their include graph, and the routes scripts call. It runs
// after the Go walk, so routes are known.
func (s *scanner) extractFrontend(churn map[string]*churnAcc) {
	if len(s.fe.renders) == 0 && len(s.fe.partial) == 0 {
		return
	}
	files := map[string]*feFile{} // abs -> file
	add := func(abs string, kind feKind) *feFile {
		if f, ok := files[abs]; ok {
			return f
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			return nil
		}
		text := string(raw)
		if kind != feTemplate && isVendorAsset(abs, text) {
			return nil
		}
		f := &feFile{abs: abs, rel: s.b.rel(abs), kind: kind, text: text, owners: map[string]bool{}}
		files[abs] = f
		return f
	}

	// Rendered templates, then every template beside them.
	tplRoots := map[string]bool{}
	for _, tpls := range s.fe.renders {
		for abs := range tpls {
			add(abs, feTemplate)
			tplRoots[s.topDir(abs)] = true
		}
	}
	s.walkFiles(tplRoots, templateExts, func(abs string) { add(abs, feTemplate) })

	// Template names that were not paths on their own.
	var tplPaths []string
	for abs, f := range files {
		if f.kind == feTemplate {
			tplPaths = append(tplPaths, abs)
		}
	}
	sort.Strings(tplPaths)
	for unit, lits := range s.fe.partial {
		for v, pos := range lits {
			if abs := s.resolveURL(v, tplPaths); abs != "" && files[abs] != nil {
				if s.fe.renders[unit] == nil {
					s.fe.renders[unit] = map[string]token.Position{}
				}
				if _, ok := s.fe.renders[unit][abs]; !ok {
					s.fe.renders[unit][abs] = pos
				}
			}
		}
	}
	if len(s.fe.renders) == 0 {
		return
	}

	// Partials resolve by file name or {{define}} name, only when unambiguous.
	byName := map[string][]*feFile{}
	for _, f := range sortedFiles(files) {
		byName[filepath.Base(f.abs)] = append(byName[filepath.Base(f.abs)], f)
		for _, m := range tplDefineRe.FindAllStringSubmatch(f.text, -1) {
			if m[1] != filepath.Base(f.abs) {
				byName[m[1]] = append(byName[m[1]], f)
			}
		}
	}

	var includes []feInclude
	// Directories a template loads scripts or stylesheets from by a name
	// only known at render time (src="/js/{{$f}}.js").
	dynamicDirs := map[feKind]map[string]bool{feScript: {}, feStylesheet: {}}
	type assetURL struct {
		from *feFile
		url  string
		kind feKind
		line int
	}
	var urls []assetURL
	for _, f := range sortedFiles(files) {
		for _, m := range tplIncludeRe.FindAllStringSubmatchIndex(f.text, -1) {
			name := f.text[m[2]:m[3]]
			if defs := byName[name]; len(defs) == 1 && defs[0] != f {
				includes = append(includes, feInclude{f, defs[0], lineAt(f.text, m[0])})
			}
		}
		for _, m := range scriptSrcRe.FindAllStringSubmatchIndex(f.text, -1) {
			url := f.text[m[2]:m[3]]
			urlPath, _, _ := strings.Cut(url, "?")
			if strings.Contains(url, "://") || strings.HasPrefix(url, "//") {
				continue // another host
			}
			if i := strings.Index(urlPath, "{{"); i >= 0 {
				// A name only known at render time: its directory is loaded
				// from by name.
				if dir := s.resolveDir(urlPath[:i]); dir != "" {
					dynamicDirs[feScript][dir] = true
				}
				continue
			}
			urls = append(urls, assetURL{f, url, feScript, lineAt(f.text, m[0])})
		}
		for _, m := range linkTagRe.FindAllStringIndex(f.text, -1) {
			tag := f.text[m[0]:m[1]]
			if !stylesheetRelRe.MatchString(tag) {
				continue
			}
			if h := hrefRe.FindStringSubmatch(tag); h != nil && !strings.Contains(h[1], "://") && !strings.HasPrefix(h[1], "//") {
				p, _, _ := strings.Cut(h[1], "?")
				if i := strings.Index(p, "{{"); i >= 0 {
					if dir := s.resolveDir(p[:i]); dir != "" {
						dynamicDirs[feStylesheet][dir] = true
					}
					continue
				}
				urls = append(urls, assetURL{f, h[1], feStylesheet, lineAt(f.text, m[0])})
			}
		}
		for _, m := range assetPathLitRe.FindAllStringSubmatchIndex(f.text, -1) {
			kind := feScript
			if strings.EqualFold(path.Ext(f.text[m[2]:m[3]]), ".css") {
				kind = feStylesheet
			}
			urls = append(urls, assetURL{f, f.text[m[2]:m[3]], kind, lineAt(f.text, m[0])})
		}
	}

	// Scripts and stylesheets: every one under the directories the loaded
	// ones live in (and the directories loaded from by name).
	roots := map[feKind]map[string]bool{feScript: {}, feStylesheet: {}}
	for _, u := range urls {
		if abs := s.resolveURL(u.url, nil); abs != "" {
			roots[u.kind][s.topDir(abs)] = true
		}
	}
	for kind, dirs := range dynamicDirs {
		for dir := range dirs {
			roots[kind][s.topDir(dir)] = true
		}
	}
	candidates := map[feKind][]string{}
	for kind, exts := range map[feKind]map[string]bool{feScript: scriptExts, feStylesheet: styleExts} {
		s.walkFiles(roots[kind], exts, func(abs string) { candidates[kind] = append(candidates[kind], abs) })
		sort.Strings(candidates[kind])
		for _, abs := range candidates[kind] {
			add(abs, kind)
		}
	}
	for _, u := range urls {
		if to := files[s.resolveURL(u.url, candidates[u.kind])]; to != nil && to.kind == u.kind && to != u.from {
			includes = append(includes, feInclude{u.from, to, u.line})
		}
	}

	// Scripts and stylesheets in a dynamically loaded directory, named by a
	// Go literal.
	type goInclude struct {
		pkg string
		to  *feFile
		pos token.Position
	}
	var goIncludes []goInclude
	for _, f := range sortedFiles(files) {
		if f.kind == feTemplate || !dynamicDirs[f.kind][filepath.Dir(f.abs)] {
			continue
		}
		base := filepath.Base(f.abs)
		for _, name := range []string{base, strings.TrimSuffix(base, filepath.Ext(base))} {
			for _, pkg := range sortedKeys(s.fe.names[name]) {
				goIncludes = append(goIncludes, goInclude{pkg, f, s.fe.names[name][pkg]})
			}
		}
	}

	// Scripts by directory and version-free name, for loaders that name them.
	siblings := map[string]map[string]*feFile{}
	for _, f := range sortedFiles(files) {
		if f.kind != feScript {
			continue
		}
		dir := filepath.Dir(f.abs)
		if siblings[dir] == nil {
			siblings[dir] = map[string]*feFile{}
		}
		name := normalizeAssetPath(filepath.Base(f.abs))
		siblings[dir][strings.TrimSuffix(name, filepath.Ext(name))] = f
	}
	for _, f := range sortedFiles(files) {
		switch f.kind {
		case feScript:
			for _, m := range jsImportRe.FindAllStringSubmatchIndex(f.text, -1) {
				target := filepath.Join(filepath.Dir(f.abs), filepath.FromSlash(f.text[m[2]:m[3]]))
				if to := files[target]; to != nil && to.kind == feScript && to != f {
					includes = append(includes, feInclude{f, to, lineAt(f.text, m[0])})
				}
			}
			// Absolute paths to other scripts or stylesheets
			// (importWithRetry('/assets/js/b.js?v=' + v)).
			for _, m := range assetPathLitRe.FindAllStringSubmatchIndex(f.text, -1) {
				ref := f.text[m[2]:m[3]]
				kind := feScript
				if strings.EqualFold(path.Ext(ref), ".css") {
					kind = feStylesheet
				}
				if to := files[s.resolveURL(ref, candidates[kind])]; to != nil && to.kind == kind && to != f {
					includes = append(includes, feInclude{f, to, lineAt(f.text, m[0])})
				}
			}
			for _, lit := range jsStrings(f.text) {
				if !jsNameLitRe.MatchString(lit.val) {
					continue
				}
				if to := siblings[filepath.Dir(f.abs)][lit.val]; to != nil && to != f {
					includes = append(includes, feInclude{f, to, lit.line})
				}
			}
		case feStylesheet:
			for _, m := range cssImportRe.FindAllStringSubmatchIndex(f.text, -1) {
				ref := f.text[m[2]:m[3]]
				target := s.resolveURL(ref, candidates[feStylesheet])
				if !strings.HasPrefix(ref, "/") {
					target = filepath.Join(filepath.Dir(f.abs), filepath.FromSlash(strings.SplitN(ref, "?", 2)[0]))
				}
				if to := files[target]; to != nil && to.kind == feStylesheet && to != f {
					includes = append(includes, feInclude{f, to, lineAt(f.text, m[0])})
				}
			}
		}
	}

	// Who includes each file directly, and the routes each script calls.
	sources := map[*feFile][]feSource{}
	for _, pkg := range sortedKeys(s.fe.renders) {
		for abs := range s.fe.renders[pkg] {
			if f := files[abs]; f != nil {
				sources[f] = append(sources[f], feSource{pkg: pkg})
				f.owners[pkg] = true
			}
		}
	}
	for _, gi := range goIncludes {
		sources[gi.to] = append(sources[gi.to], feSource{pkg: gi.pkg})
		gi.to.owners[gi.pkg] = true
	}
	for _, inc := range includes {
		sources[inc.to] = append(sources[inc.to], feSource{file: inc.from})
	}
	calls := map[*feFile][]routeCall{}
	for _, f := range sortedFiles(files) {
		if f.kind == feScript {
			calls[f] = s.scriptRouteCalls(f)
		}
	}

	// Every unit whose pages load a file: down the include graph.
	for changed := true; changed; {
		changed = false
		for _, inc := range includes {
			for o := range inc.from.owners {
				if !inc.to.owners[o] {
					inc.to.owners[o] = true
					changed = true
				}
			}
		}
	}
	owner := s.primaryOwners(sortedFiles(files), sources, calls)

	// Nodes, with ids that survive version-bump renames.
	taken := map[string]bool{}
	for _, f := range sortedFiles(files) {
		k := feKinds[f.kind]
		f.id = k.prefix + normalizeAssetPath(f.rel)
		if taken[f.id] {
			f.id = k.prefix + f.rel // two versions of one file side by side
		}
		taken[f.id] = true
		n := s.b.node(f.id, k.kind, path.Base(strings.TrimPrefix(f.id, k.prefix)))
		n.Path = f.rel
		n.ParentId = owner[f]
		n.Metrics = &pb.CodeMetrics{Files: 1, Loc: int32(countLines(f.text))}
		if acc := churn[normalizeAssetPath(f.rel)]; acc != nil {
			n.Metrics.Churn = int32(acc.lines)
			n.Metrics.Commits = int32(len(acc.commits))
			n.Metrics.Authors = int32(len(acc.authors))
		}
		n.Attrs = map[string]string{"hash": contentHash(f.text)}
		units := map[string]bool{}
		for pkg := range f.owners {
			units[s.unitID(pkg)] = true
		}
		if len(units) > 0 {
			n.Attrs["used_by"] = strings.Join(sortedKeys(units), ",")
		}
	}

	for _, pkg := range sortedKeys(s.fe.renders) {
		for abs, pos := range s.fe.renders[pkg] {
			if f := files[abs]; f != nil {
				s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_RENDERS, pkg, f.id, pos, "")
			}
		}
	}
	for _, inc := range includes {
		s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_INCLUDES, inc.from.id, inc.to.id, token.Position{Filename: inc.from.abs, Line: inc.line}, "")
	}
	for _, gi := range goIncludes {
		s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_INCLUDES, gi.pkg, gi.to.id, gi.pos, "")
	}
	for _, f := range sortedFiles(files) {
		for _, c := range calls[f] {
			s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_CALLS_ROUTE, f.id, c.route.PackageId,
				token.Position{Filename: f.abs, Line: c.line}, c.route.Method+" "+c.route.Pattern)
		}
	}
}

// primaryOwners picks each file's owner: the unit most tied to it. A file
// scores two points for each unit whose package renders or names it, or whose
// owned file includes it; a script also scores one point for each unit whose
// routes it calls (being loaded by a page is the stronger tie, calls break
// ties). The highest score wins; a tie leaves the file shared.
// Owners of includers are only known once those are decided, so it iterates
// to a fixed point.
func (s *scanner) primaryOwners(files []*feFile, sources map[*feFile][]feSource, calls map[*feFile][]routeCall) map[*feFile]string {
	owner := map[*feFile]string{}
	for round := 0; round < 20; round++ {
		changed := false
		for _, f := range files {
			score := map[string]int{}
			for _, src := range sources[f] {
				if src.pkg != "" {
					score[s.unitID(src.pkg)] += 2
				} else if u := owner[src.file]; u != "" {
					score[u] += 2
				}
			}
			called := map[string]bool{}
			for _, c := range calls[f] {
				called[s.unitID(c.route.PackageId)] = true
			}
			for u := range called {
				score[u]++
			}
			best, bestScore, tie := "", 0, false
			for _, u := range sortedKeys(score) {
				switch {
				case score[u] > bestScore:
					best, bestScore, tie = u, score[u], false
				case score[u] == bestScore:
					tie = true
				}
			}
			if tie {
				best = ""
			}
			if owner[f] != best {
				owner[f] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return owner
}

// contentHash identifies a file's content for diffs. Version strings are
// dropped first, so a release bump that rewrites "3.3.2" everywhere does not
// read as every file edited.
func contentHash(text string) string {
	sum := sha256.Sum256([]byte(semverRe.ReplaceAllString(text, "")))
	return hex.EncodeToString(sum[:8])
}

// unitID is the node a rendering unit stands for: a container is itself, a
// package its component.
func (s *scanner) unitID(pkg string) string {
	if c := s.component[pkg]; c != "" {
		return c
	}
	return pkg
}

// topDir is the first directory of abs under the module root ("templates",
// "assets"), or abs itself for a file at the root.
func (s *scanner) topDir(abs string) string {
	r, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return abs
	}
	first, _, found := strings.Cut(filepath.ToSlash(r), "/")
	if !found {
		return abs
	}
	return filepath.Join(s.dir, first)
}

// walkFiles visits the files with the given extensions under roots (a root
// that is a file is visited alone), skipping dependency and VCS dirs.
func (s *scanner) walkFiles(roots map[string]bool, exts map[string]bool, visit func(abs string)) {
	keys := make([]string, 0, len(roots))
	for r := range roots {
		keys = append(keys, r)
	}
	sort.Strings(keys)
	for _, root := range keys {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") && p != root {
					return filepath.SkipDir
				}
				return nil
			}
			if exts[strings.ToLower(filepath.Ext(p))] {
				visit(p)
			}
			return nil
		})
	}
}

// resolveDir maps a URL directory prefix ("/assets/js/") to a directory in
// the module, the same way resolveURL maps files.
func (s *scanner) resolveDir(prefix string) string {
	rel := strings.Trim(path.Clean("/"+prefix), "/")
	if rel == "" {
		return ""
	}
	segs := strings.Split(rel, "/")
	for i := range segs {
		dir := filepath.Join(s.dir, filepath.FromSlash(strings.Join(segs[i:], "/")))
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return ""
}

// resolveURL maps a script URL to a file: the module-relative path itself,
// else the one candidate whose path ends with the URL's (a static dir served
// under another prefix). Query strings and fragments are ignored.
func (s *scanner) resolveURL(url string, candidates []string) string {
	url, _, _ = strings.Cut(url, "?")
	url, _, _ = strings.Cut(url, "#")
	url = strings.TrimPrefix(path.Clean("/"+url), "/")
	if url == "" || url == "." {
		return ""
	}
	direct := filepath.Join(s.dir, filepath.FromSlash(url))
	if st, err := os.Stat(direct); err == nil && !st.IsDir() {
		return direct
	}
	// Drop leading segments until exactly one candidate ends with the rest.
	segs := strings.Split(url, "/")
	for i := 0; i < len(segs); i++ {
		suffix := "/" + strings.Join(segs[i:], "/")
		var hit string
		n := 0
		for _, c := range candidates {
			if strings.HasSuffix(filepath.ToSlash(c), suffix) {
				hit = c
				n++
			}
		}
		if n == 1 {
			return hit
		}
		if n > 1 {
			return ""
		}
	}
	return ""
}

// scriptRouteCalls lists the routes a script's path strings match.
func (s *scanner) scriptRouteCalls(f *feFile) []routeCall {
	var out []routeCall
	for _, c := range jsPathStrings(f.text) {
		for _, r := range s.b.routes {
			if r.PackageId != "" && routeMatches(r.Pattern, c.path) {
				out = append(out, routeCall{r, c.line})
			}
		}
	}
	return out
}

type jsPath struct {
	path string
	line int
}

// dynamicSeg stands for a path segment only known at run time.
const dynamicSeg = "\x00"

// jsPathStrings finds the URL paths a script builds: string literals starting
// with "/", and paths in href/action/src attributes of HTML it builds as
// strings ('<a href="/tdr/' + id), joined across `+ expr +` concatenation,
// with ${…} and the concatenated expressions turned into dynamic segments.
func jsPathStrings(src string) []jsPath {
	toks := jsStrings(src)
	var out []jsPath
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		type start struct {
			path string
			open bool // runs to the end of the literal, so concatenation continues it
		}
		var starts []start
		if strings.HasPrefix(t.val, "/") && !strings.HasPrefix(t.val, "//") {
			starts = append(starts, start{t.val, true})
		} else {
			for _, m := range attrPathRe.FindAllStringSubmatchIndex(t.val, -1) {
				if p := t.val[m[2]:m[3]]; !strings.HasPrefix(p, "//") {
					starts = append(starts, start{p, m[3] == len(t.val)})
				}
			}
		}
		next := i
		for _, st := range starts {
			p := st.path
			if st.open {
				end, j := t.end, i
				for {
					m := concatRe.FindStringSubmatchIndex(src[end:])
					if m == nil {
						break
					}
					p += dynamicSeg
					if j+1 < len(toks) && toks[j+1].start == end+m[1] {
						j++
						// The next literal continues the path up to where the
						// attribute value ends.
						rest := toks[j].val
						cut := strings.IndexAny(rest, "\"' >")
						if !strings.HasPrefix(t.val, "/") && cut >= 0 {
							p += rest[:cut]
							break
						}
						p += rest
						end = toks[j].end
						continue
					}
					break
				}
				next = max(next, j)
			}
			out = append(out, jsPath{path: p, line: t.line})
		}
		i = next
	}
	return out
}

// concatRe matches `+ expr` after a string, up to the next string or the end
// of the expression; the expression must be simple (no strings, calls with
// strings, or statement breaks).
var concatRe = regexp.MustCompile(`^\s*\+\s*[\w$.\[\]()]+\s*(?:\+\s*)?`)

type jsStr struct {
	val        string
	start, end int // byte offsets of the literal, quotes included
	line       int
}

// jsStrings tokenizes the string and template literals of a script, skipping
// comments. ${…} inside a template literal becomes a dynamic segment.
func jsStrings(src string) []jsStr {
	var out []jsStr
	line := 1
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return out
			}
			line += strings.Count(src[i:i+2+j], "\n")
			i += j + 3
		case c == '\'' || c == '"' || c == '`':
			start, startLine := i, line
			var b strings.Builder
			for i++; i < len(src) && src[i] != c; i++ {
				switch {
				case src[i] == '\\' && i+1 < len(src):
					i++
					b.WriteByte(src[i])
				case c != '`' && src[i] == '\n':
					goto unterminated
				case c == '`' && src[i] == '$' && i+1 < len(src) && src[i+1] == '{':
					depth := 0
					for ; i < len(src); i++ {
						if src[i] == '{' {
							depth++
						} else if src[i] == '}' {
							if depth--; depth == 0 {
								break
							}
						}
					}
					b.WriteString(dynamicSeg)
				default:
					if src[i] == '\n' {
						line++
					}
					b.WriteByte(src[i])
				}
			}
			out = append(out, jsStr{val: b.String(), start: start, end: min(i+1, len(src)), line: startLine})
			continue
		unterminated:
			line++
		}
	}
	return out
}

// routeMatches reports whether a path a script requests matches a Go route
// pattern. Route parameters ({id}, {id:[0-9]+}) match any one segment; a
// trailing * matches the rest. A dynamic segment in the script only matches a
// parameter, so "/fantasy/" + x does not claim "/fantasy/about".
func routeMatches(pattern, reqPath string) bool {
	reqPath, _, _ = strings.Cut(reqPath, "?")
	reqPath, _, _ = strings.Cut(reqPath, "#")
	if i := strings.Index(pattern, " "); i >= 0 { // net/http "GET /x"
		pattern = pattern[i+1:]
	}
	ps := splitPath(pattern)
	rs := splitPath(reqPath)
	if len(ps) == 0 || len(rs) == 0 {
		return false // "/" would match every page
	}
	if path.Ext(rs[len(rs)-1]) != "" && !strings.Contains(rs[len(rs)-1], dynamicSeg) {
		return false // a file, not an endpoint
	}
	for i, p := range ps {
		if p == "*" || strings.HasSuffix(p, "...}") {
			return i < len(rs)
		}
		if i >= len(rs) {
			return false
		}
		param := strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}")
		dynamic := strings.Contains(rs[i], dynamicSeg)
		switch {
		case param:
		case dynamic:
			return false
		case p != rs[i]:
			return false
		}
	}
	return len(rs) == len(ps)
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// normalizeAssetPath drops version suffixes from every path segment, so
// assets/js/fsv3_v3.3.2.js and assets/js/baq_v3.3.2/main.js keep their ids
// across a version bump.
func normalizeAssetPath(rel string) string {
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		if m := versionSuffixRe.FindStringSubmatch(seg); m != nil {
			segs[i] = m[1] + m[2]
		}
	}
	return strings.Join(segs, "/")
}

// isVendorAsset reports third-party or build output: minified files and
// bundler output.
func isVendorAsset(abs, text string) bool {
	base := strings.ToLower(filepath.Base(abs))
	if strings.Contains(base, ".min.") {
		return true
	}
	head := text
	if len(head) > 4096 {
		head = head[:4096]
	}
	if bundlerPreludeRe.MatchString(head) {
		return true
	}
	// Minified without the .min name: few, very long lines.
	lines := strings.Count(text, "\n") + 1
	return lines <= 20 && len(text)/lines > 1000
}

func countLines(text string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

func lineAt(text string, off int) int { return strings.Count(text[:off], "\n") + 1 }

func sortedFiles(m map[string]*feFile) []*feFile {
	out := make([]*feFile, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].abs < out[j].abs })
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
