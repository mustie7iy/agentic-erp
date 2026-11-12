package sales_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t, time.Now())
	id := uuid.MustParse(e.quoted(t))
	accepted, rejected := uuid.MustParse(e.quoted(t)), uuid.MustParse(e.quoted(t))
	create := &gen.QuotationCreate{LegalEntityID: e.DE, CustomerID: e.customer, ValidUntil: instant(t, far+"T00:00:00Z"), Currency: "EUR",
		Items: []gen.SalesItemCreate{
			{ItemKey: gen.NewOptString("BRACKET-M8"), Description: "Bracket", Quantity: gen.Quantity{Value: "2", Unit: "EA"},
				UnitPrice: gen.Money{Amount: "10.00", Currency: "EUR"}, AccountID: e.revenue, TaxCode: gen.NewOptString("A1")},
			{Description: "Freight", Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.Money{Amount: "5.00", Currency: "EUR"},
				PriceUnit: gen.NewOptInt32(1), AccountID: e.other, TaxCode: gen.NewOptString("A1")}}}
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateQuotation(ctx, create, gen.CreateQuotationParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetQuotation(ctx, gen.GetQuotationParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListQuotations(ctx, gen.ListQuotationsParams{})
			return err
		},
		"accept": func(ctx context.Context) error {
			_, err := h.AcceptQuotation(ctx, gen.AcceptQuotationParams{ID: accepted, IfMatch: `"1"`})
			return err
		},
		"decline": func(ctx context.Context) error {
			_, err := h.DeclineQuotation(ctx, gen.DeclineQuotationParams{ID: rejected, IfMatch: `"1"`})
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
			_, err := h.ListQuotations(ctx, gen.ListQuotationsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
