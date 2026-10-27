package codescan

import (
	"go/ast"
	"go/types"
	"path"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"golang.org/x/tools/go/packages"
)

var chiMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE",
	"Head": "HEAD", "Options": "OPTIONS", "Connect": "CONNECT", "Trace": "TRACE",
	"Handle": "*", "HandleFunc": "*",
}

// isRouter reports whether a method belongs to a router we understand: go-chi
// (any major version) or net/http's ServeMux.
func isRouter(fn *types.Func) bool {
	named := recvNamed(fn)
	if named == nil || named.Obj().Pkg() == nil {
		return false
	}
	pkg := named.Obj().Pkg().Path()
	if strings.HasPrefix(pkg, "github.com/go-chi/chi") {
		return true
	}
	return pkg == "net/http" && named.Obj().Name() == "ServeMux"
}

// walkRoutes records route registrations under n, carrying the path prefix of
// enclosing chi Route() blocks. Routes registered by a helper function that
// receives the router lose the caller's prefix; that's the accepted limit of a
// static scan.
func (s *scanner) walkRoutes(p *packages.Package, n ast.Node, prefix string) {
	info := p.TypesInfo
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calledFunc(info, call)
		if fn == nil || !isRouter(fn) {
			return true
		}
		arg := func(i int) string {
			if i >= len(call.Args) {
				return ""
			}
			if v, ok := stringConst(info, call.Args[i]); ok {
				return v
			}
			return "<dynamic>"
		}
		switch name := fn.Name(); name {
		case "Route":
			if len(call.Args) == 2 {
				if lit, ok := call.Args[1].(*ast.FuncLit); ok {
					s.walkRoutes(p, lit.Body, joinPattern(prefix, arg(0)))
					return false
				}
			}
		case "Group":
			if len(call.Args) == 1 {
				if lit, ok := call.Args[0].(*ast.FuncLit); ok {
					s.walkRoutes(p, lit.Body, prefix)
					return false
				}
			}
		case "Mount":
			if len(call.Args) == 2 {
				s.addRoute(p, call, "MOUNT", joinPattern(prefix, arg(0)), call.Args[1])
			}
		case "Method", "MethodFunc":
			if len(call.Args) == 3 {
				s.addRoute(p, call, strings.ToUpper(arg(0)), joinPattern(prefix, arg(1)), call.Args[2])
			}
		default:
			method, ok := chiMethods[name]
			if !ok || len(call.Args) != 2 {
				return true
			}
			pattern := arg(0)
			// Go 1.22 ServeMux patterns carry the method: "GET /items/{id}".
			if m, rest, found := strings.Cut(pattern, " "); found && method == "*" && strings.HasPrefix(rest, "/") {
				method, pattern = m, rest
			}
			s.addRoute(p, call, method, joinPattern(prefix, pattern), call.Args[1])
		}
		return true
	})
}

func joinPattern(prefix, pattern string) string {
	if prefix == "" {
		return pattern
	}
	joined := path.Join(prefix, pattern)
	if strings.HasSuffix(pattern, "/") && !strings.HasSuffix(joined, "/") {
		joined += "/"
	}
	return joined
}

func (s *scanner) addRoute(p *packages.Package, call *ast.CallExpr, method, pattern string, handler ast.Expr) {
	name, pkg, wrappers := s.resolveHandler(p, handler)
	if pkg == "" {
		pkg = p.PkgPath
	}
	s.b.routes = append(s.b.routes, &pb.CodeRoute{
		Method:    method,
		Pattern:   pattern,
		Handler:   name,
		PackageId: pkg,
		Wrappers:  wrappers,
		Evidence:  s.b.evidence(s.fset.Position(call.Pos())),
	})
}

// resolveHandler unwraps middleware calls around a handler expression:
// AdminAuth.Require(h), gate(...)(h) and http.HandlerFunc(h) all lead to h.
func (s *scanner) resolveHandler(p *packages.Package, e ast.Expr) (name, pkg string, wrappers []string) {
	info := p.TypesInfo
	for depth := 0; depth < 8; depth++ {
		switch x := ast.Unparen(e).(type) {
		case *ast.CallExpr:
			if len(x.Args) == 0 {
				return types.ExprString(x), "", wrappers
			}
			if tv, ok := info.Types[x.Fun]; !ok || !tv.IsType() {
				wrappers = append(wrappers, shortExpr(x.Fun))
			}
			e = x.Args[len(x.Args)-1]
		case *ast.FuncLit:
			return "func literal", p.PkgPath, wrappers
		case *ast.SelectorExpr:
			if fn, ok := info.Uses[x.Sel].(*types.Func); ok {
				return funcDisplayName(fn), funcPkgPath(fn), wrappers
			}
			return shortExpr(x), "", wrappers
		case *ast.Ident:
			if fn, ok := info.Uses[x].(*types.Func); ok {
				return funcDisplayName(fn), funcPkgPath(fn), wrappers
			}
			return x.Name, "", wrappers
		default:
			return types.ExprString(e), "", wrappers
		}
	}
	return types.ExprString(e), "", wrappers
}

// shortExpr keeps the last two selector segments: params.AdminAuth.Require
// becomes AdminAuth.Require, which is what a reader recognizes.
func shortExpr(e ast.Expr) string {
	s := types.ExprString(e)
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}

// funcDisplayName renders pkg.Func or pkg.Recv.Method.
func funcDisplayName(fn *types.Func) string {
	pkgName := ""
	if fn.Pkg() != nil {
		pkgName = fn.Pkg().Name() + "."
	}
	if named := recvNamed(fn); named != nil {
		return pkgName + named.Obj().Name() + "." + fn.Name()
	}
	return pkgName + fn.Name()
}
