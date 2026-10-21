// Package rules is the CEL engine of ADR-0046: a tenant's validation rules,
// carried per entity in the configuration document, compiled and
// type-checked when proposed against the entity's standard fields and its
// custom field schema, and evaluated within a cost limit and a timeout.
//
// A rule sees only its inputs, the row and its extensions: the environment
// has no time functions (no now(), timestamp() or duration()), no
// randomness, no custom functions and no I/O, so the same input always
// gives the same result. Time, if a rule ever needs it, arrives as an input
// field. In Phase 2 a rule only validates: it evaluates to true, or to false
// and its message refuses the write. An expression of any other type (a
// derived value, a route) is refused when compiled. Writes evaluates an
// entity's active rules on its writes and audits their refusals.
package rules

import (
	"maps"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The limits of ADR-0046: what one tenant's rules may cost a write.
const (
	MaxRules            = 20     // per entity
	MaxExpressionLength = 1000   // code points of one expression
	CostLimit           = 10_000 // CEL cost units of one evaluation
	Timeout             = 10 * time.Millisecond
	MaxCases            = 20  // unit cases of one rule
	maxMessageLength    = 255 // characters of a rule's message
	maxNameLength       = 63
	maxDepth            = 32 // parser recursion: nesting of one expression
)

// Rule is one validation rule as the configuration document carries it.
type Rule struct {
	// Expression is CEL over row.<field> and ext.<key>; it must be boolean.
	Expression string `yaml:"expression" json:"expression"`
	// Message refuses the write when the expression is false.
	Message string `yaml:"message" json:"message"`
	// Cases are the rule's unit cases, run whenever it is compiled.
	Cases []Case `yaml:"cases" json:"cases"`
}

// Case is a unit case: an input and whether the rule accepts it. Each rule
// carries at least one case it accepts and one it refuses.
type Case struct {
	Row        map[string]any `yaml:"row,omitempty" json:"row,omitempty"`
	Extensions map[string]any `yaml:"extensions,omitempty" json:"extensions,omitempty"`
	Valid      bool           `yaml:"valid" json:"valid"`
}

// typ is an input's type in CEL.
type typ int

const (
	tString  typ = iota // text, dates (YYYY-MM-DD, so they compare as dates), decimals, ids
	tInt                // whole numbers
	tBool               // booleans
	tStrings            // a list of strings
	tMoney              // Money
)

// Money is a money input as a rule reads it, the CEL object rules.Money:
// the API's decimal string and currency, and the amount in the currency's
// minor units, so that a rule compares sizes exactly
// (row.gross_amount.minor >= 1000000) and never through a float
// (ADR-0005). The amount compares only as a string (equality, sign); its
// size is minor's, within one currency.
type Money struct {
	Amount   string      `cel:"amount"`
	Currency string      `cel:"currency"`
	Minor    money.Minor `cel:"minor"`
}

// input is one variable a rule may read: row.<field> or ext.<key>. An
// optional input is a CEL optional (ext.x.hasValue(), ext.x.orValue("")),
// so a rule says what an absent value means instead of failing on it.
type input struct {
	name     string
	t        typ
	optional bool
}

// standard are the header fields of each entity a rule may read: the
// fields of its create request (api/openapi.yaml), which every write of
// the entity knows. Item lines are not inputs in Phase 2.
var standard = map[string][]input{
	"business_partner": {
		{"row.kind", tString, false}, {"row.name", tString, false}, {"row.legal_name", tString, true},
		{"row.country_code", tString, false}, {"row.language", tString, true}, {"row.roles", tStrings, false},
	},
	"purchase_order": {
		{"row.legal_entity_id", tString, false}, {"row.purchasing_org_id", tString, true},
		{"row.supplier_id", tString, false}, {"row.currency", tString, false}, {"row.order_date", tString, false},
		{"row.payment_term_code", tString, true}, {"row.requisition_id", tString, true},
		{"row.contract_id", tString, true},
	},
	"supplier_invoice": {
		{"row.legal_entity_id", tString, false}, {"row.supplier_id", tString, false},
		{"row.supplier_reference", tString, false}, {"row.invoice_date", tString, false},
		{"row.posting_date", tString, true}, {"row.gross_amount", tMoney, false}, {"row.tax_amount", tMoney, false},
		{"row.purchase_order_id", tString, true}, {"row.kind", tString, true},
		{"row.reference_invoice_id", tString, true}, {"row.source", tString, true},
	},
}

// Entities are the entities rules may be written on, in name order.
func Entities() []string { return slices.Sorted(maps.Keys(standard)) }
