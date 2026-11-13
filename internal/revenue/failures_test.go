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
	// An active contract whose schedule reads both kinds of obligation: goods part delivered and a service over the year.
	active, delivered := e.contract(t, e.goods(), e.support())
	e.Must(200, "PATCH", contracts+"/"+active, set(overTime(2, "2026-01-01", "2026-12-31", "1200.00")), `"1"`)
	e.activated(t, active)
	e.delivered(t, delivered, 1, "50", "2026-01-15")
	scheduled := uuid.MustParse(active)
	// A run that posted, and a delivery since, so the next run reads what the first posted and posts again.
	ran := uuid.MustParse(e.ran(t, 1)["id"].(string))
	e.delivered(t, delivered, 1, "70", "2026-02-15")
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
		"schedule": func(ctx context.Context) error {
			_, err := h.GetRevenueSchedule(ctx, gen.GetRevenueScheduleParams{ID: scheduled})
			return err
		},
		"run": func(ctx context.Context) error {
			_, err := h.CreateRevenueRecognitionRun(ctx, &gen.RevenueRecognitionRunCreate{LegalEntityID: e.DE, FiscalYear: 2026, FiscalPeriod: 2},
				gen.CreateRevenueRecognitionRunParams{})
			return err
		},
		"get run": func(ctx context.Context) error {
			_, err := h.GetRevenueRecognitionRun(ctx, gen.GetRevenueRecognitionRunParams{ID: ran})
			return err
		},
		"list runs": func(ctx context.Context) error {
			_, err := h.ListRevenueRecognitionRuns(ctx, gen.ListRevenueRecognitionRunsParams{})
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
	t.Run("list runs page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListRevenueRecognitionRuns(ctx, gen.ListRevenueRecognitionRunsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
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
