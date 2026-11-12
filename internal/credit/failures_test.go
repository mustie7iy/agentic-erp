package credit_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/credit"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *credit.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Credit(app.Config{Pages: pages})
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	e.story(t)
	h := endpoints(t)
	set, _ := e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
	id := uuid.MustParse(apitest.String(t, set, "id"))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateCreditLimit(ctx, &gen.CustomerCreditLimitCreate{LegalEntityID: e.DE, CustomerID: e.east,
				Limit: gen.Money{Amount: "100.00", Currency: "EUR"}}, gen.CreateCreditLimitParams{})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateCreditLimit(ctx, &gen.CustomerCreditLimitUpdate{Limit: gen.Money{Amount: "750.00", Currency: "EUR"}},
				gen.UpdateCreditLimitParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetCreditLimit(ctx, gen.GetCreditLimitParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListCreditLimits(ctx, gen.ListCreditLimitsParams{})
			return err
		},
		"exposure": func(ctx context.Context) error {
			_, err := h.GetCreditExposure(ctx, gen.GetCreditExposureParams{LegalEntityID: e.DE, CustomerID: e.north, Amount: gen.NewOptString("1.00")})
			return err
		},
		"exposure without a limit": func(ctx context.Context) error {
			_, err := h.GetCreditExposure(ctx, gen.GetCreditExposureParams{LegalEntityID: e.DE, CustomerID: e.east})
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
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListCreditLimits(ctx, gen.ListCreditLimitsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}

// owing is one open item of the amount in the company's currency for
// each amount given: what the ledger's booked amounts never add up to.
type owing []money.Minor

func (o owing) Receivable(_ context.Context, _ uuid.UUID, customers []uuid.UUID) ([]ledger.ReceivableItem, error) {
	out := make([]ledger.ReceivableItem, len(o))
	for i, a := range o {
		out[i] = ledger.ReceivableItem{PartnerID: customers[0], AmountLC: a}
	}
	return out, nil
}

// TestAmountsOutOfRange: open items, a limit and an amount that add up
// to more than an amount holds are refused, not compared wrongly: the
// items' sum, the limit less a large credit, and the exposure with the
// amount checked.
func TestAmountsOutOfRange(t *testing.T) {
	e := setup(t)
	e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
	const most = money.Minor(math.MaxInt64)
	one := "0.01"
	for _, c := range []struct {
		name     string
		customer uuid.UUID
		items    owing
		amount   *string
		code     string // empty: answered
	}{
		{"the items' sum, without a limit", e.east, owing{most, 1}, nil, "credit.amount_too_large"},
		{"the items' sum", e.north, owing{most, 1}, nil, "credit.amount_too_large"},
		{"the limit less a credit", e.north, owing{-most}, nil, "credit.amount_too_large"},
		{"the exposure with the amount", e.north, owing{most}, &one, "credit.amount_too_large"},
		{"the largest exposure alone", e.north, owing{most}, nil, ""},
		{"the largest exposure without a limit, with an amount", e.east, owing{most}, &one, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := endpoints(t)
			h.OpenItems = c.items
			var x credit.Exposure
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
				x, err = h.ExposureOf(ctx, e.DE, c.customer, c.amount)
				return err
			})
			if c.code == "" {
				if err != nil {
					t.Fatalf("got %v, want it answered", err)
				}
				if x.Owed != most {
					t.Errorf("owed: got %v, want %v", x.Owed, most)
				}
				return
			}
			if got := fmt.Sprintf("%d %s", problem.From(err).Status, problem.From(err).Code); got != "422 "+c.code {
				t.Errorf("got %s, want 422 %s", got, c.code)
			}
		})
	}
}
