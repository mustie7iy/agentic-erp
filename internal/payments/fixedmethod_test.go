package payments_test

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// fixed enters and posts a supplier document of the kind (invoice,
// credit_memo) in company 2000 that fixes the payment method, and returns
// it.
func (e env) fixed(t *testing.T, kind string, supplier uuid.UUID, ref, gross, date, method string) map[string]any {
	t.Helper()
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": %q, "kind": %q, "payment_method": %q, "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}]}`,
		e.DE, supplier, ref, date, kind, method, gross, e.expense, gross), "")
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", fmt.Sprintf(`{"posting_date": %q}`, date), `"1"`)
	return posted
}

// runByMethod proposes a run of company 2000 in EUR without a house bank,
// builds it and returns it.
func (e env) runByMethod(t *testing.T) map[string]any {
	t.Helper()
	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, e.DE), "")
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	return run
}

// paidBySource are the run's items payments, keyed by supplier and why
// each is paid by its method ("none" in a run for one house bank), as
// "bank method amount: its items' documents' amounts".
func (e env) paidBySource(t *testing.T, run map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range e.payments(t, run) {
		m := p.(map[string]any)
		source, _ := m["payment_method_source"].(string)
		if source == "" {
			source = "none"
		}
		var items []string
		for _, it := range m["items"].([]any) {
			items = append(items, it.(map[string]any)["amount"].(map[string]any)["amount"].(string))
		}
		slices.Sort(items)
		key := m["supplier_id"].(string) + " " + source
		if _, twice := out[key]; twice {
			t.Errorf("payments of %s: got two, want one per supplier and source", key)
		}
		out[key] = fmt.Sprint(m["house_bank_id"], " ", m["payment_method"], " ", m["amount"].(map[string]any)["amount"], ": ", strings.Join(items, " "))
	}
	return out
}

// excludedByMethod are the run's exclusions of suppliers' items, keyed by
// supplier and the payment method the items fix ("none" for those that
// fix none), as "reason amount", read a page of one at a time.
func (e env) excludedByMethod(t *testing.T, run map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	path := "/v1/payment-runs/" + id(run) + "/exclusions?page_size=1"
	for next := path; next != ""; {
		page, _ := e.Must(200, "GET", next, "", "")
		for _, x := range page["items"].([]any) {
			m := x.(map[string]any)
			method, _ := m["payment_method"].(string)
			if method == "" {
				method = "none"
			}
			key := m["supplier_id"].(string) + " " + method
			if _, twice := out[key]; twice {
				t.Errorf("exclusions of %s: got two, want one per supplier and fixed method", key)
			}
			if name, _ := m["supplier_name"].(string); name == "" {
				t.Errorf("exclusion of %s: got no supplier_name, want the supplier named", key)
			}
			out[key] = m["reason_code"].(string) + " " + m["amount"].(map[string]any)["amount"].(string)
		}
		next = ""
		if token, ok := page["next_page_token"].(string); ok {
			next = path + "&page_token=" + url.QueryEscape(token)
		}
	}
	return out
}

// wantAll fails unless got holds exactly want's entries.
func wantAll(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s %s: got %q, want %q", what, k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// TestFixedMethodPaidApart: a supplier's items that fix a payment method
// are paid by that method, in a payment of their own beside the payment
// of its items that fix none, which goes by the company's run order
// (ADR-0038; SAP specialist 2026-11-09, A4(c); product owner 2026-11-10:
// one payment per supplier and method). A credit memo nets within its own
// group alone. A supplier none of whose items fixes a method is paid as
// before, in one payment by the run order. A group is what its items fix,
// so a supplier whose run-order method is also the method one of its
// items fixes gets two payments by it, each netted apart (ADR-0038 As
// built, the same method twice). Each payment says why it is paid by its
// method, and the release writes each to its bank's file.
func TestFixedMethodPaidApart(t *testing.T) {
	e, db := methodSetup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.methods(t, e.Supplier, "{S,T}")
	e.methods(t, e.Vendor, "{S,T}")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.document(t, "credit_memo", e.Supplier, "CM-1", "10.00", "2026-03-02")
	e.fixed(t, "invoice", e.Supplier, "INV-S", "60.00", "2026-03-01", "S")
	e.fixed(t, "credit_memo", e.Supplier, "CM-S", "5.00", "2026-03-02", "S")
	e.invoice(t, e.Vendor, "INV-V1", "40.00", "2026-03-01")
	e.document(t, "credit_memo", e.Vendor, "CM-V", "15.00", "2026-03-02")
	// The vendor's items that fix none go by T, the run order's first, and
	// one of its items fixes T: two payments by T, each netted apart, as a
	// group is what its items fix (ADR-0038 As built, the same method twice).
	e.fixed(t, "invoice", e.Vendor, "INV-VT", "30.00", "2026-03-01", "T")

	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, e.DE), "")
	job := payments.Job{RunID: uuidOf(op["resource"].(string)[len("/v1/payment-runs/"):]), ProposedBy: e.Tenant.Human.ID, ProposerKind: "human"}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "build with fixed methods", func(ctx context.Context) error { return endpoints(nil).Process(ctx, job) })
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	main := e.bank.String()
	wantAll(t, "payment", e.paidBySource(t, run), map[string]string{
		e.Supplier.String() + " run_order": db.String() + " T 90.00: -10.00 100.00",
		e.Supplier.String() + " item":      main + " S 55.00: -5.00 60.00",
		e.Vendor.String() + " run_order":   db.String() + " T 25.00: -15.00 40.00",
		e.Vendor.String() + " item":        db.String() + " T 30.00: 30.00",
	})
	wantAll(t, "exclusion", e.excludedByMethod(t, run), map[string]string{})
	if got := run["total_amount"].(map[string]any)["amount"]; got != "200.00" {
		t.Errorf("run total: got %v, want 200.00", got)
	}
	proposed := events(t, e.Tenant, "payment_run_proposed")
	if len(proposed) != 1 {
		t.Fatalf("payment_run_proposed events: got %d %v, want 1", len(proposed), proposed)
	}
	if got := proposed[0]["payment_count"]; got != float64(4) {
		t.Errorf("payment_run_proposed payment_count: got %v, want 4", got)
	}
	if got := proposed[0]["exclusion_count"]; got != float64(0) {
		t.Errorf("payment_run_proposed exclusion_count: got %v, want 0", got)
	}

	e.release(t, run, "")
	e.checkFiles(t, run, map[string]int{db.String() + " T": 3, main + " S": 1})
	for _, p := range e.payments(t, run) {
		if m := p.(map[string]any); m["journal_entry_id"] == nil {
			t.Errorf("payment %s of the released run: got no journal entry, want it posted", m["id"])
		}
	}
}

// TestFixedMethodGroupsNetApart: a supplier's items that fix a method and
// those that fix none are netted apart, never across methods (product
// owner 2026-11-10). A group left in debit is excluded with its debit
// balance, naming the method its items fix, while the supplier's other
// group is paid in full; the exclusion of one group does not mark the
// other's payment as dropped, and the run releases what it pays.
func TestFixedMethodGroupsNetApart(t *testing.T) {
	e, db := methodSetup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.methods(t, e.Supplier, "{S,T}")
	e.methods(t, e.Vendor, "{S,T}")
	// The supplier's items fixing S are in debit; those that fix none pay.
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.fixed(t, "invoice", e.Supplier, "INV-S", "60.00", "2026-03-01", "S")
	e.fixed(t, "credit_memo", e.Supplier, "CM-S", "80.00", "2026-03-02", "S")
	// The vendor's items that fix none are in debit; those fixing S pay.
	e.invoice(t, e.Vendor, "INV-V1", "30.00", "2026-03-01")
	e.document(t, "credit_memo", e.Vendor, "CM-V", "50.00", "2026-03-02")
	e.fixed(t, "invoice", e.Vendor, "INV-VS", "70.00", "2026-03-01", "S")

	run := e.runByMethod(t)
	main := e.bank.String()
	wantAll(t, "payment", e.paidBySource(t, run), map[string]string{
		e.Supplier.String() + " run_order": db.String() + " T 100.00: 100.00",
		e.Vendor.String() + " item":        main + " S 70.00: 70.00",
	})
	wantAll(t, "exclusion", e.excludedByMethod(t, run), map[string]string{
		e.Supplier.String() + " S":  "debit_balance -20.00",
		e.Vendor.String() + " none": "debit_balance -20.00",
	})
	proposed := events(t, e.Tenant, "payment_run_proposed")
	if len(proposed) != 1 {
		t.Fatalf("payment_run_proposed events: got %d %v, want 1", len(proposed), proposed)
	}
	if got := proposed[0]["exclusion_count"]; got != float64(2) {
		t.Errorf("payment_run_proposed exclusion_count: got %v, want 2", got)
	}
	e.release(t, run, "")
	e.checkFiles(t, run, map[string]int{db.String() + " T": 1, main + " S": 1})
	if got := len(e.payments(t, run)); got != 2 {
		t.Errorf("payments of the released run: got %d, want 2, none dropped", got)
	}
}

// TestFixedMethodThatCannotPay: a supplier's items that fix a payment
// method are left out with the reason when the method cannot pay them,
// never paid by another method (ADR-0038; product owner 2026-11-10),
// while its items that fix none are paid as before: the supplier may not
// be paid by the method (method_not_allowed), no house bank is ranked for
// it in the run currency (no_house_bank), it is no longer an active
// method of the company (method_not_active); a supplier blocked for
// payment is excluded for that, once per group. A run proposed for one
// house bank pays by no method: it leaves every item fixing one out
// (fixed_method_in_bank_run) and pays the others from its bank.
func TestFixedMethodThatCannotPay(t *testing.T) {
	e, db := methodSetup(t)
	pgtest.Exec(t, `INSERT INTO payments.payment_method (tenant_id, legal_entity_id, code, description, is_active, created_by, updated_by)
		VALUES ($1, $2, 'Q', 'Q', true, $3, $3)`, e.Tenant.ID, e.DE, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO payments.bank_ranking (tenant_id, legal_entity_id, method, currency, house_banks, created_by, updated_by)
		VALUES ($1, $2, 'Q', 'EUR', '{MAIN}', $3, $3)`, e.Tenant.ID, e.DE, e.Tenant.Human.ID)
	third, blocked := e.NewSupplier(t, "BP90000007"), e.NewSupplier(t, "BP90000008")
	for _, s := range []struct {
		id                  uuid.UUID
		iban, name, methods string
		fixes               string
	}{{e.Supplier, "DE89370400440532013000", "Supplier", "{T}", "S"}, {e.Vendor, "DE02120300000000202051", "Vendor", "{T,C}", "C"},
		{third, "DE75512108001245126199", "Third", "{T,Q}", "Q"}, {blocked, "DE02500105170137075030", "Blocked", "{T,S}", "S"}} {
		e.bankAccount(t, s.id, s.iban, s.name, "2026-01-01", "", "create")
		e.methods(t, s.id, s.methods)
		e.invoice(t, s.id, "INV-"+s.name, "100.00", "2026-03-01")
		e.fixed(t, "invoice", s.id, "FIX-"+s.name, "60.00", "2026-03-01", s.fixes)
	}
	pgtest.Exec(t, `UPDATE payments.payment_method SET is_active = false, version = version + 1 WHERE legal_entity_id = $1 AND code = 'Q'`, e.DE)
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_payment_blocked = true, version = version + 1 WHERE partner_id = $1`, blocked)

	run := e.runByMethod(t)
	byOrder := db.String() + " T 100.00: 100.00"
	wantAll(t, "payment of the run without a house bank", e.paidBySource(t, run), map[string]string{
		e.Supplier.String() + " run_order": byOrder, e.Vendor.String() + " run_order": byOrder, third.String() + " run_order": byOrder})
	wantAll(t, "exclusion of the run without a house bank", e.excludedByMethod(t, run), map[string]string{
		e.Supplier.String() + " S": "method_not_allowed 60.00",
		e.Vendor.String() + " C":   "no_house_bank 60.00",
		third.String() + " Q":      "method_not_active 60.00",
		blocked.String() + " S":    "payment_blocked 60.00",
		blocked.String() + " none": "payment_blocked 100.00",
	})
	e.decide(t, 200, run, "reject", `{"reason": "Proposed again from one bank"}`)

	bankRun := e.run(t, "2026-04-15", "2026-04-15")
	fromBank := e.bank.String() + " <nil> 100.00: 100.00" // the run's bank, by no method
	wantAll(t, "payment of the run for one house bank", e.paidBySource(t, bankRun), map[string]string{
		e.Supplier.String() + " none": fromBank, e.Vendor.String() + " none": fromBank, third.String() + " none": fromBank})
	wantAll(t, "exclusion of the run for one house bank", e.excludedByMethod(t, bankRun), map[string]string{
		e.Supplier.String() + " S": "fixed_method_in_bank_run 60.00",
		e.Vendor.String() + " C":   "fixed_method_in_bank_run 60.00",
		third.String() + " Q":      "fixed_method_in_bank_run 60.00",
		blocked.String() + " S":    "payment_blocked 60.00",
		blocked.String() + " none": "payment_blocked 100.00",
	})
	e.release(t, bankRun, "")
}

// TestFixedMethodPaymentDropped: a release drops the payment whose item
// was reversed since the proposal and excludes the group it paid, the
// supplier's items fixing its method or those that fix none, leaving the
// supplier's other payment standing and released. Run again, the step
// drops nothing twice (ADR-0012).
func TestFixedMethodPaymentDropped(t *testing.T) {
	for name, c := range map[string]struct {
		reverseFixed bool
		excluded     string // the group excluded: the method its items fix, or none
		kept         string // why the payment that stands is paid by its method
		total        string
	}{
		"the payment of the items fixing a method": {true, "S", "run_order", "100.00"},
		"the payment of the items that fix none":   {false, "none", "item", "60.00"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := methodSetup(t)
			h := endpoints(e.connector)
			e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
			e.methods(t, e.Supplier, "{S,T}")
			plain := e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
			fixed := e.fixed(t, "invoice", e.Supplier, "INV-S", "60.00", "2026-03-01", "S")
			run := e.runByMethod(t)
			gone, amount := plain, "100.00"
			if c.reverseFixed {
				gone, amount = fixed, "60.00"
			}
			var drop uuid.UUID
			for _, p := range e.payments(t, run) {
				m := p.(map[string]any)
				if (m["payment_method_source"] == "item") == c.reverseFixed {
					drop = uuidOf(id(m))
				}
			}
			e.decide(t, 202, run, "release", "")
			e.unpayable(t, gone)
			post := func(ctx context.Context) error {
				r, err := payments.GetRun(ctx, uuidOf(id(run)))
				if err != nil {
					return err
				}
				_, err = h.PostPayment(ctx, r, drop)
				return err
			}
			apitest.EveryFailure(t, e.Tenant, e.Approver, "drop a payment of a supplier paid twice", post)
			if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
				t.Fatalf("release worker: %v", err)
			}
			got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
			if got["status"] != "released" {
				t.Fatalf("run status: got %v, want released", got["status"])
			}
			if total := got["total_amount"].(map[string]any)["amount"]; total != c.total {
				t.Errorf("run total: got %v, want %s, the payment that stands alone", total, c.total)
			}
			wantAll(t, "exclusion", e.excludedByMethod(t, run), map[string]string{e.Supplier.String() + " " + c.excluded: "item_reversed " + amount})
			pays := e.payments(t, run)
			if len(pays) != 1 {
				t.Fatalf("payments listed: got %d %v, want 1, the payment that stands", len(pays), pays)
			}
			stands := pays[0].(map[string]any)
			if got := stands["payment_method_source"]; got != c.kept {
				t.Errorf("the payment that stands, why it is paid by its method: got %v, want %s", got, c.kept)
			}
			if stands["journal_entry_id"] == nil {
				t.Errorf("the payment that stands: got no journal entry, want it posted")
			}
			// The step run again on the dropped payment does nothing twice.
			if err := apitest.Committed(t, e.Tenant, e.Approver, post); err != nil {
				t.Errorf("post the dropped payment again: got %v, want nil", err)
			}
			if again := events(t, e.Tenant, "payment_excluded"); len(again) != 1 {
				t.Errorf("payment_excluded events after the step ran twice: got %d, want 1", len(again))
			}
			var rows int
			pgtest.Scan(t, `SELECT (SELECT count(*) FROM payments.method_exclusion WHERE payment_run_id = $1) + (SELECT count(*) FROM payments.payment_exclusion
				WHERE payment_run_id = $1)`, []any{uuidOf(id(run))}, &rows)
			if rows != 1 {
				t.Errorf("exclusions after the step ran twice: got %d, want 1", rows)
			}
		})
	}
}
