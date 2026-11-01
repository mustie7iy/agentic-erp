package payables_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestDatabaseFailures calls every handler and context function outside a
// unit of work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(nil)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	parked := e.clean(t, po, "P-1")
	// On an order of its own: one P-3 invoices in full would leave it past
	// the received quantities when it posts (a match again at :post).
	other := e.order(t, e.Supplier, "PO-2")
	e.receive(t, other, 1, "10")
	e.receive(t, other, 2, "5")
	matched := e.clean(t, other, "P-2")
	e.act(t, 200, id(matched), "match", "")
	posted := e.clean(t, po, "P-3")
	e.act(t, 200, id(posted), "match", "")
	e.act(t, 200, id(posted), "post", "")
	over, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "P-4", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	e.act(t, 200, id(over), "match", "") // on hold: item 1 is already invoiced by P-3
	excused, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "P-5", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	e.act(t, 200, id(excused), "match", "")
	e.Approve(t, id(excused)) // parked on its approved exception
	results, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(matched)+"/match-results", "", "")
	result := uuid.MustParse(results["items"].([]any)[0].(map[string]any)["id"].(string))
	u := func(m map[string]any) uuid.UUID { return uuid.MustParse(id(m)) }
	cleanItems := []gen.SupplierInvoiceItem{
		{PurchaseOrderID: gen.NewOptUUID(po), PoItemNo: gen.NewOptInt32(1), Quantity: gen.Quantity{Value: "10", Unit: "EA"}, Amount: gen.Money{Amount: "120.00", Currency: "EUR"}},
		{AccountID: gen.NewOptUUID(e.expense), Quantity: gen.Quantity{Value: "1", Unit: "EA"}, Amount: gen.Money{Amount: "15.00", Currency: "EUR"}},
	}
	expand := []string{"supplier"}

	calls := map[string]func(ctx context.Context) error{
		"sample for rules": func(ctx context.Context) error {
			_, err := payables.RuleSamples{}.Sample(ctx, time.Time{}, 10)
			return err
		},
		"create": func(ctx context.Context) error {
			_, err := h.CreateSupplierInvoice(ctx, &gen.SupplierInvoiceCreate{LegalEntityID: e.DE, SupplierID: e.Supplier, SupplierReference: "NEW",
				InvoiceDate: date("2026-03-10"), GrossAmount: gen.Money{Amount: "157.80", Currency: "EUR"}, TaxAmount: gen.Money{Amount: "22.80", Currency: "EUR"},
				Items: cleanItems}, gen.CreateSupplierInvoiceParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetSupplierInvoice(ctx, gen.GetSupplierInvoiceParams{ID: u(parked), Expand: expand})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListSupplierInvoices(ctx, gen.ListSupplierInvoicesParams{Expand: expand})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateSupplierInvoice(ctx, &gen.SupplierInvoiceUpdate{SupplierReference: gen.NewOptString("P-1B"), Items: cleanItems},
				gen.UpdateSupplierInvoiceParams{ID: u(parked), IfMatch: "*"})
			return err
		},
		"match": func(ctx context.Context) error {
			_, err := h.MatchSupplierInvoice(ctx, gen.MatchSupplierInvoiceParams{ID: u(parked), IfMatch: "*"})
			return err
		},
		"match with issues": func(ctx context.Context) error {
			if _, err := h.HoldSupplierInvoice(ctx, &gen.ReasonInput{Reason: "x"}, gen.HoldSupplierInvoiceParams{ID: u(parked), IfMatch: "*"}); err != nil {
				return err
			}
			_, err := h.ReleaseSupplierInvoice(ctx, gen.OptDecisionInput{}, gen.ReleaseSupplierInvoiceParams{ID: u(parked), IfMatch: "*"})
			if err != nil {
				return err
			}
			_, err = h.MatchSupplierInvoice(ctx, gen.MatchSupplierInvoiceParams{ID: u(over), IfMatch: "*"})
			return err
		},
		"match of an approved exception": func(ctx context.Context) error {
			_, err := h.MatchSupplierInvoice(ctx, gen.MatchSupplierInvoiceParams{ID: u(excused), IfMatch: "*"})
			return err
		},
		"clean match after hold": func(ctx context.Context) error {
			if _, err := h.ReverseSupplierInvoice(ctx, &gen.SupplierInvoiceReverse{Reason: "r"}, gen.ReverseSupplierInvoiceParams{ID: u(posted), IfMatch: "*"}); err != nil {
				return err
			}
			_, err := h.MatchSupplierInvoice(ctx, gen.MatchSupplierInvoiceParams{ID: u(over), IfMatch: "*"})
			return err
		},
		"post": func(ctx context.Context) error {
			_, err := h.PostSupplierInvoice(ctx, gen.OptSupplierInvoicePost{}, gen.PostSupplierInvoiceParams{ID: u(matched), IfMatch: "*"})
			return err
		},
		"hold": func(ctx context.Context) error {
			_, err := h.HoldSupplierInvoice(ctx, &gen.ReasonInput{Reason: "r"}, gen.HoldSupplierInvoiceParams{ID: u(parked), IfMatch: "*"})
			return err
		},
		"release": func(ctx context.Context) error {
			_, err := h.ReleaseSupplierInvoice(ctx, gen.NewOptDecisionInput(gen.DecisionInput{Comment: gen.NewOptString("ok")}),
				gen.ReleaseSupplierInvoiceParams{ID: u(over), IfMatch: "*"})
			return err
		},
		"cancel": func(ctx context.Context) error {
			_, err := h.CancelSupplierInvoice(ctx, &gen.ReasonInput{Reason: "r"}, gen.CancelSupplierInvoiceParams{ID: u(over), IfMatch: "*"})
			return err
		},
		"reverse": func(ctx context.Context) error {
			_, err := h.ReverseSupplierInvoice(ctx, &gen.SupplierInvoiceReverse{Reason: "r"}, gen.ReverseSupplierInvoiceParams{ID: u(posted), IfMatch: "*"})
			return err
		},
		"match results": func(ctx context.Context) error {
			_, err := h.ListMatchResults(ctx, gen.ListMatchResultsParams{ID: u(matched)})
			return err
		},
		"match result": func(ctx context.Context) error {
			_, err := h.GetMatchResult(ctx, gen.GetMatchResultParams{ID: result})
			return err
		},
		"approved": func(ctx context.Context) error { return payables.Approvals{}.Approved(ctx, u(over), uuid.New()) },
		"rejected": func(ctx context.Context) error { return payables.Approvals{}.Rejected(ctx, u(over), uuid.New(), "c") },
		"withdrawn": func(ctx context.Context) error {
			return payables.Approvals{}.Cancelled(ctx, u(over), uuid.New(), "c")
		},
		"has invoices": func(ctx context.Context) error {
			_, err := payables.Invoices{}.HasInvoices(ctx, po)
			return err
		},
		"invoiced": func(ctx context.Context) error {
			_, err := payables.Invoices{}.Invoiced(ctx, po)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"rejected unknown": func(ctx context.Context) error {
			return payables.Approvals{}.Rejected(ctx, uuid.New(), uuid.New(), "c")
		},
		"withdrawn unknown": func(ctx context.Context) error {
			return payables.Approvals{}.Cancelled(ctx, uuid.New(), uuid.New(), "c")
		},
		"list page size": func(ctx context.Context) error {
			_, err := h.ListSupplierInvoices(ctx, gen.ListSupplierInvoicesParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"results page size": func(ctx context.Context) error {
			_, err := h.ListMatchResults(ctx, gen.ListMatchResultsParams{ID: u(matched), PageSize: gen.NewOptInt32(0)})
			return err
		},
		"get unknown": func(ctx context.Context) error {
			_, err := h.GetSupplierInvoice(ctx, gen.GetSupplierInvoiceParams{ID: uuid.New()})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); err == nil {
				t.Error("got nil error, want a refusal")
			}
		})
	}
	for action, body := range map[string]string{"hold": `{"reason": "r"}`, "release": "", "cancel": `{"reason": "r"}`, "match": "", "post": "",
		"reverse": `{"reason": "r"}`} {
		t.Run("stale "+action, func(t *testing.T) {
			api := e.Client
			api.T = t
			api.Problem(412, "request.version_mismatch", "POST", "/v1/supplier-invoices/"+id(parked)+":"+action, body, `"99"`)
		})
	}
	if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "supplier_invoice_capture_once"}); p.Code != "payables.already_captured" {
		t.Errorf("capture constraint: %v", p)
	}
}

func date(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

// noApproval is a workflow that requires no approval.
type noApproval struct{ approvals.Approvals }

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func TestEdgeCases(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 2, "5")
	// Item prices round half away from zero: 1.555 KG at 3.00 is 4.67.
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "E-1", "4.67", "0.00", poItem(po, 2, "1.555", "KG", "4.67")), "")
	if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != true {
		t.Fatalf("rounded price: %v", m)
	}
	// Items on accounts are left out of the match; only parked invoices post.
	mixed, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "E-6", "5.67", "0.00", poItem(po, 2, "1", "KG", "3.00"),
		strings.Replace(accountItem(e.expense, "2.67"), `, "tax_code": "V1"`, "", 1)), "")
	if m := e.act(t, 200, id(mixed), "match", ""); m["is_clean"] != true {
		t.Fatalf("mixed: %v", m)
	}
	e.act(t, 200, id(mixed), "hold", `{"reason": "r"}`)
	e.Problem(409, "payables.invalid_status", "POST", "/v1/supplier-invoices/"+id(mixed)+":post", "", e.tag(t, id(mixed)))
	// A workflow without exception approvers leaves the invoice on hold without a request.
	h := endpoints(nil)
	h.Approvals = noApproval{}
	over, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "E-2", "30.00", "0.00", poItem(po, 2, "10", "KG", "30.00")), "")
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		m, err := h.MatchInvoice(ctx, uuid.MustParse(id(over)), func(int32) error { return nil })
		if err == nil && m.IsClean {
			t.Error("over-received invoice matched clean")
		}
		v, _ := payables.GetInvoice(ctx, uuid.MustParse(id(over)))
		if v.Status != payables.OnHold {
			t.Errorf("held status: got %v, want %v", v.Status, payables.OnHold)
		}
		if v.WorkflowRequestID != nil {
			t.Errorf("held workflow request: got %v, want none", *v.WorkflowRequestID)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tax without a tax code goes to the input tax account; without one there is nothing to post to.
	plain := strings.Replace(accountItem(e.expense, "100.00"), `, "tax_code": "V1"`, "", 1)
	taxed, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "E-3", "119.00", "19.00", plain), "")
	untaxed, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "E-4", "119.00", "19.00", plain), "")
	posted := e.act(t, 200, id(taxed), "post", "")
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	if acc := je["lines"].([]any)[1].(map[string]any)["account_id"]; acc != e.tax.String() {
		t.Fatalf("tax line on %v", acc)
	}
	pgtest.Exec(t, `DELETE FROM ledger.account_determination WHERE legal_entity_id = $1 AND key = 'input_tax'`, e.DE)
	e.Problem(422, "payables.no_tax_account", "POST", "/v1/supplier-invoices/"+id(untaxed)+":post", "", e.tag(t, id(untaxed)))
	// Custom fields merge as a merge patch: given keys replace, null removes.
	withExt, tag := e.Must(201, "POST", "/v1/supplier-invoices", strings.Replace(e.invoice(e.Vendor, "E-5", "1.00", "0.00", plain),
		`"items"`, `"extensions": {"region": "CC1", "channel": "edi"}, "items"`, 1), "")
	got, tag := e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(withExt), `{"extensions": {"region": "CC2", "channel": null}}`, tag)
	var schema *int
	pgtest.Scan(t, `SELECT extensions_schema FROM payables.supplier_invoice WHERE id = $1`, []any{id(withExt)}, &schema)
	if schema == nil || *schema != 1 {
		t.Errorf("invoice's extensions schema: got %v, want 1", schema)
	}
	bad := e.Problem(422, "request.invalid_extension", "PATCH", "/v1/supplier-invoices/"+id(withExt), `{"extensions": {"Bad": 1, "channel": 2}}`, tag)
	if errs := bad["errors"].([]any); len(errs) != 2 {
		t.Errorf("field errors: got %v, want one for Bad and one for channel", errs)
	}
	if r := got["extensions"].(map[string]any); len(r) != 1 || r["region"] != "CC2" {
		t.Errorf("merged: got %v, want map[region:CC2]", r)
	}
}
