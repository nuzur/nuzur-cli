package codescan

import (
	"go/ast"
	"go/token"
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
//     marks its directory dynamic: a Go string literal naming a file there
//     ("drag_v3.3.2") makes the package include that script;
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
	// A quoted absolute path to a script anywhere in a template or script.
	jsPathLitRe = regexp.MustCompile("[\"'`](/[^\"'`\\s?#{}]+\\.m?js)(?:[?#][^\"'`]*)?[\"'`]")
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
	if goNameLitRe.MatchString(v) {
		if s.fe.names[v] == nil {
			s.fe.names[v] = map[string]token.Position{}
		}
		if _, ok := s.fe.names[v][unit]; !ok {
			s.fe.names[v][unit] = s.fset.Position(lit.Pos())
		}
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

func (s *scanner) inModule(abs string) bool {
	r, err := filepath.Rel(s.dir, abs)
	return err == nil && !strings.HasPrefix(r, "..") && !strings.Contains(filepath.ToSlash(r), "node_modules/")
}

type feFile struct {
	abs, rel, id string
	tpl          bool
	text         string
	owners       map[string]bool // units (components/containers) that use it
}

// extractFrontend turns the recorded renders into template and script nodes,
// their include graph, and the routes scripts call. It runs after the Go
// walk, so routes are known.
func (s *scanner) extractFrontend(churn map[string]*churnAcc) {
	if len(s.fe.renders) == 0 {
		return
	}
	files := map[string]*feFile{} // abs -> file
	add := func(abs string, tpl bool) *feFile {
		if f, ok := files[abs]; ok {
			return f
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			return nil
		}
		text := string(raw)
		if !tpl && isVendorScript(abs, text) {
			return nil
		}
		f := &feFile{abs: abs, rel: s.b.rel(abs), tpl: tpl, text: text, owners: map[string]bool{}}
		files[abs] = f
		return f
	}

	// Rendered templates, then every template beside them.
	tplRoots := map[string]bool{}
	for _, tpls := range s.fe.renders {
		for abs := range tpls {
			add(abs, true)
			tplRoots[s.topDir(abs)] = true
		}
	}
	s.walkFiles(tplRoots, templateExts, func(abs string) { add(abs, true) })

	// Template names that were not paths on their own.
	var tplPaths []string
	for abs, f := range files {
		if f.tpl {
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

	type include struct {
		from, to *feFile
		line     int
	}
	var includes []include
	jsRoots := map[string]bool{}
	dynamicDirs := map[string]bool{}
	var scriptURLs []struct {
		from *feFile
		url  string
		line int
	}
	for _, f := range sortedFiles(files) {
		for _, m := range tplIncludeRe.FindAllStringSubmatchIndex(f.text, -1) {
			name := f.text[m[2]:m[3]]
			if defs := byName[name]; len(defs) == 1 && defs[0] != f {
				includes = append(includes, include{f, defs[0], lineAt(f.text, m[0])})
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
					dynamicDirs[dir] = true
				}
				continue
			}
			scriptURLs = append(scriptURLs, struct {
				from *feFile
				url  string
				line int
			}{f, url, lineAt(f.text, m[0])})
		}
		for _, m := range jsPathLitRe.FindAllStringSubmatchIndex(f.text, -1) {
			scriptURLs = append(scriptURLs, struct {
				from *feFile
				url  string
				line int
			}{f, f.text[m[2]:m[3]], lineAt(f.text, m[0])})
		}
	}

	// Scripts: every JS file under the directories the loaded ones live in.
	var candidates []string
	collect := func(roots map[string]bool) {
		s.walkFiles(roots, scriptExts, func(abs string) { candidates = append(candidates, abs) })
	}
	for _, su := range scriptURLs {
		if abs := s.resolveURL(su.url, nil); abs != "" {
			jsRoots[s.topDir(abs)] = true
		}
	}
	collect(jsRoots)
	sort.Strings(candidates)
	for _, su := range scriptURLs {
		abs := s.resolveURL(su.url, candidates)
		if abs == "" {
			continue
		}
		if to := add(abs, false); to != nil {
			includes = append(includes, include{su.from, to, su.line})
		}
	}
	for dir := range dynamicDirs {
		jsRoots[s.topDir(dir)] = true
	}
	candidates = candidates[:0]
	collect(jsRoots)
	sort.Strings(candidates)
	for _, abs := range candidates {
		add(abs, false)
	}

	// Scripts in a dynamically loaded directory, named by a Go literal.
	type goInclude struct {
		unit string
		to   *feFile
		pos  token.Position
	}
	var goIncludes []goInclude
	for _, f := range sortedFiles(files) {
		if f.tpl || !dynamicDirs[filepath.Dir(f.abs)] {
			continue
		}
		base := filepath.Base(f.abs)
		for _, name := range []string{base, strings.TrimSuffix(base, filepath.Ext(base))} {
			for unit, pos := range s.fe.names[name] {
				goIncludes = append(goIncludes, goInclude{unit, f, pos})
				f.owners[unit] = true
			}
		}
	}
	// Scripts by directory and version-free name, for loaders that name them.
	siblings := map[string]map[string]*feFile{}
	for _, f := range sortedFiles(files) {
		if f.tpl {
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
		if f.tpl {
			continue
		}
		for _, m := range jsImportRe.FindAllStringSubmatchIndex(f.text, -1) {
			target := filepath.Join(filepath.Dir(f.abs), filepath.FromSlash(f.text[m[2]:m[3]]))
			if to := files[target]; to != nil && !to.tpl && to != f {
				includes = append(includes, include{f, to, lineAt(f.text, m[0])})
			}
		}
		for _, m := range jsPathLitRe.FindAllStringSubmatchIndex(f.text, -1) {
			if to := files[s.resolveURL(f.text[m[2]:m[3]], candidates)]; to != nil && !to.tpl && to != f {
				includes = append(includes, include{f, to, lineAt(f.text, m[0])})
			}
		}
		for _, lit := range jsStrings(f.text) {
			if !jsNameLitRe.MatchString(lit.val) {
				continue
			}
			if to := siblings[filepath.Dir(f.abs)][lit.val]; to != nil && to != f {
				includes = append(includes, include{f, to, lit.line})
			}
		}
	}

	// Ownership flows from the rendering unit down the include graph.
	for unit, tpls := range s.fe.renders {
		for abs := range tpls {
			if f := files[abs]; f != nil {
				f.owners[unit] = true
			}
		}
	}
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

	// Nodes, with ids that survive version-bump renames.
	taken := map[string]bool{}
	for _, f := range sortedFiles(files) {
		prefix := "js:"
		kind := pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT
		if f.tpl {
			prefix, kind = "tpl:", pb.CodeNodeKind_CODE_NODE_KIND_TEMPLATE
		}
		f.id = prefix + normalizeAssetPath(f.rel)
		if taken[f.id] {
			f.id = prefix + f.rel // two versions of one file side by side
		}
		taken[f.id] = true
		n := s.b.node(f.id, kind, path.Base(strings.TrimPrefix(f.id, prefix)))
		n.Path = f.rel
		n.Metrics = &pb.CodeMetrics{Files: 1, Loc: int32(countLines(f.text))}
		if acc := churn[normalizeAssetPath(f.rel)]; acc != nil {
			n.Metrics.Churn = int32(acc.lines)
			n.Metrics.Commits = int32(len(acc.commits))
			n.Metrics.Authors = int32(len(acc.authors))
		}
		if len(f.owners) == 1 {
			for o := range f.owners {
				n.ParentId = s.unitID(o)
			}
		}
	}

	for unit, tpls := range s.fe.renders {
		for abs, pos := range tpls {
			if f := files[abs]; f != nil {
				s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_RENDERS, unit, f.id, pos, "")
			}
		}
	}
	for _, inc := range includes {
		s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_INCLUDES, inc.from.id, inc.to.id, token.Position{Filename: inc.from.abs, Line: inc.line}, "")
	}
	for _, gi := range goIncludes {
		s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_INCLUDES, gi.unit, gi.to.id, gi.pos, "")
	}
	for _, f := range sortedFiles(files) {
		if !f.tpl {
			s.scriptRouteCalls(f)
		}
	}
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

// scriptRouteCalls links a script to the packages handling the routes its
// path strings match.
func (s *scanner) scriptRouteCalls(f *feFile) {
	if len(s.b.routes) == 0 {
		return
	}
	for _, c := range jsPathStrings(f.text) {
		for _, r := range s.b.routes {
			if r.PackageId == "" || !routeMatches(r.Pattern, c.path) {
				continue
			}
			s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_CALLS_ROUTE, f.id, r.PackageId,
				token.Position{Filename: f.abs, Line: c.line}, r.Method+" "+r.Pattern)
		}
	}
}

type jsPath struct {
	path string
	line int
}

// dynamicSeg stands for a path segment only known at run time.
const dynamicSeg = "\x00"

// jsPathStrings finds the URL paths a script builds: string literals starting
// with "/", joined across `+ expr +` concatenation, with ${…} and the
// concatenated expressions turned into dynamic segments.
func jsPathStrings(src string) []jsPath {
	toks := jsStrings(src)
	var out []jsPath
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if !strings.HasPrefix(t.val, "/") || strings.HasPrefix(t.val, "//") {
			continue
		}
		p := t.val
		end := t.end
		for {
			rest := src[end:]
			m := concatRe.FindStringSubmatchIndex(rest)
			if m == nil {
				break
			}
			p += dynamicSeg
			if i+1 < len(toks) && toks[i+1].start == end+m[1] {
				i++
				p += toks[i].val
				end = toks[i].end
				continue
			}
			break
		}
		out = append(out, jsPath{path: p, line: t.line})
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

// isVendorScript reports third-party or build output: minified files and
// bundler output.
func isVendorScript(abs, text string) bool {
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
