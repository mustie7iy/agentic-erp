package payables_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// invoiceRules are a tenant's rules on supplier invoices: a size limit
// read in minor units, and one on a custom field.
func invoiceRules() map[string]rules.Rule {
	row := func(gross string) map[string]any {
		return map[string]any{"legal_entity_id": "le", "supplier_id": "s", "supplier_reference": "r", "invoice_date": "2026-03-10",
			"gross_amount": map[string]any{"amount": gross, "currency": "EUR"}, "tax_amount": map[string]any{"amount": "0.00", "currency": "EUR"}}
	}
	return map[string]rules.Rule{
		"gross_limit": {Expression: `row.gross_amount.minor <= 100000`, Message: "Invoices above 1000.00 need a contract.",
			Cases: []rules.Case{{Row: row("1000.00"), Valid: true}, {Row: row("1000.01"), Valid: false}}},
		"no_fax": {Expression: `ext.channel.orValue("") != "fax"`, Message: "Fax invoices are not accepted.",
			Cases: []rules.Case{{Row: row("1.00"), Valid: true}, {Row: row("1.00"), Extensions: map[string]any{"channel": "fax"}, Valid: false}}},
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
	if a := got[0]; a.Outcome != "refused" || a.Rule != "gross_limit" || a.ConfigVersion != version || a.ObjectID != nil ||
		a.ObjectType != "payables.supplier_invoice" {
		t.Errorf("audit record: got %+v, want refused by gross_limit in version %d, no object", a, version)
	}

	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "SMALL", "100.00", "0.00", accountItem(e.expense, "100.00")), "")
	p = e.Problem(422, "request.rule_refused", "PATCH", "/v1/supplier-invoices/"+id(inv), `{"extensions": {"channel": "fax"}}`, tag)
	refusedBy(t, p, "no_fax", "Fax invoices are not accepted.")
	got = fixture.RuleAudits(t, e.Tenant.ID, "payables.supplier_invoice.update")
	if len(got) != 1 || got[0].ObjectID == nil || got[0].ObjectID.String() != id(inv) {
		t.Errorf("update audit: got %+v, want one naming invoice %s", got, id(inv))
	}
	e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), `{"extensions": {"channel": "mail"}}`, tag)
}

// A captured invoice a rule refuses lands held naming the rule instead of
// being lost, and the hold is audited. One already held for a missing
// required field is judged once a person completes it: its rules may read
// the field.
func TestCaptureHeldByRule(t *testing.T) {
	for name, c := range map[string]struct{ required, unsure bool }{"rule": {}, "missing field": {required: true}, "unsure extraction": {unsure: true}} {
		required := c.required
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.order(t, e.Supplier, "PO-1")
			if required {
				pgtest.Unguarded(t, `UPDATE config.config_version SET document = jsonb_set(document, '{extensions,supplier_invoice,required}', '["cost_center"]')
					WHERE tenant_id = $1`, e.Tenant.ID)
			}
			rs := invoiceRules()
			r := rs["gross_limit"]
			r.Expression = `row.gross_amount.minor <= 10000`
			r.Cases[0].Row["gross_amount"] = map[string]any{"amount": "100.00", "currency": "EUR"}
			if required {
				for i := range r.Cases {
					r.Cases[i].Extensions = map[string]any{"cost_center": "CC1"}
				}
			}
			version := fixture.ActivateRules(t, e.Tenant.ID, "supplier_invoice", map[string]rules.Rule{"gross_limit": r})
			var res gen.CaptureSupplierInvoiceRes
			if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
				x := extraction()
				if c.unsure {
					x.Confidence = "low"
				}
				res, err = endpoints(extractor{e: x}).CaptureSupplierInvoice(ctx,
					&gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE, Document: gen.NewOptString("%PDF-1 above the limit " + name)}, gen.CaptureSupplierInvoiceParams{})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			path := res.(*gen.OperationHeaders).Response.Resource.Value
			held, _ := e.Must(200, "GET", path, "", "")
			if held["status"] != "on_hold" {
				t.Errorf("status: got %v, want on_hold", held["status"])
			}
			reason, _ := held["hold_reason"].(string)
			if got := strings.Contains(reason, "Rule gross_limit refuses it: Invoices above 1000.00 need a contract."); got == required {
				t.Errorf("hold reason names gross_limit and its message: got %t, want %t (%q)", got, !required, reason)
			}
			if got := strings.HasPrefix(reason, "Rule gross_limit"); !required && got == c.unsure {
				t.Errorf("hold reason starts with the rule: got %t, want %t (%q)", got, !c.unsure, reason)
			}
			if got := strings.Contains(reason, "cost_center"); got != required {
				t.Errorf("hold reason names the missing field: got %t, want %t (%q)", got, required, reason)
			}
			got := fixture.RuleAudits(t, e.Tenant.ID, "payables.supplier_invoice.create")
			if required {
				if len(got) != 0 {
					t.Errorf("audit: got %+v, want none before the invoice is completed", got)
				}
				return
			}
			if len(got) != 1 || got[0].Outcome != "held" || got[0].ConfigVersion != version || got[0].ObjectID == nil ||
				"/v1/supplier-invoices/"+got[0].ObjectID.String() != path {
				t.Errorf("audit: got %+v, want one held in version %d naming %s", got, version, path)
			}
		})
	}
}
