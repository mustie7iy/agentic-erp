package taxfiling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// A chosen tax date is held to the periods no return reports yet
// (ADR-0049 P6): a manual entry's, and a supplier invoice's a person set.

var holdPerms = append([]string{"ledger.entry.park", "payables.invoice.capture", "payables.invoice.post"}, lifecyclePerms...)

// saleBody is sale's journal entry.
func saleBody(t *testing.T, e fixture.Env, day, taxDate string) string {
	t.Helper()
	return entryBody(t, e, day, taxDate, "100000 105.00", "400000 -100.00 S5", "240000 -5.00 S5")
}

// heldBy asserts that posting the body is refused for the return of
// TEST's first quarter in the status.
func heldBy(t *testing.T, c apitest.Client, method, path, body, ifMatch, ret, status string) {
	t.Helper()
	code, m, _ := c.In(t).Do(method, path, body, ifMatch)
	want(t, "status code", code, 409)
	want(t, "problem", m["code"], any("ledger.tax_date_in_return"))
	detail := fmt.Sprintf("period 2026-01-01 to 2026-03-31 of tax registration TEST, whose original return %s is %s", ret, status)
	if d, _ := m["detail"].(string); !strings.Contains(d, detail) {
		t.Errorf("detail: got %q, want it to name %q", d, detail)
	}
}

// invoice enters a supplier invoice of company 2000 dated 2026-03-10 of
// 100.00 with 19.00 of V1, with the tax date a person set when given, and
// posts it on the day.
func invoice(t *testing.T, e fixture.Env, ref, taxDate, day string) (int, map[string]any) {
	t.Helper()
	td := ""
	if taxDate != "" {
		td = fmt.Sprintf(`"tax_date": %q, `, taxDate)
	}
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q, %s
		"invoice_date": "2026-03-10", "gross_amount": {"amount": "119.00", "currency": "EUR"}, "tax_amount": {"amount": "19.00", "currency": "EUR"}, "items": [%s]}`,
		e.DE, e.Supplier, ref, td, e.AccountItem(t, "Services", "100.00")), "")
	code, m, _ := e.Do("POST", "/v1/supplier-invoices/"+inv["id"].(string)+":post", fmt.Sprintf(`{"posting_date": %q}`, day), `"1"`)
	return code, m
}

// TestTaxDateHold: once a return of the period is prepared, a manual entry
// with tax dated in it is refused, posted at once or parked and posted,
// naming the period, the return and its status, while it is prepared,
// approved and filed; entries without tax, or dated in an open period,
// post. A withdrawn return holds its period no longer. A supplier
// invoice's tax date a person set is held as well; one dated by its
// invoice date is not, and with a reversal it arrives late in the filed
// period, for its amendment.
func TestTaxDateHold(t *testing.T) {
	e := returnsEnv(t, holdPerms...)
	feb := sale(t, e, "2026-02-10", "")
	closePeriods(t, e, 1, 3)
	testID := id(t, e, "TEST")
	body := `{"period_start": "2026-01-01"}`
	ret, tag := e.Must(201, "POST", returnsOf(testID), body, "")
	first := ret["id"].(string)
	late := saleBody(t, e, "2026-05-05", "2026-03-15")
	t.Run("prepared", func(t *testing.T) {
		e := in(t, e)
		heldBy(t, e.Client, "POST", "/v1/journal-entries", late, "", first, "prepared")
		pe, ptag := e.Must(201, "POST", "/v1/parked-journal-entries", late, "")
		heldBy(t, e.Client, "POST", "/v1/parked-journal-entries/"+pe["id"].(string)+":post", "", ptag, first, "prepared")
		// Above the approval limit it waits for approval, and its approval
		// is refused when it would post.
		big, btag := e.Must(201, "POST", "/v1/parked-journal-entries",
			entryBody(t, e, "2026-05-05", "2026-03-15", "100000 21000.00", "400000 -20000.00 S5", "240000 -1000.00 S5"), "")
		submitted, _ := e.Must(200, "POST", "/v1/parked-journal-entries/"+big["id"].(string)+":post", "", btag)
		want(t, "the large entry", submitted["status"], any("pending_approval"))
		r := e.Pending(t, big["id"].(string))
		heldBy(t, e.As(e.Approver), "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]),
			first, "prepared")
		post(t, e, "2026-05-05", "2026-03-15", "100000 10.00", "400000 -10.00") // no tax
		sale(t, e, "2026-05-05", "2026-04-10")                                  // an open period
		sale(t, e, "2026-05-05", "")                                            // dated its posting date, in one
	})
	e.Must(200, "POST", "/v1/tax-returns/"+first+":withdraw", `{"reason": "A late invoice first"}`, tag)
	sale(t, e, "2026-05-06", "2026-03-20") // the period open again
	ret, _ = e.Must(201, "POST", returnsOf(testID), body, "")
	again := ret["id"].(string)
	want(t, "items of the return prepared again", claimedBy(t, e, again), 2)
	e.Approve(t, again)
	t.Run("approved", func(t *testing.T) {
		heldBy(t, in(t, e).Client, "POST", "/v1/journal-entries", late, "", again, "approved")
	})
	e.Must(200, "POST", "/v1/tax-returns/"+again+":file", `{"confirmation_number": "TAX-1", "filed_on": "2026-04-20"}`, `"2"`)
	t.Run("filed", func(t *testing.T) {
		e := in(t, e)
		heldBy(t, e.Client, "POST", "/v1/journal-entries", late, "", again, "filed")
		code, m := invoice(t, e, "INV-CHOSEN", "2026-03-20", "2026-05-07")
		want(t, "an invoice whose tax date a person set", fmt.Sprintf("%d %v", code, m["code"]), "409 ledger.tax_date_in_return")
		code, m = invoice(t, e, "INV-LATE", "", "2026-05-07")
		want(t, "an invoice dated in the period, posted late", fmt.Sprintf("%d %v", code, m["code"]), "200 <nil>")
		e.Must(200, "POST", "/v1/journal-entries/"+feb+":reverse", `{"posting_date": "2026-05-07"}`, "*")
		amendment, _ := e.Must(201, "POST", returnsOf(testID), `{"period_start": "2026-01-01", "kind": "amendment"}`, "")
		want(t, "the amendment's items: the invoice's input tax and the reversal's output tax", fmt.Sprint(amendment["item_count"]), "2")
	})
}

// TestTaxDateHoldGroup: a return of a tax group's registration holds the
// tax dates of its members' entries too.
func TestTaxDateHoldGroup(t *testing.T) {
	e := returnsEnv(t)
	ret := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tax.tax_return (tenant_id, id, registration_id, period_start, period_end, kind, return_type, return_name, pack_country,
			pack_version, currency, item_count, items_hash, prior_item_count, prior_tax, above_threshold, approval_request_id, created_by, updated_by)
		VALUES ($1, $2, $3, '2026-01-15', '2026-02-14', 'original', 'VAT201', 'VAT', 'DE', '2026.1', 'EUR', 0, repeat('0', 64), 0, 0, false,
			gen_random_uuid(), $4, $4)`, e.Tenant.ID, ret, id(t, e, "VAT"), e.Tenant.Human.ID)
	code, m, _ := e.Do("POST", "/v1/journal-entries", saleBody(t, e, "2026-05-05", "2026-02-01"), "")
	want(t, "a member's entry", fmt.Sprintf("%d %v", code, m["code"]), "409 ledger.tax_date_in_return")
	if d, _ := m["detail"].(string); !strings.Contains(d, "of tax registration VAT, whose original return "+ret.String()+" is prepared") {
		t.Errorf("detail: got %q, want it to name VAT's return %s", d, ret)
	}
	sale(t, e, "2026-05-05", "2026-02-15") // the next period
}

// TestTaxDateHoldSerializes: a manual entry and the prepare of its tax
// date's period queue on the registration. A post under way is claimed by
// the prepare that waited for it; a post that waited for a prepare is
// refused, so no item slips into a period being prepared unreported.
func TestTaxDateHoldSerializes(t *testing.T) {
	e := lifecycleEnv(t)
	sale(t, e, "2026-02-10", "")
	closePeriods(t, e, 1, 3)
	testID := uuid.MustParse(id(t, e, "TEST"))
	lib, err := countrypack.LoadAll(apitest.Packs(t, "testdata/packs"))
	if err != nil {
		t.Fatal(err)
	}
	h := endpoints(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	h.Packs, h.Items, h.Periods = lib, ledger.TaxItems{}, ledger.ReturnPeriods{}
	h.Approvals = approvals.Approvals{Settings: configuration.Settings{}, Roles: identity.Directory{}}
	lg := &ledger.Endpoints{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, Approvals: fixture.Approvals(),
		Settings: configuration.Settings{}, TaxReturns: taxfiling.Holds{}}
	prepare := func(ctx context.Context) error {
		_, err := h.PrepareTaxReturn(ctx, &gen.TaxReturnPrepare{PeriodStart: date("2026-01-01")}, gen.PrepareTaxReturnParams{ID: testID})
		return err
	}
	postLate := func(ctx context.Context) error {
		var req gen.JournalEntryCreate
		if err := req.UnmarshalJSON([]byte(saleBody(t, e, "2026-05-05", "2026-03-15"))); err != nil {
			return err
		}
		_, err := lg.PostJournalEntry(ctx, &req, gen.PostJournalEntryParams{})
		return err
	}
	t.Run("a post under way", func(t *testing.T) {
		posted, prepared := apitest.Serialized(t, e.Tenant, e.Tenant.Human, postLate, prepare)
		want(t, "post, prepare", fmt.Sprint(posted, prepared), "<nil> <nil>")
		var ret string
		pgtest.Scan(t, `SELECT id::text FROM tax.tax_return WHERE tenant_id = $1 AND status = 'prepared'`, []any{e.Tenant.ID}, &ret)
		want(t, "items claimed, the late sale's among them", claimedBy(t, e, ret), 2)
		_, tag := e.Must(200, "GET", "/v1/tax-returns/"+ret, "", "")
		e.Must(200, "POST", "/v1/tax-returns/"+ret+":withdraw", `{"reason": "Again"}`, tag)
	})
	t.Run("a prepare under way", func(t *testing.T) {
		prepared, posted := apitest.Serialized(t, e.Tenant, e.Tenant.Human, prepare, postLate)
		want(t, "prepare", prepared, error(nil))
		want(t, "the post that waited for it", problem.From(posted).Code, "ledger.tax_date_in_return")
	})
}

// TestTaxDateHoldLockOrder: a held posting locks the tax registrations
// only once it holds its number range, the order a configuration
// activation locks them in (number ranges, then the sections'
// registrations), so the two cannot deadlock: a posting waiting for its
// range holds no registration, which an activation may still lock.
func TestTaxDateHoldLockOrder(t *testing.T) {
	e := returnsEnv(t)
	held, release, firstDone := make(chan int32), make(chan struct{}), make(chan error, 1)
	go func() {
		firstDone <- apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			var pid int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM config.number_range WHERE legal_entity_id = $1 AND document_type = 'journal_sa'
				AND fiscal_year = 2026 FOR UPDATE`, e.DE).Scan(&pid); err != nil {
				return err
			}
			held <- pid
			<-release
			return nil
		})
	}()
	var pid int32
	select {
	case pid = <-held:
	case err := <-firstDone:
		t.Fatalf("lock the number range: %v", err)
	}
	posted := make(chan int, 1)
	go func() {
		code, _, _ := e.Do("POST", "/v1/journal-entries", saleBody(t, e, "2026-05-05", "2026-04-10"), "")
		posted <- code
	}()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var waiting bool
		pgtest.Scan(t, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE datname = current_database() AND $1::int = ANY (pg_blocking_pids(pid)))`,
			[]any{pid}, &waiting)
		return waiting
	}, "the posting did not wait for the number range")
	admin := pgtest.Admin(t)
	tx, err := admin.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, lockErr := tx.Exec(context.Background(), `SELECT FROM tax.registration WHERE tenant_id = $1 ORDER BY code FOR UPDATE NOWAIT`, e.Tenant.ID)
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("number range transaction: %v", err)
	}
	if lockErr != nil {
		t.Errorf("the registrations while the posting waits for its number range: got %v, want them free", lockErr)
	}
	want(t, "the posting", <-posted, 201)
}
