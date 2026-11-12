package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Pricing is how the tenant prices sales items (ADR-0067 §11): its
// condition types and the one procedure that applies them, in order. None
// is shipped. A tenant without a procedure prices an item at its price
// alone, entered or recorded (ADR-0067 §10).
type Pricing struct {
	ConditionTypes []ConditionType `yaml:"condition_types,omitempty" json:"condition_types,omitempty"`
	Procedure      []ProcedureStep `yaml:"procedure,omitempty" json:"procedure,omitempty"`
}

// ConditionType is a kind of pricing record: a price, or a discount as a
// percentage of the value before it.
type ConditionType struct {
	Code string `yaml:"code" json:"code"`
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"`
}

// ProcedureStep is one step of the pricing procedure: the condition type
// applied at this place in the order.
type ProcedureStep struct {
	ConditionType string `yaml:"condition_type" json:"condition_type"`
}

// The kinds of condition type.
const (
	ConditionPrice           = "price"
	ConditionDiscountPercent = "discount_percent"
)

// MaxConditionTypes bounds the condition types, and so the conditions an
// item carries.
const MaxConditionTypes = 50

var conditionCodeRe = regexp.MustCompile(`^[A-Z0-9_]{1,10}$`)

// ConditionType returns the condition type of the code, if configured.
func (p Pricing) ConditionType(code string) (ConditionType, bool) {
	for _, t := range p.ConditionTypes {
		if t.Code == code {
			return t, true
		}
	}
	return ConditionType{}, false
}

// Steps are the procedure's condition types in the order they apply: the
// price first, then the discounts. It is empty for a tenant without a
// procedure. The document is a validated one: every step names a
// configured type.
func (p Pricing) Steps() []ConditionType {
	out := make([]ConditionType, 0, len(p.Procedure))
	for _, s := range p.Procedure {
		if t, ok := p.ConditionType(s.ConditionType); ok {
			out = append(out, t)
		}
	}
	return out
}

// pricing checks the condition types (a code, unique; a name; a kind)
// and the procedure: each step a configured type, each type once, a
// price first and no other price after it, since one price is what the
// discounts are taken from.
func (v *validator) pricing(c *Config) {
	types := c.Pricing.ConditionTypes
	if len(types) > MaxConditionTypes {
		v.err("pricing.condition_types", "at most %d condition types", MaxConditionTypes)
	}
	kinds := map[string]string{}
	for i, t := range types {
		p := fmt.Sprintf("pricing.condition_types[%s]", t.Code)
		if !conditionCodeRe.MatchString(t.Code) {
			p = fmt.Sprintf("pricing.condition_types[%d]", i)
			v.err(p+".code", "must be 1-10 capital letters, digits or underscores")
		} else if _, dup := kinds[t.Code]; dup {
			v.err(p, "duplicate condition type code")
		}
		if n := strings.TrimSpace(t.Name); n == "" || len(t.Name) > 60 {
			v.err(p+".name", "is required, at most 60 characters")
		}
		if t.Kind != ConditionPrice && t.Kind != ConditionDiscountPercent {
			v.err(p+".kind", "must be %s or %s", ConditionPrice, ConditionDiscountPercent)
		}
		kinds[t.Code] = t.Kind
	}
	used := map[string]bool{}
	for i, s := range c.Pricing.Procedure {
		p := fmt.Sprintf("pricing.procedure[%d].condition_type", i)
		kind, known := kinds[s.ConditionType]
		switch {
		case !known:
			v.err(p, "%q is not a condition type", s.ConditionType)
		case used[s.ConditionType]:
			v.err(p, "the procedure names each condition type once")
		case i == 0 && kind != ConditionPrice:
			v.err(p, "the procedure starts with a condition type of kind %s", ConditionPrice)
		case i > 0 && kind == ConditionPrice:
			v.err(p, "the procedure has one price, its first step")
		}
		used[s.ConditionType] = true
	}
}

// PricingTransitions checks the condition types against the base
// document (nil for the first version): a condition type is never
// removed once activated and keeps its kind, since price records name it
// and say by it what they are, as a payment term is never removed
// (partner.PaymentTerms.Check). Its name may change, and it may leave
// the procedure.
func PricingTransitions(base, proposed *Config) []Finding {
	if base == nil {
		return nil
	}
	var out []Finding
	for _, was := range base.Pricing.ConditionTypes {
		p := "pricing.condition_types[" + was.Code + "]"
		now, ok := proposed.Pricing.ConditionType(was.Code)
		switch {
		case !ok:
			out = append(out, Finding{SevError, p, "a condition type cannot be removed once activated"})
		case now.Kind != was.Kind:
			out = append(out, Finding{SevError, p + ".kind", "a condition type's kind cannot change once activated"})
		}
	}
	return out
}
