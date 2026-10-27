package codescan

import (
	"go/ast"
	"go/types"
	"regexp"
	"strings"
	"unicode"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"golang.org/x/tools/go/packages"
)

// coreModuleRe matches a go-code-gen core module package: one per entity.
var coreModuleRe = regexp.MustCompile(`/core/module/([a-z0-9_]+)$`)

var opPrefixes = []struct{ prefix, op string }{
	{"List", "list"}, {"Count", "list"}, {"Search", "list"},
	{"Fetch", "get"}, {"Get", "get"}, {"Find", "get"},
	{"Create", "create"}, {"Insert", "create"},
	{"Update", "update"}, {"Upsert", "update"}, {"Patch", "update"},
	{"Delete", "delete"}, {"Remove", "delete"},
}

// splitOp splits a generated method name into its operation and the rest:
// ListSeason -> (list, Season). The prefix must end at a word boundary.
func splitOp(name string) (op, rest string) {
	for _, p := range opPrefixes {
		if !strings.HasPrefix(name, p.prefix) {
			continue
		}
		rest = name[len(p.prefix):]
		if rest == "" || unicode.IsUpper(rune(rest[0])) {
			return p.op, rest
		}
	}
	return "", name
}

// entityCall maps a call into nuzur-generated code to the entity it touches:
// either a gRPC client method (ListSeason on sfapi's SfapiClient) or a method
// of a go-code-gen core module (core/module/season.Implementation.List).
func (s *scanner) entityCall(p *packages.Package, call *ast.CallExpr) {
	fn := calledFunc(p.TypesInfo, call)
	if fn == nil {
		return
	}
	gen := s.generatedNodeOf(funcPkgPath(fn))
	if gen == "" {
		return
	}
	named := recvNamed(fn)
	if named == nil {
		return
	}
	pos := s.fset.Position(call.Pos())

	var entity, op string
	if m := coreModuleRe.FindStringSubmatch(funcPkgPath(fn)); m != nil {
		entity = m[1]
		if op, _ = splitOp(fn.Name()); op == "" {
			op = "other"
		}
	} else if _, isIface := named.Underlying().(*types.Interface); isIface && strings.HasSuffix(named.Obj().Name(), "Client") {
		var rest string
		op, rest = splitOp(fn.Name())
		if op == "" || rest == "" {
			s.b.unresolve("entity_method", fn.Name(), p.PkgPath, pos)
			return
		}
		if i := byBoundary(rest); i > 0 {
			rest = rest[:i] // FetchSeasonByUUID -> Season
		}
		entity = snakeCase(rest)
	} else {
		return
	}

	if len(s.known) > 0 && !s.known[entity] {
		s.b.unresolve("unknown_entity", entity, p.PkgPath, pos)
	}
	s.ensureGeneratedNode(gen)
	id := "entity:" + entity
	n := s.b.node(id, pb.CodeNodeKind_CODE_NODE_KIND_ENTITY_REF, entity)
	if n.ParentId == "" {
		n.ParentId = gen
	}
	s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_CALLS_ENTITY, p.PkgPath, id, pos, op)
}

// byBoundary finds "By" followed by an upper-case letter, the generated
// selector suffix (ByUUID, ByShowUUID).
func byBoundary(s string) int {
	for i := 1; i+2 < len(s); i++ {
		if s[i] == 'B' && s[i+1] == 'y' && unicode.IsUpper(rune(s[i+2])) {
			return i
		}
	}
	return -1
}

// snakeCase converts CamelCase back to a nuzur identifier: EpisodeHasDrag ->
// episode_has_drag, DragShowUUID -> drag_show_uuid.
func snakeCase(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) {
			prevLower := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			nextLower := i > 0 && i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1])
			if prevLower || nextLower {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
