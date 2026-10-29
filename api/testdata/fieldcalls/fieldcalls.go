// Package fieldcalls is TestProblemCodesFieldCalls' program (ADR-0052 G7):
// calls of a function value given as a parameter or held in a field of a
// struct parameter, which the call graph joins across callers, told apart
// where every caller makes the value, and followed where one may not.
package fieldcalls

import "github.com/mustie7iy/agentic-erp/internal/platform/problem"

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
