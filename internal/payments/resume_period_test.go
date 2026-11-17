package payments_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// postOneAndFail posts payment pay of the released run, then fails the
// release: the run stays releasing with its other payments not posted,
// to be resumed by :release.
func (e env) postOneAndFail(t *testing.T, h *payments.Endpoints, run map[string]any, pay uuid.UUID) {
	t.Helper()
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		if _, err := h.PostPayment(ctx, r, pay); err != nil {
			return err
		}
		_, err = h.FailRelease(ctx, r, problem.Unavailable("The bank file could not be stored.", 0))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", ""); got["status"] != "releasing" {
		t.Fatalf("run after the failed release: got %v, want releasing", got["status"])
	}
}

// resumed resumes the run's release and runs it to the end.
func (e env) resumed(t *testing.T, run map[string]any) map[string]any {
	t.Helper()
	e.decide(t, 202, run, "release", "")
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	if got["status"] != "released" {
		t.Fatalf("run status after the resumed release: got %v, want released", got["status"])
	}
	return got
}

// TestReleaseHoldsItsPeriodOpen (review batch 77): a release stopped with
// a payment posted stays releasing, its other payments still to post on
// the run date. Closing that date's period is refused (409
// ledger.payment_release_pending, naming the run), as is locking it when
// it was closed before the release's first posting, which would refuse
// the rest for good and leave the posted payment without its bank file.
// Once the resumed release has finished, the period closes and locks.
func TestReleaseHoldsItsPeriodOpen(t *testing.T) {
	e := setupWith(t, []string{"ledger.period.read", "ledger.period.manage", "ledger.period.lock"}, func(*app.Config) {})
	h := endpoints(e.connector)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-A", "100.00", "2026-03-01")
	e.invoice(t, e.Vendor, "INV-X", "40.00", "2026-03-01")
	run := e.run(t, "2026-03-31", "2026-03-31")
	payA := e.paymentOf(t, run, e.Supplier)
	e.decide(t, 202, run, "release", "")
	e.postOneAndFail(t, h, run, payA)

	var period uuid.UUID
	pgtest.Scan(t, `SELECT s.id FROM ledger.period_status s JOIN ledger.ledger l ON l.tenant_id = s.tenant_id AND l.id = s.ledger_id AND l.is_leading
		WHERE s.tenant_id = $1 AND s.legal_entity_id = $2 AND s.fiscal_year = 2026 AND s.period = 3`, []any{e.Tenant.ID, e.DE}, &period)
	path := "/v1/fiscal-periods/" + period.String()
	refused := func(action string) {
		t.Helper()
		_, tag := e.Must(200, "GET", path, "", "")
		p := e.Problem(409, "ledger.payment_release_pending", "POST", path+":"+action, "", tag)
		if detail, _ := p["detail"].(string); !strings.Contains(detail, run["document_number"].(string)+" ("+id(run)+")") {
			t.Errorf(":%s refused: got detail %q, want it to name run %v by number and id", action, detail, run["document_number"])
		}
		if got, again := e.Must(200, "GET", path, "", ""); again != tag {
			t.Errorf(":%s refused: got the period at %s (%v), want it unchanged at %s", action, again, got["status"], tag)
		}
	}
	refused("close")
	// A close that committed before the release's first posting leaves
	// the period closed while the run is releasing: it is not locked.
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE id = $1`, period)
	refused("lock")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE id = $1`, period)

	e.resumed(t, run)
	// Every statement of the close and its check failing is reported (each
	// attempt is rolled back), and the port outside a request is refused.
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "period close", func(ctx context.Context) error {
		_, err := ledger.ClosePeriod(ctx, payments.PeriodReleases{}, period, func(int32) error { return nil })
		return err
	})
	if _, err := (payments.PeriodReleases{}).Releasing(context.Background(), e.DE, time.Now(), time.Now()); err == nil {
		t.Errorf("releasing runs outside a request: got none refused, want an error")
	}
	_, tag := e.Must(200, "GET", path, "", "")
	_, tag = e.Must(200, "POST", path+":close", "", tag)
	if got, _ := e.Must(200, "POST", path+":lock", "", tag); got["status"] != "locked" {
		t.Errorf("period after the release: got %v, want locked", got["status"])
	}
}

// TestResumedReleaseDropsAnEndedTaxCode (review batch 77): a release
// stopped with a payment posted is resumed after the tax code of a down
// payment request it pays has ended before the run date. The down
// payment, whose posting the ledger would refuse for good
// (ledger.unknown_tax_code), is dropped, its request excluded with
// tax_code_invalid naming the code and its rate when requested, as a
// proposal leaves it out, and the release finishes with the payment that
// posted; the request stays open for a later run.
func TestResumedReleaseDropsAnEndedTaxCode(t *testing.T) {
	e := downPaymentSetup(t)
	h := endpoints(e.connector)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-A", "100.00", "2026-03-01")
	taxed := e.request(t, e.Vendor, "119.00", "2026-03-15", `, "tax_code": "V1"`)
	run := e.run(t, "2026-03-31", "2026-03-31")
	payA := e.paymentOf(t, run, e.Supplier)
	e.decide(t, 202, run, "release", "")
	e.postOneAndFail(t, h, run, payA)
	endV1(t, e)

	apitest.EveryFailure(t, e.Tenant, e.Approver, "resumed release", func(ctx context.Context) error {
		_, err := h.StartRelease(ctx, uuidOf(id(run)), func(int32) error { return nil }, "")
		return err
	})
	got := e.resumed(t, run)
	if total := got["total_amount"].(map[string]any)["amount"]; total != "100.00" {
		t.Errorf("run total: got %v, want 100.00, payment A alone", total)
	}
	want := map[string]string{"request " + id(taxed): "tax_code_invalid 119.00"}
	if exs := e.exclusions(t, run); len(exs) != 1 || exs["request "+id(taxed)] != want["request "+id(taxed)] {
		t.Errorf("exclusions: got %v, want %v", exs, want)
	}
	page, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run)+"/exclusions", "", "")
	x := page["items"].([]any)[0].(map[string]any)
	for field, want := range map[string]any{"tax_code": "V1", "request_tax_rate_percent": "19", "run_date_tax_rate_percent": nil} {
		if x[field] != want {
			t.Errorf("exclusion's %s: got %v, want %v", field, x[field], want)
		}
	}
	if got, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(taxed), "", ""); got["status"] != "open" {
		t.Errorf("request dropped: got %v, want open", got["status"])
	}
	dropped := events(t, e.Tenant, "payment_excluded")
	if len(dropped) != 1 || dropped[0]["reason_code"] != "tax_code_invalid" {
		t.Errorf("payment_excluded events: got %v, want one with tax_code_invalid", dropped)
	}
}
