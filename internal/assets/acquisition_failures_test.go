package assets_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestAcquisitionDatabaseFailures calls each acquisition and worklist
// handler with each of its database statements failing in turn (a manual
// acquisition, a take-off through a reversal's line, reversals, the
// worklist with a reversed line), and the ledger's and payables' reads
// for them outside a unit of work.
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
	reversedInvoice, reversedEntry := e.invoiceOf(t, "MILL-2", "2026-06-05")
	held := uuid.MustParse(e.acquire(t, asset, reversedEntry, 1, `, "amount": {"amount": "50.00", "currency": "EUR"}`)["id"].(string))
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+reversedInvoice.String(), "", "")
	inv, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+reversedInvoice.String()+":reverse", `{"reason": "Duplicate", "posting_date": "2026-06-12"}`, tag)
	reversal := uuid.MustParse(inv["reversal_entry_id"].(string))
	capital := e.Account(t, "2000", "300000")
	from := func(entry uuid.UUID, line int32, amount gen.OptMoney) *gen.AssetAcquisitionCreate {
		return &gen.AssetAcquisitionCreate{AssetID: asset, Source: gen.NewOptAssetAcquisitionSourceRef(gen.AssetAcquisitionSourceRef{EntryID: entry, LineNo: line}), Amount: amount}
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
		"a take-off through a reversal's line": func(ctx context.Context) error {
			_, err := h.CreateAssetAcquisition(ctx, from(reversal, 1, gen.OptMoney{}), gen.CreateAssetAcquisitionParams{})
			return err
		},
		"a manual acquisition": func(ctx context.Context) error {
			_, err := h.CreateAssetAcquisition(ctx, &gen.AssetAcquisitionCreate{AssetID: asset, OffsetAccountID: gen.NewOptUUID(capital),
				Amount: gen.NewOptMoney(gen.Money{Amount: "5.00", Currency: "EUR"})}, gen.CreateAssetAcquisitionParams{})
			return err
		},
		"a reversal": func(ctx context.Context) error {
			_, err := h.ReverseAssetAcquisition(ctx, &gen.AssetAcquisitionReverse{Reason: "r"}, gen.ReverseAssetAcquisitionParams{ID: id, IfMatch: "*"})
			return err
		},
		"a reversal from a reversed line": func(ctx context.Context) error {
			_, err := h.ReverseAssetAcquisition(ctx, &gen.AssetAcquisitionReverse{Reason: "r"}, gen.ReverseAssetAcquisitionParams{ID: held, IfMatch: "*"})
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
		"an offset account": func(ctx context.Context) error {
			_, _, err := ledger.AssetReads{}.OffsetAccount(ctx, e.DE, capital)
			return err
		},
		"accounts' names": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.AccountNames(ctx, nil)
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
	// The contract refuses a page size of 0 before the handler; called
	// directly, the handler refuses it itself.
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListAssetClearingItems(ctx, gen.ListAssetClearingItemsParams{LegalEntityID: e.DE, FiscalYear: 2026, PageSize: gen.NewOptInt32(0)})
		return err
	})
	pgtest.WantErr(t, err, "request.invalid_page_size")
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := ledger.Calendar{LegalEntities: tenancy.Directory{}}.Period(ctx, uuid.Must(uuid.NewV7()), assetDay)
		return err
	})
	pgtest.WantErr(t, err, "ledger.no_fiscal_period")
}

// classesLost is the active document without company 2000's asset
// accounts, and with its zone one the binary cannot load.
type classesLost struct{ assets.Settings }

func (c classesLost) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, n, err := c.Settings.Document(ctx)
	if err == nil {
		doc.Company("2000").AssetAccounts = nil
	}
	return doc, n, err
}

// TestAcquisitionSettingsLost: an acquisition onto an asset whose class
// the company no longer has accounts for is refused, and one without a
// posting date whose company's zone does not load fails rather than
// dating it in UTC.
func TestAcquisitionSettingsLost(t *testing.T) {
	e := newAcquisitions(t)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", `, "cost_center_id": "`+e.c.Admin.String()+`"`))
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "300.00"}, line{"100000", "-300.00"})
	in := &gen.AssetAcquisitionCreate{AssetID: uuid.MustParse(a["id"].(string)), Source: gen.NewOptAssetAcquisitionSourceRef(gen.AssetAcquisitionSourceRef{EntryID: entry, LineNo: 1})}
	for name, c := range map[string]struct {
		settings assets.Settings
		want     string
	}{
		"no asset accounts": {classesLost{configuration.Settings{}}, "assets.class_not_in_company"},
		"a zone lost":       {zoneLost{configuration.Settings{}}, "Europe/Nowhere"},
	} {
		t.Run(name, func(t *testing.T) {
			h := app.Assets(app.Config{})
			h.Settings = c.settings
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CreateAssetAcquisition(ctx, in, gen.CreateAssetAcquisitionParams{})
				return err
			})
			if err == nil || !strings.Contains(fmt.Sprint(err, problemCode(err)), c.want) {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
}

func problemCode(err error) string {
	if p := problem.From(err); p != nil {
		return p.Code
	}
	return ""
}

// ledgersLost is the ledger with a leading ledger no acquisition posted
// to.
type ledgersLost struct{ assets.Ledger }

func (ledgersLost) Ledgers(context.Context, *uuid.UUID) ([]assets.LedgerRef, error) {
	return []assets.LedgerRef{{ID: uuid.Must(uuid.NewV7()), Code: "9L"}}, nil
}

// TestReversalWithoutLeadingEntry: an acquisition without an entry in the
// leading ledger, which cannot be posted, fails its reversal rather than
// reversing a copy.
func TestReversalWithoutLeadingEntry(t *testing.T) {
	e := newAcquisitions(t)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", `, "cost_center_id": "`+e.c.Admin.String()+`"`))
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "300.00"}, line{"100000", "-300.00"})
	q := e.acquire(t, a["id"], entry, 1, "")
	h := app.Assets(app.Config{})
	h.Ledger = ledgersLost{h.Ledger}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ReverseAssetAcquisition(ctx, &gen.AssetAcquisitionReverse{Reason: "r"}, gen.ReverseAssetAcquisitionParams{ID: uuid.MustParse(q["id"].(string)), IfMatch: "*"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "no entry in the leading ledger") {
		t.Errorf("got %v, want the missing leading entry named", err)
	}
}
