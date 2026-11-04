package layout

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"testing"

	"golang.org/x/tools/go/packages"
)

const problemPath = module + "platform/problem"

// TestNoSharedProblems: no package-level variable holds a problem. A
// *problem.Problem is a request's answer: problem.Write stamps the
// request's instance and trace id on what it sends, WithCause records the
// request's cause, and helpers rename its field errors (ledger's asItems).
// One kept in a variable and returned by every request that is refused for
// the same reason is shared by concurrent requests, possibly of different
// tenants: whatever one sets, the others see (review:T53f4). A refusal is
// a function returning a new problem each call. Refused here are a
// variable whose type holds a problem (a *Problem, a map or slice of them,
// a struct with one) and one whose initializer makes a problem outside a
// function literal (an error variable holding one). Not seen: an error
// variable initialized by a function returning error that makes a
// problem, or one assigned in init or later; none exists today, and
// problem.Write and WithCause work on a copy for such a value anyway.
func TestNoSharedProblems(t *testing.T) {
	for _, bad := range sharedProblems(t, "../../internal/...", "../../cmd/...") {
		t.Errorf("%s: got a package-level variable holding a *problem.Problem, want a function returning a new one each call", bad)
	}
}

// sharedProblems loads the packages the patterns name and lists, by
// position, every package-level variable that holds a problem.
func sharedProblems(t *testing.T, patterns ...string) []string {
	t.Helper()
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
		packages.NeedTypes | packages.NeedTypesInfo}, patterns...)
	if err != nil {
		t.Fatalf("load %v: %v", patterns, err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatalf("%v do not load", patterns)
	}
	var out []string
	for _, p := range pkgs {
		out = append(out, packageProblems(p)...)
	}
	slices.Sort(out)
	return out
}

// packageProblems lists the package-level variables of p that hold a
// problem, by their type or by their initializer.
func packageProblems(p *packages.Package) []string {
	var out []string
	pos := func(at token.Pos) string { return p.Fset.Position(at).String() }
	scope := p.Types.Scope()
	for _, name := range scope.Names() {
		if v, ok := scope.Lookup(name).(*types.Var); ok && holdsProblem(v.Type(), map[types.Type]bool{}) {
			out = append(out, pos(v.Pos())+" "+name)
		}
	}
	for _, in := range p.TypesInfo.InitOrder {
		if makesProblem(p.TypesInfo, in.Rhs) && !slices.ContainsFunc(in.Lhs, func(v *types.Var) bool {
			return holdsProblem(v.Type(), map[types.Type]bool{}) // listed above
		}) {
			out = append(out, pos(in.Lhs[0].Pos())+" "+in.Lhs[0].Name())
		}
	}
	return out
}

// isProblem reports t is *problem.Problem.
func isProblem(t types.Type) bool {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	n, ok := ptr.Elem().(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == problemPath && n.Obj().Name() == "Problem"
}

// holdsProblem reports a value of type t can hold a *problem.Problem
// outside an interface or a function: itself, or in an element, a key or
// a field.
func holdsProblem(t types.Type, seen map[types.Type]bool) bool {
	if isProblem(t) {
		return true
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return holdsProblem(u.Elem(), seen)
	case *types.Slice:
		return holdsProblem(u.Elem(), seen)
	case *types.Array:
		return holdsProblem(u.Elem(), seen)
	case *types.Chan:
		return holdsProblem(u.Elem(), seen)
	case *types.Map:
		return holdsProblem(u.Key(), seen) || holdsProblem(u.Elem(), seen)
	case *types.Struct:
		for f := range u.Fields() {
			if holdsProblem(f.Type(), seen) {
				return true
			}
		}
	}
	return false
}

// makesProblem reports an initializer yields a *problem.Problem other
// than inside a function literal, which makes a new one each call.
func makesProblem(info *types.Info, e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok || found {
			return false
		}
		if x, ok := n.(ast.Expr); ok {
			tv, typed := info.Types[x]
			found = typed && tv.IsValue() && isProblem(tv.Type) // not the type of a function literal's result
		}
		return !found
	})
	return found
}
