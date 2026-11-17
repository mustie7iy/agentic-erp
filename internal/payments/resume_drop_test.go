package payments_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestResumedReleaseDropsUnpayableSuppliers: a release that failed with a
// payment posted is resumed by :release (product owner 2026-11-16). The
// posted payment is not checked again, though its supplier was blocked
// since: its entry stands and the release completes with it. A payment not
// posted whose supplier can no longer be paid as proposed is dropped, its
// items back to open and its supplier excluded with the reason (the bank
// account changed), as is a down payment's, by its request (its supplier
// blocked), which a later run pays once the supplier can be paid again.
func TestResumedReleaseDropsUnpayableSuppliers(t *testing.T) {
	e := downPaymentSetup(t)
	h := endpoints(e.connector)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	down := e.NewSupplier(t, "BP90000005")
	e.bankAccount(t, down, "DE89370400440532013000", "D", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-A", "100.00", "2026-03-01")
	e.invoice(t, e.Vendor, "INV-X", "40.00", "2026-03-01")
	request := e.request(t, down, "70.00", "2026-03-10", "")
	run := e.run(t, "2026-03-31", "2026-03-31")
	pays := byKind(e.payments(t, run))
	payA := uuidOf(id(pays[e.Supplier.String()+" items"]))
	e.decide(t, 202, run, "release", "")

	// Payment A posts, then the release fails: the run stays releasing.
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		if _, err := h.PostPayment(ctx, r, payA); err != nil {
			return err
		}
		_, err = h.FailRelease(ctx, r, problem.Unavailable("The bank file could not be stored.", 0))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Every supplier can no longer be paid as proposed: A's posted payment
	// stands, the vendor's bank account changed, D is blocked.
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', version = version + 1 WHERE id = ANY($1)`, []uuid.UUID{e.Supplier, down})
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET valid_to = '2026-02-01' WHERE partner_id = '%s'; END $$`, e.Vendor))
	e.bankAccount(t, e.Vendor, "DE89370400440532013000", "Vendor", "2026-02-01", "", "create")

	run, _ = e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	if run["status"] != "releasing" {
		t.Fatalf("run after the failed release: got %v, want releasing", run["status"])
	}
	op := e.decide(t, 202, run, "release", "")
	// The drops leave the new operation queued, for the dispatcher to take.
	if op, _ = e.Must(200, "GET", "/v1/operations/"+id(op), "", ""); op["status"] != "queued" {
		t.Fatalf("resumed release operation: got %v, want queued", op["status"])
	}
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	if got["status"] != "released" {
		t.Fatalf("run status: got %v, want released", got["status"])
	}
	if total := got["total_amount"].(map[string]any)["amount"]; total != "100.00" {
		t.Errorf("run total: got %v, want 100.00, payment A alone", total)
	}
	listed := e.payments(t, run)
	if len(listed) != 1 || id(listed[0].(map[string]any)) != payA.String() {
		t.Errorf("payments listed: got %v, want payment A alone", listed)
	}
	exs := e.exclusions(t, run)
	for key, want := range map[string]string{e.Vendor.String(): "bank_account_changed 40.00", "request " + id(request): "supplier_blocked 70.00"} {
		if exs[key] != want {
			t.Errorf("exclusion %s: got %q, want %q", key, exs[key], want)
		}
	}
	if _, ok := exs[e.Supplier.String()]; ok {
		t.Errorf("exclusions: got %v, want none for the supplier whose payment posted", exs)
	}
	if dropped := events(t, e.Tenant, "payment_excluded"); len(dropped) != 2 {
		t.Errorf("payment_excluded events: got %d %v, want 2", len(dropped), dropped)
	}

	// The vendor's items and D's request are free for the next run.
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'active', version = version + 1 WHERE id = $1`, down)
	next := byKind(e.payments(t, e.run(t, "2026-04-01", "2026-04-01")))
	for _, key := range []string{e.Vendor.String() + " items", down.String() + " down_payment"} {
		if _, ok := next[key]; !ok {
			t.Errorf("next run's payments: got %v, want one for %s", next, key)
		}
	}
}
