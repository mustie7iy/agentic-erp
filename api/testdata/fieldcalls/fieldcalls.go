// Package fieldcalls is TestProblemCodesFieldCalls' program (ADR-0052 G7):
// calls of a function value given as a parameter or held in a field of a
// struct parameter, which the call graph joins across callers, told apart
// where every caller makes the value, and followed where one may not or
// where code outside the program can call the function.
package fieldcalls

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"sync"
	"text/template"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// step is a move and the work it does, as integration's runStep takes it.
type step struct {
	name string
	work func() error
}

// run calls the work its caller put in the step.
func run(st step) error {
	if st.name == "" {
		return nil
	}
	return st.work()
}

// Load and Reconcile give run work they make: each answers its own code.
func Load() error      { return run(step{name: "load", work: load}) }
func Reconcile() error { return run(step{name: "reconcile", work: reconcile}) }

func load() error      { return problem.New(409, "fixture.load", "Load", "") }
func reconcile() error { return problem.New(409, "fixture.reconcile", "Reconcile", "") }

// hooks is filled where it is wired, not where it is called.
type hooks struct{ work func() error }

var wired = hooks{work: wiredWork}

// runAny calls a step whose work Wired reads from a variable, so the
// edge stays: Direct's call may run any work the graph finds.
func runAny(st step) error { return st.work() }

func Wired() error  { return runAny(step{work: wired.work}) }
func Direct() error { return runAny(step{work: direct}) }

func wiredWork() error { return problem.New(409, "fixture.wired", "Wired", "") }
func direct() error    { return problem.New(409, "fixture.direct", "Direct", "") }

// runHooks calls work read through a pointer, which anything holding the
// pointer could have set (here the variable's initializer): the edge
// stays.
func runHooks(h *hooks) error { return h.work() }

var hooked = &hooks{work: pointed}

func Pointer() error { return runHooks(hooked) }

func pointed() error { return problem.New(409, "fixture.pointer", "Pointer", "") }

// Chosen gives run work a phi picks from two functions it makes.
func Chosen(second bool) error {
	work := first
	if second {
		work = secondWork
	}
	return run(step{name: "chosen", work: work})
}

func first() error      { return problem.New(409, "fixture.first", "First", "") }
func secondWork() error { return problem.New(409, "fixture.second", "Second", "") }

// spill sets the work on its own copy of the step before calling it.
func spill(st step) error {
	if st.name == "" {
		st.work = spilled
	}
	return st.work()
}

func Spilled() error { return spill(step{work: spilled}) }

func spilled() error { return problem.New(409, "fixture.spilled", "Spilled", "") }

// pass returns the work it is given: Returned's comes from a variable.
func pass(f func() error) func() error { return f }

func runReturned(st step) error { return st.work() }

var probes = hooks{work: returned}

func Returned() error       { return runReturned(step{work: pass(probes.work)}) }
func ReturnedDirect() error { return runReturned(step{work: pass(direct)}) }

func returned() error { return problem.New(409, "fixture.returned", "Returned", "") }

// fill puts the work its caller gave it in a step: Passed's comes from a
// variable.
func fill(f func() error) error { return runPassed(step{work: f}) }

func runPassed(st step) error { return st.work() }

var passedHooks = hooks{work: passed}

func Passed() error       { return fill(passedHooks.work) }
func PassedDirect() error { return fill(direct) }

func passed() error { return problem.New(409, "fixture.passed", "Passed", "") }

// Captured's closure fills the step with work its maker captured from a
// variable.
func runCaptured(st step) error { return st.work() }

var capturedHooks = hooks{work: captured}

func Captured() error {
	w := capturedHooks.work
	return func() error { return runCaptured(step{work: w}) }()
}

func CapturedDirect() error { return runCaptured(step{work: direct}) }

func captured() error { return problem.New(409, "fixture.captured", "Captured", "") }

// callP calls the function value it is given. CallVar passes one read from
// a package variable, which holdsMade cannot prove made, so the edge stays
// and CallVar keeps its code (CallMade, beside it, then gets both).
func callP(f func() error) error { return f() }

var hookP = varWork

func CallVar() error  { return callP(hookP) }
func CallMade() error { return callP(madeWork) }

func varWork() error  { return problem.New(409, "fixture.var", "Var", "") }
func madeWork() error { return problem.New(409, "fixture.made", "Made", "") }

// callQ is given only functions its callers make: the graph joins them,
// and the cut leaves each its own code.
func callQ(f func() error) error { return f() }

func CallQA() error { return callQ(qa) }
func CallQB() error { return callQ(qb) }

func qa() error { return problem.New(409, "fixture.qa", "QA", "") }
func qb() error { return problem.New(409, "fixture.qb", "QB", "") }

// callE is given only functions its callers make, but its value goes to
// code outside the program (a sync.Map), which may call it with any
// function: the edge stays, and each caller gets both codes.
func callE(f func() error) error { return f() }

var registry sync.Map

func Register() { registry.Store("e", callE) }

func CallEA() error { return callE(ea) }
func CallEB() error { return callE(eb) }

func ea() error { return problem.New(409, "fixture.ea", "EA", "") }
func eb() error { return problem.New(409, "fixture.eb", "EB", "") }

// Runner's Run is given only functions its callers make, but a *Runner goes
// to code outside the program in an interface (fmt.Sprint), which could
// call Run with any function: the edge stays.
type Runner struct{}

func (*Runner) Run(f func() error) error { return f() }

func Show() string { return fmt.Sprint(&Runner{}) }

func CallRA() error { return (&Runner{}).Run(ra) }
func CallRB() error { return (&Runner{}).Run(rb) }

func ra() error { return problem.New(409, "fixture.ra", "RA", "") }
func rb() error { return problem.New(409, "fixture.rb", "RB", "") }

// A *Keeper in an interface stays in the program (a variable whose loads
// it calls Keep on), so Keep is cut to each caller's own code.
type keeper interface{ Keep(f func() error) error }

type Keeper struct{}

func (*Keeper) Keep(f func() error) error { return f() }

var kept keeper = &Keeper{}

func CallKA() error { return kept.Keep(ka) }
func CallKB() error { return (&Keeper{}).Keep(kb) }

func ka() error { return problem.New(409, "fixture.ka", "KA", "") }
func kb() error { return problem.New(409, "fixture.kb", "KB", "") }

// Render executes a template with data whose type it does not show:
// TestProblemCodesEscapes' templateData refuses it.
func Render(w io.Writer, data any) error {
	return template.Must(template.New("t").Parse("{{.}}")).Execute(w, data)
}

// RenderPage's template may call any exported method of what its data
// holds: a *Page in an exported field, whose Title then escapes.
type Page struct{}

func (*Page) Title(f func() error) error { return f() }

type pageData struct{ P *Page }

func RenderPage(w io.Writer) error {
	return template.Must(template.New("p").Parse("{{.P}}")).Execute(w, pageData{P: &Page{}})
}

// The ways escapes follows a value, each in TestProblemCodesEscapes:
// callF is stored in a field of the program's struct and read back into
// fmt.Sprint's arguments (a slice); callS is stored in another struct's,
// read back only to be called (every value of a struct type shares its
// fields, so a second type keeps them apart); callR is
// returned by giveR, whose result goes to the sync.Map; Hook's closure is
// stored in flag.Usage, a variable of code outside; callL is stored in a
// literal struct with an exported field, which code outside can spell; a
// *Gen instantiates slices.Contains, which has no body here.
type slot struct{ f func(func() error) error }

type keptSlot struct{ f func(func() error) error }

func callF(f func() error) error { return f() }
func callS(f func() error) error { return f() }
func callR(f func() error) error { return f() }
func callL(f func() error) error { return f() }

func Slots() string {
	held, kept := slot{f: callF}, keptSlot{f: callS}
	_ = kept.f(qa)
	return fmt.Sprint(held.f)
}

func giveR() func(func() error) error { return callR }

func RegisterR() { registry.Store("r", giveR()) }

func Hook() { flag.Usage = func() { _ = callQ(qa) } }

func Literal() string { return fmt.Sprint(struct{ F func(func() error) error }{F: callL}) }

type Gen struct{}

func (*Gen) Run(f func() error) error { return f() }

func Generic() bool { return slices.Contains([]*Gen{}, nil) }

// A template's data holding a value of interface type, or a function, in
// a field could reach anything, an error's too (a template may call any
// method of its value): RenderAny, RenderFunc and RenderErr are refused.
// RenderChain's template reaches a *Sub through Book.Child's result, so
// Sub's Do escapes.
type anyData struct{ P any }

type funcData struct{ F func(func() error) error }

func RenderAny(w io.Writer) error {
	return template.Must(template.New("a").Parse("{{.P}}")).Execute(w, anyData{P: &Page{}})
}

func RenderFunc(w io.Writer) error {
	return template.Must(template.New("f").Parse("{{.F}}")).Execute(w, funcData{F: callQ})
}

type errData struct{ E error }

func RenderErr(w io.Writer) error {
	return template.Must(template.New("e").Parse("{{.E}}")).Execute(w, errData{E: io.EOF})
}

type Book struct{}

type Sub struct{}

func (*Book) Child() *Sub { return &Sub{} }

func (*Sub) Do(f func() error) error { return f() }

func RenderChain(w io.Writer) error {
	return template.Must(template.New("c").Parse("{{.Child}}")).Execute(w, &Book{})
}

// RenderFuncs registers a template function, whose result the template
// may call into, HandOn converts a template to an interface and HandOff
// gives one to a function value, either of which may execute it with any
// data: all refused.
func RenderFuncs(w io.Writer) error {
	t := template.Must(template.New("s").Funcs(template.FuncMap{"sub": func() *Sub { return &Sub{} }}).Parse("{{sub}}"))
	return t.Execute(w, 0)
}

func HandOn(w io.Writer) {
	t := template.Must(template.New("h").Parse("{{.}}"))
	_, _ = fmt.Fprintln(w, t.Name(), t)
}

var runTemplate = func(*template.Template) {}

func HandOff() { runTemplate(template.New("o")) }
