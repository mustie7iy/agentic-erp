package taxfiling_test

import (
	"context"
	"fmt"
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
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
)

// TestDatabaseFailures calls every handler and the setup outside a unit of
// work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := env(t)
	h := endpoints(t, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	gstID := uuid.MustParse(id(t, e, "GST"))
	doc := fixture.Document(t)
	ended := gst()
	ended.LastPeriodEnd = "2026-12-31"
	registered(ended)(doc)
	calls := map[string]func(ctx context.Context) error{
		"list registrations": func(ctx context.Context) error {
			_, err := h.ListTaxRegistrations(ctx, gen.ListTaxRegistrationsParams{})
			return err
		},
		"list periods to today": func(ctx context.Context) error {
			_, err := h.ListTaxPeriods(ctx, gen.ListTaxPeriodsParams{ID: gstID})
			return err
		},
		"check": func(ctx context.Context) error {
			_, err := setup().Check(ctx, nil, doc)
			return err
		},
		"apply": func(ctx context.Context) error { return setup().Apply(ctx, doc) },
		"the return holding a tax date": func(ctx context.Context) error {
			_, _, err := taxfiling.Holds{}.Holding(ctx, e.DE, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}

// TestRefusals: what only a direct call can send, the contract refusing
// it first: a page size or order the listing does not take.
func TestRefusals(t *testing.T) {
	e := env(t)
	h := endpoints(t, time.Now())
	in := func(fn func(ctx context.Context) error) string {
		return problem.From(apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)).Code
	}
	want(t, "registrations' page size", in(func(ctx context.Context) error {
		_, err := h.ListTaxRegistrations(ctx, gen.ListTaxRegistrationsParams{PageSize: gen.NewOptInt32(0)})
		return err
	}), "request.invalid_page_size")
	want(t, "periods' order", in(func(ctx context.Context) error {
		_, err := h.ListTaxPeriods(ctx, gen.ListTaxPeriodsParams{ID: uuid.New(), OrderBy: gen.NewOptString("end_date")})
		return err
	}), "request.invalid_order_by")
}

// TestUnreadableNumber: a stored number the tenant's keys cannot open
// fails the check rather than passing a change unseen.
func TestUnreadableNumber(t *testing.T) {
	e := env(t)
	doc := fixture.Document(t)
	registered(gst())(doc)
	pgtest.Exec(t, `UPDATE tax.registration SET number_cipher = '\x02', version = version + 1 WHERE tenant_id = $1 AND code = 'GST'`, e.Tenant.ID)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := setup().Check(ctx, nil, doc)
		return err
	})
	if err == nil {
		t.Fatal("check: got no error, want the number not opened")
	}
}

// TestReturnDatabaseFailures computes a draft and lists a box's items
// outside a unit of work and with each database statement failing in
// turn, the ledger's included.
func TestReturnDatabaseFailures(t *testing.T) {
	e := returnsEnv(t)
	sale(t, e, "2026-02-10", "")
	lib, err := countrypack.LoadAll(apitest.Packs(t, "testdata/packs"))
	if err != nil {
		t.Fatal(err)
	}
	h := endpoints(t, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	h.Packs, h.Items = lib, ledger.TaxItems{}
	testID := uuid.MustParse(id(t, e, "TEST"))
	calls := map[string]func(ctx context.Context) error{
		"compute": func(ctx context.Context) error {
			_, err := h.GetTaxReturnDraft(ctx, gen.GetTaxReturnDraftParams{ID: testID, PeriodStart: date("2026-01-01")})
			return err
		},
		"a box's items": func(ctx context.Context) error {
			_, err := h.ListTaxReturnDraftItems(ctx, gen.ListTaxReturnDraftItemsParams{ID: testID, PeriodStart: date("2026-01-01"), Box: "out"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}

// lifecycleScene is a tenant whose registration TEST has Q1 to Q3 filed,
// in period order, each with an item that arrived after its filing (a
// reversal): Q1's claimed by its approved amendment, Q2's by its prepared
// one, Q3's unclaimed; every fiscal period of 2026 closed or locked (January
// closed again) and a sale in Q4; and endpoints whose today is in 2027, so Q4 has ended and
// its original may be prepared.
type lifecycleScene struct {
	e                  fixture.Env
	h                  *taxfiling.Endpoints
	testID             uuid.UUID
	approved, prepared uuid.UUID
	preparedRequest    uuid.UUID
}

func newLifecycleScene(t *testing.T) lifecycleScene {
	t.Helper()
	e := lifecycleEnv(t)
	var sales []string
	for _, day := range []string{"2026-02-10", "2026-05-10", "2026-08-10", "2026-10-10"} {
		sales = append(sales, sale(t, e, day, ""))
	}
	s := lifecycleScene{e: e, testID: uuid.MustParse(id(t, e, "TEST"))}
	for i, q := range []struct{ start, filedOn string }{{"2026-01-01", "2026-04-20"}, {"2026-04-01", "2026-07-20"}, {"2026-07-01", "2026-10-20"}} {
		closePeriods(t, e, 3*i+1, 3*i+3)
		ret := s.prepare(t, `{"period_start": "`+q.start+`"}`, true)
		e.Must(200, "POST", "/v1/tax-returns/"+ret.String()+":file", `{"confirmation_number": "CRA-`+q.start+`", "filed_on": "`+q.filedOn+`"}`, `"2"`)
	}
	// After the filings, dated in their quarters as their originals.
	for _, sale := range sales[:3] {
		e.Must(200, "POST", "/v1/journal-entries/"+sale+":reverse", `{"posting_date": "2026-10-20"}`, "*")
	}
	s.approved = s.prepare(t, `{"period_start": "2026-01-01", "kind": "amendment"}`, true)
	s.prepared = s.prepare(t, `{"period_start": "2026-04-01", "kind": "amendment"}`, false)
	closePeriods(t, e, 10, 12)
	// Closed rather than locked, as before Q1's filing, so filing Q1's
	// amendment locks it (the transition trigger allows no unlock here).
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = '%s' AND legal_entity_id = '%s'
			AND fiscal_year = 2026 AND period = 1; END $$`, e.Tenant.ID, e.DE))
	r, _ := e.Must(200, "GET", "/v1/tax-returns/"+s.prepared.String(), "", "")
	s.preparedRequest = uuid.MustParse(r["approval_request_id"].(string))
	lib, err := countrypack.LoadAll(apitest.Packs(t, "testdata/packs"))
	if err != nil {
		t.Fatal(err)
	}
	s.h = endpoints(t, time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC))
	s.h.Packs, s.h.Items, s.h.Periods = lib, ledger.TaxItems{}, ledger.ReturnPeriods{}
	s.h.Approvals = approvals.Approvals{Settings: configuration.Settings{}, Roles: identity.Directory{}}
	return s
}

// prepare prepares the return the body asks for through the API,
// approved when approve is set, and returns its id.
func (s lifecycleScene) prepare(t *testing.T, body string, approve bool) uuid.UUID {
	t.Helper()
	r, _ := s.e.Must(201, "POST", returnsOf(s.testID.String()), body, "")
	if approve {
		s.e.Approve(t, r["id"].(string))
	}
	return uuid.MustParse(r["id"].(string))
}

// TestLifecycleDatabaseFailures prepares, amends, files and withdraws
// returns, reads and lists them, and decides their approval requests,
// outside a unit of work and with each database statement failing in
// turn, the ledger's and the approvals context's included.
func TestLifecycleDatabaseFailures(t *testing.T) {
	s := newLifecycleScene(t)
	h, tr := s.h, taxfiling.TaxReturns{Endpoints: s.h}
	calls := map[string]struct {
		as   pgtest.Actor
		call func(ctx context.Context) error
	}{
		"prepare": {s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.PrepareTaxReturn(ctx, &gen.TaxReturnPrepare{PeriodStart: date("2026-10-01")}, gen.PrepareTaxReturnParams{ID: s.testID})
			return err
		}},
		"amend": {s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.PrepareTaxReturn(ctx, &gen.TaxReturnPrepare{PeriodStart: date("2026-07-01"), Kind: gen.NewOptString(taxfiling.Amendment)},
				gen.PrepareTaxReturnParams{ID: s.testID})
			return err
		}},
		"file": {s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.FileTaxReturn(ctx, &gen.TaxReturnFile{ConfirmationNumber: "CRA-2", FiledOn: date("2026-07-20")},
				gen.FileTaxReturnParams{ID: s.approved, IfMatch: `"2"`})
			return err
		}},
		"withdraw approved": {s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.WithdrawTaxReturn(ctx, &gen.ReasonInput{Reason: "Wrong"}, gen.WithdrawTaxReturnParams{ID: s.approved, IfMatch: `"2"`})
			return err
		}},
		"withdraw prepared": {s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.WithdrawTaxReturn(ctx, &gen.ReasonInput{Reason: "Wrong"}, gen.WithdrawTaxReturnParams{ID: s.prepared, IfMatch: `"1"`})
			return err
		}},
		"get": {s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.GetTaxReturn(ctx, gen.GetTaxReturnParams{ID: s.prepared})
			return err
		}},
		"list": {s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListTaxReturns(ctx, gen.ListTaxReturnsParams{ID: s.testID})
			return err
		}},
		"approved": {s.e.Approver, func(ctx context.Context) error { return tr.Approved(ctx, s.prepared, s.preparedRequest) }},
		"rejected": {s.e.Approver, func(ctx context.Context) error { return tr.Rejected(ctx, s.prepared, s.preparedRequest, "No") }},
		"cancelled": {s.e.Tenant.Human, func(ctx context.Context) error {
			return tr.Cancelled(ctx, s.prepared, s.preparedRequest, "")
		}},
	}
	for name, c := range calls {
		t.Run(name, func(t *testing.T) {
			if err := c.call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, s.e.Tenant, c.as, name, c.call)
		})
	}
	t.Run("a decision on another request", func(t *testing.T) {
		for name, call := range map[string]func(ctx context.Context) error{
			"approved":  func(ctx context.Context) error { return tr.Approved(ctx, s.prepared, uuid.New()) },
			"not found": func(ctx context.Context) error { return tr.Rejected(ctx, uuid.New(), s.preparedRequest, "") },
		} {
			t.Run(name, func(t *testing.T) {
				if err := apitest.InRequest(t, s.e.Tenant, s.e.Approver, call); err == nil {
					t.Error("got no error, want one")
				}
			})
		}
	})
	t.Run("a page token of another listing", func(t *testing.T) {
		err := apitest.InRequest(t, s.e.Tenant, s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListTaxReturns(ctx, gen.ListTaxReturnsParams{ID: s.testID, PageToken: gen.NewOptString("x")})
			return err
		})
		want(t, "code", problem.From(err).Code, "request.invalid_page_token")
	})
}
