package api

// Which functions code outside the program can call (ADR-0052 G7). The
// cut of a call of a function value given as a parameter (givenMade)
// counts the calls the graph shows, which are the program's own: the
// standard library, third-party packages and api/gen have no SSA bodies
// here, so a call they make is not in the graph. Such code can call a
// function of the program only through a value it is given, since none of
// it imports the program (outsideImports): the function's own value, or a
// value of a type whose methods it then calls, by an interface or by
// reflection. escapes finds those, and givenMade proves nothing for them.
// Not checked, since the dependencies' code is not read: that code outside
// reads no function from a field of a program's struct it is given and
// calls it, which only reflection could do, and calls by reflection on
// values nested in what it is given only the methods of the interfaces it
// checks for (exportedMethods); a template, which calls any, is checked
// (templateData).

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// TestProblemCodesEscapes proves escapes on testdata/fieldcalls: callE's
// value goes to a sync.Map and a *Runner to fmt.Sprint, so both escape;
// callQ is only called by name and a *Keeper's interface stays in the
// program, so neither does; the other ways the flow goes, each with its
// fixture function. And that outsideImports refuses a package outside the
// program importing one of it, and templateData data it cannot follow.
func TestProblemCodesEscapes(t *testing.T) {
	const fixture = module + "/api/testdata/fieldcalls"
	a, _ := build(t, fixture)
	for name, want := range map[string]string{
		fixture + ".callE":               "which has no body here",
		"(*" + fixture + ".Runner).Run":  "converted to any",
		fixture + ".callQ":               "",
		fixture + ".callF":               "is stored at &t",
		fixture + ".callS":               "",
		fixture + ".callR":               "which has no body here",
		fixture + ".Hook$1":              "a variable of code outside",
		fixture + ".callL":               "which code outside declares",
		"(*" + fixture + ".Gen).Run":     "instantiates slices.Contains",
		"(*" + fixture + ".Keeper).Keep": "",
	} {
		t.Run(name, func(t *testing.T) {
			got := a.escapes(a.fns[name])
			if want == "" && got != "" || !strings.Contains(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	problems := a.index().problems
	for name, want := range map[string]string{
		"Render":      "fieldcalls.Render executes a template with data whose type",
		"RenderAny":   "anyData.P is of interface type any",
		"RenderFunc":  "funcData.F is a function",
		"RenderErr":   "errData.E is of interface type error",
		"RenderFuncs": "registers template functions",
		"HandOn":      "converts a template to any",
		"HandOff":     "hands a template to",
	} {
		t.Run(name, func(t *testing.T) {
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
				t.Errorf("got %q, want a problem saying %q", problems, want)
			}
		})
	}
	if got := len(problems); got != 7 {
		t.Errorf("got %d template problems, want 7 (Render, RenderAny, RenderFunc, RenderErr, RenderFuncs, HandOn, HandOff)", got)
	}
	if got := a.escapes(a.fns["(*"+fixture+".Sub).Do"]); !strings.Contains(got, "a template executed in") {
		t.Errorf("Sub.Do: got %q, want it escaping through RenderChain's template, by Book.Child's result", got)
	}
	if got := a.escapes(a.fns["(*"+fixture+".Page).Title"]); !strings.Contains(got, "a template executed in") {
		t.Errorf("Page.Title: got %q, want it escaping through RenderPage's template", got)
	}
	program := map[string]bool{module + "/internal/app": true}
	deps := []*packages.Package{{PkgPath: module + "/api/gen", Imports: map[string]*packages.Package{module + "/internal/app": {PkgPath: module + "/internal/app"}}}}
	if got := outsideImports(deps, program); len(got) != 1 || !strings.Contains(got[0], "can call the program's functions by name") {
		t.Errorf("got %q, want api/gen refused for importing internal/app", got)
	}
}

// flowIndex is where the program uses values it may have stored: each
// function used as a value (not called by name), each global, each struct
// field's reads and addresses, the conversions to an interface that give
// code holding the interface each exported method, and the generic
// functions outside the program instantiated with a type, which may call
// its methods through their constraint.
type flowIndex struct {
	funcUses   map[*ssa.Function][]ssa.Instruction
	globalUses map[*ssa.Global][]ssa.Instruction
	fields     map[string][]ssa.Instruction // *ssa.Field and *ssa.FieldAddr, by fieldKey
	converted  map[*ssa.Function][]*ssa.MakeInterface
	typeArgs   map[*ssa.Function][]ssa.CallInstruction // calls of a generic function outside the program instantiated with a type having the method
	templated  map[*ssa.Function][]ssa.CallInstruction // template executions whose data reaches a type having the method
	problems   []string                                // template data the index cannot type (templateData)
}

// escapes reports whether code outside the program can call f, and why:
// f's value, or a value whose type has f among its exported methods,
// reaches it (leaks). "" when it cannot. The flow may come back to a
// function whose answer is pending (a value it returns, followed to its
// callers): that way out is the one being checked, so it adds none, and
// only an answer not resting on such a return is kept, which leaves the
// answers independent of the order functions are asked in.
func (a *codeAnalysis) escapes(f *ssa.Function) string {
	if why, done := a.escaped[f]; done {
		return why
	}
	if a.pending[f] {
		return ""
	}
	top := len(a.pending) == 0
	a.pending[f] = true
	why := a.valueEscapes(f)
	if why == "" {
		why = a.receiverEscapes(f)
	}
	delete(a.pending, f)
	if top || why != "" {
		a.escaped[f] = why
	}
	return why
}

// valueEscapes is where f's value, used other than to call it by name,
// reaches code outside the program.
func (a *codeAnalysis) valueEscapes(f *ssa.Function) string {
	seen := map[ssa.Value]bool{}
	for _, in := range a.index().funcUses[f] {
		if why := a.useLeaks(f, in, seen); why != "" {
			return "its value " + why
		}
	}
	return ""
}

// receiverEscapes is where a value converted to an interface, whose
// exported methods include f, reaches code outside the program, which can
// then call f through an interface of its own or by reflection, or a
// generic function outside the program instantiated with the type, which
// can call f through its constraint. An unexported method no code outside
// its package can call. The API's
// handler goes to api/gen, which calls only gen.Handler's methods on it,
// once sealHandler holds (served).
func (a *codeAnalysis) receiverEscapes(f *ssa.Function) string {
	x := a.index()
	if calls := x.typeArgs[f]; len(calls) > 0 {
		return fmt.Sprintf("its receiver's type instantiates %s, outside the program, in %s", calls[0].Common().StaticCallee(), calls[0].Parent())
	}
	if calls := x.templated[f]; len(calls) > 0 {
		return fmt.Sprintf("a template executed in %s reaches its receiver's type in its data", calls[0].Parent())
	}
	seen := map[ssa.Value]bool{}
	for _, mi := range x.converted[f] {
		if why := a.valueLeaks(mi, seen); why != "" {
			return fmt.Sprintf("a %s converted to %s in %s %s", mi.X.Type(), mi.Type(), mi.Parent(), why)
		}
	}
	return ""
}

// index builds the flowIndex once; after sealHandler, since the handler's
// conversion gives api/gen only the served methods.
func (a *codeAnalysis) index() *flowIndex {
	if a.flow != nil {
		return a.flow
	}
	x := &flowIndex{funcUses: map[*ssa.Function][]ssa.Instruction{}, globalUses: map[*ssa.Global][]ssa.Instruction{},
		fields: map[string][]ssa.Instruction{}, converted: map[*ssa.Function][]*ssa.MakeInterface{},
		typeArgs: map[*ssa.Function][]ssa.CallInstruction{}, templated: map[*ssa.Function][]ssa.CallInstruction{}}
	for f := range a.all {
		for _, b := range f.Blocks {
			for _, in := range b.Instrs {
				a.indexInstr(x, in)
			}
		}
	}
	x.problems = a.templateData(x)
	a.flow = x
	return x
}

func (a *codeAnalysis) indexInstr(x *flowIndex, in ssa.Instruction) {
	var callee *ssa.Value
	if c, ok := in.(ssa.CallInstruction); ok && !c.Common().IsInvoke() {
		callee = &c.Common().Value
	}
	switch in := in.(type) {
	case *ssa.Field:
		k := fieldKey(in.X.Type(), in.Field)
		x.fields[k] = append(x.fields[k], in)
	case *ssa.FieldAddr:
		k := fieldKey(in.X.Type(), in.Field)
		x.fields[k] = append(x.fields[k], in)
	case *ssa.MakeInterface:
		for _, m := range a.exportedMethods(in.X.Type()) {
			x.converted[m] = append(x.converted[m], in)
		}
	case ssa.CallInstruction:
		if g := in.Common().StaticCallee(); g != nil && len(g.Blocks) == 0 {
			for _, ta := range g.TypeArgs() {
				for _, m := range a.exportedMethods(ta) {
					x.typeArgs[m] = append(x.typeArgs[m], in)
				}
			}
		}
	}
	for _, op := range in.Operands(nil) {
		switch v := (*op).(type) {
		case *ssa.Function:
			if op != callee {
				x.funcUses[v] = append(x.funcUses[v], in)
			}
		case *ssa.Global:
			x.globalUses[v] = append(x.globalUses[v], in)
		}
	}
}

// exportedMethods is what code holding a value of type t in an interface
// can call: its exported methods, each with the method it promotes or
// wraps (a pointer's wrapper of a value method, an embedded field's
// method), which the wrapper calls with the same arguments. The API's
// handler gives only gen.Handler's methods once the seal holds. Methods of
// values nested in it (an exported field, an element) code outside can
// reach only by reflection, which calls them only through the interfaces
// the walking package checks for (fmt's Stringer and Formatter, error,
// the encoders' Marshalers), none of which takes a function; the one
// standard package calling any method it is told to, a template, gets
// every method reflection reaches in its data (templateData).
func (a *codeAnalysis) exportedMethods(t types.Type) []*ssa.Function {
	if types.IsInterface(t) || hasTypeParam(t) {
		return nil
	}
	var out []*ssa.Function
	mset := a.prog.MethodSets.MethodSet(t)
	for i := range mset.Len() {
		sel := mset.At(i)
		name := sel.Obj().Name()
		if !token.IsExported(name) || isHandler(t) && a.served != nil && !a.served[name] {
			continue
		}
		out = append(out, a.prog.MethodValue(sel))
		if decl, ok := sel.Obj().(*types.Func); ok {
			if fn := a.prog.FuncValue(decl); fn != nil {
				out = append(out, fn)
			}
		}
	}
	return out
}

// templateData is every execution in the program of a text/template or
// html/template template, whose text may call any exported method of any
// value nested in its data, with the data's type: the types reflection
// reaches from it (templateReach) give templated their methods. Data
// whose type the call does not show (an interface value handed on), or
// reaching a value the template could call that the index does not see
// (an interface or function in a field or a method's result), is
// returned as a problem, as are data reaching the API's handler (a
// template may call any of its methods, not only those api/gen does),
// template functions (Funcs) and a template handed on (templateHandedOn).
func (a *codeAnalysis) templateData(x *flowIndex) []string {
	var out []string
	for f := range a.all {
		for _, b := range f.Blocks {
			for _, in := range b.Instrs {
				if p := a.templateHandedOn(in); p != "" {
					out = append(out, fmt.Sprintf("%s %s (%s)", f, p, a.prog.Fset.Position(in.Pos())))
				}
				c, ok := in.(ssa.CallInstruction)
				if !ok {
					continue
				}
				g := c.Common().StaticCallee()
				if g == nil || g.Pkg == nil || !isTemplate(g.Pkg.Pkg.Path()) || !strings.HasPrefix(g.Name(), "Execute") {
					continue
				}
				mi, ok := c.Common().Args[len(c.Common().Args)-1].(*ssa.MakeInterface)
				if !ok {
					out = append(out, fmt.Sprintf("%s executes a template with data whose type the call does not show (%s)", f, a.prog.Fset.Position(in.Pos())))
					continue
				}
				r := &templateReach{prog: a.prog, seen: map[string]bool{}}
				r.reach(mi.X.Type(), "the data", false)
				for _, p := range r.problems {
					out = append(out, fmt.Sprintf("%s executes a template whose data reaches what the test cannot follow: %s (%s)", f, p,
						a.prog.Fset.Position(in.Pos())))
				}
				for _, t := range r.types {
					if isHandler(t) {
						out = append(out, fmt.Sprintf("%s executes a template whose data reaches the API's handler, any of whose methods it may call", f))
					}
					for _, m := range append(a.exportedMethods(t), a.exportedMethods(types.NewPointer(t))...) {
						x.templated[m] = append(x.templated[m], c)
					}
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// templateReach is what a template can reach from its data: the types,
// and what it could call that the index cannot follow.
type templateReach struct {
	prog     *ssa.Program
	seen     map[string]bool
	types    []types.Type
	problems []string
}

// reach adds t, without a pointer, and the types a template can reach
// from a value of it: through pointers, exported fields and the fields an
// embedded one promotes, the elements and keys of containers, and the
// results of exported methods (a template chains through them). A field,
// element, key or result of an interface type, or of a function type,
// holds a value the template can call into or call (its call builtin)
// that the index does not see, so it is a problem; but for an error a
// method returns (result): the method escapes through templated, and a
// value it returns goes with it (returnLeaks).
func (r *templateReach) reach(t types.Type, at string, result bool) {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		r.reach(p.Elem(), at, result)
		return
	}
	switch {
	case result && types.Identical(t, types.Universe.Lookup("error").Type()):
		return
	case types.IsInterface(t):
		r.problems = append(r.problems, fmt.Sprintf("%s is of interface type %s, whose value the template may call into", at, t))
		return
	}
	if _, ok := t.Underlying().(*types.Signature); ok {
		r.problems = append(r.problems, fmt.Sprintf("%s is a function (%s), which the template may call", at, t))
		return
	}
	k := types.TypeString(t, nil)
	if r.seen[k] {
		return
	}
	r.seen[k] = true
	r.types = append(r.types, t)
	r.reachParts(t, k)
	for _, mt := range []types.Type{t, types.NewPointer(t)} {
		if hasTypeParam(mt) {
			continue
		}
		mset := r.prog.MethodSets.MethodSet(mt)
		for i := range mset.Len() {
			sig, ok := mset.At(i).Type().(*types.Signature)
			if !ok || !token.IsExported(mset.At(i).Obj().Name()) {
				continue
			}
			for v := range sig.Results().Variables() {
				r.reach(v.Type(), k+"."+mset.At(i).Obj().Name()+"'s result", true)
			}
		}
	}
}

// reachParts reaches t's fields a template can name and its elements.
func (r *templateReach) reachParts(t types.Type, k string) {
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for f := range u.Fields() {
			if f.Exported() || f.Embedded() {
				r.reach(f.Type(), k+"."+f.Name(), false)
			}
		}
	case *types.Slice:
		r.reach(u.Elem(), k+"'s element", false)
	case *types.Array:
		r.reach(u.Elem(), k+"'s element", false)
	case *types.Chan:
		r.reach(u.Elem(), k+"'s element", false)
	case *types.Map:
		r.reach(u.Key(), k+"'s key", false)
		r.reach(u.Elem(), k+"'s element", false)
	}
}

// templateHandedOn is what is wrong with in for templateData: a function
// registered with Funcs, whose results the template may call into, or a
// template converted to an interface or handed to a function outside the
// template packages, which may execute it with data the test does not see.
func (a *codeAnalysis) templateHandedOn(in ssa.Instruction) string {
	if mi, ok := in.(*ssa.MakeInterface); ok && isTemplateType(mi.X.Type()) {
		return "converts a template to " + mi.Type().String() + ", which may execute it with any data"
	}
	c, ok := in.(ssa.CallInstruction)
	if !ok {
		return ""
	}
	g := c.Common().StaticCallee()
	inTemplate := g != nil && g.Pkg != nil && isTemplate(g.Pkg.Pkg.Path())
	switch {
	case inTemplate && g.Name() == "Funcs":
		return "registers template functions, whose results the template may call into"
	case !inTemplate && slices.ContainsFunc(c.Common().Args, func(v ssa.Value) bool { return isTemplateType(v.Type()) }):
		return fmt.Sprintf("hands a template to %s, which may execute it with any data", c.Common())
	}
	return ""
}

// isTemplateType reports whether t is a text/template or html/template
// Template, or a pointer to one.
func isTemplateType(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && isTemplate(n.Obj().Pkg().Path()) && n.Obj().Name() == "Template"
}

func isTemplate(path string) bool { return path == "text/template" || path == "html/template" }

// hasTypeParam reports whether t is, points to or instantiates with a
// type parameter: a generic body's conversion, whose instances the index
// sees instead.
func hasTypeParam(t types.Type) bool {
	switch t := t.(type) {
	case *types.TypeParam:
		return true
	case *types.Pointer:
		return hasTypeParam(t.Elem())
	case *types.Named:
		args := t.TypeArgs()
		for i := range args.Len() {
			if hasTypeParam(args.At(i)) {
				return true
			}
		}
	}
	return false
}

// fieldKey names a field of a struct type, or of the struct a pointer
// points to.
func fieldKey(t types.Type, field int) string {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	return fmt.Sprintf("%s#%d", types.TypeString(t, nil), field)
}

// valueLeaks is where v, a value that may hold the function followed,
// reaches code outside the program through any of its uses; "" when none.
func (a *codeAnalysis) valueLeaks(v ssa.Value, seen map[ssa.Value]bool) string {
	if seen[v] {
		return ""
	}
	seen[v] = true
	for _, r := range *v.Referrers() {
		if why := a.useLeaks(v, r, seen); why != "" {
			return why
		}
	}
	return ""
}

// useLeaks is where v reaches code outside the program through its use
// by in. Followed: calls of it, phis and conversions, closures capturing
// it, functions given it (their parameter), the callers of a function
// returning it, and stores into a local variable, a global or a field of
// a struct type the program declares (every read of it). Anything else
// (an element of a slice or map, a channel, a field of a type declared
// outside the program, a function the graph cannot name) counts as a
// leak, since code outside could read it there.
func (a *codeAnalysis) useLeaks(v ssa.Value, in ssa.Instruction, seen map[ssa.Value]bool) string {
	switch in := in.(type) {
	case *ssa.DebugRef, *ssa.BinOp:
		return "" // a comparison with nil
	case *ssa.MakeClosure:
		return a.capturedLeaks(v, in, seen)
	case ssa.CallInstruction:
		return a.callLeaks(v, in, seen)
	case *ssa.Return:
		return a.returnLeaks(in.Parent(), seen)
	case *ssa.Store:
		if in.Val != v {
			return ""
		}
		return a.storeLeaks(in.Addr, seen)
	case *ssa.Phi, *ssa.ChangeType, *ssa.MakeInterface, *ssa.ChangeInterface, *ssa.TypeAssert, *ssa.Extract:
		return a.valueLeaks(in.(ssa.Value), seen)
	}
	return fmt.Sprintf("goes to %s in %s (%s), which the test does not follow", in, in.Parent(), a.prog.Fset.Position(in.Pos()))
}

// capturedLeaks follows v made into a closure (mc.Fn) or captured by one
// (its free variable).
func (a *codeAnalysis) capturedLeaks(v ssa.Value, mc *ssa.MakeClosure, seen map[ssa.Value]bool) string {
	if mc.Fn == v {
		return a.valueLeaks(mc, seen)
	}
	fn := mc.Fn.(*ssa.Function)
	for i, b := range mc.Bindings {
		if b == v {
			if why := a.valueLeaks(fn.FreeVars[i], seen); why != "" {
				return why
			}
		}
	}
	return ""
}

// callLeaks follows v through a call: called, or invoked on, it stays in
// the program; given as an argument to a function with a body, that
// function's parameter is followed. A function without one (the standard
// library, a dependency, api/gen), a builtin other than len and cap, or a
// call of a function value or interface method, whose callees code
// outside may have provided, leaks it.
func (a *codeAnalysis) callLeaks(v ssa.Value, call ssa.CallInstruction, seen map[ssa.Value]bool) string {
	c := call.Common()
	at := fmt.Sprintf("%s (%s)", call.Parent(), a.prog.Fset.Position(call.Pos()))
	for i, arg := range c.Args {
		if arg != v {
			continue
		}
		g := c.StaticCallee()
		switch {
		case g != nil && len(g.Blocks) > 0 && i < len(g.Params):
			if why := a.valueLeaks(g.Params[i], seen); why != "" {
				return why
			}
		case g != nil:
			return fmt.Sprintf("goes to %s, which has no body here, in %s", g, at)
		default:
			if b, ok := c.Value.(*ssa.Builtin); ok && (b.Name() == "len" || b.Name() == "cap") {
				continue
			}
			return fmt.Sprintf("goes to %s, whose callee the test does not name, in %s", c, at)
		}
	}
	return "" // called, or a method invoked on it, in the program
}

// returnLeaks follows a value f returns to the calls of f the graph
// shows; code outside calling f (escapes) gets it.
func (a *codeAnalysis) returnLeaks(f *ssa.Function, seen map[ssa.Value]bool) string {
	if why := a.escapes(f); why != "" {
		return fmt.Sprintf("is returned by %s, which code outside can call: %s", f, why)
	}
	if n := a.cg.Nodes[f]; n != nil {
		for _, e := range n.In {
			if call, ok := e.Site.(*ssa.Call); ok {
				if why := a.valueLeaks(call, seen); why != "" {
					return why
				}
			}
		}
	}
	return ""
}

// storeLeaks follows a value stored at addr to every read of the place:
// a local variable's loads, a global's of the program, or a field's
// anywhere (the field of every value of the struct type, as the graph's
// VTA joins them). A place whose address is handed on, a variable or a
// field of a type code outside declares, or any other place, leaks it.
func (a *codeAnalysis) storeLeaks(addr ssa.Value, seen map[ssa.Value]bool) string {
	x := a.index()
	switch ad := addr.(type) {
	case *ssa.Alloc:
		return a.loadsLeak(ad, *ad.Referrers(), seen)
	case *ssa.Global:
		if ad.Pkg == nil || !a.program[ad.Pkg.Pkg.Path()] {
			return fmt.Sprintf("is stored in %s, a variable of code outside", ad)
		}
		return a.loadsLeak(ad, x.globalUses[ad], seen)
	case *ssa.FieldAddr:
		if !a.declaredHere(ad.X.Type()) {
			return fmt.Sprintf("is stored in a field of %s, which code outside declares and may read, in %s", ad.X.Type(), ad.Parent())
		}
		for _, in := range x.fields[fieldKey(ad.X.Type(), ad.Field)] {
			var why string
			switch in := in.(type) {
			case *ssa.Field:
				why = a.valueLeaks(in, seen)
			case *ssa.FieldAddr:
				why = a.loadsLeak(in, *in.Referrers(), seen)
			}
			if why != "" {
				return why
			}
		}
		return ""
	}
	return fmt.Sprintf("is stored at %s in %s, which the test does not follow", addr, addr.Parent())
}

// loadsLeak follows the loads among uses of place: a store into it writes
// it, and any other use (its address handed on or captured) leaks what it
// holds.
func (a *codeAnalysis) loadsLeak(place ssa.Value, uses []ssa.Instruction, seen map[ssa.Value]bool) string {
	for _, in := range uses {
		switch in := in.(type) {
		case *ssa.DebugRef:
		case *ssa.Store:
			if in.Addr != place {
				return fmt.Sprintf("is stored where %s's address is stored, in %s", place, in.Parent())
			}
		case *ssa.UnOp:
			if in.Op != token.MUL {
				return fmt.Sprintf("is held at %s, used by %s in %s", place, in, in.Parent())
			}
			if why := a.valueLeaks(in, seen); why != "" {
				return why
			}
		default:
			return fmt.Sprintf("is held at %s, whose address goes to %s in %s (%s)", place, in, in.Parent(), a.prog.Fset.Position(in.Pos()))
		}
	}
	return ""
}

// declaredHere reports whether t, or what it points to, is a struct type
// the program declares. A literal struct type with an exported field
// counts as outside's: code outside can spell the same type and read the
// field, from an interface or a parameter, without reflection.
func (a *codeAnalysis) declaredHere(t types.Type) bool {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		st, isStruct := t.Underlying().(*types.Struct)
		return !isStruct || !slices.ContainsFunc(slices.Collect(st.Fields()), (*types.Var).Exported)
	}
	return n.Obj().Pkg() != nil && a.program[n.Obj().Pkg().Path()]
}

// outsideImports is every package outside the program, in erp-server's
// imports, that imports one of the program's: such code could call a
// function of the program by name, which no graph here shows.
func outsideImports(deps []*packages.Package, program map[string]bool) []string {
	var out []string
	packages.Visit(deps, nil, func(p *packages.Package) {
		if program[p.PkgPath] {
			return
		}
		for path := range p.Imports {
			if program[path] {
				out = append(out, fmt.Sprintf("%s imports %s, so it can call the program's functions by name", p.PkgPath, path))
			}
		}
	})
	slices.Sort(out)
	return out
}
