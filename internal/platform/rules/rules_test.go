package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

func partnerSchema() *extensions.Compiled {
	return &extensions.Compiled{Entity: "business_partner", Version: 3, Fields: []extensions.Field{
		{Key: "credit_days", Kind: extensions.Integer},
		{Key: "limit", Kind: extensions.Money},
		{Key: "vat_id", Kind: extensions.String},
		{Key: "vip", Kind: extensions.Boolean, Required: true},
	}}
}

func partner(country string) map[string]any {
	return map[string]any{"kind": "organization", "name": "Acme", "country_code": country, "roles": []any{"supplier"}}
}

const vatRule = `row.country_code != "DE" || ext.vat_id.orValue("") != ""`

func vat() Rule {
	return Rule{Expression: vatRule, Message: "a German partner needs a VAT id", Cases: []Case{
		{Row: partner("DE"), Extensions: map[string]any{"vip": false, "vat_id": "DE1"}, Valid: true},
		{Row: partner("DE"), Extensions: map[string]any{"vip": false}, Valid: false},
	}}
}

func mustCompile(t *testing.T, rs map[string]Rule) *Set {
	t.Helper()
	s, issues := Compile("business_partner", partnerSchema(), rs)
	if len(issues) > 0 {
		t.Fatalf("got issues %v, want none", issues)
	}
	return s
}

func TestCheck(t *testing.T) {
	s := mustCompile(t, map[string]Rule{"vat": vat(), "credit": {
		Expression: `!ext.credit_days.hasValue() || ext.credit_days.value() <= 90 && ext.vip`,
		Message:    "credit beyond 90 days, or for a partner that is not vip",
		Cases: []Case{
			{Row: partner("FR"), Extensions: map[string]any{"vip": true, "credit_days": 30}, Valid: true},
			{Row: partner("FR"), Extensions: map[string]any{"vip": true, "credit_days": 120}, Valid: false},
		},
	}})
	if s.SchemaVersion != 3 {
		t.Errorf("schema version: got %d, want 3", s.SchemaVersion)
	}
	got, err := s.Check(context.Background(), partner("DE"), map[string]any{"vip": false, "credit_days": float64(30)})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]Violation{{Rule: "credit", Message: "credit beyond 90 days, or for a partner that is not vip"}, {Rule: "vat", Message: "a German partner needs a VAT id"}})
	if fmt.Sprint(got) != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, err := s.Check(context.Background(), partner("FR"), map[string]any{"vip": true, "vat_id": nil}); err != nil || len(got) != 0 {
		t.Errorf("valid row: got %v, %v, want no violations", got, err)
	}
	none, issues := Compile("purchase_order", nil, nil)
	if len(issues) > 0 || none.SchemaVersion != 0 {
		t.Errorf("no schema: got %v, version %d, want no issues, version 0", issues, none.SchemaVersion)
	}
}

// The same input gives the same result, even where a rule filters a list.
func TestDeterministic(t *testing.T) {
	r := vat()
	r.Expression = `["DE", "A", "FR"].filter(k, k != row.country_code)[0] == "DE" || ` + vatRule
	r.Cases[1].Row = partner("DE")
	// Six hundred evaluations, each under ADR-0046's 10 ms of wall-clock
	// time, fail on a loaded machine whenever the process is descheduled
	// for 10 ms (seen at load 25 and more), whatever the rule does. What
	// is proved here is the result, so the limit is one the machine cannot
	// reach; TestTimeLimit proves the limit and that Compile sets it.
	s, issues := compile("business_partner", partnerSchema(), map[string]Rule{"vat": r}, time.Hour)
	if len(issues) > 0 {
		t.Fatalf("got issues %v, want none", issues)
	}
	for _, country := range []string{"DE", "A", "FR"} {
		first, err := s.Check(context.Background(), partner(country), map[string]any{"vip": true})
		if err != nil {
			t.Fatal(err)
		}
		for range 200 {
			got, err := s.Check(context.Background(), partner(country), map[string]any{"vip": true})
			if err != nil || fmt.Sprint(got) != fmt.Sprint(first) {
				t.Fatalf("%s: got %v, %v, want %v every time", country, got, err, first)
			}
		}
	}
}

// TestTimeLimit holds every rule Compile builds to ADR-0046's Timeout and
// shows a limit stops a rule, by one already past when it starts, so
// neither depends on how fast the machine is.
func TestTimeLimit(t *testing.T) {
	for _, r := range mustCompile(t, map[string]Rule{"vat": vat()}).rules {
		if r.timeout != Timeout {
			t.Errorf("%s: got a time limit of %v, want %v", r.name, r.timeout, Timeout)
		}
	}
	r := vat()
	r.Expression = `["DE", "A", "FR"].filter(k, k != row.country_code)[0] == "DE" || ` + vatRule
	_, issues := compile("business_partner", partnerSchema(), map[string]Rule{"vat": r}, time.Nanosecond)
	// Case 0 passes all the same: its VAT id makes the right side true,
	// and CEL's || is true when either side is, an error on the other.
	want := []extensions.Issue{{Path: "vat.cases[1]", Message: "evaluation: operation interrupted: context deadline exceeded"}}
	if !slices.Equal(issues, want) {
		t.Errorf("got issues %v, want %v", issues, want)
	}
}

func TestCompileRefuses(t *testing.T) {
	many := map[string]Rule{}
	for i := range MaxRules + 1 {
		many[fmt.Sprintf("r%d", i)] = vat()
	}
	with := func(f func(*Rule)) map[string]Rule {
		r := vat()
		f(&r)
		return map[string]Rule{"vat": r}
	}
	expr := func(e string) map[string]Rule { return with(func(r *Rule) { r.Expression = e }) }
	for name, c := range map[string]struct {
		rules      map[string]Rule
		path, want string
	}{
		"too many rules":     {many, "", "at most 20 rules"},
		"name":               {map[string]Rule{"Vat": vat()}, "Vat", "snake_case"},
		"message":            {with(func(r *Rule) { r.Message = "" }), "vat.message", "a message of 1 to 255"},
		"syntax":             {expr(`row.name ==`), "vat.expression", "Syntax error"},
		"unknown field":      {expr(`row.nope == ""`), "vat.expression", "undeclared reference"},
		"unknown extension":  {expr(`ext.nope == ""`), "vat.expression", "undeclared reference"},
		"now":                {expr(`now() > 0`), "vat.expression", "undeclared reference"},
		"timestamp":          {expr(`timestamp("2026-01-01T00:00:00Z") > timestamp("2025-01-01T00:00:00Z")`), "vat.expression", "undeclared reference"},
		"duration":           {expr(`duration("1s") == duration("1s")`), "vat.expression", "undeclared reference"},
		"year":               {expr(`dyn(row.name).getFullYear() == 2026`), "vat.expression", "undeclared reference"},
		"double":             {expr(`double(ext.credit_days.orValue(0)) * 1.5 > 2.0`), "vat.expression", "no floating-point"},
		"double of amount":   {expr(`double(ext.limit.orValue(rules.Money{}).minor) > 1.0`), "vat.expression", "no floating-point"},
		"map comprehension":  {expr(`{"amount": ext.limit.orValue(rules.Money{}).amount}.map(k, k)[0] == "amount"`), "vat.expression", "range over lists only"},
		"map literal":        {expr(`{"a": 1, "b": 2}.filter(k, k != "")[0] == "a"`), "vat.expression", "range over lists only"},
		"dyn comprehension":  {expr(`dyn({"a": 1}).exists(k, k == "a")`), "vat.expression", "range over lists only"},
		"derivation":         {expr(`row.name + "!"`), "vat.expression", "must be boolean, not string"},
		"dynamic":            {expr(`dyn(true)`), "vat.expression", "must be boolean"},
		"too long":           {expr(strings.Repeat(" ", MaxExpressionLength) + "true"), "vat.expression", "expression code point size exceeds limit"},
		"too deep":           {expr(strings.Repeat("(", maxDepth+1) + "true" + strings.Repeat(")", maxDepth+1)), "vat.expression", "recursion limit exceeded"},
		"too many cases":     {with(func(r *Rule) { r.Cases = append(r.Cases, make([]Case, MaxCases)...) }), "vat.cases", "at most 20 cases"},
		"no refusing case":   {with(func(r *Rule) { r.Cases = r.Cases[:1] }), "vat.cases", "at least one case"},
		"wrong case":         {with(func(r *Rule) { r.Cases[0].Valid, r.Cases[1].Valid = false, true }), "vat.cases[0]", "got valid true, want false"},
		"case input":         {with(func(r *Rule) { r.Cases[0].Row["typo"] = 1 }), "vat.cases[0]", "row.typo is not an input"},
		"case fails":         {expr(`1 / (size(row.roles) - 1) == 1`), "vat.cases[0]", "evaluation: division by zero"},
		"case required":      {with(func(r *Rule) { delete(r.Cases[0].Extensions, "vip") }), "vat.cases[0]", "ext.vip is required"},
		"case type":          {with(func(r *Rule) { r.Cases[0].Extensions["vip"] = "yes" }), "vat.cases[0]", "ext.vip: want a boolean"},
		"case cost exceeded": {with(func(r *Rule) { r.Expression, r.Cases[0].Row["name"] = longName, strings.Repeat("x", 8*CostLimit) }), "vat.cases[0]", "actual cost limit exceeded"},
	} {
		t.Run(name, func(t *testing.T) {
			_, issues := Compile("business_partner", partnerSchema(), c.rules)
			for _, is := range issues {
				if is.Path == c.path && strings.Contains(is.Message, c.want) {
					return
				}
			}
			t.Errorf("got %v, want %q at %q", issues, c.want, c.path)
		})
	}
}

func TestCompileRefusesEntities(t *testing.T) {
	if _, issues := Compile("journal_entry", nil, nil); len(issues) != 1 || !strings.Contains(issues[0].Message, "not supported") {
		t.Errorf("unsupported entity: got %v, want one issue", issues)
	}
}

// longName costs a tenth of its length per concatenation: a long name
// exceeds the cost limit in one step, long before the timeout.
const longName = `(row.name + row.name).size() > 0`

func TestCheckFails(t *testing.T) {
	s := mustCompile(t, map[string]Rule{"vat": vat(), "name": {Expression: longName, Message: "a long name",
		Cases: []Case{{Row: partner("FR"), Extensions: map[string]any{"vip": true}, Valid: true}, {Row: map[string]any{
			"kind": "organization", "name": "", "country_code": "FR", "roles": []string{},
		}, Extensions: map[string]any{"vip": true}, Valid: false}}},
		"roles": {
			Expression: `row.roles.all(r, r.size() > 0)`, Message: "an empty role",
			Cases: []Case{{Row: partner("FR"), Extensions: map[string]any{"vip": true}, Valid: true}, {Row: map[string]any{
				"kind": "organization", "name": "Acme", "country_code": "FR", "roles": []string{""},
			}, Extensions: map[string]any{"vip": true}, Valid: false}},
		}})
	bp := partner("FR")
	bp["name"] = strings.Repeat("x", 8*CostLimit)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, c := range map[string]struct {
		ctx  context.Context
		row  map[string]any
		ext  map[string]any
		want string
	}{
		"input type":    {context.Background(), partner("FR"), map[string]any{"vip": 1}, "rules: business_partner: ext.vip: want a boolean"},
		"cost":          {context.Background(), bp, map[string]any{"vip": true}, "rules: business_partner.name: evaluation: operation cancelled: actual cost limit exceeded"},
		"interrupted":   {cancelled, partner("FR"), map[string]any{"vip": true}, "rules: business_partner.roles: evaluation: operation interrupted: context canceled"},
		"unknown input": {context.Background(), partner("FR"), map[string]any{"vip": true, "x": 1, "a": 2}, "rules: business_partner: ext.a, ext.x is not an input"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Check(c.ctx, c.row, c.ext); err == nil || err.Error() != c.want {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
}

func TestConvert(t *testing.T) {
	for name, c := range map[string]struct {
		t    typ
		v    any
		want bool
	}{
		"int":             {tInt, 7, true},
		"int64":           {tInt, int64(7), true},
		"json number":     {tInt, json.Number("7"), true},
		"json fraction":   {tInt, json.Number("7.5"), false},
		"beyond json":     {tInt, int64(1 << 54), false},
		"float whole":     {tInt, float64(7), true},
		"float fraction":  {tInt, 7.5, false},
		"float too large": {tInt, float64(1 << 54), false},
		"int from string": {tInt, "7", false},
		"strings":         {tStrings, []string{"a"}, true},
		"any strings":     {tStrings, []any{"a"}, true},
		"any non-string":  {tStrings, []any{1}, false},
		"not a list":      {tStrings, "a", false},
		"money":           {tMoney, map[string]any{"amount": "1.00", "currency": "EUR"}, true},
		"money amount":    {tMoney, map[string]any{"amount": 1, "currency": "EUR"}, false},
		"money extra key": {tMoney, map[string]any{"amount": "1", "currency": "EUR", "x": "y"}, false},
		"money not a map": {tMoney, "1 EUR", false},
		"money parsed":    {tMoney, Money{Amount: "1.00", Currency: "EUR", Minor: 100}, true},
		"money digits":    {tMoney, map[string]any{"amount": "1.0", "currency": "EUR"}, false},
		"money currency":  {tMoney, map[string]any{"amount": "1.00", "currency": "XXQ"}, false},
		"string":          {tString, "a", true},
		"string from int": {tString, 1, false},
		"date":            {tString, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), true},
		"timestamp":       {tString, time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC), false},
		"zoned date":      {tString, time.Date(2026, 10, 20, 0, 0, 0, 0, time.FixedZone("x", 3600)), false},
		"bool":            {tBool, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := convert(mustCompile(t, nil).adapter, c.t, c.v); ok != c.want {
				t.Errorf("got %t, want %t", ok, c.want)
			}
		})
	}
}

// Money, its minor units included, and the optional standard fields read
// as rules expect.
func TestInvoiceInputs(t *testing.T) {
	money := func(a string) map[string]any { return map[string]any{"amount": a, "currency": "EUR"} }
	inv := func(tax string) map[string]any {
		return map[string]any{"legal_entity_id": "le", "supplier_id": "s", "supplier_reference": "r", "invoice_date": "2026-10-20",
			"gross_amount": money("100.00"), "tax_amount": money(tax), "posting_date": "2026-10-21"}
	}
	r := Rule{Expression: `row.tax_amount.currency == row.gross_amount.currency && row.tax_amount.amount != "0.00" && row.posting_date.orValue(row.invoice_date) >= row.invoice_date &&
		row.tax_amount.minor * 10 < row.gross_amount.minor * 3`,
		Message: "tax is missing", Cases: []Case{{Row: inv("19.00"), Valid: true}, {Row: inv("0.00"), Valid: false}}}
	if _, issues := Compile("supplier_invoice", nil, map[string]Rule{"tax": r}); len(issues) > 0 {
		t.Errorf("got issues %v, want none", issues)
	}
}

// The inputs are the whole activation: nothing is resolved above them.
func TestInputsHaveNoParent(t *testing.T) {
	if p := (inputs{}).Parent(); p != nil {
		t.Errorf("got parent %v, want none", p)
	}
}

// The validator's name is unique among cel-go's own, so it replaces none.
func TestExactName(t *testing.T) {
	if got := (exact{}).Name(); got != "rules.exact" {
		t.Errorf("got %q, want rules.exact", got)
	}
}
