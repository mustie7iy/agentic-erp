package api

// x-problem-codes (ADR-0052 G7): every operation lists the stable problem
// codes its handler can answer, and the document lists those the request
// pipeline can answer for any operation (authentication, authorization,
// rate limits, idempotency, timeouts, the database's generic codes). Both
// are derived here from the server's code and must match it exactly:
//
//	go test ./api -run TestProblemCodes -update
//
// rewrites them after a handler's codes change.
//
// How codes are found. Every problem is made by problem.New, whose code
// argument is traced back through parameters, phis and closures to string
// constants, each at the function that supplies it (its gate). A code is
// an operation's when its gate is reachable from the operation's method on
// the API's handler (internal/app), in a call graph of the erp-server
// program (VTA over CHA). Function values that escape into code outside
// the program (pgx's row callbacks, net/http's handlers) are taken as
// called, as are all function values a reached function makes. Three
// kinds of edges are cut: problem.From's lookup of a registered
// constraint's problem (constraints are attributed below);
// http.Handler.ServeHTTP called through the interface (the next handler,
// the model gateway, the loopback API: other routes, not this operation's
// work); and a call of a function value a function was given as a
// parameter (database.Pool.InTx's work: the graph would join every
// caller's), whose values the callers make, so they count where their
// callers are reached. A problem made by a package initializer and kept
// in a variable is an operation's when the operation's code reads that
// variable. A code a constraint's problem (problem.RegisterConstraint)
// carries is an operation's when a function it reaches runs a statement
// writing the constraint's table (constraint and table read from the
// migrations). Codes built at run time, or set on a problem after
// problem.New, cannot be traced; dynamicCodes declares them per function,
// and the test fails on a site it does not declare.
//
// The graph knows no branch conditions, so an operation that reaches a
// shared function gets every code it makes. untakenBranches narrows the
// ones whose branch is taken only for an input a few functions make: such
// a code is an operation's only when it reaches one of them. Each entry's
// premise is checked: that no other function sets the field the branch
// tests, or that no other value reaches the parameter it tests. Branches
// it does not name stay a superset (ADR-0052, G7 as built).

import (
	"flag"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/mustie7iy/agentic-erp/db"
)

var update = flag.Bool("update", false, "rewrite openapi.yaml's x-problem-codes from the handlers")

const module = "github.com/mustie7iy/agentic-erp"

// dynamicCodes are the codes of the sites that build a code at run time,
// by the function holding the site. nil: the function relays codes it was
// given (the semantic layer passing on the API's own answers).
var dynamicCodes = map[string][]string{
	module + "/internal/partner.checkCodes": {"partner.duplicate_payment_method", "partner.duplicate_withholding_tax_code",
		"partner.unknown_payment_method", "partner.unknown_withholding_tax_code"},
	"(" + module + "/internal/partner.supplierCodes).apply": {"partner.payment_method_in_use", "partner.withholding_tax_code_in_use"},
	module + "/internal/analytics/endpoint.apiError":        nil,
}

// untaken is a code its gate makes only on a branch taken for an input
// that the functions in by alone make, with the premise the test checks:
// fields, which no other function sets (package path, type, field), or
// param, the gate's parameter, which no value but those by passes carries
// taking.
type untaken struct {
	gate, code string
	by         []string
	fields     [][3]string
	param      string
	taking     string
	why        string
}

// untakenBranches are the branches of shared code that only another
// caller's input takes (pre-existing finding on batch 21).
var untakenBranches = []untaken{
	{gate: "(" + module + "/internal/ledger.grirPosting).takeOver", code: "ledger.gr_ir_position_exists",
		by:     []string{"(*" + module + "/internal/integration.loader).grirPosting"},
		fields: [][3]string{{module + "/internal/ledger", "GRIRItem", "TakesOver"}},
		why:    "itemLines takes a position over only for a GRIRItem.TakesOver item, which a migration run's load alone makes"},
	{gate: "(" + module + "/internal/ledger.grirPosting).checkClears", code: "ledger.gr_ir_clearing_outdated",
		by:     []string{module + "/internal/procurement.clearingPosting"},
		fields: [][3]string{{module + "/internal/ledger", "GRIRItem", "Clears"}},
		why:    "checkClears refuses only a GRIRItem.Clears item, which a GR/IR clearing's approval alone makes"},
	{gate: "(" + module + "/internal/ledger.Postings).prepareGRIR", code: "ledger.no_price_difference_account",
		by:     []string{module + "/internal/payables.grirItem", module + "/internal/procurement.clearingPosting"},
		fields: [][3]string{{module + "/internal/ledger", "GRIRItem", "Invoiced"}, {module + "/internal/ledger", "GRIRItem", "Clears"}},
		why: "prepareGRIR looks the price difference account up only for an invoiced or a clearing item; a goods movement's items " +
			"name their offset account"},
	{gate: "(*" + module + "/internal/procurement.Endpoints).valuation", code: "procurement.no_gr_ir_account",
		by:    []string{"(*" + module + "/internal/procurement.Endpoints).ReleaseLoadedOrder"},
		param: "loaded", taking: "gr_ir",
		why: "valuation refuses only an order a migration run loaded as valued through GR/IR; the API's and the approval's releases pass \"\""},
}

// outsideTheAPI are tables only other listeners write, whose constraints'
// problems no public operation answers, with the writer.
var outsideTheAPI = map[string]string{
	"identity.identity_provider": "the control plane registers identity providers (api/control.yaml)",
}

var codeForm = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)

// written is the table a statement writes.
var written = regexp.MustCompile(`(?i)\b(?:INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+(?:ONLY\s+)?([a-z_0-9]+\.[a-z_0-9]+)`)

// emission is a code and the function that supplies it.
type emission struct {
	gate *ssa.Function
	site ssa.CallInstruction // the call in gate the code goes into
	code string
}

// codeAnalysis is the erp-server program's call graph and the codes it can
// make.
type codeAnalysis struct {
	prog        *ssa.Program
	all         map[*ssa.Function]bool
	fns         map[string]*ssa.Function // by name
	cg          *callgraph.Graph
	from        *ssa.Function // problem.From
	gated       map[*ssa.Function][]string
	globals     map[*ssa.Global][]string
	unresolved  map[string][]token.Position // by the function holding the site
	constraints map[string][]string         // codes of the constraints on each table
	writes      map[*ssa.Function][]string  // the tables each function's statements write
	unplaced    []string                    // registered constraints no migration names
	narrowed    map[int][]string            // the operations each of untakenBranches takes its code from
}

func TestProblemCodes(t *testing.T) {
	a := analyse(t)
	spec := loadSpec(t)
	common := keys(a.codesOf(a.reach(a.pipeline()), nil))
	a.checkUntaken(t)
	derived := a.operationCodes(t, spec, common)
	if *update {
		writeCodes(t, common, derived)
		spec = loadSpec(t)
	}
	known := a.allCodes()
	checkList(t, "the document's x-problem-codes", spec["x-problem-codes"], common, known)
	for _, o := range operations(t, spec) {
		id, _ := o.op["operationId"].(string)
		checkList(t, id+"'s x-problem-codes", o.op["x-problem-codes"], derived[id], known)
	}
	for _, name := range a.unplaced {
		t.Errorf("constraint %s has a registered problem, but no migration names it or its table", name)
	}
	listed := map[string]bool{}
	for _, c := range common {
		listed[c] = true
	}
	for _, cs := range derived {
		for _, c := range cs {
			listed[c] = true
		}
	}
	for tbl, cs := range a.constraints {
		for _, c := range cs {
			if _, outside := outsideTheAPI[tbl]; !outside && !listed[c] {
				t.Errorf("%s, the problem of a constraint on %s, is no operation's: none writes the table in a statement of its own", c, tbl)
			}
		}
	}
	for fn, sites := range a.unresolved {
		if _, ok := dynamicCodes[fn]; !ok {
			t.Errorf("%s builds a problem code at run time (%v): declare its codes in dynamicCodes", fn, sites)
		}
	}
	for fn := range dynamicCodes {
		if _, ok := a.unresolved[fn]; !ok {
			t.Errorf("dynamicCodes declares %s, which builds no code at run time any more", fn)
		}
	}
	for i, u := range untakenBranches {
		if len(a.narrowed[i]) == 0 {
			t.Errorf("untakenBranches: %s of %s is no operation's to leave out any more; remove the entry", u.code, u.gate)
		}
	}
}

// checkList checks a listed x-problem-codes against the derived codes:
// equal, sorted, each a code the server makes.
func checkList(t *testing.T, what string, listed any, want []string, known map[string]bool) {
	t.Helper()
	raw, ok := listed.([]any)
	if !ok {
		t.Errorf("%s: got %v, want a list (go test ./api -run TestProblemCodes -update)", what, listed)
		return
	}
	got := make([]string, len(raw))
	for i, c := range raw {
		got[i] = fmt.Sprint(c)
		if !codeForm.MatchString(got[i]) || !known[got[i]] {
			t.Errorf("%s: %q is not a code the server makes", what, got[i])
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %v, want %v (go test ./api -run TestProblemCodes -update)", what, got, want)
	}
}

// analyse loads erp-server's packages of this module but the generated
// server (from export data: it makes no problem), builds the call graph
// and finds every code.
func analyse(t *testing.T) *codeAnalysis {
	t.Helper()
	deps, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps, Dir: ".."}, module+"/cmd/erp-server")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	packages.Visit(deps, nil, func(p *packages.Package) {
		if strings.HasPrefix(p.PkgPath, module+"/") && p.PkgPath != module+"/api/gen" {
			paths = append(paths, p.PkgPath)
		}
	})
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes |
		packages.NeedTypesInfo | packages.NeedImports, Dir: ".."}, paths...)
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("erp-server's packages do not load")
	}
	prog, _ := ssautil.Packages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	fns := ssautil.AllFunctions(prog)
	problem := prog.ImportedPackage(module + "/internal/platform/problem")
	a := &codeAnalysis{prog: prog, all: fns, fns: map[string]*ssa.Function{}, cg: vta.CallGraph(fns, cha.CallGraph(prog)), from: problem.Func("From"),
		gated: map[*ssa.Function][]string{}, globals: map[*ssa.Global][]string{}, unresolved: map[string][]token.Position{},
		constraints: map[string][]string{}, writes: map[*ssa.Function][]string{}, narrowed: map[int][]string{}}
	for f := range fns {
		a.fns[f.String()] = f
	}
	var emitted []emission
	for _, e := range a.cg.Nodes[problem.Func("New")].In {
		emitted = append(emitted, a.resolve(e.Caller.Func, e.Site, e.Site.Common().Args[1], map[ssa.Value]bool{})...)
	}
	for _, e := range emitted {
		a.gated[e.gate] = append(a.gated[e.gate], e.code)
	}
	for fn, codes := range dynamicCodes {
		for f := range fns {
			if f.String() == fn {
				a.gated[f] = append(a.gated[f], codes...)
			}
		}
	}
	for f := range fns {
		a.writes[f] = tablesWritten(f)
		a.codeAssigned(f, problem)
	}
	a.keptInVariables(emitted)
	a.registered(t, problem.Func("RegisterConstraint"))
	return a
}

// resolve traces a code value to its constants, each with the function
// supplying it and the call it goes into there.
func (a *codeAnalysis) resolve(gate *ssa.Function, site ssa.CallInstruction, v ssa.Value, seen map[ssa.Value]bool) []emission {
	if seen[v] {
		return nil
	}
	seen[v] = true
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			return []emission{{gate, site, constant.StringVal(v.Value)}}
		}
	case *ssa.Phi:
		var out []emission
		for _, e := range v.Edges {
			out = append(out, a.resolve(gate, site, e, seen)...)
		}
		return out
	case *ssa.ChangeType:
		return a.resolve(gate, site, v.X, seen)
	case *ssa.Parameter:
		return a.fromCallers(v, seen)
	case *ssa.FreeVar:
		return a.fromClosures(gate, site, v, seen)
	case *ssa.Call:
		if g := v.Call.StaticCallee(); g != nil {
			var out []emission
			for _, b := range g.Blocks {
				if r, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && len(r.Results) == 1 {
					out = append(out, a.resolve(gate, site, r.Results[0], seen)...)
				}
			}
			return out
		}
	}
	at := v.Parent().String()
	a.unresolved[at] = append(a.unresolved[at], a.prog.Fset.Position(v.Pos()))
	return nil
}

// fromCallers resolves a parameter at every call of its function.
func (a *codeAnalysis) fromCallers(p *ssa.Parameter, seen map[ssa.Value]bool) []emission {
	var out []emission
	a.eachArgument(p, func(e *callgraph.Edge, arg ssa.Value) {
		out = append(out, a.resolve(e.Caller.Func, e.Site, arg, seen)...)
	})
	return out
}

// eachArgument calls do with the argument every call of p's function
// passes for p; none when nothing calls it.
func (a *codeAnalysis) eachArgument(p *ssa.Parameter, do func(*callgraph.Edge, ssa.Value)) {
	fn := p.Parent()
	idx := slices.Index(fn.Params, p)
	node := a.cg.Nodes[fn]
	if node == nil {
		return
	}
	for _, e := range node.In {
		c, i := e.Site.Common(), idx
		if c.IsInvoke() && fn.Signature.Recv() != nil {
			i-- // an interface call's receiver is not among its arguments
		}
		if i >= 0 && i < len(c.Args) {
			do(e, c.Args[i])
		}
	}
}

// fromClosures resolves a closure's free variable at the closure's making.
func (a *codeAnalysis) fromClosures(gate *ssa.Function, site ssa.CallInstruction, v *ssa.FreeVar, seen map[ssa.Value]bool) []emission {
	fn := v.Parent()
	idx := slices.Index(fn.FreeVars, v)
	var out []emission
	if p := fn.Parent(); p != nil {
		for _, b := range p.Blocks {
			for _, in := range b.Instrs {
				if mc, ok := in.(*ssa.MakeClosure); ok && mc.Fn == fn {
					out = append(out, a.resolve(gate, site, mc.Bindings[idx], seen)...)
				}
			}
		}
	}
	return out
}

// codeAssigned reports, as a site the analysis cannot trace, a function
// outside package problem that sets a problem's Code: the code would not
// be problem.New's argument.
func (a *codeAnalysis) codeAssigned(f *ssa.Function, problem *ssa.Package) {
	if f.Pkg == problem {
		return
	}
	for _, pos := range fieldStores(f, problem.Pkg.Path(), "Problem", "Code") {
		a.unresolved[f.String()] = append(a.unresolved[f.String()], a.prog.Fset.Position(pos))
	}
}

// fieldStores is where f may set a field of a struct type of package
// pkg: every use of the field's address but a load (a store, or the
// address passed on, as to a row's Scan).
func fieldStores(f *ssa.Function, pkg, typ, field string) []token.Pos {
	var out []token.Pos
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			fa, ok := in.(*ssa.FieldAddr)
			if !ok || !isField(fa, pkg, typ, field) {
				continue
			}
			for _, use := range *fa.Referrers() {
				if load, ok := use.(*ssa.UnOp); !ok || load.Op != token.MUL {
					out = append(out, use.Pos())
				}
			}
		}
	}
	return out
}

// isField reports whether fa is the address of the field of the struct
// type of package pkg.
func isField(fa *ssa.FieldAddr, pkg, typ, field string) bool {
	ptr, ok := fa.X.Type().Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkg && named.Obj().Name() == typ &&
		named.Underlying().(*types.Struct).Field(fa.Field).Name() == field
}

// tablesWritten is the tables the statements in a function's own body
// write.
func tablesWritten(f *ssa.Function) []string {
	var out []string
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			for _, op := range in.Operands(nil) {
				if c, ok := (*op).(*ssa.Const); ok && c.Value != nil && c.Value.Kind() == constant.String {
					for _, m := range written.FindAllStringSubmatch(constant.StringVal(c.Value), -1) {
						out = append(out, strings.ToLower(m[1]))
					}
				}
			}
		}
	}
	return out
}

// keptInVariables finds the problems package initializers keep in
// variables: those made at the stored call, and those its callee makes.
func (a *codeAnalysis) keptInVariables(emitted []emission) {
	for _, pkg := range a.prog.AllPackages() {
		init := pkg.Func("init")
		if init == nil {
			continue
		}
		for _, b := range init.Blocks {
			for _, in := range b.Instrs {
				st, ok := in.(*ssa.Store)
				if !ok {
					continue
				}
				g, ok := st.Addr.(*ssa.Global)
				if !ok {
					continue
				}
				call, ok := unwrap(st.Val).(*ssa.Call)
				if !ok {
					continue
				}
				for _, e := range emitted {
					if e.site == call {
						a.globals[g] = append(a.globals[g], e.code)
					}
				}
				if callee := call.Call.StaticCallee(); callee != nil {
					a.globals[g] = append(a.globals[g], keys(a.reach([]*ssa.Function{callee}).codes)...)
				}
			}
		}
	}
}

func unwrap(v ssa.Value) ssa.Value {
	for {
		switch x := v.(type) {
		case *ssa.MakeInterface:
			v = x.X
		case *ssa.ChangeType:
			v = x.X
		case *ssa.ChangeInterface:
			v = x.X
		default:
			return v
		}
	}
}

// registered finds the codes of the problems registered for constraints,
// by the table each constraint is on (constraintTables).
func (a *codeAnalysis) registered(t *testing.T, register *ssa.Function) {
	t.Helper()
	tables := constraintTables(t)
	for _, e := range a.cg.Nodes[register].In {
		args := e.Site.Common().Args
		name, ok := args[0].(*ssa.Const)
		if !ok {
			t.Fatalf("%s registers a constraint whose name is not a constant", e.Caller.Func)
		}
		var fn *ssa.Function
		switch p := args[1].(type) {
		case *ssa.MakeClosure:
			fn = p.Fn.(*ssa.Function)
		case *ssa.Function:
			fn = p
		default:
			t.Fatalf("%s registers constraint %s with a problem made elsewhere", e.Caller.Func, name.Value)
		}
		on := tables(constant.StringVal(name.Value))
		if len(on) == 0 {
			a.unplaced = append(a.unplaced, constant.StringVal(name.Value))
			continue
		}
		codes := keys(a.reach([]*ssa.Function{fn}).codes)
		for _, tbl := range on {
			a.constraints[tbl] = append(a.constraints[tbl], codes...)
		}
	}
}

// The migrations' statements constraintTables reads.
var (
	indexStmt  = regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_0-9]+)\s+ON\s+(?:ONLY\s+)?([a-z_0-9]+\.[a-z_0-9]+)`)
	tableStmt  = regexp.MustCompile(`(?is)^\s*(?:CREATE|ALTER)\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:ONLY\s+)?([a-z_0-9]+\.[a-z_0-9]+)`)
	namedIn    = regexp.MustCompile(`(?i)\bCONSTRAINT\s+([a-z_0-9]+)`)
	function   = regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+([a-z_0-9]+\.[a-z_0-9]+)\s*\(.*?\$\$(.*?)\$\$`)
	raises     = regexp.MustCompile(`(?i)\bCONSTRAINT\s*=\s*'([a-z_0-9]+)'`)
	trigger    = regexp.MustCompile(`(?is)CREATE\s+(?:CONSTRAINT\s+)?TRIGGER\s+[a-z_0-9]+\s+[^;]*?\bON\s+([a-z_0-9]+\.[a-z_0-9]+)[^;]*?EXECUTE\s+(?:FUNCTION|PROCEDURE)\s+([a-z_0-9]+\.[a-z_0-9]+)`)
	sqlComment = regexp.MustCompile(`--[^\n]*`)
)

// constraintTables reads the migrations and returns the tables a
// constraint name is on: a named constraint's or index's where it is
// created; a name a trigger function raises errors with, the tables whose
// triggers run that function; otherwise (PostgreSQL's default names,
// <table>_..._pkey, _key, _fkey, _excl) the longest table name it begins
// with, when one table has that name.
func constraintTables(t *testing.T) func(name string) []string {
	t.Helper()
	byName, byShort := map[string]string{}, map[string][]string{}
	raisedBy, triggered := map[string][]string{}, map[string][]string{} // name -> functions, function -> tables
	for _, m := range db.Migrations() {
		sql := strings.ToLower(sqlComment.ReplaceAllString(m.Up, ""))
		for _, x := range indexStmt.FindAllStringSubmatch(sql, -1) {
			byName[x[1]] = x[2]
		}
		for _, x := range function.FindAllStringSubmatch(sql, -1) {
			for _, r := range raises.FindAllStringSubmatch(x[2], -1) {
				raisedBy[r[1]] = append(raisedBy[r[1]], x[1])
			}
		}
		for _, x := range trigger.FindAllStringSubmatch(sql, -1) {
			triggered[x[2]] = append(triggered[x[2]], x[1])
		}
		for _, stmt := range strings.Split(sql, ";") {
			x := tableStmt.FindStringSubmatch(stmt)
			if x == nil {
				continue
			}
			short := x[1][strings.Index(x[1], ".")+1:]
			if !slices.Contains(byShort[short], x[1]) {
				byShort[short] = append(byShort[short], x[1])
			}
			for _, c := range namedIn.FindAllStringSubmatch(stmt, -1) {
				byName[c[1]] = x[1]
			}
		}
	}
	return func(name string) []string {
		if tbl, ok := byName[name]; ok {
			return []string{tbl}
		}
		var tables []string
		for _, fn := range raisedBy[name] {
			tables = append(tables, triggered[fn]...)
		}
		if len(tables) > 0 {
			return tables
		}
		for i := len(name); i > 0; i-- {
			if i < len(name) && name[i] != '_' {
				continue // a prefix ends where a word does
			}
			if ts := byShort[name[:i]]; len(ts) == 1 {
				return ts
			}
		}
		return nil
	}
}

// reached is what a traversal found: the functions, their codes, and of
// those the codes of the variables they read.
type reached struct {
	fns   map[*ssa.Function]bool
	codes map[string]bool
	kept  map[string]bool
}

// reach walks the call graph from roots, as the file's comment says.
func (a *codeAnalysis) reach(roots []*ssa.Function) reached {
	r := reached{fns: map[*ssa.Function]bool{}, codes: map[string]bool{}, kept: map[string]bool{}}
	stack := slices.Clone(roots)
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f == nil || r.fns[f] {
			continue
		}
		r.fns[f] = true
		for _, c := range a.gated[f] {
			r.codes[c] = true
		}
		stack = append(stack, a.referenced(f, r.kept)...)
		if n := a.cg.Nodes[f]; n != nil {
			for _, e := range n.Out {
				if !a.cut(f, e) {
					stack = append(stack, e.Callee.Func)
				}
			}
		}
	}
	for c := range r.kept {
		r.codes[c] = true
	}
	return r
}

// referenced adds the codes of the variables f reads and returns the
// functions it makes values of, which escape and are taken as called.
func (a *codeAnalysis) referenced(f *ssa.Function, codes map[string]bool) []*ssa.Function {
	var out []*ssa.Function
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			if mc, ok := in.(*ssa.MakeClosure); ok {
				out = append(out, mc.Fn.(*ssa.Function))
			}
			for _, op := range in.Operands(nil) {
				switch v := (*op).(type) {
				case *ssa.Global:
					for _, c := range a.globals[v] {
						codes[c] = true
					}
				case *ssa.Function:
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// cut reports the edges the traversal does not follow: problem.From's
// constraint lookup, http.Handler.ServeHTTP through the interface, and a
// call of a function its caller was given (a callback: the graph does not
// tell one caller's from another's, and the functions each caller passes
// are taken as called where it is reached).
func (a *codeAnalysis) cut(f *ssa.Function, e *callgraph.Edge) bool {
	c := e.Site.Common()
	if c.StaticCallee() != nil {
		return false
	}
	if _, param := c.Value.(*ssa.Parameter); param && !c.IsInvoke() {
		return true
	}
	return f == a.from || (c.IsInvoke() && c.Method.Name() == "ServeHTTP")
}

// pipeline is what every request runs before and after its handler:
// httpapi.New's pipeline, with the unit of work.
func (a *codeAnalysis) pipeline() []*ssa.Function {
	return []*ssa.Function{a.prog.ImportedPackage(module + "/internal/platform/httpapi").Func("New")}
}

// operationCodes derives each operation's codes beyond the pipeline's,
// from its method on the API's handler.
func (a *codeAnalysis) operationCodes(t *testing.T, spec node, common []string) map[string][]string {
	t.Helper()
	handler := a.prog.ImportedPackage(module + "/internal/app").Type("handler").Type()
	methods := a.prog.MethodSets.MethodSet(handler)
	byName := map[string]*ssa.Function{}
	for i := range methods.Len() {
		sel := methods.At(i)
		byName[strings.ToLower(sel.Obj().Name())] = a.prog.MethodValue(sel)
	}
	out := map[string][]string{}
	for _, o := range operations(t, spec) {
		id, _ := o.op["operationId"].(string)
		m, ok := byName[strings.ToLower(id)]
		if !ok {
			t.Fatalf("%s: no method on the API's handler", id)
		}
		r := a.reach([]*ssa.Function{m})
		skip := a.untakenIn(r)
		codes := a.codesOf(r, skip)
		for i, u := range untakenBranches {
			if skip[a.fns[u.gate]][u.code] && !codes[u.code] {
				a.narrowed[i] = append(a.narrowed[i], id)
			}
		}
		for _, c := range common {
			delete(codes, c)
		}
		out[id] = keys(codes)
	}
	return out
}

// codesOf is a traversal's codes, but those skip says a gate makes on a
// branch the traversal never takes, with those of the constraints on the
// tables its functions write.
func (a *codeAnalysis) codesOf(r reached, skip map[*ssa.Function]map[string]bool) map[string]bool {
	codes := maps.Clone(r.kept)
	for f := range r.fns {
		for _, c := range a.gated[f] {
			if !skip[f][c] {
				codes[c] = true
			}
		}
		for _, tbl := range a.writes[f] {
			for _, c := range a.constraints[tbl] {
				codes[c] = true
			}
		}
	}
	return codes
}

// untakenIn is, by gate, the codes of untakenBranches a traversal reaches
// the gate of but none of the functions that make the input taking the
// branch.
func (a *codeAnalysis) untakenIn(r reached) map[*ssa.Function]map[string]bool {
	skip := map[*ssa.Function]map[string]bool{}
	for _, u := range untakenBranches {
		gate := a.fns[u.gate]
		if gate == nil || !r.fns[gate] || r.any(u.by) {
			continue
		}
		if skip[gate] == nil {
			skip[gate] = map[string]bool{}
		}
		skip[gate][u.code] = true
	}
	return skip
}

// any reports whether the traversal reached one of the functions named,
// or a closure in one.
func (r reached) any(names []string) bool {
	for f := range r.fns {
		if within(f, names) {
			return true
		}
	}
	return false
}

// within reports whether f is one of the functions named, or a closure in
// one.
func within(f *ssa.Function, names []string) bool {
	for ; f != nil; f = f.Parent() {
		if slices.Contains(names, f.String()) {
			return true
		}
	}
	return false
}

// checkUntaken checks each of untakenBranches against the program: its
// gate still makes its code, the functions it names exist, and its
// premise holds.
func (a *codeAnalysis) checkUntaken(t *testing.T) {
	t.Helper()
	for _, u := range untakenBranches {
		gate := a.fns[u.gate]
		sites := 0
		if gate != nil {
			sites = len(slices.DeleteFunc(slices.Clone(a.gated[gate]), func(c string) bool { return c != u.code }))
		}
		switch {
		case sites == 0:
			t.Errorf("untakenBranches: %s no longer makes %s", u.gate, u.code)
			continue
		case sites > 1:
			t.Errorf("untakenBranches: %s makes %s at %d sites, which the entry cannot tell apart", u.gate, u.code, sites)
		}
		for _, fn := range u.by {
			if a.fns[fn] == nil {
				t.Errorf("untakenBranches, %s: %s no longer exists", u.code, fn)
			}
		}
		if len(u.fields) == 0 && u.param == "" {
			t.Errorf("untakenBranches, %s: no premise the test can check (fields or param)", u.code)
		}
		for _, fl := range u.fields {
			a.checkFieldPremise(t, u, fl)
		}
		if u.param != "" {
			a.checkParamPremise(t, u, gate)
		}
	}
}

// checkFieldPremise checks that the functions of u alone set the field.
func (a *codeAnalysis) checkFieldPremise(t *testing.T, u untaken, fl [3]string) {
	t.Helper()
	set := false
	for f := range a.all {
		if pos := fieldStores(f, fl[0], fl[1], fl[2]); len(pos) > 0 {
			set = true
			if !within(f, u.by) {
				t.Errorf("untakenBranches, %s: %s sets %s.%s (%v), so %v are not alone in taking the branch", u.code, f, fl[1], fl[2],
					a.prog.Fset.Position(pos[0]), u.by)
			}
		}
	}
	if !set {
		t.Errorf("untakenBranches, %s: nothing sets %s.%s any more", u.code, fl[1], fl[2])
	}
}

// checkParamPremise checks that every value reaching the gate's
// parameter is a constant other than the one taking the branch, or comes
// from a parameter of a function of u.
func (a *codeAnalysis) checkParamPremise(t *testing.T, u untaken, gate *ssa.Function) {
	t.Helper()
	i := slices.IndexFunc(gate.Params, func(p *ssa.Parameter) bool { return p.Name() == u.param })
	if i < 0 {
		t.Errorf("untakenBranches, %s: %s has no parameter %s", u.code, u.gate, u.param)
		return
	}
	seen := map[ssa.Value]bool{}
	var check func(v ssa.Value, in *ssa.Function)
	check = func(v ssa.Value, in *ssa.Function) {
		if seen[v] {
			return
		}
		seen[v] = true
		switch v := v.(type) {
		case *ssa.Const:
			if v.Value != nil && v.Value.Kind() == constant.String && constant.StringVal(v.Value) != u.taking {
				return
			}
		case *ssa.Phi:
			for _, e := range v.Edges {
				check(e, in)
			}
			return
		case *ssa.ChangeType:
			check(v.X, in)
			return
		case *ssa.Parameter:
			if !within(v.Parent(), u.by) {
				a.eachArgument(v, func(e *callgraph.Edge, arg ssa.Value) { check(arg, e.Caller.Func) })
			}
			return
		}
		t.Errorf("untakenBranches, %s: %s's %s may be %q, as %s passes it", u.code, u.gate, u.param, u.taking, in)
	}
	check(gate.Params[i], gate)
}

// allCodes is every code the server can make.
func (a *codeAnalysis) allCodes() map[string]bool {
	all := map[string]bool{}
	for _, cs := range a.gated {
		for _, c := range cs {
			all[c] = true
		}
	}
	for _, cs := range a.constraints {
		for _, c := range cs {
			all[c] = true
		}
	}
	return all
}

func keys(m map[string]bool) []string { return slices.Sorted(maps.Keys(m)) }

// writeCodes rewrites openapi.yaml's x-problem-codes: the document's
// before paths, each operation's before its parameters, body or responses.
func writeCodes(t *testing.T, common []string, byOperation map[string][]string) {
	t.Helper()
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	lines := strings.Split(string(raw), "\n")
	opID, inCodes, codesIndent := "", false, 0
	for _, line := range lines {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)
		switch {
		case inCodes && trimmed != "" && (indent > codesIndent || (indent == codesIndent && strings.HasPrefix(trimmed, "- "))):
			continue // the old list's items
		case trimmed == "x-problem-codes:" || strings.HasPrefix(trimmed, "x-problem-codes: "):
			inCodes, codesIndent = true, indent
			continue
		}
		inCodes = false
		switch {
		case line == "paths:":
			out = append(out, block("", common)...)
		case strings.HasPrefix(line, "      operationId: "):
			opID = strings.TrimPrefix(line, "      operationId: ")
		case opID != "" && indent == 6 && (trimmed == "parameters:" || trimmed == "requestBody:" || trimmed == "responses:"):
			out = append(out, block("      ", byOperation[opID])...)
			opID = ""
		}
		out = append(out, line)
	}
	if err := os.WriteFile("openapi.yaml", []byte(strings.Join(out, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// block is an x-problem-codes entry at indent.
func block(indent string, codes []string) []string {
	if len(codes) == 0 {
		return []string{indent + "x-problem-codes: []"}
	}
	out := []string{indent + "x-problem-codes:"}
	for _, c := range codes {
		out = append(out, indent+"  - "+c)
	}
	return out
}
