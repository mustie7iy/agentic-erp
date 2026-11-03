package closing_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/closing"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// endpoints are the closing context's as the API wires them, with a page
// codec for the lists.
func endpoints(t *testing.T) *closing.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Closing(app.Config{Pages: pages})
}

// TestCloseDatabaseFailures calls each handler of the close and its tasks
// with each of its database statements failing in turn, and outside a
// unit of work.
func TestCloseDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	_, tag = e.Must(200, "PATCH", "/v1/period-closes/"+id.String()+"/tasks/REVIEW", `{"assignee_id": "`+e.Tenant.Human.ID.String()+`"}`, tag)
	_, tag = e.Must(200, "POST", "/v1/period-closes/"+id.String()+"/tasks/REVIEW:complete", "", tag)
	h := endpoints(t)
	assignee := gen.NewOptNilUUID(e.Approver.ID)
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreatePeriodClose(ctx, &gen.PeriodCloseCreate{LegalEntityID: e.DE, FiscalYear: 2026, FiscalPeriod: 2}, gen.CreatePeriodCloseParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetPeriodClose(ctx, gen.GetPeriodCloseParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListPeriodCloses(ctx, gen.ListPeriodClosesParams{})
			return err
		},
		"list tasks": func(ctx context.Context) error {
			_, err := h.ListCloseTasks(ctx, gen.ListCloseTasksParams{Assignee: gen.NewOptString("me"), PageSize: gen.NewOptInt32(1)})
			return err
		},
		"assign": func(ctx context.Context) error {
			_, err := h.UpdateCloseTask(ctx, &gen.CloseTaskUpdate{AssigneeID: assignee}, gen.UpdateCloseTaskParams{ID: id, Code: "AP_CUTOFF", IfMatch: tag})
			return err
		},
		"complete": func(ctx context.Context) error {
			_, err := h.CompleteCloseTask(ctx, gen.OptCloseTaskComplete{}, gen.CompleteCloseTaskParams{ID: id, Code: "AP_CUTOFF", IfMatch: tag})
			return err
		},
		"reopen": func(ctx context.Context) error {
			_, err := h.ReopenCloseTask(ctx, &gen.ReasonInput{Reason: "again"}, gen.ReopenCloseTaskParams{ID: id, Code: "REVIEW", IfMatch: tag})
			return err
		},
		"calendar": func(ctx context.Context) error {
			_, err := h.GetCloseCalendar(ctx, gen.GetCloseCalendarParams{LegalEntityID: e.DE, FiscalYear: 2026})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListCloseTasks(ctx, gen.ListCloseTasksParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	if got := problem.From(err).Code; got != "request.invalid_page_size" {
		t.Errorf("page size 0: got %s, want request.invalid_page_size", got)
	}
}

// zoneless is the active configuration with company 2000's time zone one
// no binary has: its date cannot be told.
type zoneless struct{ configuration.Settings }

func (z zoneless) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, v, err := z.Settings.Document(ctx)
	if err == nil {
		c := *doc
		c.Companies = append([]config.Company{}, doc.Companies...)
		c.Company("2000").TimeZone = "Nowhere/Never"
		doc = &c
	}
	return doc, v, err
}

// TestCloseBrokenData: a close whose legal entity the tenancy context
// does not hold, a task whose assignee the identity context does not, or a
// company whose time zone cannot be loaded fails the response rather than
// showing it without a name or on another day.
func TestCloseBrokenData(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	got, _ := open(t, e.Client, e.DE, 1, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	h := endpoints(t)
	h.Settings = zoneless{}
	cases := map[string]func(ctx context.Context) error{
		"a time zone, reading the close": func(ctx context.Context) error {
			_, err := h.GetPeriodClose(ctx, gen.GetPeriodCloseParams{ID: id})
			return err
		},
		"a time zone, reading the calendar": func(ctx context.Context) error {
			_, err := h.GetCloseCalendar(ctx, gen.GetCloseCalendarParams{LegalEntityID: e.DE, FiscalYear: 2026})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); err == nil || !strings.Contains(err.Error(), "Nowhere/Never") {
				t.Errorf("got %v, want the zone's failure", err)
			}
		})
	}

	pgtest.Exec(t, `UPDATE closing.close_task SET assignee_id = $2, version = version + 1 WHERE close_id = $1 AND code = 'REVIEW'`, id, uuid.New())
	h = endpoints(t)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetPeriodClose(ctx, gen.GetPeriodCloseParams{ID: id})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not held by the identity context") {
		t.Errorf("an unknown assignee: got %v, want the identity context's failure", err)
	}

	orphan := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), 2026, 1, '2026-01-31', 1, $3, $3)`, e.Tenant.ID, orphan, e.Tenant.Human.ID)
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetPeriodClose(ctx, gen.GetPeriodCloseParams{ID: orphan})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not held by the tenancy context") {
		t.Errorf("an unknown legal entity: got %v, want the tenancy context's failure", err)
	}
}
