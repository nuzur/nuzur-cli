package codescan

import (
	"go/ast"
	"go/types"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

const fxPath = "go.uber.org/fx"

// fxProvidedPackages returns the hand-written packages whose constructors are
// passed to fx.Provide anywhere in the module. In an fx app those are the
// application's components (dragium's modules/modules.go lists all of them).
// It also records the packages making those calls: they are composition
// roots, which depend on everything by design.
func (s *scanner) fxProvidedPackages() map[string]bool {
	out := map[string]bool{}
	s.fxRoots = map[string]bool{}
	for _, p := range s.handPkgs() {
		if p.TypesInfo == nil {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn := calledFunc(p.TypesInfo, call)
				if fn == nil || funcPkgPath(fn) != fxPath || fn.Name() != "Provide" {
					return true
				}
				s.fxRoots[p.PkgPath] = true
				for _, arg := range call.Args {
					if path := s.constructorPkg(p.TypesInfo, arg); path != "" && s.hand[path] != nil {
						out[path] = true
					}
				}
				return true
			})
		}
	}
	return out
}

// constructorPkg resolves an fx.Provide argument to the package declaring it,
// looking through fx.Annotate(ctor, ...).
func (s *scanner) constructorPkg(info *types.Info, arg ast.Expr) string {
	switch x := ast.Unparen(arg).(type) {
	case *ast.CallExpr:
		if fn := calledFunc(info, x); fn != nil && funcPkgPath(fn) == fxPath && fn.Name() == "Annotate" && len(x.Args) > 0 {
			return s.constructorPkg(info, x.Args[0])
		}
	case *ast.Ident:
		if fn, ok := info.Uses[x].(*types.Func); ok {
			return funcPkgPath(fn)
		}
	case *ast.SelectorExpr:
		if fn, ok := info.Uses[x.Sel].(*types.Func); ok {
			return funcPkgPath(fn)
		}
	}
	return ""
}

// extractFx emits an injects edge from every package declaring an fx.In
// parameter struct to each in-module package whose type it asks for. This is
// the dependency graph fx actually wires, which is often more telling than
// imports (an import can be for a type only).
func (s *scanner) extractFx() {
	for _, p := range s.handPkgs() {
		if p.TypesInfo == nil {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				st, ok := n.(*ast.StructType)
				if !ok || !embedsFxIn(p.TypesInfo, st) {
					return true
				}
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 {
						continue // the embedded fx.In itself
					}
					target := namedPkg(p.TypesInfo.TypeOf(field.Type))
					if target == "" || target == p.PkgPath {
						continue
					}
					pos := s.fset.Position(field.Pos())
					if s.hand[target] != nil {
						s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_INJECTS, p.PkgPath, target, pos, "")
					}
				}
				return true
			})
		}
	}
}

func embedsFxIn(info *types.Info, st *ast.StructType) bool {
	for _, field := range st.Fields.List {
		if len(field.Names) != 0 {
			continue
		}
		// fx.In is an alias of dig.In, so the resolved type lives in dig.
		if named, ok := types.Unalias(info.TypeOf(field.Type)).(*types.Named); ok {
			obj := named.Obj()
			if obj.Pkg() != nil && (obj.Pkg().Path() == fxPath || obj.Pkg().Path() == "go.uber.org/dig") && obj.Name() == "In" {
				return true
			}
		}
	}
	return false
}

// namedPkg returns the package of the named type behind t, looking through
// pointers, slices and maps.
func namedPkg(t types.Type) string {
	for t != nil {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Slice:
			t = x.Elem()
		case *types.Map:
			t = x.Elem()
		case *types.Named:
			if x.Obj().Pkg() == nil {
				return ""
			}
			return x.Obj().Pkg().Path()
		default:
			return ""
		}
	}
	return ""
}
