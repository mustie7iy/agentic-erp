package rules

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/env"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/ext"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// timeFunctions are left out of the standard library: with no way to make
// a timestamp or a duration, a rule cannot read the clock or do date
// arithmetic that would depend on the zone it runs in.
var timeFunctions = []string{
	"timestamp", "duration", "getFullYear", "getMonth", "getDate", "getDayOfMonth", "getDayOfWeek",
	"getDayOfYear", "getHours", "getMinutes", "getSeconds", "getMilliseconds",
}

// moneyType is Money's name in CEL.
const moneyType = "rules.Money"

func baseOptions() []cel.EnvOption {
	subset := env.NewLibrarySubset()
	for _, f := range timeFunctions {
		subset.AddExcludedFunctions(env.NewFunction(f))
	}
	return []cel.EnvOption{
		cel.StdLib(cel.StdLibSubset(subset)),
		cel.OptionalTypes(),
		ext.NativeTypes(reflect.TypeFor[Money](), ext.ParseStructTags(true)),
		cel.ParserExpressionSizeLimit(MaxExpressionLength),
		cel.ParserRecursionLimit(maxDepth),
		cel.ASTValidators(exact{}),
	}
}

// exact refuses what would make a rule inexact or its result depend on
// more than its inputs: a floating-point value anywhere (ADR-0005), and a
// comprehension over anything but a list, since a map's iteration order is
// not fixed (map and filter over one return its keys in a random order).
type exact struct{}

func (exact) Name() string { return "rules.exact" }

func (exact) Validate(_ *cel.Env, _ cel.ValidatorConfig, a *ast.AST, iss *cel.Issues) {
	for _, e := range ast.MatchDescendants(ast.NavigateAST(a), ast.AllMatcher()) {
		switch {
		case a.GetType(e.ID()).Kind() == types.DoubleKind:
			iss.ReportErrorAtID(e.ID(), "no floating-point numbers: amounts and quantities are exact (ADR-0005)")
		case e.Kind() == ast.ComprehensionKind && a.GetType(e.AsComprehension().IterRange().ID()).Kind() != types.ListKind:
			iss.ReportErrorAtID(e.ID(), "comprehensions range over lists only: a map's order is not fixed")
		}
	}
}

// Set is an entity's compiled rules, ready to evaluate, in name order.
type Set struct {
	Entity        string
	SchemaVersion int // the custom field schema the rules were checked against; 0 without one
	inputs        []input
	rules         []compiled
	adapter       types.Adapter // makes a Money the CEL object rules.Money
}

type compiled struct {
	name, message string
	prg           cel.Program
}

// Compile checks an entity's rules against ADR-0046 and compiles them
// against its standard fields and its custom field schema (nil when the
// entity has none), then runs each rule's unit cases. Issue paths are
// relative to the entity's rules. The set is usable only when there are no
// issues.
func Compile(entity string, schema *extensions.Compiled, rs map[string]Rule) (*Set, []extensions.Issue) {
	c := &compiler{}
	fields, ok := standard[entity]
	if !ok {
		c.issue("", "rules are not supported on %q; supported: %v", entity, slices.Sorted(maps.Keys(standard)))
		return nil, c.issues
	}
	if len(rs) > MaxRules {
		c.issue("", "at most %d rules per entity", MaxRules)
	}
	s := &Set{Entity: entity, inputs: slices.Clone(fields)}
	if schema != nil {
		s.SchemaVersion = schema.Version
		s.inputs = append(s.inputs, extensionInputs(schema)...)
	}
	// The library options are fixed and variables are only declared, so
	// the environment cannot fail to build.
	e, _ := cel.NewCustomEnv(append(baseOptions(), variables(s.inputs)...)...)
	s.adapter = e.CELTypeAdapter()
	for _, name := range slices.Sorted(maps.Keys(rs)) {
		if r, ok := c.rule(e, s, name, rs[name]); ok {
			s.rules = append(s.rules, r)
		}
	}
	return s, c.issues
}

func extensionInputs(schema *extensions.Compiled) []input {
	out := make([]input, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		out = append(out, input{"ext." + f.Key, kindTypes[f.Kind], !f.Required})
	}
	return out
}

// kindTypes are the custom field kinds that are not strings in CEL; every
// other kind (text, decimal, date, enum, partner) reads as a string.
var kindTypes = map[extensions.Kind]typ{extensions.Integer: tInt, extensions.Boolean: tBool, extensions.Money: tMoney}

func variables(in []input) []cel.EnvOption {
	out := make([]cel.EnvOption, len(in))
	for i, v := range in {
		t := celTypes[v.t]
		if v.optional {
			t = cel.OptionalType(t)
		}
		out[i] = cel.Variable(v.name, t)
	}
	return out
}

var celTypes = map[typ]*cel.Type{
	tString: cel.StringType, tInt: cel.IntType, tBool: cel.BoolType,
	tStrings: cel.ListType(cel.StringType), tMoney: cel.ObjectType(moneyType),
}

type compiler struct{ issues []extensions.Issue }

func (c *compiler) issue(path, format string, a ...any) {
	c.issues = append(c.issues, extensions.Issue{Path: path, Message: fmt.Sprintf(format, a...)})
}

// rule compiles one rule and runs its cases; ok is false when it does not
// compile.
func (c *compiler) rule(e *cel.Env, s *Set, name string, r Rule) (compiled, bool) {
	if !namePattern.MatchString(name) || len(name) > maxNameLength {
		c.issue(name, "rule names are snake_case, starting with a letter, at most %d characters", maxNameLength)
	}
	if n := utf8.RuneCountInString(r.Message); n == 0 || n > maxMessageLength {
		c.issue(name+".message", "a message of 1 to %d characters is required", maxMessageLength)
	}
	ast, iss := e.Compile(r.Expression)
	if iss.Err() != nil {
		c.issue(name+".expression", "%s", strings.TrimSpace(iss.Err().Error()))
		return compiled{}, false
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		c.issue(name+".expression", "must be boolean, not %s: in Phase 2 a rule only validates; it derives no values and routes nothing", ast.OutputType())
		return compiled{}, false
	}
	// The AST was checked by this environment and the options are fixed,
	// so planning the program cannot fail.
	prg, _ := e.Program(ast, cel.CostLimit(CostLimit), cel.InterruptCheckFrequency(1))
	out := compiled{name: name, message: r.Message, prg: prg}
	c.cases(s, out, r.Cases)
	return out, true
}

// cases runs a rule's unit cases: at least one it accepts and one it
// refuses, so the rule is shown to do both.
func (c *compiler) cases(s *Set, r compiled, cases []Case) {
	p := r.name + ".cases"
	if len(cases) > MaxCases {
		c.issue(p, "at most %d cases", MaxCases)
	}
	seen := map[bool]bool{}
	for i, k := range cases {
		seen[k.Valid] = true
		act, err := s.bind(k.Row, k.Extensions)
		if err != nil {
			c.issue(fmt.Sprintf("%s[%d]", p, i), "%v", err)
			continue
		}
		switch ok, err := r.eval(context.Background(), act); {
		case err != nil:
			c.issue(fmt.Sprintf("%s[%d]", p, i), "%v", err)
		case ok != k.Valid:
			c.issue(fmt.Sprintf("%s[%d]", p, i), "got valid %t, want %t", ok, k.Valid)
		}
	}
	if !seen[true] || !seen[false] {
		c.issue(p, "at least one case the rule accepts (valid: true) and one it refuses (valid: false)")
	}
}
