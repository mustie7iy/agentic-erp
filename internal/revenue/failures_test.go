package revenue_test

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
	h := endpoints(t)
	order, _ := e.confirmed(t, e.goods(), e.support())
	other, _ := e.confirmed(t, e.goods())
	created, _ := e.Must(201, "POST", contracts, forOrder(other), "")
	id := uuid.MustParse(created["id"].(string))
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateRevenueContract(ctx, &gen.RevenueContractCreate{SalesOrderID: uuid.MustParse(order)}, gen.CreateRevenueContractParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetRevenueContract(ctx, gen.GetRevenueContractParams{ID: id})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateRevenueContract(ctx, &gen.RevenueContractUpdate{Obligations: []gen.PerformanceObligationUpdate{{ObligationNo: 1,
				Timing:      gen.NewOptString("over_time"),
				ServiceFrom: gen.NewOptNilDate(day), ServiceTo: gen.NewOptNilDate(day),
				StandaloneSellingPrice: gen.NewOptMoney(gen.Money{Amount: "900.00", Currency: "EUR"})}}},
				gen.UpdateRevenueContractParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"activate": func(ctx context.Context) error {
			_, err := h.ActivateRevenueContract(ctx, gen.ActivateRevenueContractParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListRevenueContracts(ctx, gen.ListRevenueContractsParams{})
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
			_, err := h.ListRevenueContracts(ctx, gen.ListRevenueContractsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
