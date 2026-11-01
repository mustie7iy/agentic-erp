package rules

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Formula is an amount computed from other amounts by a CEL expression,
// such as a tax return's box from other boxes (ADR-0049 §11: 109 = 105 -
// 108): the same environment as a rule's, with no time functions, no
// floating-point numbers and the same limits, whose inputs are amounts in
// minor units of one currency, as CEL integers, and whose result is one.
// CEL's integer arithmetic fails on overflow rather than wrapping.
type Formula struct{ prg cel.Program }

// ErrFormula is an expression that does not compile to an amount over the
// inputs: a syntax or type error, an unknown input, or a result that is
// not an integer.
var ErrFormula = errors.New("rules: not a formula over the inputs")

// CompileFormula compiles expr over the named inputs. An input not in
// inputs is unknown, so a formula over earlier boxes cannot reach a later
// one.
func CompileFormula(expr string, inputs []string) (*Formula, error) {
	opts := baseOptions()
	for _, in := range inputs {
		opts = append(opts, cel.Variable(in, cel.IntType))
	}
	// The library options are fixed and the inputs are only declared, so
	// the environment cannot fail to build (as Compile's).
	e, _ := cel.NewCustomEnv(opts...)
	ast, iss := e.Compile(expr)
	if iss.Err() != nil {
		return nil, fmt.Errorf("%w: %s", ErrFormula, strings.TrimSpace(iss.Err().Error()))
	}
	if !ast.OutputType().IsExactType(cel.IntType) {
		return nil, fmt.Errorf("%w: it must be an amount (an integer of minor units), not %s", ErrFormula, ast.OutputType())
	}
	// The AST was checked by this environment and the options are fixed,
	// so planning the program cannot fail.
	prg, _ := e.Program(ast, cel.CostLimit(CostLimit), cel.InterruptCheckFrequency(1))
	return &Formula{prg: prg}, nil
}

// Eval computes the formula from the inputs' amounts, each given. An
// overflow, a division by zero or the cost or time limit fails it.
func (f *Formula) Eval(ctx context.Context, in map[string]money.Minor) (money.Minor, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	vars := make(map[string]any, len(in))
	for k, v := range in {
		vars[k] = int64(v)
	}
	out, _, err := f.prg.ContextEval(ctx, vars)
	if err != nil {
		return 0, fmt.Errorf("rules: formula: %w", err)
	}
	n := out.(types.Int) // the checked output type is int
	if n == math.MinInt64 {
		return 0, fmt.Errorf("rules: formula: %w", money.ErrOverflow)
	}
	return money.Minor(n), nil
}
