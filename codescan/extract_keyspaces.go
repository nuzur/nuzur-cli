package codescan

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"golang.org/x/tools/go/packages"
)

// Key-value method verbs, matched as name prefixes so wrapper methods like
// SetWithTime or HSetJSONWithTTL classify the same as the raw client's.
var (
	kvWritePrefixes = []string{"Set", "HSet", "HMSet", "MSet", "Del", "HDel", "Incr", "Decr", "HIncr", "Expire",
		"Persist", "LPush", "RPush", "LPop", "RPop", "SAdd", "SRem", "ZAdd", "ZRem", "ZIncr", "Add", "Publish",
		"Append", "Rename", "Put"}
	kvReadPrefixes = []string{"Get", "HGet", "MGet", "Exists", "HExists", "Keys", "Scan", "SMembers", "SIsMember",
		"LRange", "LLen", "ZRange", "ZRevRange", "ZScore", "ZRank", "ZCard", "TTL", "Type", "Subscribe", "List", "Fetch"}
)

func kvMode(method string) string {
	for _, p := range kvWritePrefixes {
		if hasWordPrefix(method, p) {
			return "write"
		}
	}
	for _, p := range kvReadPrefixes {
		if hasWordPrefix(method, p) {
			return "read"
		}
	}
	return ""
}

func hasWordPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	rest := s[len(prefix):]
	return rest == "" || (rest[0] >= 'A' && rest[0] <= 'Z')
}

// redisWrapper reports whether a hand-written package wraps a Redis client:
// it imports a Redis SDK itself. Calls through such wrappers carry the keys.
func (s *scanner) redisWrapper(path string) bool {
	p := s.hand[path]
	if p == nil {
		return false
	}
	for imp := range p.Imports {
		if isRedisSDK(imp) {
			return true
		}
	}
	return false
}

// keyspaceCall records which Redis key prefixes a package reads and writes.
// It is a heuristic (keys built from variables are invisible) and the UI
// labels keyspaces as such.
func (s *scanner) keyspaceCall(p *packages.Package, call *ast.CallExpr) {
	fn := calledFunc(p.TypesInfo, call)
	if fn == nil {
		return
	}
	named := recvNamed(fn)
	if named == nil || named.Obj().Pkg() == nil {
		return
	}
	recvPkg := named.Obj().Pkg().Path()
	if recvPkg == p.PkgPath || !(isRedisSDK(recvPkg) || s.redisWrapper(recvPkg)) {
		return
	}
	mode := kvMode(fn.Name())
	if mode == "" {
		return
	}
	var keyArg ast.Expr
	for _, a := range call.Args {
		if b, ok := p.TypesInfo.TypeOf(a).Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
			keyArg = a
			break
		}
	}
	if keyArg == nil {
		return
	}
	prefix, ok := keyPrefix(p.TypesInfo, keyArg)
	if !ok {
		return
	}
	s.b.node("ext:redis", pb.CodeNodeKind_CODE_NODE_KIND_EXTERNAL_SYSTEM, "redis")
	if s.b.nodes["ext:redis"].Attrs == nil {
		s.b.nodes["ext:redis"].Attrs = map[string]string{"via": "sdk"}
	}
	id := "ks:redis:" + prefix
	n := s.b.node(id, pb.CodeNodeKind_CODE_NODE_KIND_KEYSPACE, prefix)
	n.ParentId = "ext:redis"
	kind := pb.CodeEdgeKind_CODE_EDGE_KIND_READS_KEYSPACE
	if mode == "write" {
		kind = pb.CodeEdgeKind_CODE_EDGE_KIND_WRITES_KEYSPACE
	}
	s.b.edge(kind, p.PkgPath, id, s.fset.Position(call.Pos()), "")
}

// keyPrefix evaluates the static part of a key expression and trims it to a
// keyspace: "fv3:%s" -> "fv3:", "tdr:league:"+id+":members" -> "tdr:league:".
// Fully static keys are kept whole.
func keyPrefix(info *types.Info, e ast.Expr) (string, bool) {
	prefix, complete, ok := evalKey(info, e)
	if !ok {
		return "", false
	}
	if i := strings.IndexAny(prefix, "%*{[?"); i >= 0 {
		prefix, complete = prefix[:i], false
	}
	if !complete {
		if i := strings.LastIndex(prefix, ":"); i >= 0 {
			prefix = prefix[:i+1]
		}
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || prefix == ":" {
		return "", false
	}
	return prefix, true
}

// evalKey returns the leading static text of a string expression and whether
// that text is the whole value.
func evalKey(info *types.Info, e ast.Expr) (string, bool, bool) {
	if v, ok := stringConst(info, e); ok {
		return v, true, true
	}
	switch x := ast.Unparen(e).(type) {
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false, false
		}
		left, complete, ok := evalKey(info, x.X)
		if !ok {
			return "", false, false
		}
		if !complete {
			return left, false, true
		}
		right, rc, rok := evalKey(info, x.Y)
		if !rok {
			return left, false, true
		}
		return left + right, rc, true
	case *ast.CallExpr:
		fn := calledFunc(info, x)
		if fn != nil && funcPkgPath(fn) == "fmt" && fn.Name() == "Sprintf" && len(x.Args) > 0 {
			if format, ok := stringConst(info, x.Args[0]); ok {
				return format, !strings.Contains(format, "%"), true
			}
		}
	}
	return "", false, false
}
