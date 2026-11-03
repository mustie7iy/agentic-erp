package layout

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"
)

const pagination = module + "platform/pagination"

// TestPageScopes holds every page-token scope to values whose printed form
// is the value itself. A page token is signed with the listing's scope, its
// name and filters as text, and refused when the next call's scope differs
// (04 §4.5). A pointer formatted into the scope prints its address, which
// the next request does not share, so paging with that filter stopped after
// the first page (listHierarchies' is_standard; T64b's listPeriodCloses).
// The test traces each scope given to the codec back to where it is built
// and refuses a formatted value that is, or holds, a pointer, an interface,
// a map, a channel or a function, unless its type prints itself through a
// String or Error method.
func TestPageScopes(t *testing.T) {
	pkgs := loadTyped(t, "./internal/...", "./cmd/...")
	got := checkScopes(pkgs)
	if len(got) > 0 {
		t.Errorf("page-token scopes formatted from values whose printed form is not the value; "+
			"format the value (an Opt's .Value, a dereferenced pointer, a date string), not the pointer:\n%s", strings.Join(got, "\n"))
	}
	if len(scopeSites(pkgs)) == 0 {
		t.Error("no call to the page-token codec found: the test no longer sees the listings")
	}
}

// TestPageScopesRefuse runs the check on testdata/pagescope, a package of
// one listing per case, and wants a finding for exactly the bad ones.
func TestPageScopesRefuse(t *testing.T) {
	findings := strings.Join(checkScopes(loadTyped(t, "./internal/layout/testdata/pagescope")), "\n")
	cases := map[string]bool{
		"PointerFormatted": true, "PointerInVariable": true, "PointerInHelper": true, "PointerInField": true,
		"PointerInParameter": true, "StructWithPointer": true, "InterfaceFormatted": true, "MapFormatted": true,
		"SpreadArguments": true, "DynamicCall": true, "PointerEncoded": true, "PointerConverted": true,
		"PointerSprint": true, "PointerInClosure": true, "PointerInPackageVar": true, "PointerFromTuple": true,
		"InterfaceMethod": true, "LibraryArguments": true, "Builder": true, "RangeVariable": true, "FunctionValue": true,
		"PointerWrongVerb": true, "UnexportedPointerField": true, "ElementOfPointer": true, "DispatchedPointer": true,
		"DispatchedValue": false, "JoinOfValues": false, "TupleOfValues": false,
		"VariadicPointer": true, "VariadicSpread": true, "MethodExpression": true, "BytesOfPointer": true,
		"ScannedInto": true, "CopiedPointer": true, "VariadicNone": false,
		"OptValue": false, "Dereferenced": false, "Stringer": false, "Constant": false, "Concatenated": false,
		"HelperOfValues": false, "FieldOfValues": false, "ParameterOfValue": false, "NilableStringer": false,
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if got := strings.Contains(findings, "pagescope."+name+" gives:"); got != bad {
				t.Errorf("finding for %s: got %v, want %v; findings:\n%s", name, got, bad, findings)
			}
		})
	}
}

func loadTyped(t *testing.T, patterns ...string) []*packages.Package {
	t.Helper()
	mode := packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports
	pkgs, err := packages.Load(&packages.Config{Mode: mode, Dir: "../.."}, patterns...)
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatalf("%v do not load", patterns)
	}
	return pkgs
}

// scopeAt is an expression with the type information and the function
// body it is read in, which locates the assignments to its local variables.
type scopeAt struct {
	e    ast.Expr
	info *types.Info
	body ast.Node
	fn   string // the function the scope is given to the codec in
	in   string // the function e is in
}

type funcAt struct {
	decl *ast.FuncDecl
	info *types.Info
}

// scopeIndex is what the trace looks up across the loaded packages: the
// declarations of functions, the calls to them and the writes to fields
// and package variables.
type scopeIndex struct {
	fset   *token.FileSet
	funcs  map[*types.Func]funcAt
	calls  map[*types.Func][]scopeAt // each call, as its CallExpr
	writes map[*types.Var][]scopeAt  // the values written to a field or package variable
	uses   map[*types.Func]int       // the references to each function, calls included
	called map[string][]*types.Func  // the interface methods called, by name
	loaded map[*types.Package]bool   // the packages whose code the trace reads
	seen   map[ast.Expr]bool
	found  []string
}

// checkScopes returns a finding per formatted value of a scope that does
// not print as itself, and per scope the trace cannot follow.
func checkScopes(pkgs []*packages.Package) []string {
	x := index(pkgs)
	for _, s := range scopeSites(pkgs) {
		x.trace(s)
	}
	slices.Sort(x.found)
	return slices.Compact(x.found)
}

// scopeSites are the scope arguments of every call to the codec's Keyset,
// Encode and Decode.
func scopeSites(pkgs []*packages.Package) []scopeAt {
	var sites []scopeAt
	eachFunc(pkgs, func(fd *ast.FuncDecl, info *types.Info) {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			f, ok := typeutil.Callee(info, call).(*types.Func)
			if !ok || f.Pkg() == nil || f.Pkg().Path() != pagination || !slices.Contains([]string{"Keyset", "Encode", "Decode"}, f.Name()) {
				return true
			}
			if recv := f.Signature().Recv(); recv == nil || !strings.HasSuffix(recv.Type().String(), "pagination.Codec") {
				return true
			}
			arg := call.Args[0]
			if f.Name() == "Keyset" {
				arg = call.Args[1]
			}
			name := funcName(info, fd)
			sites = append(sites, scopeAt{e: arg, info: info, body: fd.Body, fn: name, in: name})
			return true
		})
	})
	return sites
}

func eachFunc(pkgs []*packages.Package, do func(*ast.FuncDecl, *types.Info)) {
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
					do(fd, p.TypesInfo)
				}
			}
		}
	}
}

func funcName(info *types.Info, fd *ast.FuncDecl) string {
	f := info.Defs[fd.Name].(*types.Func)
	return f.Pkg().Name() + "." + fd.Name.Name
}

func index(pkgs []*packages.Package) *scopeIndex {
	x := &scopeIndex{funcs: map[*types.Func]funcAt{}, calls: map[*types.Func][]scopeAt{}, writes: map[*types.Var][]scopeAt{}, seen: map[ast.Expr]bool{},
		uses: map[*types.Func]int{}, called: map[string][]*types.Func{},
		loaded: map[*types.Package]bool{}}
	for _, p := range pkgs { // the loaded packages: their imports come from export data
		x.fset, x.loaded[p.Types] = p.Fset, true
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				x.indexDecl(d, p.TypesInfo)
			}
		}
	}
	return x
}

func (x *scopeIndex) indexDecl(d ast.Decl, info *types.Info) {
	var body ast.Node = d
	in := "a package variable"
	if fd, ok := d.(*ast.FuncDecl); ok {
		if fd.Body == nil {
			return
		}
		x.funcs[info.Defs[fd.Name].(*types.Func)] = funcAt{decl: fd, info: info}
		body, in = fd.Body, funcName(info, fd)
	}
	ast.Inspect(d, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if f, ok := info.Uses[n].(*types.Func); ok {
				x.uses[f.Origin()]++
			}
		case *ast.CallExpr:
			if f, ok := typeutil.Callee(info, n).(*types.Func); ok {
				f = f.Origin()
				x.calls[f] = append(x.calls[f], scopeAt{e: n, info: info, body: body, in: in})
				if recv := f.Signature().Recv(); recv != nil && types.IsInterface(recv.Type()) && !slices.Contains(x.called[f.Name()], f) {
					x.called[f.Name()] = append(x.called[f.Name()], f)
				}
			}
		case *ast.CompositeLit:
			x.indexLiteral(n, info, body, in)
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if v := writtenVar(info, l); v != nil && len(n.Rhs) == len(n.Lhs) {
					x.writes[v] = append(x.writes[v], scopeAt{e: n.Rhs[i], info: info, body: body, in: in})
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if v, ok := info.Defs[name].(*types.Var); ok && i < len(n.Values) && !v.IsField() && v.Parent() == v.Pkg().Scope() {
					x.writes[v] = append(x.writes[v], scopeAt{e: n.Values[i], info: info, body: body, in: in})
				}
			}
		}
		return true
	})
}

// writtenVar is the field or package variable an assignment's left side
// writes, or nil for a local variable (traced in its function instead).
func writtenVar(info *types.Info, l ast.Expr) *types.Var {
	switch l := l.(type) {
	case *ast.SelectorExpr:
		if s := info.Selections[l]; s != nil && s.Kind() == types.FieldVal {
			return s.Obj().(*types.Var)
		}
		if v, ok := info.Uses[l.Sel].(*types.Var); ok {
			return v // a package variable of another package
		}
	case *ast.Ident:
		if v, ok := info.ObjectOf(l).(*types.Var); ok && v.Pkg() != nil && v.Parent() == v.Pkg().Scope() {
			return v
		}
	}
	return nil
}

func (x *scopeIndex) indexLiteral(lit *ast.CompositeLit, info *types.Info, body ast.Node, in string) {
	st, ok := info.TypeOf(lit).Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if v, ok := info.Uses[kv.Key.(*ast.Ident)].(*types.Var); ok {
				x.writes[v] = append(x.writes[v], scopeAt{e: kv.Value, info: info, body: body, in: in})
			}
			continue
		}
		x.writes[st.Field(i)] = append(x.writes[st.Field(i)], scopeAt{e: el, info: info, body: body, in: in})
	}
}

func (x *scopeIndex) refuse(s scopeAt, why string) {
	x.found = append(x.found, fmt.Sprintf("the scope %s gives: %s: in %s: %s", s.fn, x.fset.Position(s.e.Pos()), s.in, why))
}

// trace follows a string value of the scope to the places it is made.
func (x *scopeIndex) trace(s scopeAt) {
	if x.seen[s.e] {
		return
	}
	x.seen[s.e] = true
	if tv, ok := s.info.Types[s.e]; ok && tv.Value != nil {
		return // a constant
	}
	switch e := s.e.(type) {
	case *ast.ParenExpr:
		x.trace(s.with(e.X))
	case *ast.BinaryExpr:
		x.trace(s.with(e.X))
		x.trace(s.with(e.Y))
	case *ast.CallExpr:
		x.traceCall(s, e)
	case *ast.Ident:
		x.traceIdent(s, e)
	case *ast.SelectorExpr:
		x.traceSelector(s, e)
	case *ast.CompositeLit: // a []string a scope is joined from
		for _, el := range e.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			x.trace(s.with(el))
		}
	default:
		x.refuse(s, "a scope the check cannot follow")
	}
}

func (s scopeAt) with(e ast.Expr) scopeAt {
	return scopeAt{e: e, info: s.info, body: s.body, fn: s.fn, in: s.in}
}

func (x *scopeIndex) traceCall(s scopeAt, call *ast.CallExpr) {
	if tv := s.info.Types[call.Fun]; tv.IsType() {
		x.trace(s.with(call.Args[0])) // string(b): the scope is what made b
		return
	}
	if b, ok := typeutil.Callee(s.info, call).(*types.Builtin); ok && (b.Name() == "append" || b.Name() == "make") {
		x.traceArguments(s, call) // make's are sizes: it holds empty strings
		return
	}
	f, ok := typeutil.Callee(s.info, call).(*types.Func)
	switch {
	case !ok:
		x.refuse(s, "a scope from a call the check cannot resolve")
	case f.Pkg() != nil && f.Pkg().Path() == "fmt" && f.Signature().Variadic():
		x.traceFormat(s, call, f)
	default:
		x.traceCallee(s, call, f.Origin())
	}
}

// buffers are the methods that return what was written into a buffer by
// other calls, which the check does not follow.
var buffers = []string{"(*strings.Builder).String", "(*bytes.Buffer).String"}

// traceCallee follows a function of the loaded packages into its return
// values, and one from elsewhere (strconv, a uuid's String, strings.Join)
// into what it is given. A call it cannot see the body of (an interface's
// method, a function of the loaded code without one) or a buffer's
// contents is refused.
func (x *scopeIndex) traceCallee(s scopeAt, call *ast.CallExpr, f *types.Func) {
	if _, ok := x.funcs[f]; ok {
		x.traceResult(s, f, 0)
		return
	}
	recv := f.Signature().Recv()
	switch {
	case recv != nil && types.IsInterface(recv.Type()):
		x.refuse(s, "a scope from "+f.Name()+", called through an interface")
	case x.loaded[f.Pkg()]:
		x.refuse(s, "a scope from "+f.Name()+", which has no body to follow")
	case slices.Contains(buffers, f.FullName()):
		x.refuse(s, "a scope written into a buffer the check cannot follow")
	default:
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && s.info.Selections[sel] != nil && isStrings(s.info.TypeOf(sel.X)) {
			x.trace(s.with(sel.X)) // a method of a string type: the string
		}
		x.traceArguments(s, call)
	}
}

// traceArguments follows the arguments of a call that are strings or
// slices of strings; the others (a number, a uuid, a time) print as values.
func (x *scopeIndex) traceArguments(s scopeAt, call *ast.CallExpr) {
	for _, a := range call.Args {
		if !s.info.Types[a].IsType() && isStrings(s.info.TypeOf(a)) { // make([]string, n) names a type
			x.trace(s.with(a))
		}
	}
}

// isStrings says whether t is a string, bytes, or a slice or array of
// strings: what text a scope is made of passes through.
func isStrings(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Slice:
		b, ok := u.Elem().Underlying().(*types.Basic)
		return isString(u.Elem()) || ok && b.Kind() == types.Byte
	case *types.Array:
		return isString(u.Elem())
	}
	return isString(t)
}

// traceFormat checks the values a fmt function formats (its arguments from
// the variadic one on), each under its verb, and follows the ones before
// (a format, a buffer).
func (x *scopeIndex) traceFormat(s scopeAt, call *ast.CallExpr, f *types.Func) {
	if call.Ellipsis.IsValid() {
		x.refuse(s, "formats a spread argument list")
		return
	}
	variadic := f.Signature().Params().Len() - 1
	for _, a := range call.Args[:variadic] {
		x.trace(s.with(a))
	}
	var verbs []rune // nil: unknown, every value checked as if its String were not called
	if !strings.HasSuffix(f.Name(), "f") {
		verbs = slices.Repeat([]rune{'v'}, len(call.Args)-variadic)
	} else if tv := s.info.Types[call.Args[variadic-1]]; tv.Value != nil && tv.Value.Kind() == constant.String {
		verbs = formatVerbs(constant.StringVal(tv.Value))
	}
	for i, a := range call.Args[variadic:] {
		verb := rune(0)
		if i < len(verbs) {
			verb = verbs[i]
		}
		if why := unprintable(s.info.TypeOf(a), verb); why != "" {
			x.refuse(s.with(a), "formats "+why)
		} else if isString(s.info.TypeOf(a)) {
			x.trace(s.with(a))
		}
	}
}

// formatVerbs are the verbs of a format, one per argument, or nil when a
// verb takes its argument by index or a width or precision takes one (*).
func formatVerbs(format string) []rune {
	var verbs []rune
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.ContainsRune("+-# 0123456789.", rune(format[i])) {
			i++
		}
		switch {
		case i == len(format):
			return verbs
		case format[i] == '%':
		case format[i] == '[' || format[i] == '*':
			return nil
		default:
			verbs = append(verbs, rune(format[i]))
		}
	}
	return verbs
}

func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// traceResult follows a function of the loaded packages into its i-th
// result: what each return statement gives, or the named result a bare
// return gives.
func (x *scopeIndex) traceResult(s scopeAt, f *types.Func, i int) {
	d := x.funcs[f]
	in := scopeAt{info: d.info, body: d.decl.Body, fn: s.fn, in: funcName(d.info, d.decl)}
	ast.Inspect(d.decl.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		r, ok := n.(*ast.ReturnStmt)
		switch {
		case !ok:
		case len(r.Results) == 0:
			in.e = d.decl.Name // where a refusal points
			x.traceLocal(in, f.Signature().Results().At(i))
		case len(r.Results) == f.Signature().Results().Len():
			x.trace(in.with(r.Results[i]))
		default:
			x.refuse(s, "a scope from "+f.Name()+", whose results the check cannot follow")
		}
		return true
	})
}

func (x *scopeIndex) traceIdent(s scopeAt, id *ast.Ident) {
	v, ok := s.info.ObjectOf(id).(*types.Var)
	switch {
	case !ok:
		x.refuse(s, "a scope the check cannot follow")
	case v.Pkg() != nil && v.Parent() == v.Pkg().Scope():
		x.traceWrites(s, v)
	default:
		x.traceLocal(s, v)
	}
}

func (x *scopeIndex) traceSelector(s scopeAt, e *ast.SelectorExpr) {
	if sel := s.info.Selections[e]; sel != nil && sel.Kind() == types.FieldVal {
		x.traceWrites(s, sel.Obj().(*types.Var))
		return
	}
	if v, ok := s.info.Uses[e.Sel].(*types.Var); ok {
		x.traceWrites(s, v)
		return
	}
	x.refuse(s, "a scope the check cannot follow")
}

// traceWrites follows a field or package variable of the loaded code to
// every value written to it. One declared elsewhere (an Opt's Value, which
// the generated decoder fills from the request) holds data, not formatting;
// following its writes would follow every response that sets one.
func (x *scopeIndex) traceWrites(s scopeAt, v *types.Var) {
	if !x.loaded[v.Pkg()] {
		return
	}
	for _, w := range x.writes[v] {
		w.fn = s.fn
		x.trace(w)
	}
}

// traceLocal follows a local variable to its assignments in the function,
// and a parameter to the arguments of every call of the function. A local
// whose values it does not find (a range variable, a receiver) is refused.
func (x *scopeIndex) traceLocal(s scopeAt, v *types.Var) {
	if x.traceParameter(s, v) {
		return
	}
	found := false
	ast.Inspect(s.body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.UnaryExpr: // &v: written through a pointer the check does not follow
			if id, ok := n.X.(*ast.Ident); ok && n.Op == token.AND && s.info.ObjectOf(id) == v {
				found = true
				x.refuse(s, "a local variable written through its address")
			}
		case *ast.CallExpr: // copy(v, src): the elements of src
			if b, ok := typeutil.Callee(s.info, n).(*types.Builtin); ok && b.Name() == "copy" {
				if id, ok := n.Args[0].(*ast.Ident); ok && s.info.ObjectOf(id) == v {
					x.trace(s.with(n.Args[1]))
				}
			}
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if ix, ok := l.(*ast.IndexExpr); ok { // v[i] = …: an element of v
					l = ix.X
				}
				if id, ok := l.(*ast.Ident); !ok || s.info.ObjectOf(id) != v {
					continue
				}
				found = true
				if len(n.Rhs) == len(n.Lhs) {
					x.trace(s.with(n.Rhs[i]))
				} else {
					x.traceTuple(s, n.Rhs[0], i)
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if s.info.Defs[name] == v && i < len(n.Values) {
					found = true
					x.trace(s.with(n.Values[i]))
				}
			}
		}
		return true
	})
	if !found {
		x.refuse(s, "a local variable whose value the check cannot find")
	}
}

// traceParameter follows a parameter of a declared function to the
// arguments of its calls; a closure's parameter cannot be followed.
func (x *scopeIndex) traceParameter(s scopeAt, v *types.Var) bool {
	for f, d := range x.funcs {
		params := f.Signature().Params()
		for i := range params.Len() {
			if params.At(i) != v || d.decl.Body != s.body {
				continue
			}
			x.traceArgument(s, f, i)
			return true
		}
	}
	if v.Pos().IsValid() && !s.declaredIn(v) {
		x.refuse(s, "a scope from a closure's parameter")
		return true
	}
	return false
}

// traceTuple follows the i-th result of a call of several results to a
// function of the loaded packages.
func (x *scopeIndex) traceTuple(s scopeAt, e ast.Expr, i int) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		x.refuse(s, "a scope the check cannot follow")
		return
	}
	f, ok := typeutil.Callee(s.info, call).(*types.Func)
	if ok {
		f = f.Origin()
	}
	if _, found := x.funcs[f]; !found {
		x.refuse(s, "a scope from a call of several results the check cannot follow")
		return
	}
	x.traceResult(s, f, i)
}

// traceArgument follows the i-th argument of every call of f, the calls
// of the interface methods it implements included. A function also used
// as a value has calls the check cannot see, and is refused.
func (x *scopeIndex) traceArgument(s scopeAt, f *types.Func, i int) {
	calls := x.calls[f]
	if x.uses[f] > len(calls) {
		x.refuse(s, "a scope from a parameter of "+f.Name()+", which is also used as a value")
	}
	if recv := f.Signature().Recv(); recv != nil {
		for _, m := range x.called[f.Name()] {
			iface := m.Signature().Recv().Type().Underlying().(*types.Interface)
			if types.Implements(recv.Type(), iface) || types.Implements(types.NewPointer(recv.Type()), iface) {
				calls = append(calls, x.calls[m]...)
			}
		}
	}
	variadic := f.Signature().Variadic() && i == f.Signature().Params().Len()-1
	for _, c := range calls {
		call := c.e.(*ast.CallExpr)
		args := call.Args
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && c.info.Selections[sel] != nil && c.info.Selections[sel].Kind() == types.MethodExpr {
			args = args[1:] // T.m(recv, …): the receiver comes first
		}
		last := i + 1
		if variadic && !call.Ellipsis.IsValid() {
			last = len(args) // every argument the variadic parameter collects, none included
		}
		c.fn = c.in // the scope is the caller's
		for _, a := range args[i:last] {
			x.trace(c.with(a))
		}
	}
}

// declaredIn says whether v is declared by a statement of s's body, not as
// a parameter of a function literal in it.
func (s scopeAt) declaredIn(v *types.Var) bool {
	inLit := false
	ast.Inspect(s.body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok {
			for _, f := range lit.Type.Params.List {
				for _, name := range f.Names {
					if s.info.Defs[name] == v {
						inLit = true
					}
				}
			}
		}
		return !inLit
	})
	return !inLit
}

// unprintable says why a value formatted under verb (0: unknown) does not
// print as itself, or "" when it does. A type with a String or Error
// method prints through it under the verbs fmt calls them for (%v %s %q
// %x %X), and as an exported field; otherwise a pointer, interface, map,
// channel, function or unsafe pointer, or a struct or array holding one,
// does not.
func unprintable(t types.Type, verb rune) string {
	return unprintableIn(t, map[types.Type]bool{}, verb != 0 && strings.ContainsRune("vsqxX", verb))
}

func unprintableIn(t types.Type, seen map[types.Type]bool, methods bool) string {
	if seen[t] || methods && printsItself(t) {
		return ""
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *types.Basic:
		if u.Kind() == types.UnsafePointer {
			return "an unsafe pointer"
		}
		return ""
	case *types.Pointer:
		return "a pointer (" + t.String() + "), which prints its address"
	case *types.Interface:
		return "an interface (" + t.String() + "), whose dynamic value the check cannot see"
	case *types.Map, *types.Chan, *types.Signature:
		return "a " + t.String() + ", whose printed form is not stable"
	case *types.Struct:
		for i := range u.NumFields() {
			// fmt cannot call an unexported field's methods
			if why := unprintableIn(u.Field(i).Type(), seen, methods && u.Field(i).Exported()); why != "" {
				return t.String() + " holding " + why
			}
		}
	case *types.Array:
		return unprintableIn(u.Elem(), seen, methods)
	case *types.Slice:
		return unprintableIn(u.Elem(), seen, methods)
	}
	return ""
}

func printsItself(t types.Type) bool {
	ms := types.NewMethodSet(t)
	for _, name := range []string{"String", "Error"} {
		sel := ms.Lookup(nil, name)
		if sel == nil {
			continue
		}
		sig := sel.Obj().(*types.Func).Signature()
		if sig.Params().Len() == 0 && sig.Results().Len() == 1 && isString(sig.Results().At(0).Type()) {
			return true
		}
	}
	return false
}
