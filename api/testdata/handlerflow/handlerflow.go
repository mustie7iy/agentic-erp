// Package handlerflow is the program checkSealRefuses gives flowsTo
// (ADR-0052 G7): a handler converted to the server's interface, handed on
// to NewServer through a parameter, kept in a variable, converted to
// any, or given to a function value.
package handlerflow

import "github.com/mustie7iy/agentic-erp/internal/platform/problem"

// build traces problem.New's calls, so the program has one.
var _ = problem.New

type Handler interface{ Serve() }

type handler struct{}

func (handler) Serve() {}

func NewServer(Handler) {}

func wire(h Handler) { NewServer(h) }

// Wired hands the handler to NewServer through wire's parameter.
func Wired() { wire(handler{}) }

var kept Handler

// Kept keeps it in a variable as well.
func Kept() {
	var h Handler = handler{}
	kept = h
	NewServer(h)
}

// Printed converts it to any, which the test does not follow.
func Printed() {
	var h Handler = handler{}
	println(any(h))
	NewServer(h)
}

var register = func(Handler) {}

// Registered gives it to a function value, whose callee the test does not
// follow.
func Registered() {
	var h Handler = handler{}
	register(h)
	NewServer(h)
}

// twice hands its first parameter on and keeps its second.
func twice(h, k Handler) {
	kept = k
	NewServer(h)
}

// Doubled passes the handler as both.
func Doubled() {
	var h Handler = handler{}
	twice(h, h)
}
