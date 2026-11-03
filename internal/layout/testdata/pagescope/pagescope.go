// Package pagescope is TestPageScopesRefuse's fixture: one listing per case,
// each giving the page-token codec a scope built one way. The names of the
// bad ones are the findings the test wants.
package pagescope

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

type opt struct {
	Value bool
	Set   bool
}

type filter struct{ standard *bool }

type held struct{ scope string }

type named struct{ code string }

func (n named) String() string { return n.code }

var c *pagination.Codec

var sort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "code", Type: "text"}}}

var packageScope = fmt.Sprintf("list|%v", new(bool))

func list(scope string) { _, _ = c.Keyset(sort, scope, pagination.Request{}) }

func PointerFormatted(p *bool) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v", p), pagination.Request{})
}

func PointerInVariable(p *bool) {
	scope := fmt.Sprintf("list|%v", p)
	_, _ = c.Keyset(sort, scope, pagination.Request{})
}

func helper(p *bool) string { return fmt.Sprint(p) }

func PointerInHelper(p *bool) { _, _ = c.Keyset(sort, "list|"+helper(p), pagination.Request{}) }

func PointerInField(p *bool) {
	h := held{scope: fmt.Sprintf("list|%v", p)}
	_, _ = c.Keyset(sort, h.scope, pagination.Request{})
}

func listScope(scope string) { _, _ = c.Keyset(sort, scope, pagination.Request{}) }

func PointerInParameter(p *bool) { listScope(fmt.Sprintf("list|%v", p)) }

func StructWithPointer(f filter) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v", f), pagination.Request{})
}

func InterfaceFormatted(v any) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v", v), pagination.Request{})
}

func MapFormatted(m map[string]bool) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v", m), pagination.Request{})
}

func SpreadArguments(args []any) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v", args...), pagination.Request{})
}

func DynamicCall(f func() string) { _, _ = c.Keyset(sort, f(), pagination.Request{}) }

func PointerEncoded(p *bool) { _, _ = c.Encode(fmt.Sprintf("list|%v", p), nil) }

func PointerConverted(p *bool) { _ = c.Decode(string(fmt.Append(nil, p)), "", nil) }

func PointerSprint(p *bool) { _, _ = c.Keyset(sort, fmt.Sprint("list|", p), pagination.Request{}) }

func PointerInClosure() {
	f := func(p *bool) { _, _ = c.Keyset(sort, fmt.Sprintf("list|%v", p), pagination.Request{}) }
	f(nil)
	g := func(scope string) { _, _ = c.Keyset(sort, scope, pagination.Request{}) }
	g("x")
}

func PointerInPackageVar() { _, _ = c.Keyset(sort, packageScope, pagination.Request{}) }

func scopeAndError(p *bool) (string, error) { return fmt.Sprint(p), nil }

func PointerFromTuple(p *bool) {
	scope, _ := scopeAndError(p)
	_, _ = c.Keyset(sort, scope, pagination.Request{})
}

func OptValue(o opt) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v|%v", o.Value, o), pagination.Request{})
}

func Dereferenced(p *bool) {
	v := ""
	if p != nil {
		v = fmt.Sprint(*p)
	}
	_, _ = c.Keyset(sort, "list|"+v, pagination.Request{})
}

func Stringer(n named, t time.Time) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v|%s", n, t), pagination.Request{})
}

func Constant() { _, _ = c.Keyset(sort, "list", pagination.Request{}) }

func Concatenated(code string, n int) {
	_, _ = c.Keyset(sort, "list|"+code+"|"+fmt.Sprint(n), pagination.Request{})
}

func valueHelper(n int) string { return fmt.Sprintf("%d", n) }

func HelperOfValues(n int) { _, _ = c.Keyset(sort, "list|"+valueHelper(n), pagination.Request{}) }

func FieldOfValues(n named) {
	h := struct{ scope string }{scope: "list|" + n.code}
	_, _ = c.Keyset(sort, h.scope, pagination.Request{})
}

func ParameterOfValue(code string) { list("list|" + code) }

func NilableStringer(n *named) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v", n), pagination.Request{})
}

type scoper interface{ Scope() string }

type pointerScoper struct{ p *bool }

func (s pointerScoper) Scope() string { return fmt.Sprint(s.p) }

func InterfaceMethod(s scoper) { _, _ = c.Keyset(sort, s.Scope(), pagination.Request{}) }

func LibraryArguments(p *bool) {
	_, _ = c.Keyset(sort, strings.Join([]string{"list", fmt.Sprint(p)}, "|"), pagination.Request{})
}

func Builder(p *bool) {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "list|%v", p)
	_, _ = c.Keyset(sort, b.String(), pagination.Request{})
}

func RangeVariable(p *bool) {
	for _, s := range []string{fmt.Sprint(p)} {
		_, _ = c.Keyset(sort, s, pagination.Request{})
	}
}

// FunctionValue is called only as a value, whose calls the check does not
// see.
func FunctionValue(scope string) { _, _ = c.Keyset(sort, scope, pagination.Request{}) }

func ListThroughValue(p *bool) {
	f := FunctionValue
	f(fmt.Sprint(p))
}

type number int

func (n number) String() string { return strconv.Itoa(int(n)) }

// PointerWrongVerb formats under %d, for which fmt does not call String.
func PointerWrongVerb(n *number) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%d", n), pagination.Request{})
}

func UnexportedPointerField(t *time.Time) {
	_, _ = c.Keyset(sort, fmt.Sprintf("list|%v", struct{ t *time.Time }{t}), pagination.Request{})
}

func ElementOfPointer(p *bool) {
	parts := make([]string, 1)
	parts[0] = fmt.Sprint(p)
	_, _ = c.Keyset(sort, strings.Join(parts, "|"), pagination.Request{})
}

type lister interface{ list(scope string) }

type listing struct{}

func (listing) list(scope string) { _, _ = c.Keyset(sort, scope, pagination.Request{}) }

func DispatchedPointer(l lister, p *bool) { l.list(fmt.Sprint(p)) }

func DispatchedValue(l lister, code string) { l.list("list|" + code) }

func JoinOfValues(code string, n int) {
	parts := make([]string, 0, 2)
	parts = append(parts, code, strconv.Itoa(n))
	_, _ = c.Keyset(sort, strings.Join(parts, "|"), pagination.Request{})
}

func order(desc bool) (cmp, dir string, err error) {
	cmp, dir = ">", "ASC"
	if desc {
		cmp, dir = "<", "DESC"
	}
	return
}

func TupleOfValues(desc bool) {
	_, dir, _ := order(desc)
	_, _ = c.Keyset(sort, "list|"+dir, pagination.Request{})
}

func joined(parts ...string) string { return strings.Join(parts, "|") }

func VariadicPointer(p *bool) {
	_, _ = c.Keyset(sort, joined("list", fmt.Sprint(p)), pagination.Request{})
}

func tail(head string, rest ...string) string { return head + strings.Join(rest, "|") }

func VariadicNone() { _, _ = c.Keyset(sort, tail("list"), pagination.Request{}) }

func VariadicSpread(p *bool) {
	_, _ = c.Keyset(sort, tail("list", []string{fmt.Sprint(p)}...), pagination.Request{})
}

type expression struct{}

func (expression) Scope(scope string) { _, _ = c.Keyset(sort, scope, pagination.Request{}) }

func MethodExpression(p *bool) { expression.Scope(expression{}, fmt.Sprint(p)) }

func BytesOfPointer(p *bool) {
	_, _ = c.Keyset(sort, string(bytes.TrimSpace(fmt.Append(nil, p))), pagination.Request{})
}

func ScannedInto(text string) {
	var scope string
	_, _ = fmt.Sscan(text, &scope)
	_, _ = c.Keyset(sort, scope, pagination.Request{})
}

func CopiedPointer(p *bool) {
	parts := make([]string, 1)
	copy(parts, []string{fmt.Sprint(p)})
	_, _ = c.Keyset(sort, strings.Join(parts, "|"), pagination.Request{})
}
