package payments_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// unpayable reverses the invoice's entry behind its run's back, as the
// ledger's :reverse allowed before runs reserved their items in the ledger
// (F5): the reservation is ended by hand first.
func (e env) unpayable(t *testing.T, inv map[string]any) {
	t.Helper()
	entry := uuidOf(inv["journal_entry_id"].(string))
	pgtest.Exec(t, `UPDATE ledger.item_reservation SET released_at = now(), released_by = $2 WHERE journal_entry_id = $1 AND released_at IS NULL`,
		entry, e.Tenant.Human.ID)
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}.Reverse(ctx, entry, nil, nil)
		return err
	}); err != nil {
		t.Fatalf("reverse the invoice's entry: %v", err)
	}
}

// paymentOf is the id of the run's payment to the supplier.
func (e env) paymentOf(t *testing.T, run map[string]any, supplier uuid.UUID) uuid.UUID {
	t.Helper()
	for _, pay := range e.payments(t, run) {
		if m := pay.(map[string]any); m["supplier_id"] == supplier.String() {
			return uuidOf(id(m))
		}
	}
	t.Fatalf("payments of run %s: got none for supplier %s, want one", id(run), supplier)
	return uuid.Nil
}

// TestUnpayablePaymentIsDropped: a releasing run whose remaining payment
// can never post (its invoice was reversed after the proposal) is not stuck:
// the release drops that payment, excluded with the reason and its items
// freed, and releases the run with the payments that stand.
func TestUnpayablePaymentIsDropped(t *testing.T) {
	e := setup(t)
	h := endpoints(e.connector)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-A", "100.00", "2026-03-01")
	x := e.invoice(t, e.Vendor, "INV-X", "40.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	payA, payX := e.paymentOf(t, run, e.Supplier), e.paymentOf(t, run, e.Vendor)
	e.decide(t, 202, run, "release", "")
	// Payment A posts; then X's invoice is reversed, so X's payment can never post.
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		_, err = h.PostPayment(ctx, r, payA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.unpayable(t, x)
	postX := func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		_, err = h.PostPayment(ctx, r, payX)
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Approver, "drop payment", postX)

	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	if got["status"] != "released" {
		t.Fatalf("run status: got %v, want released", got["status"])
	}
	if got := got["total_amount"].(map[string]any)["amount"]; got != "100.00" {
		t.Errorf("run total: got %v, want 100.00, payment A alone", got)
	}
	if exs := e.exclusions(t, run); exs[e.Vendor.String()] != "item_reversed 40.00" {
		t.Errorf("exclusions: got %v, want vendor item_reversed 40.00", exs)
	}
	// The dropped payment is not listed; its items are freed.
	pays := e.payments(t, run)
	if len(pays) != 1 {
		t.Fatalf("payments listed: got %d %v, want 1, payment A alone", len(pays), pays)
	}
	if got := id(pays[0].(map[string]any)); got != payA.String() {
		t.Errorf("payment listed: got %s, want payment A %s", got, payA)
	}
	var active bool
	pgtest.Scan(t, `SELECT bool_or(is_active) FROM payments.payment_item WHERE payment_id = $1`, []any{payX}, &active)
	if active {
		t.Errorf("dropped payment's items active: got %v, want false", active)
	}
	dropped := events(t, e.Tenant, "payment_excluded")
	if len(dropped) != 1 {
		t.Fatalf("payment_excluded events: got %d %v, want 1", len(dropped), dropped)
	}
	if dropped[0]["payment_id"] != payX.String() {
		t.Errorf("payment_excluded payment: got %v, want %s", dropped[0]["payment_id"], payX)
	}
	released := events(t, e.Tenant, "payment_run_released")
	if len(released) != 1 {
		t.Fatalf("payment_run_released events: got %d %v, want 1", len(released), released)
	}
	if got := released[0]["payment_count"]; got != float64(1) {
		t.Errorf("payment_run_released payment_count: got %v, want 1", got)
	}
	var held int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.item_reservation WHERE owner_id = $1 AND released_at IS NULL`, []any{uuidOf(id(run))}, &held)
	if held != 0 {
		t.Errorf("ledger reservations of the released run: got %d, want 0", held)
	}
	// A late step on the dropped payment does nothing.
	if err := apitest.Committed(t, e.Tenant, e.Approver, postX); err != nil {
		t.Errorf("post the dropped payment again: got %v, want nil", err)
	}
	if again := events(t, e.Tenant, "payment_excluded"); len(again) != 1 {
		t.Errorf("payment_excluded events after a late step: got %d, want 1", len(again))
	}
}

// TestUnpayableItemsOfAProposedRun: a proposed run with an item reversed
// since the proposal is not released (nothing is posted yet, so it is
// rejected and proposed again); a release that drops every payment fails
// with nothing to pay and returns the run to proposed, to be rejected.
func TestUnpayableItemsOfAProposedRun(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	first := e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.unpayable(t, first)
	refused := e.decide(t, 409, run, "release", "")
	if refused["code"] != "payments.proposal_outdated" {
		t.Errorf("release of a proposed run with a reversed item: got %v, want payments.proposal_outdated", refused["code"])
	}
	if detail, _ := refused["detail"].(string); !strings.Contains(detail, "reversed") {
		t.Errorf("detail: got %q, want it to say the item is reversed", refused["detail"])
	}
	e.decide(t, 200, run, "reject", `{"reason": "Invoice reversed"}`)

	second := e.invoice(t, e.Supplier, "INV-2", "50.00", "2026-03-02")
	run = e.run(t, "2026-04-16", "2026-04-16")
	e.decide(t, 202, run, "release", "")
	e.unpayable(t, second)
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	if got["status"] != "proposed" {
		t.Fatalf("run whose every payment was dropped: got %v, want proposed", got["status"])
	}
	op, _ := e.Must(200, "GET", "/v1/operations/"+got["release_operation_id"].(string), "", "")
	if code := op["error"].(map[string]any)["code"]; code != "payments.nothing_to_pay" {
		t.Errorf("release operation error: got %v, want payments.nothing_to_pay", code)
	}
	e.decide(t, 409, run, "release", "")
	e.decide(t, 200, run, "reject", `{"reason": "Nothing left to pay"}`)
}
