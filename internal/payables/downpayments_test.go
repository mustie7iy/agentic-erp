package payables_test

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// downPayment is a down payment request body for legal entity 2000.
func (e env) downPayment(supplier uuid.UUID, amount, currency, extra string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "amount": {"amount": %q, "currency": %q}, "due_date": "2026-03-15"%s}`,
		e.DE, supplier, amount, currency, extra)
}

// TestDownPaymentRequests: a down payment request (SAP F-47, ADR-0037,
// T46b) is entered open, with the input tax its amount includes when it
// names a tax code, listed and read, and cancelled while open; each
// change is audited and published.
func TestDownPaymentRequests(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	taxed, tag := e.Must(201, "POST", "/v1/down-payment-requests",
		e.downPayment(e.Supplier, "1190.00", "EUR", fmt.Sprintf(`, "purchase_order_id": %q, "tax_code": "V1", "reference": "AB-17"`, po)), "")
	for field, want := range map[string]any{"status": "open", "tax_code": "V1", "reference": "AB-17", "purchase_order_id": po.String(), "due_date": "2026-03-15"} {
		t.Run(field, func(t *testing.T) {
			if got := taxed[field]; got != want {
				t.Errorf("%s: got %v, want %v", field, got, want)
			}
		})
	}
	if got, _ := taxed["document_number"].(string); !strings.HasPrefix(got, "DP") {
		t.Errorf("document number: got %q, want one from the DP range", got)
	}
	if got := taxed["tax_amount"].(map[string]any)["amount"]; got != "190.00" {
		t.Errorf("tax amount: got %v, want 190.00 (1190.00 × 19/119)", got)
	}
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	plain, _ := e.Must(201, "POST", "/v1/down-payment-requests", e.downPayment(e.Vendor, "500.00", "EUR", ""), "")
	if got := plain["tax_amount"].(map[string]any)["amount"]; got != "0.00" {
		t.Errorf("tax amount without a tax code: got %v, want 0.00", got)
	}
	requested := events(t, e.Tenant, "down_payment_requested")
	if len(requested) != 2 {
		t.Fatalf("DownPaymentRequested: got %d events, want 2", len(requested))
	}
	if got := requested[0]["tax_amount"].(map[string]any)["amount"]; got != "190.00" {
		t.Errorf("DownPaymentRequested tax amount: got %v, want 190.00", got)
	}
	got, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(taxed), "", "")
	if got["id"] != taxed["id"] {
		t.Errorf("get: got %v, want %v", got["id"], taxed["id"])
	}
	for query, want := range map[string]int{"": 2, "?supplier_id=" + e.Supplier.String(): 1, "?status=cancelled": 0,
		"?purchase_order_id=" + po.String(): 1, "?legal_entity_id=" + e.US.String(): 0} {
		t.Run("list"+query, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", "/v1/down-payment-requests"+query, "", "")
			if n := len(page["items"].([]any)); n != want {
				t.Errorf("list%s: got %d, want %d", query, n, want)
			}
		})
	}
	first, _ := e.Must(200, "GET", "/v1/down-payment-requests?page_size=1", "", "")
	if _, ok := first["next_page_token"]; !ok {
		t.Error("first page of 1: got no next_page_token, want one")
	}
	// The list joins ref.currency; page 2 proves the keyset condition still resolves.
	second, _ := e.Must(200, "GET", "/v1/down-payment-requests?page_size=1&page_token="+first["next_page_token"].(string), "", "")
	paged := map[any]bool{}
	for _, p := range []map[string]any{first, second} {
		for _, it := range p["items"].([]any) {
			paged[it.(map[string]any)["id"]] = true
		}
	}
	if want := map[any]bool{taxed["id"]: true, plain["id"]: true}; fmt.Sprint(paged) != fmt.Sprint(want) {
		t.Errorf("requests over two pages of 1: got %v, want %v", paged, want)
	}
	if next, ok := second["next_page_token"]; ok {
		t.Errorf("last page's next_page_token: got %v, want none", next)
	}

	e.Must(400, "GET", "/v1/down-payment-requests?page_token=not-a-token", "", "")
	e.Must(412, "POST", "/v1/down-payment-requests/"+id(taxed)+":cancel", `{"reason": "x"}`, `"9"`)
	cancelled, _ := e.Must(200, "POST", "/v1/down-payment-requests/"+id(taxed)+":cancel", `{"reason": "Order changed"}`, tag)
	if cancelled["status"] != "cancelled" {
		t.Errorf("status after :cancel: got %v, want cancelled", cancelled["status"])
	}
	if cancelled["cancel_reason"] != "Order changed" {
		t.Errorf("cancel reason: got %v, want Order changed", cancelled["cancel_reason"])
	}
	if n := len(events(t, e.Tenant, "down_payment_request_cancelled")); n != 1 {
		t.Errorf("DownPaymentRequestCancelled: got %d events, want 1", n)
	}
	e.Problem(409, "payables.invalid_status", "POST", "/v1/down-payment-requests/"+id(taxed)+":cancel", `{"reason": "again"}`, `"2"`)
	e.Problem(404, "request.not_found", "GET", "/v1/down-payment-requests/"+uuid.NewString(), "", "")
	pgtest.Exec(t, `DELETE FROM identity.company_access WHERE tenant_id = $1 AND principal_id = $2`, e.Tenant.ID, e.Tenant.Agent.ID)
	e.As(e.Tenant.Agent).Problem(404, "request.not_found", "GET", "/v1/down-payment-requests/"+id(taxed), "", "")
	var actions int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_type = 'payables.down_payment_request'`,
		[]any{e.Tenant.ID}, &actions)
	if actions != 3 {
		t.Errorf("audit entries: got %d, want 3 (two requests, one cancellation)", actions)
	}
}

// TestDownPaymentRequestRefusals: a request needs a usable supplier, a
// positive amount in a known currency with its digits, an order of the
// supplier and an input tax code of the legal entity.
func TestDownPaymentRequestRefusals(t *testing.T) {
	e := setup(t)
	otherPO := e.order(t, e.Vendor, "PO-2")
	blocked := e.NewSupplier(t, "BP90000003")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, blocked)
	e.TaxCodes(t, config.TaxCode{Code: "A1", Description: "Output tax", Direction: config.TaxOutput, RatePercent: "19", Account: "140000"}, twoParts)
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"unknown legal entity": {strings.Replace(e.downPayment(e.Supplier, "1.00", "EUR", ""), e.DE.String(), uuid.NewString(), 1), 422, "request.reference_not_found"},
		"unknown supplier":     {e.downPayment(uuid.New(), "1.00", "EUR", ""), 422, "request.reference_not_found"},
		"blocked supplier":     {e.downPayment(blocked, "1.00", "EUR", ""), 422, "payables.supplier_not_usable"},
		"unknown currency":     {e.downPayment(e.Supplier, "1.00", "XXQ", ""), 422, "payables.unknown_currency"},
		"decimals":             {e.downPayment(e.Supplier, "1.0", "EUR", ""), 400, "request.invalid_amount"},
		"zero":                 {e.downPayment(e.Supplier, "0.00", "EUR", ""), 422, "payables.invalid_amount"},
		"negative":             {e.downPayment(e.Supplier, "-1.00", "EUR", ""), 422, "payables.invalid_amount"},
		"order of another":     {e.downPayment(e.Supplier, "1.00", "EUR", `, "purchase_order_id": "`+otherPO.String()+`"`), 422, "payables.invalid_purchase_order"},
		"unknown tax code":     {e.downPayment(e.Supplier, "1.00", "EUR", `, "tax_code": "V7"`), 422, "payables.unknown_tax_code"},
		"blank reference":      {e.downPayment(e.Supplier, "1.00", "EUR", `, "reference": "  "`), 422, "payables.invalid_reference"},
		"no fiscal year":       {strings.Replace(e.downPayment(e.Supplier, "1.00", "EUR", ""), "2026-03-15", "2031-03-15", 1), 422, "ledger.no_fiscal_period"},
		"output tax code":      {e.downPayment(e.Supplier, "1.00", "EUR", `, "tax_code": "A1"`), 422, "payables.unknown_tax_code"},
		"code with components": {e.downPayment(e.Supplier, "1.00", "EUR", `, "tax_code": "V2"`), 422, "payables.tax_code_components"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(c.status, c.code, "POST", "/v1/down-payment-requests", c.body, "")
		})
	}
}

// TestDownPaymentRequestPermission: requesting and cancelling a down
// payment needs payables.down_payment.request; capturing invoices is not
// enough, since a request pays a supplier before any invoice backs it.
func TestDownPaymentRequestPermission(t *testing.T) {
	var without []string
	for _, p := range perms {
		if p != "payables.down_payment.request" {
			without = append(without, p)
		}
	}
	e := env{Env: fixture.New(t, without...)}
	e.Problem(403, "identity.permission_missing", "POST", "/v1/down-payment-requests", e.downPayment(e.Supplier, "1.00", "EUR", ""), "")
	e.Problem(403, "identity.permission_missing", "POST", "/v1/down-payment-requests/"+uuid.NewString()+":cancel", `{"reason": "x"}`, `"1"`)
	e.Must(200, "GET", "/v1/down-payment-requests", "", "")
}

// TestDownPaymentDatabaseFailures calls the down payment handlers outside a
// unit of work and with each of their database statements failing in turn.
func TestDownPaymentDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(nil)
	open, _ := e.Must(201, "POST", "/v1/down-payment-requests", e.downPayment(e.Supplier, "119.00", "EUR", `, "tax_code": "V1"`), "")
	u := uuid.MustParse(id(open))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateDownPaymentRequest(ctx, &gen.DownPaymentRequestCreate{LegalEntityID: e.DE, SupplierID: e.Supplier,
				Amount: gen.Money{Amount: "119.00", Currency: "EUR"}, TaxCode: gen.NewOptString("V1"), DueDate: date("2026-03-15")}, gen.CreateDownPaymentRequestParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetDownPaymentRequest(ctx, gen.GetDownPaymentRequestParams{ID: u})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListDownPaymentRequests(ctx, gen.ListDownPaymentRequestsParams{})
			return err
		},
		"cancel": func(ctx context.Context) error {
			_, err := h.CancelDownPaymentRequest(ctx, &gen.ReasonInput{Reason: "x"}, gen.CancelDownPaymentRequestParams{ID: u, IfMatch: "*"})
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
}

// TestDownPaymentRequestPorts: what the payments context reads and records
// through DownPaymentRequests (T46c): the open requests due by a date, the
// open requests in every currency (T53e2), one request, the requests' numbers (an unknown id left out), and that an
// entry paid one, which is recorded once; a request paid by another entry,
// or no longer open, is refused, and a paid request is not cancelled.
func TestDownPaymentRequestPorts(t *testing.T) {
	e := setup(t)
	paid, _ := e.Must(201, "POST", "/v1/down-payment-requests", e.downPayment(e.Supplier, "100.00", "EUR", ""), "")
	open, _ := e.Must(201, "POST", "/v1/down-payment-requests", e.downPayment(e.Vendor, "50.00", "EUR", ""), "")
	ports := payables.DownPaymentRequests{}
	entry := uuid.New()
	pay := func(ctx context.Context) error { return ports.Paid(ctx, uuid.MustParse(id(paid)), 2026, entry) }
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, pay); err != nil {
		t.Fatalf("paid: %v", err)
	}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		if err := pay(ctx); err != nil {
			return fmt.Errorf("paid again by the same entry: %w", err)
		}
		pgtest.WantErr(t, ports.Paid(ctx, uuid.MustParse(id(paid)), 2026, uuid.New()), "payables.invalid_status")
		due, err := ports.Due(ctx, e.DE, "EUR", date("2026-03-31"))
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID.String() != id(open) {
			t.Errorf("due: got %v, want only the open request", due)
		}
		all, err := ports.Open(ctx, e.DE)
		if err != nil {
			return err
		}
		if len(all) != 1 || all[0].ID.String() != id(open) || all[0].Currency != "EUR" {
			t.Errorf("open: got %v, want only the open request, in EUR", all)
		}
		r, err := ports.Request(ctx, uuid.MustParse(id(paid)))
		if err != nil {
			return err
		}
		if r.JournalEntryID == nil || *r.JournalEntryID != entry {
			t.Errorf("paid request's entry: got %v, want %s", r.JournalEntryID, entry)
		}
		numbers, err := ports.Numbers(ctx, []uuid.UUID{uuid.MustParse(id(paid)), uuid.MustParse(id(open)), uuid.New()})
		want := map[uuid.UUID]string{uuid.MustParse(id(paid)): paid["document_number"].(string), uuid.MustParse(id(open)): open["document_number"].(string)}
		if !maps.Equal(numbers, want) {
			t.Errorf("numbers: got %v, want %v", numbers, want)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(paid), "", "")
	if got["status"] != "paid" {
		t.Errorf("status: got %v, want paid", got["status"])
	}
	if got["journal_entry_id"] != entry.String() {
		t.Errorf("journal entry: got %v, want %s", got["journal_entry_id"], entry)
	}
	e.Problem(409, "payables.invalid_status", "POST", "/v1/down-payment-requests/"+id(paid)+":cancel", `{"reason": "x"}`, `"2"`)
	for name, call := range map[string]func(ctx context.Context) error{
		"due":     func(ctx context.Context) error { _, err := ports.Due(ctx, e.DE, "EUR", date("2026-03-31")); return err },
		"open":    func(ctx context.Context) error { _, err := ports.Open(ctx, e.DE); return err },
		"request": func(ctx context.Context) error { _, err := ports.Request(ctx, uuid.MustParse(id(open))); return err },
		"numbers": func(ctx context.Context) error { _, err := ports.Numbers(ctx, []uuid.UUID{uuid.New()}); return err },
		"paid":    func(ctx context.Context) error { return ports.Paid(ctx, uuid.MustParse(id(open)), 2026, uuid.New()) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}
