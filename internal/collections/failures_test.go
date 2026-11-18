package collections_test

import (
	"context"
	"errors"
	"fmt"
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
	entries := e.story(t)
	h := endpoints(t)
	// A promise of CI-A, read back, listed and judged by the worklist and the runs.
	promised, _ := e.Must(201, "POST", "/v1/promises-to-pay", promiseOf(e.DE, e.north, "1.00", lastHeld, entries[0]), "")
	promiseID := uuid.MustParse(apitest.String(t, promised, "id"))
	started, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	id := uuid.MustParse(apitest.String(t, started, "id"))
	// A case open on CI-B, assigned, so that reading it names a person.
	opened, _ := e.Must(201, "POST", "/v1/dispute-cases", dispute(e.DE, entries[1], "1.00", "EUR", fmt.Sprintf(`, "assignee_id": %q`, e.Tenant.Human.ID)), "")
	caseID := uuid.MustParse(apitest.String(t, opened, "id"))
	colleague := e.Reader(t, "A colleague in company 2000", e.DE)
	// A block placed and released, so that reading it names a person, and one to release.
	placed, placedTag := e.Must(201, "POST", "/v1/dunning-blocks", blockOf(e.DE, e.east, nil, lastHeld), "")
	e.Must(200, "POST", "/v1/dunning-blocks/"+apitest.String(t, placed, "id")+":release", "", placedTag)
	releasedID := uuid.MustParse(apitest.String(t, placed, "id"))
	active, _ := e.Must(201, "POST", "/v1/dunning-blocks", blockOf(e.DE, e.east, nil, lastHeld), "")
	activeID := uuid.MustParse(apitest.String(t, active, "id"))
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateDunningRun(ctx, &gen.DunningRunCreate{LegalEntityID: e.DE, KeyDate: date(t, keyDate)}, gen.CreateDunningRunParams{})
			return err
		},
		"confirm": func(ctx context.Context) error {
			_, err := h.ConfirmDunningRun(ctx, gen.ConfirmDunningRunParams{ID: id, IfMatch: `"1"`})
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
		"worklist": func(ctx context.Context) error {
			_, err := h.GetCollectionWorklist(ctx, gen.GetCollectionWorklistParams{LegalEntityID: e.DE, KeyDate: date(t, keyDate)})
			return err
		},
		"open dispute": func(ctx context.Context) error {
			_, err := h.CreateDisputeCase(ctx, &gen.DisputeCaseCreate{LegalEntityID: e.DE, FiscalYear: 2026, JournalEntryID: entries[0].ID, LineNo: 1,
				DisputedAmount: gen.Money{Amount: "1.00", Currency: "EUR"}, Reason: "x", AssigneeID: gen.NewOptUUID(e.Tenant.Human.ID)},
				gen.CreateDisputeCaseParams{})
			return err
		},
		"get dispute": func(ctx context.Context) error {
			_, err := h.GetDisputeCase(ctx, gen.GetDisputeCaseParams{ID: caseID})
			return err
		},
		"list disputes": func(ctx context.Context) error {
			_, err := h.ListDisputeCases(ctx, gen.ListDisputeCasesParams{})
			return err
		},
		"update dispute": func(ctx context.Context) error {
			_, err := h.UpdateDisputeCase(ctx, &gen.DisputeCaseUpdate{Note: gen.NewOptNilString("x")}, gen.UpdateDisputeCaseParams{ID: caseID, IfMatch: `"1"`})
			return err
		},
		"reassign dispute": func(ctx context.Context) error {
			_, err := h.UpdateDisputeCase(ctx, &gen.DisputeCaseUpdate{AssigneeID: gen.NewOptNilUUID(colleague.ID)},
				gen.UpdateDisputeCaseParams{ID: caseID, IfMatch: `"1"`})
			return err
		},
		"place item block": func(ctx context.Context) error {
			_, err := h.CreateDunningBlock(ctx, &gen.DunningBlockCreate{LegalEntityID: e.DE, CustomerID: e.north, FiscalYear: gen.NewOptInt32(2026),
				JournalEntryID: gen.NewOptUUID(entries[0].ID), LineNo: gen.NewOptInt32(1), Reason: "x", ValidTo: date(t, lastHeld)},
				gen.CreateDunningBlockParams{})
			return err
		},
		"get block": func(ctx context.Context) error {
			_, err := h.GetDunningBlock(ctx, gen.GetDunningBlockParams{ID: releasedID})
			return err
		},
		"list blocks": func(ctx context.Context) error {
			_, err := h.ListDunningBlocks(ctx, gen.ListDunningBlocksParams{})
			return err
		},
		"release block": func(ctx context.Context) error {
			_, err := h.ReleaseDunningBlock(ctx, gen.ReleaseDunningBlockParams{ID: activeID, IfMatch: `"1"`})
			return err
		},
		"record promise": func(ctx context.Context) error {
			_, err := h.CreatePromiseToPay(ctx, &gen.PromiseToPayCreate{LegalEntityID: e.DE, CustomerID: e.north, PromisedAmount: gen.Money{Amount: "1.00",
				Currency: "EUR"}, PromisedDate: date(t, lastHeld), Items: []gen.PromiseToPayItemRef{{FiscalYear: 2026, JournalEntryID: entries[1].ID, LineNo: 1}}},
				gen.CreatePromiseToPayParams{})
			return err
		},
		"get promise": func(ctx context.Context) error {
			_, err := h.GetPromiseToPay(ctx, gen.GetPromiseToPayParams{ID: promiseID})
			return err
		},
		"list promises": func(ctx context.Context) error {
			_, err := h.ListPromisesToPay(ctx, gen.ListPromisesToPayParams{})
			return err
		},
		"worklist after the promise": func(ctx context.Context) error {
			_, err := h.GetCollectionWorklist(ctx, gen.GetCollectionWorklistParams{LegalEntityID: e.DE, KeyDate: date(t, "2100-12-31")})
			return err
		},
		"close dispute": func(ctx context.Context) error {
			_, err := h.CloseDisputeCase(ctx, &gen.DisputeCaseClose{Outcome: "rejected", Reason: "x"},
				gen.CloseDisputeCaseParams{ID: caseID, IfMatch: `"1"`})
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
		"blocks page size": func(ctx context.Context) error {
			_, err := h.ListDunningBlocks(ctx, gen.ListDunningBlocksParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"promises page size": func(ctx context.Context) error {
			_, err := h.ListPromisesToPay(ctx, gen.ListPromisesToPayParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"disputes page size": func(ctx context.Context) error {
			_, err := h.ListDisputeCases(ctx, gen.ListDisputeCasesParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"worklist page size": func(ctx context.Context) error {
			_, err := h.GetCollectionWorklist(ctx, gen.GetCollectionWorklistParams{LegalEntityID: e.DE, KeyDate: date(t, keyDate), PageSize: gen.NewOptInt32(0)})
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
	t.Run("a worklist customer that is no business partner", func(t *testing.T) {
		h := endpoints(t)
		h.Partners = strangers{}
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.GetCollectionWorklist(ctx, gen.GetCollectionWorklistParams{LegalEntityID: e.DE, KeyDate: date(t, keyDate)})
			return err
		})
		var p *problem.Problem
		if err == nil || errors.As(err, &p) || !strings.Contains(err.Error(), "is no business partner") {
			t.Errorf("got %v, want an error that is no refusal, naming the customer", err)
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

// nobody names no principal.
type nobody struct{}

func (nobody) PersonNames(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return map[uuid.UUID]string{}, nil
}

// TestDisputesThroughThePort: what only a port could answer: an open item
// whose customer is no business partner fails the case; an assignee the
// identity context does not name fails the read.
func TestDisputesThroughThePort(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	opened, _ := e.Must(201, "POST", "/v1/dispute-cases", dispute(e.DE, entries[0], "1.00", "EUR", fmt.Sprintf(`, "assignee_id": %q`, e.Tenant.Human.ID)), "")
	id := uuid.MustParse(apitest.String(t, opened, "id"))
	for _, c := range []struct {
		name   string
		change func(*collections.Endpoints)
		call   func(ctx context.Context, h *collections.Endpoints) error
		want   string
	}{
		{"a customer that is no business partner", func(h *collections.Endpoints) { h.Partners = strangers{} },
			func(ctx context.Context, h *collections.Endpoints) error {
				_, err := h.OpenCase(ctx, collections.CaseInput{LegalEntityID: e.DE, Item: ledger.ItemRef{FiscalYear: 2026, EntryID: entries[1].ID, LineNo: 1},
					Amount: "1.00", Currency: "EUR", Reason: "x"})
				return err
			}, "is no business partner"},
		{"an assignee nobody names", func(h *collections.Endpoints) { h.People = nobody{} },
			func(ctx context.Context, h *collections.Endpoints) error {
				_, err := h.GetDisputeCase(ctx, gen.GetDisputeCaseParams{ID: id})
				return err
			}, "is not held by the identity context"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := endpoints(t)
			c.change(h)
			err := e.inRequest(t, func(ctx context.Context) error { return c.call(ctx, h) })
			var p *problem.Problem
			if err == nil || errors.As(err, &p) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error that is no refusal, saying %q", err, c.want)
			}
		})
	}
}
