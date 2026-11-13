package leases_test

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
	id := uuid.MustParse(e.create(t, office)["id"].(string))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateLeaseContract(ctx, &gen.LeaseContractCreate{LegalEntityID: e.DE, LessorID: e.Supplier, CostCenterID: e.center,
				CommencementDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), TermMonths: 12, PaymentAmount: gen.Money{Amount: "100.00", Currency: "EUR"},
				PaymentFrequency: "monthly", PaymentTiming: "arrears", AnnualDiscountRatePercent: "5"}, gen.CreateLeaseContractParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetLeaseContract(ctx, gen.GetLeaseContractParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListLeaseContracts(ctx, gen.ListLeaseContractsParams{})
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
			_, err := h.ListLeaseContracts(ctx, gen.ListLeaseContractsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
