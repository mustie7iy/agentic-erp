package receivables_test

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
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *receivables.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Receivables(app.Config{Pages: pages})
}

func date(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	c := e.CostCenters(t, outputCodes(""))
	h := endpoints(t)
	entered, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1")), "")
	id := uuid.MustParse(entered["id"].(string))
	create := &gen.CustomerInvoiceCreate{LegalEntityID: e.DE, CustomerID: e.customer, InvoiceDate: date(t, "2026-03-10"),
		PostingDate: gen.NewOptDate(date(t, "2026-03-11")), Currency: "EUR", Items: []gen.CustomerInvoiceItemCreate{
			{AccountID: e.revenue, NetAmount: gen.Money{Amount: "100.00", Currency: "EUR"}, TaxCode: gen.NewOptString("A1"),
				CostCenterID: gen.NewOptUUID(c.Sales), ProfitCenterID: gen.NewOptUUID(c.Profit), Description: gen.NewOptString("Goods")},
			{AccountID: e.revenue, NetAmount: gen.Money{Amount: "5.00", Currency: "EUR"}}}}
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateCustomerInvoice(ctx, create, gen.CreateCustomerInvoiceParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetCustomerInvoice(ctx, gen.GetCustomerInvoiceParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListCustomerInvoices(ctx, gen.ListCustomerInvoicesParams{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("list page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListCustomerInvoices(ctx, gen.ListCustomerInvoicesParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", err)
		}
	})
}

// badRate is a ledger whose tax codes carry a rate the calculator cannot
// read: what a code the ledger read on the date never is.
type badRate struct{ ledger.Accounts }

func (badRate) TaxCode(context.Context, uuid.UUID, string, time.Time) (ledger.TaxCode, bool, error) {
	return ledger.TaxCode{Code: "A1", Direction: config.TaxOutput, Components: []ledger.TaxComponent{{Key: "vat", AccountKey: config.KeyOutput,
		RecoverablePct: "100", RatePct: "a lot"}}}, true, nil
}

// noCompany is a configuration without the invoice's company.
type noCompany struct{}

func (noCompany) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{}, 1, nil
}

// TestEntryThroughThePort: what the contract's own validation keeps from
// the handler and what only a port could answer: an invoice without
// items is refused; a company the configuration lacks rounds by the
// default; a tax code the calculator cannot calculate fails the entry.
func TestEntryThroughThePort(t *testing.T) {
	e := setup(t)
	in := receivables.Input{LegalEntityID: e.DE, CustomerID: e.customer, InvoiceDate: date(t, "2026-03-10"), Currency: "EUR",
		Items: []receivables.ItemInput{{AccountID: e.revenue, Net: receivables.Amount{Value: "0.09", Currency: "EUR"}, TaxCode: ptr("A1")}}}
	t.Run("no items", func(t *testing.T) {
		none := in
		none.Items = nil
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t).CreateInvoice(ctx, none)
			return err
		})
		if p := problem.From(err); p.Code != "receivables.items_required" || p.Status != 422 {
			t.Errorf("got %v, want 422 receivables.items_required", err)
		}
	})
	t.Run("a company the configuration lacks", func(t *testing.T) {
		h := endpoints(t)
		h.Settings = noCompany{}
		err := e.inRequest(t, func(ctx context.Context) error {
			v, err := h.CreateInvoice(ctx, in)
			if err == nil && (v.Tax != 2 || v.Gross != 11) {
				t.Errorf("tax and gross: got %d and %d minor units, want 2 and 11", v.Tax, v.Gross)
			}
			return err
		})
		if err != nil {
			t.Errorf("got %v, want the invoice entered", err)
		}
	})
	t.Run("a code the calculator cannot calculate", func(t *testing.T) {
		h := endpoints(t)
		h.Accounts = badRate{}
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.CreateInvoice(ctx, in)
			return err
		})
		var p *problem.Problem
		if err == nil || errors.As(err, &p) {
			t.Errorf("got %v, want an error that is no refusal", err)
		}
	})
}

func ptr[T any](v T) *T { return &v }
