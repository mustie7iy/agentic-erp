package payments_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/partner"
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
	other := e.request(t, down, "20.00", "2026-03-11", "")
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
	// Every statement of the resumption failing, and its ports failing, is
	// reported and leaves nothing (each attempt is rolled back).
	resume := func(h *payments.Endpoints) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.StartRelease(ctx, uuidOf(id(run)), func(int32) error { return nil }, "")
			return err
		}
	}
	apitest.EveryFailure(t, e.Tenant, e.Approver, "resumed release", resume(h))
	for name, broken := range map[string]func(*payments.Endpoints){
		"settings": func(h *payments.Endpoints) { h.Settings = failingSettings{} },
		"payees":   func(h *payments.Endpoints) { h.Payees = failingPayees{h.Payees} },
	} {
		t.Run(name, func(t *testing.T) {
			b := *h
			broken(&b)
			if err := apitest.Committed(t, e.Tenant, e.Approver, resume(&b)); !errors.Is(err, errPort) {
				t.Errorf("resumed release with the %s failing: got %v, want %v", name, err, errPort)
			}
		})
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
	for key, want := range map[string]string{e.Vendor.String(): "bank_account_changed 40.00", "request " + id(request): "supplier_blocked 70.00",
		"request " + id(other): "supplier_blocked 20.00"} {
		if exs[key] != want {
			t.Errorf("exclusion %s: got %q, want %q", key, exs[key], want)
		}
	}
	if _, ok := exs[e.Supplier.String()]; ok {
		t.Errorf("exclusions: got %v, want none for the supplier whose payment posted", exs)
	}
	dropped := events(t, e.Tenant, "payment_excluded")
	if len(dropped) != 3 {
		t.Errorf("payment_excluded events: got %d %v, want 3", len(dropped), dropped)
	}
	// A down payment's event names the request it frees.
	named := map[string]string{}
	for _, ev := range dropped {
		if r, ok := ev["down_payment_request_id"].(string); ok {
			named[r], _ = ev["down_payment_request_number"].(string)
		}
	}
	for _, r := range []map[string]any{request, other} {
		if named[id(r)] != r["document_number"] {
			t.Errorf("payment_excluded of request %s: got number %q, want %v", id(r), named[id(r)], r["document_number"])
		}
	}

	// The dropped requests are no longer in a live run: one can be cancelled.
	e.Must(200, "POST", "/v1/down-payment-requests/"+id(other)+":cancel", `{"reason": "Supplier blocked"}`, `"1"`)

	// The vendor's items and D's request are free for the next run.
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'active', version = version + 1 WHERE id = $1`, down)
	next := byKind(e.payments(t, e.run(t, "2026-04-01", "2026-04-01")))
	for _, key := range []string{e.Vendor.String() + " items", down.String() + " down_payment"} {
		if _, ok := next[key]; !ok {
			t.Errorf("next run's payments: got %v, want one for %s", next, key)
		}
	}
}

var errPort = errors.New("port down")

// failingSettings has no configuration to give.
type failingSettings struct{}

func (failingSettings) Document(context.Context) (*config.Config, int32, error) {
	return nil, 0, errPort
}

// failingPayees reads names but not payees.
type failingPayees struct{ inner payments.Payees }

func (failingPayees) Payees(context.Context, uuid.UUID, []uuid.UUID, time.Time) (map[uuid.UUID]partner.Payee, error) {
	return nil, errPort
}

func (f failingPayees) BankDetails(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.BankDetail, error) {
	return f.inner.BankDetails(ctx, ids)
}

func (f failingPayees) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error) {
	return f.inner.Names(ctx, ids)
}

func (f failingPayees) BankAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.BankAccount, error) {
	return f.inner.BankAccounts(ctx, ids)
}
