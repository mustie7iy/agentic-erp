package countrypack

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// testReturns is a test return of three boxes, not any country's: output
// tax, input tax recovered and their difference.
const testReturns = `returns:
  - type: TEST-1
    name: Test return (not any country's)
    prior_period_threshold: "10000"
    boxes:
      - code: out
        label: Output tax
        select: {direction: output, kinds: [standard, zero], measure: tax, negate: true}
      - code: in
        label: Input tax recovered
        select: {direction: input, components: [vat], jurisdictions: [ZZ, ZZ-01], account_keys: [input], measure: recoverable}
      - code: net
        label: Net tax
        expression: box_out - box_in
`

// returnsPack is the test pack with testReturns.
func returnsPack(t *testing.T) *Pack {
	t.Helper()
	files := map[string][]byte{"returns.yaml": []byte(testReturns)}
	for _, f := range Files {
		files[f] = nil
	}
	parsed, err := Parse(files)
	if err != nil {
		t.Fatal(err)
	}
	p := testPack(t, "2026.1")
	p.Returns = parsed.Returns
	return p
}

// TestReturns: a pack declares its returns' boxes (ADR-0049 §11, T61h),
// selections of tax items or expressions over earlier boxes, found by
// type and evaluated in order.
func TestReturns(t *testing.T) {
	p := returnsPack(t)
	if fs := Validate(p); len(fs) != 0 {
		t.Fatalf("Validate: got %v, want no findings", fs)
	}
	if p.Return("TEST-2") != nil {
		t.Error("an undeclared return: got a definition, want nil")
	}
	d := p.Return("TEST-1")
	boxes, err := d.Evaluate(context.Background(), map[string]money.Minor{"out": 5_000, "in": 1_250})
	if err != nil {
		t.Fatal(err)
	}
	if got := boxes["net"]; got != 3_750 {
		t.Errorf("net: got %v, want 3750 minor", got)
	}
	if got, set := d.Threshold(2); got != 1_000_000 || !set {
		t.Errorf("threshold: got %v %v, want 1000000 minor, set", got, set)
	}
	t.Run("an overflow", func(t *testing.T) {
		if _, err := d.Evaluate(context.Background(), map[string]money.Minor{"out": math.MaxInt64, "in": -1}); err == nil ||
			!strings.Contains(err.Error(), "box net") {
			t.Errorf("got %v, want an error naming box net", err)
		}
	})
	t.Run("an expression not validated", func(t *testing.T) {
		bad := ReturnDefinition{Type: "BAD", Boxes: []Box{{Code: "x", Expression: "box_y"}}}
		if _, err := bad.Evaluate(context.Background(), nil); err == nil {
			t.Error("got nil, want an error")
		}
	})
	t.Run("no threshold", func(t *testing.T) {
		if _, set := (&ReturnDefinition{}).Threshold(2); set {
			t.Error("got set, want none")
		}
	})
}

func TestValidateReturns(t *testing.T) {
	r := func(p *Pack) *ReturnDefinition { return &p.Returns.Returns[0] }
	tests := []struct {
		name   string
		change func(p *Pack)
		path   string
	}{
		{"type", func(p *Pack) { r(p).Type = "test 1" }, "returns.yaml: returns[0].type"},
		{"type twice", func(p *Pack) { p.Returns.Returns = append(p.Returns.Returns, *r(p)) }, "returns.yaml: returns[1].type"},
		{"name", func(p *Pack) { r(p).Name = "" }, "returns.yaml: returns[0].name"},
		{"threshold", func(p *Pack) { r(p).PriorPeriodThreshold = "10000.00" }, "returns.yaml: returns[0].prior_period_threshold"},
		{"threshold's size", func(p *Pack) { r(p).PriorPeriodThreshold = "99999999999999" }, "returns.yaml: returns[0].prior_period_threshold"},
		{"no boxes", func(p *Pack) { r(p).Boxes = nil }, "returns.yaml: returns[0].boxes"},
		{"box code", func(p *Pack) { r(p).Boxes[0].Code = "Out" }, "returns.yaml: returns[0].boxes[Out]"},
		{"box code twice", func(p *Pack) { r(p).Boxes[1].Code = "out" }, "returns.yaml: returns[0].boxes[out]"},
		{"label", func(p *Pack) { r(p).Boxes[0].Label = "" }, "returns.yaml: returns[0].boxes[out].label"},
		{"both", func(p *Pack) { r(p).Boxes[0].Expression = "1" }, "returns.yaml: returns[0].boxes[out]"},
		{"neither", func(p *Pack) { r(p).Boxes[0].Select = nil }, "returns.yaml: returns[0].boxes[out]"},
		{"direction", func(p *Pack) { r(p).Boxes[0].Select.Direction = "both" }, "returns.yaml: returns[0].boxes[out].select.direction"},
		{"measure", func(p *Pack) { r(p).Boxes[0].Select.Measure = "gross" }, "returns.yaml: returns[0].boxes[out].select.measure"},
		{"kind", func(p *Pack) { r(p).Boxes[0].Select.Kinds = []string{"reduced"} }, "returns.yaml: returns[0].boxes[out].select.kinds"},
		{"component", func(p *Pack) { r(p).Boxes[1].Select.Components = []string{"VAT"} }, "returns.yaml: returns[0].boxes[in].select.components"},
		{"jurisdiction", func(p *Pack) { r(p).Boxes[1].Select.Jurisdictions = []string{"zz"} }, "returns.yaml: returns[0].boxes[in].select.jurisdictions"},
		{"account key", func(p *Pack) { r(p).Boxes[1].Select.AccountKeys = []string{"tax"} }, "returns.yaml: returns[0].boxes[in].select.account_keys"},
		{"a later box", func(p *Pack) { r(p).Boxes[0], r(p).Boxes[2] = r(p).Boxes[2], r(p).Boxes[0] }, "returns.yaml: returns[0].boxes[net].expression"},
		{"itself", func(p *Pack) { r(p).Boxes[2].Expression = "box_net" }, "returns.yaml: returns[0].boxes[net].expression"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := returnsPack(t)
			tt.change(p)
			if fs := Validate(p); !hasError(fs, tt.path) {
				t.Errorf("Validate: got %v, want an error at %q", fs, tt.path)
			}
		})
	}
}
