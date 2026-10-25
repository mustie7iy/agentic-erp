package payables_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// invoiceRules are a tenant's rules on supplier invoices: a size limit
// read in minor units, and one on a custom field.
func invoiceRules() map[string]rules.Rule {
	return map[string]rules.Rule{
		"gross_limit": {Expression: `row.gross_amount.minor <= 100000`, Message: "Invoices above 1000.00 need a contract.",
			Cases: []rules.Case{{Row: row("1000.00"), Valid: true}, {Row: row("1000.01"), Valid: false}}},
		"no_fax": {Expression: `ext.channel.orValue("") != "fax"`, Message: "Fax invoices are not accepted.",
			Cases: []rules.Case{{Row: row("1.00"), Valid: true}, {Row: row("1.00"), Extensions: map[string]any{"channel": "fax"}, Valid: false}}},
	}
}

// row is a rule case's invoice header with the gross amount given.
func row(gross string) map[string]any {
	return map[string]any{"legal_entity_id": "le", "supplier_id": "s", "supplier_reference": "r", "invoice_date": "2026-03-10",
		"gross_amount": map[string]any{"amount": gross, "currency": "EUR"}, "tax_amount": map[string]any{"amount": "0.00", "currency": "EUR"}}
}

// TestRulesSeeThePostingDateOfThePost: at posting a rule reads the posting
// date the post uses, the request's or else today, not the one stored on
// the invoice (here none, which the rule accepts).
func TestRulesSeeThePostingDateOfThePost(t *testing.T) {
	e := setup(t)
	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "DATED", "100.00", "0.00", untaxedItem(e.expense, "100.00")), "")
	dated := func(d string) map[string]any { r := row("1.00"); r["posting_date"] = d; return r }
	fixture.ActivateRules(t, e.Tenant.ID, "supplier_invoice", map[string]rules.Rule{"march": {
		Expression: `!row.posting_date.hasValue() || (row.posting_date.value() >= "2026-03-15" && row.posting_date.value() < "2026-04-01")`,
		Message:    "Invoices post in the second half of March.",
		Cases:      []rules.Case{{Row: row("1.00"), Valid: true}, {Row: dated("2026-03-21"), Valid: true}, {Row: dated("2026-03-10"), Valid: false}},
	}})
	path := "/v1/supplier-invoices/" + id(inv) + ":post"
	for name, body := range map[string]string{"the request's": `{"posting_date": "2026-03-10"}`, "today": ""} {
		t.Run(name, func(t *testing.T) {
			refusedBy(t, e.Problem(422, "request.rule_refused", "POST", path, body, tag), "march", "Invoices post in the second half of March.")
		})
	}
	posted, _ := e.Must(200, "POST", path, `{"posting_date": "2026-03-21"}`, tag)
	if posted["status"] != "posted" {
		t.Errorf("status: got %v, want posted", posted["status"])
	}
}

// TestRuleSamplesHistorical: a simulation's sample marks an invoice no
// write reaches any more (here a posted one) historical, and a parked one
// not: only the latter's next write is judged by the rules.
func TestRuleSamplesHistorical(t *testing.T) {
	e := setup(t)
	parked, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "OPEN", "100.00", "0.00", untaxedItem(e.expense, "100.00")), "")
	done, tag := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "DONE", "100.00", "0.00", untaxedItem(e.expense, "100.00")), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(done)+":post", `{"posting_date": "2026-03-21"}`, tag)
	var got []rules.Sample
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		got, err = payables.RuleSamples{}.Sample(ctx, time.Time{}, 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{id(parked): false, id(done): true}
	if len(got) != len(want) {
		t.Fatalf("samples: got %d, want %d", len(got), len(want))
	}
	for _, sm := range got {
		if h, ok := want[sm.ID.String()]; !ok || sm.Historical != h {
			t.Errorf("sample %s: got historical %t, want %t (known %t)", sm.Reference, sm.Historical, h, ok)
		}
	}
}

// refusedBy checks a 422 request.rule_refused names exactly the rule with
// its message.
func refusedBy(t *testing.T, p map[string]any, rule, message string) {
	t.Helper()
	errs, _ := p["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors: got %v, want one", p["errors"])
	}
	fe, _ := errs[0].(map[string]any)
	if fe["field"] != "rules."+rule {
		t.Errorf("field: got %v, want rules.%s", fe["field"], rule)
	}
	if fe["message"] != message {
		t.Errorf("message: got %v, want %q", fe["message"], message)
	}
}

// TestRulesOnInvoiceWrites: a rule that refuses an invoice's entry or
// edit answers 422 with its name and message, and the refusal is audited
// with source rule, the rule and the configuration version although the
// write rolled back; an invoice the rules accept is entered.
func TestRulesOnInvoiceWrites(t *testing.T) {
	e := setup(t)
	version := fixture.ActivateRules(t, e.Tenant.ID, "supplier_invoice", invoiceRules())
	p := e.Problem(422, "request.rule_refused", "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "BIG", "2000.00", "0.00", accountItem(e.expense, "2000.00")), "")
	refusedBy(t, p, "gross_limit", "Invoices above 1000.00 need a contract.")
	var entered int
	pgtest.Scan(t, `SELECT count(*) FROM payables.supplier_invoice WHERE tenant_id = $1`, []any{e.Tenant.ID}, &entered)
	if entered != 0 {
		t.Errorf("invoices entered: got %d, want 0", entered)
	}
	got := fixture.RuleAudits(t, e.Tenant.ID, "payables.supplier_invoice.create")
	if len(got) != 1 {
		t.Fatalf("audit records: got %v, want one", got)
	}
	fixture.WantRuleAudit(t, got[0], fixture.RuleAudit{ObjectType: "payables.supplier_invoice", Outcome: "refused", Rule: "gross_limit", ConfigVersion: version})

	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "SMALL", "100.00", "0.00", accountItem(e.expense, "100.00")), "")
	p = e.Problem(422, "request.rule_refused", "PATCH", "/v1/supplier-invoices/"+id(inv), `{"extensions": {"channel": "fax"}}`, tag)
	refusedBy(t, p, "no_fax", "Fax invoices are not accepted.")
	got = fixture.RuleAudits(t, e.Tenant.ID, "payables.supplier_invoice.update")
	if len(got) != 1 {
		t.Fatalf("update audit records: got %v, want one", got)
	}
	invID := uuid.MustParse(id(inv))
	fixture.WantRuleAudit(t, got[0], fixture.RuleAudit{ObjectType: "payables.supplier_invoice", Outcome: "refused", Rule: "no_fax", ObjectID: &invID, ConfigVersion: version})
	e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), `{"extensions": {"channel": "mail"}}`, tag)
}

// A captured invoice a rule refuses lands held naming the rule, after any
// other hold reason, instead of being lost; the hold is audited, and a
// person who releases it cannot post it while the rule refuses it. One
// held for a missing required field is judged once a person completes it:
// its rules may read the field.
func TestCaptureHeldByRule(t *testing.T) {
	const byRule = "Rule gross_limit refuses it: Invoices above 1000.00 need a contract."
	for name, c := range map[string]struct {
		required, unsure bool
		wantPrefix       string // the hold reason starts with it
		wantRule         bool   // the reason names the rule, and the hold is audited
	}{
		"rule":              {wantPrefix: byRule, wantRule: true},
		"unsure extraction": {unsure: true, wantPrefix: "Low extraction confidence", wantRule: true},
		"missing field":     {required: true, wantPrefix: "Required custom fields are missing: region."},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.order(t, e.Supplier, "PO-1")
			if c.required {
				pgtest.Unguarded(t, `UPDATE config.config_version SET document = jsonb_set(document, '{extensions,supplier_invoice,required}', '["region"]')
					WHERE tenant_id = $1`, e.Tenant.ID)
			}
			version := fixture.ActivateRules(t, e.Tenant.ID, "supplier_invoice", map[string]rules.Rule{"gross_limit": captureLimit(c.required)})
			path := captured(t, e, c.unsure, name)
			held, tag := e.Must(200, "GET", path, "", "")
			if held["status"] != "on_hold" {
				t.Errorf("status: got %v, want on_hold", held["status"])
			}
			reason, _ := held["hold_reason"].(string)
			if !strings.HasPrefix(reason, c.wantPrefix) {
				t.Errorf("hold reason: got %q, want it to start with %q", reason, c.wantPrefix)
			}
			if got := strings.Contains(reason, byRule); got != c.wantRule {
				t.Errorf("hold reason names the rule: got %t, want %t (%q)", got, c.wantRule, reason)
			}
			got := fixture.RuleAudits(t, e.Tenant.ID, "payables.supplier_invoice.create")
			if !c.wantRule {
				if len(got) != 0 {
					t.Errorf("audit: got %+v, want none before the invoice is completed", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("audit records: got %v, want one", got)
			}
			invID := uuid.MustParse(strings.TrimPrefix(path, "/v1/supplier-invoices/"))
			fixture.WantRuleAudit(t, got[0], fixture.RuleAudit{ObjectType: "payables.supplier_invoice", Outcome: "held", Rule: "gross_limit", ObjectID: &invID, ConfigVersion: version})
			_, tag = e.Must(200, "POST", path+":release", `{"comment": "checked"}`, tag)
			refusedBy(t, e.Problem(422, "request.rule_refused", "POST", path+":post", "", tag), "gross_limit", "Invoices above 1000.00 need a contract.")
		})
	}
}

// A captured invoice whose rule cannot be evaluated is held naming the
// rule, audited, rather than refused: a captured document is never lost.
// Other writes keep the 422 request.rule_unevaluable.
func TestCaptureHeldByUnevaluableRule(t *testing.T) {
	e := setup(t)
	e.order(t, e.Supplier, "PO-1")
	// Divides by zero on the captured invoice's 157.80 only.
	version := fixture.ActivateRules(t, e.Tenant.ID, "supplier_invoice", map[string]rules.Rule{"odd": {
		Expression: `100 / (row.gross_amount.minor - 15780) >= 0`, Message: "Odd.",
		Cases: []rules.Case{{Row: row("1000.00"), Valid: true}, {Row: row("157.79"), Valid: false}}}})
	path := captured(t, e, false, "unevaluable")
	held, _ := e.Must(200, "GET", path, "", "")
	if held["status"] != "on_hold" {
		t.Errorf("status: got %v, want on_hold", held["status"])
	}
	if want := "Rule odd cannot be evaluated on it: the rule fails to evaluate on this input; have the rule corrected."; held["hold_reason"] != want {
		t.Errorf("hold reason: got %v, want %q", held["hold_reason"], want)
	}
	got := fixture.RuleAudits(t, e.Tenant.ID, "payables.supplier_invoice.create")
	if len(got) != 1 {
		t.Fatalf("audit records: got %v, want one", got)
	}
	invID := uuid.MustParse(strings.TrimPrefix(path, "/v1/supplier-invoices/"))
	fixture.WantRuleAudit(t, got[0], fixture.RuleAudit{ObjectType: "payables.supplier_invoice", Outcome: "held", Rule: "odd", ObjectID: &invID, ConfigVersion: version})
}

// captureLimit is gross_limit at 100.00, below the captured invoice's
// 157.80; with the cost center required, its cases carry one.
func captureLimit(required bool) rules.Rule {
	r := invoiceRules()["gross_limit"]
	r.Expression = `row.gross_amount.minor <= 10000`
	r.Cases[0].Row["gross_amount"] = map[string]any{"amount": "100.00", "currency": "EUR"}
	if required {
		for i := range r.Cases {
			r.Cases[i].Extensions = map[string]any{"region": "CC1"}
		}
	}
	return r
}

// captured captures the standard extraction, unsure or not, and returns the
// invoice's path.
func captured(t *testing.T, e env, unsure bool, document string) string {
	t.Helper()
	var res gen.CaptureSupplierInvoiceRes
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		x := extraction()
		if unsure {
			x.Confidence = "low"
		}
		res, err = endpoints(extractor{e: x}).CaptureSupplierInvoice(ctx,
			&gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE, Document: gen.NewOptString("%PDF-1 above the limit " + document)}, gen.CaptureSupplierInvoiceParams{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return res.(*gen.OperationHeaders).Response.Resource.Value
}
