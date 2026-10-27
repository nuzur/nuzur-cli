package codescan

import (
	"go/ast"
	"go/token"
	"go/types"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"golang.org/x/tools/go/packages"
)

// tickerCall records periodic work started with the time package. In a
// horizontally scaled deployment each replica runs its own copy, which is
// one of the findings the server derives from these facts.
func (s *scanner) tickerCall(p *packages.Package, fn *ast.FuncDecl, call *ast.CallExpr) {
	f := calledFunc(p.TypesInfo, call)
	if f == nil || funcPkgPath(f) != "time" {
		return
	}
	switch f.Name() {
	case "NewTicker", "Tick", "AfterFunc":
		s.addBackground(p, fn, pb.CodeBackgroundKind_CODE_BACKGROUND_KIND_TICKER, call.Pos())
	}
}

// lifecycleHook records fx.Hook{OnStart: ...}: work started with the app.
func (s *scanner) lifecycleHook(p *packages.Package, fn *ast.FuncDecl, lit *ast.CompositeLit) {
	named, ok := p.TypesInfo.TypeOf(lit).(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != fxPath || named.Obj().Name() != "Hook" {
		return
	}
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "OnStart" {
				s.addBackground(p, fn, pb.CodeBackgroundKind_CODE_BACKGROUND_KIND_LIFECYCLE_HOOK, lit.Pos())
				return
			}
		}
	}
}

func (s *scanner) addBackground(p *packages.Package, fn *ast.FuncDecl, kind pb.CodeBackgroundKind, pos token.Pos) {
	s.b.background = append(s.b.background, &pb.CodeBackgroundTask{
		Kind:      kind,
		PackageId: p.PkgPath,
		Function:  declName(fn),
		Evidence:  s.b.evidence(s.fset.Position(pos)),
	})
}

// declName renders Recv.Method or Func for a declaration.
func declName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if idx, ok := t.(*ast.IndexExpr); ok {
		t = idx.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
