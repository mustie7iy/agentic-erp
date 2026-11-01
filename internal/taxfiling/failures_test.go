package taxfiling_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
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
