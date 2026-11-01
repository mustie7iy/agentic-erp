package countrypack

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// Returns is returns.yaml: the tax returns of the country, each a list of
// boxes computed from tax items (ADR-0049 §11, T61h). Optional: packs
// released before T61h have none, and a pack without a return's
// definition cannot compute it.
type Returns struct {
	Returns []ReturnDefinition `yaml:"returns"`
}

// ReturnDefinition is one return type's boxes, in the order they are
// computed and shown.
type ReturnDefinition struct {
	// Type is the code a tax registration's return_type names.
	Type string `yaml:"type"`
	Name string `yaml:"name"`
	// PriorPeriodThreshold is the tax, in whole units of the return's
	// currency, up to which items dated in an earlier period are reported
	// in the next return as a prior-period adjustment; above it a person
	// chooses an amendment or a voluntary disclosure (P7). Absent: always
	// the next return.
	PriorPeriodThreshold string `yaml:"prior_period_threshold,omitempty"`
	Boxes                []Box  `yaml:"boxes"`
}

// Box is one box of a return: a sum of the tax items Select chooses, or
// Expression over earlier boxes (box_<code>), never both.
type Box struct {
	Code       string     `yaml:"code"`
	Label      string     `yaml:"label"`
	Select     *Selection `yaml:"select,omitempty"`
	Expression string     `yaml:"expression,omitempty"`
}

// Selection chooses tax items by their direction and, when listed, kinds,
// components, jurisdictions and account keys, and sums one measure of
// them in local currency. Items are signed like the journal (debit
// positive), so output tax and its base, credits, are summed negated to
// show positive.
type Selection struct {
	Direction     string   `yaml:"direction"`
	Kinds         []string `yaml:"kinds,omitempty"`
	Components    []string `yaml:"components,omitempty"`
	Jurisdictions []string `yaml:"jurisdictions,omitempty"`
	AccountKeys   []string `yaml:"account_keys,omitempty"`
	Measure       string   `yaml:"measure"`
	Negate        bool     `yaml:"negate,omitempty"`
}

// The measures a box sums.
const (
	MeasureBase           = "base"
	MeasureTax            = "tax"
	MeasureRecoverable    = "recoverable"
	MeasureNonRecoverable = "non_recoverable"
)

var (
	boxCodeRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,19}$`)
	thresholdRe = regexp.MustCompile(`^[0-9]{1,13}$`)
	measures    = []string{MeasureBase, MeasureTax, MeasureRecoverable, MeasureNonRecoverable}
)

// Variable is the name a box's value has in a later box's expression.
func Variable(code string) string { return "box_" + code }

// Return is the pack's definition of a return type, or nil.
func (p *Pack) Return(returnType string) *ReturnDefinition {
	for i := range p.Returns.Returns {
		if p.Returns.Returns[i].Type == returnType {
			return &p.Returns.Returns[i]
		}
	}
	return nil
}

// Threshold is the prior-period threshold in minor units of a currency of
// exp decimals, and whether the definition sets one.
func (d *ReturnDefinition) Threshold(exp int) (money.Minor, bool) {
	if d.PriorPeriodThreshold == "" {
		return 0, false
	}
	// Validated: at most 13 digits, and a currency has at most 4 decimals
	// (ref.currency's exponent check), so it is a whole amount in range.
	m, _ := money.ParseDecimal(d.PriorPeriodThreshold, exp)
	return m, true
}

// Evaluate computes the expression boxes in order from the selection
// boxes' values, which sel holds by code, and returns every box's value
// by code.
func (d *ReturnDefinition) Evaluate(ctx context.Context, sel map[string]money.Minor) (map[string]money.Minor, error) {
	out := make(map[string]money.Minor, len(d.Boxes))
	vars := make(map[string]money.Minor, len(d.Boxes))
	names := make([]string, 0, len(d.Boxes))
	for _, b := range d.Boxes {
		v := sel[b.Code]
		if b.Select == nil {
			f, err := rules.CompileFormula(b.Expression, names)
			if err == nil {
				v, err = f.Eval(ctx, vars)
			}
			if err != nil {
				return nil, fmt.Errorf("countrypack: return %s box %s: %w", d.Type, b.Code, err)
			}
		}
		out[b.Code], vars[Variable(b.Code)] = v, v
		names = append(names, Variable(b.Code))
	}
	return out, nil
}

// returns checks returns.yaml: each type once, named, with boxes of
// unique codes, labelled, each a selection of known values or an
// expression over the boxes before it.
func (v *validator) returns(rs []ReturnDefinition) {
	seen := map[string]bool{}
	for i, d := range rs {
		p := fmt.Sprintf("returns.yaml: returns[%d]", i)
		if !config.IsTaxReturnType(d.Type) || seen[d.Type] {
			v.err(p+".type", "a return type is unique, 1-20 upper-case letters, digits, dashes or underscores, as registrations name it")
		}
		seen[d.Type] = true
		if d.Name == "" {
			v.err(p+".name", "name is required")
		}
		if d.PriorPeriodThreshold != "" && !thresholdRe.MatchString(d.PriorPeriodThreshold) {
			v.err(p+".prior_period_threshold", "the threshold is a whole amount of the return's currency, such as \"10000\"")
		}
		if len(d.Boxes) == 0 {
			v.err(p+".boxes", "a return has at least one box")
		}
		v.boxes(p, d.Boxes)
	}
}

func (v *validator) boxes(p string, bs []Box) {
	codes := map[string]bool{}
	var names []string
	for _, b := range bs {
		bp := p + ".boxes[" + b.Code + "]"
		if !boxCodeRe.MatchString(b.Code) || codes[b.Code] {
			v.err(bp, "a box code is unique in its return, 1-20 lower-case letters, digits or underscores")
		}
		codes[b.Code] = true
		if b.Label == "" {
			v.err(bp+".label", "label is required")
		}
		switch {
		case (b.Select == nil) == (b.Expression == ""):
			v.err(bp, "a box is either a selection of tax items (select) or an expression over earlier boxes, not both or neither")
		case b.Select != nil:
			v.selection(bp+".select", b.Select)
		default:
			if _, err := rules.CompileFormula(b.Expression, names); err != nil {
				v.err(bp+".expression", "%v (an expression reads earlier boxes as box_<code>)", err)
			}
		}
		names = append(names, Variable(b.Code))
	}
}

func (v *validator) selection(p string, s *Selection) {
	if s.Direction != config.TaxInput && s.Direction != config.TaxOutput {
		v.err(p+".direction", "direction %q must be input or output", s.Direction)
	}
	if !slices.Contains(measures, s.Measure) {
		v.err(p+".measure", "measure %q must be one of %v", s.Measure, measures)
	}
	for _, c := range []struct {
		field  string
		values []string
		ok     func(string) bool
	}{
		{"kinds", s.Kinds, config.IsTaxKind},
		{"components", s.Components, config.IsTaxComponentKey},
		{"jurisdictions", s.Jurisdictions, config.IsTaxJurisdiction},
		{"account_keys", s.AccountKeys, config.IsTaxAccountKey},
	} {
		for _, x := range c.values {
			if !c.ok(x) {
				v.err(p+"."+c.field, "%q is not a tax item's %s", x, c.field)
			}
		}
	}
}
