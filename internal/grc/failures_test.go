package grc_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/grc"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *grc.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.GRC(app.Config{Pages: pages})
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	recorded, _ := e.record(t, "PAY-01", "Payments")
	id := uuid.MustParse(recorded)
	e.Must(201, "POST", "/v1/controls/"+recorded+"/tests", testOf(day(t, 0), "passed", "Seen."), "")
	spare, _ := e.record(t, "PAY-02", "Payments")
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateControl(ctx, &gen.ControlCreate{Code: "PAY-09", Objective: "x", Process: "Payments", OwnerID: e.carla.ID,
				Frequency: "daily", Nature: "manual"}, gen.CreateControlParams{})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateControl(ctx, &gen.ControlUpdate{Frequency: gen.NewOptString("daily"), OwnerID: gen.NewOptUUID(e.carla.ID)},
				gen.UpdateControlParams{ID: id, IfMatch: `"1"`})
			return err
		},
		"retire": func(ctx context.Context) error {
			_, err := h.RetireControl(ctx, gen.RetireControlParams{ID: uuid.MustParse(spare), IfMatch: `"1"`})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetControl(ctx, gen.GetControlParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListControls(ctx, gen.ListControlsParams{})
			return err
		},
		"test": func(ctx context.Context) error {
			_, err := h.CreateControlTest(ctx, &gen.ControlTestCreate{TestedOn: e.today(t), Result: "failed", Note: "Seen."},
				gen.CreateControlTestParams{ID: id})
			return err
		},
		"tests": func(ctx context.Context) error {
			_, err := h.ListControlTests(ctx, gen.ListControlTestsParams{ID: id})
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
			_, err := h.ListControls(ctx, gen.ListControlsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"tests page size": func(ctx context.Context) error {
			_, err := h.ListControlTests(ctx, gen.ListControlTestsParams{ID: id, PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call)
			if p := problem.From(err); p.Code != "request.invalid_page_size" {
				t.Errorf("got %v, want request.invalid_page_size", p)
			}
		})
	}
}
