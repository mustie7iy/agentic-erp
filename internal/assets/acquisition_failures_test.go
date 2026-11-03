package assets_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestAcquisitionDatabaseFailures calls each acquisition and worklist
// handler with each of its database statements failing in turn, and the
// ledger's and payables' reads for them outside a unit of work.
func TestAcquisitionDatabaseFailures(t *testing.T) {
	e := newAcquisitions(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages, Now: func() time.Time { return assetDay }})
	a, _ := e.create(t, e.body(e.DE, "MACHINES", `, "cost_center_id": "`+e.c.Admin.String()+`"`))
	asset := uuid.MustParse(a["id"].(string))
	invoice := e.invoice(t, "MILL-1", "2026-06-04")
	credit := e.post(t, "2000", "2026-06-11", "EUR", nil, line{"100000", "10.00"}, line{"169000", "-10.00"})
	q := e.acquire(t, asset, invoice, 1, `, "amount": {"amount": "40.00", "currency": "EUR"}`)
	id := uuid.MustParse(q["id"].(string))
	from := func(entry uuid.UUID, line int32, amount gen.OptMoney) *gen.AssetAcquisitionCreate {
		return &gen.AssetAcquisitionCreate{AssetID: asset, Source: gen.AssetAcquisitionSourceRef{EntryID: entry, LineNo: line}, Amount: amount}
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"an acquisition of the rest": func(ctx context.Context) error {
			_, err := h.CreateAssetAcquisition(ctx, from(invoice, 1, gen.OptMoney{}), gen.CreateAssetAcquisitionParams{})
			return err
		},
		"a part": func(ctx context.Context) error {
			_, err := h.CreateAssetAcquisition(ctx, from(invoice, 1, gen.NewOptMoney(gen.Money{Amount: "1.00", Currency: "EUR"})), gen.CreateAssetAcquisitionParams{})
			return err
		},
		"a reduction": func(ctx context.Context) error {
			_, err := h.CreateAssetAcquisition(ctx, from(credit, 2, gen.OptMoney{}), gen.CreateAssetAcquisitionParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetAssetAcquisition(ctx, gen.GetAssetAcquisitionParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListAssetAcquisitions(ctx, gen.ListAssetAcquisitionsParams{})
			return err
		},
		"the worklist": func(ctx context.Context) error {
			_, err := h.ListAssetClearingItems(ctx, gen.ListAssetClearingItemsParams{LegalEntityID: e.DE, FiscalYear: 2026, PageSize: gen.NewOptInt32(1)})
			return err
		},
		"the worklist of what is left": func(ctx context.Context) error {
			_, err := h.ListAssetClearingItems(ctx, gen.ListAssetClearingItemsParams{LegalEntityID: e.DE, FiscalYear: 2026, Remaining: gen.NewOptBool(true)})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
				t.Errorf("outside a unit of work: got %v, want it refused", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"a line": func(ctx context.Context) error {
			_, _, err := ledger.AssetReads{}.Line(ctx, invoice, 1)
			return err
		},
		"clearing lines": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.ClearingLines(ctx, ledger.ClearingQuery{})
			return err
		},
		"entries": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.Entries(ctx, 2026, invoice)
			return err
		},
		"accounts": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.Accounts(ctx, e.DE, nil)
			return err
		},
		"a period": func(ctx context.Context) error {
			_, err := ledger.Calendar{LegalEntities: tenancy.Directory{}}.Period(ctx, e.DE, assetDay)
			return err
		},
		"supplier invoices": func(ctx context.Context) error {
			_, err := payables.Invoices{}.Sources(ctx, nil)
			return err
		},
	} {
		if err := fn(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
			t.Errorf("%s outside a unit of work: got %v, want reqctx.ErrOutsideRequest", name, err)
		}
	}
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := ledger.Calendar{LegalEntities: tenancy.Directory{}}.Period(ctx, uuid.Must(uuid.NewV7()), assetDay)
		return err
	})
	pgtest.WantErr(t, err, "ledger.no_fiscal_period")
}
