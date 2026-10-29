// Package fieldcalls is TestProblemCodesFieldCalls' program (ADR-0052 G7):
// calls of a function value held in a field of a struct parameter, which
// the call graph joins across callers, told apart where every caller makes
// the value, and followed where one may not.
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
