package collections_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *collections.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Collections(app.Config{Pages: pages})
}

func (e env) inRequest(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	e.story(t)
	h := endpoints(t)
	started, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	id := uuid.MustParse(apitest.String(t, started, "id"))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateDunningRun(ctx, &gen.DunningRunCreate{LegalEntityID: e.DE, KeyDate: date(t, keyDate)}, gen.CreateDunningRunParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetDunningRun(ctx, gen.GetDunningRunParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListDunningRuns(ctx, gen.ListDunningRunsParams{})
			return err
		},
		"notices": func(ctx context.Context) error {
			_, err := h.ListDunningNotices(ctx, gen.ListDunningNoticesParams{ID: id})
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
	for name, call := range map[string]func(ctx context.Context) error{
		"list page size": func(ctx context.Context) error {
			_, err := h.ListDunningRuns(ctx, gen.ListDunningRunsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"notices page size": func(ctx context.Context) error {
			_, err := h.ListDunningNotices(ctx, gen.ListDunningNoticesParams{ID: id, PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if p := problem.From(e.inRequest(t, call)); p.Code != "request.invalid_page_size" {
				t.Errorf("got %v, want request.invalid_page_size", p)
			}
		})
	}
}

// noCompany is a configuration without the run's company.
type noCompany struct{}

func (noCompany) Document(context.Context) (*config.Config, int32, error) {
	return &config.Config{}, 1, nil
}

// huge are open items whose amounts in the company's currency add up to
// more than an amount holds, within one customer's notice or across two
// customers' notices: what the ledger's booked amounts never do.
type huge struct {
	ledger.OpenItems
	customers []uuid.UUID
}

func (h huge) Receivable(ctx context.Context, legalEntity uuid.UUID, _ []uuid.UUID) ([]ledger.ReceivableItem, error) {
	real, err := h.OpenItems.Receivable(ctx, legalEntity, nil)
	if err != nil || len(real) == 0 {
		return nil, errors.Join(err, errors.New("no open item to enlarge"))
	}
	out := make([]ledger.ReceivableItem, len(h.customers))
	for i, c := range h.customers {
		out[i] = real[0]
		out[i].PartnerID, out[i].LineNo, out[i].AmountLC = c, int32(i+1), money.Minor(1)<<62
	}
	return out, nil
}

// strangers names no business partner.
type strangers struct{}

func (strangers) Names(context.Context, []uuid.UUID) (map[uuid.UUID]partner.Named, error) {
	return map[uuid.UUID]partner.Named{}, nil
}

// TestRunsThroughThePort: what only a port could answer: a company the
// configuration lacks has no dunning levels; more items than one run
// proposes are refused, the limit named; amounts that add up to more than
// an amount holds are refused, in a notice and across notices; an open
// item whose customer is no business partner fails the run.
func TestRunsThroughThePort(t *testing.T) {
	e := setup(t)
	e.story(t)
	in := collections.RunInput{LegalEntityID: e.DE, KeyDate: date(t, keyDate)}
	propose := func(t *testing.T, h *collections.Endpoints) error {
		t.Helper()
		return e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ProposeRun(ctx, in)
			return err
		})
	}
	withItems := func(customers ...uuid.UUID) func(*collections.Endpoints) {
		return func(h *collections.Endpoints) { h.OpenItems = huge{customers: customers} }
	}
	for _, c := range []struct {
		name   string
		change func(*collections.Endpoints)
		code   string
		detail string
	}{
		{"a company the configuration lacks", func(h *collections.Endpoints) { h.Settings = noCompany{} }, "collections.no_dunning_levels", "Company 2000"},
		{"more items than a run proposes", func(h *collections.Endpoints) { h.MaxItems = 2 }, "collections.run_too_large", "more than 2 overdue items"},
		{"a notice's amounts out of range", withItems(e.north, e.north), "collections.amount_too_large", "more than an amount can hold"},
		{"the notices' amounts out of range", withItems(e.north, e.south), "collections.amount_too_large", "more than an amount can hold"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := endpoints(t)
			c.change(h)
			p := problem.From(propose(t, h))
			if p.Code != c.code || p.Status != 422 || !strings.Contains(p.Detail, c.detail) {
				t.Errorf("got %d %s %q, want 422 %s saying %q", p.Status, p.Code, p.Detail, c.code, c.detail)
			}
		})
	}
	t.Run("as many items as a run proposes", func(t *testing.T) {
		h := endpoints(t)
		h.MaxItems = 3
		if err := propose(t, h); err != nil {
			t.Errorf("a run of 3 items at a limit of 3: got %v, want it proposed", err)
		}
	})
	t.Run("a customer that is no business partner", func(t *testing.T) {
		h := endpoints(t)
		h.Partners = strangers{}
		err := propose(t, h)
		var p *problem.Problem
		if err == nil || errors.As(err, &p) || !strings.Contains(err.Error(), "is no business partner") {
			t.Errorf("got %v, want an error that is no refusal, naming the customer", err)
		}
	})
}
