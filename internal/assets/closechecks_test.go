package assets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// periodEnd is the last day of 2026's period.
func periodEnd(period int) time.Time {
	return time.Date(2026, time.Month(period)+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
}

// closeDepreciation is what the close's port reads of company 2000's
// depreciation of 2026's period in the ledger, as the actor: "run <id>"
// or "<n> to depreciate".
func (e assetEnv) closeDepreciation(t *testing.T, as pgtest.Actor, ledger uuid.UUID, period int) (string, error) {
	t.Helper()
	var d assets.PeriodDepreciation
	err := apitest.InRequest(t, e.Tenant, as, func(ctx context.Context) error {
		var err error
		d, err = assets.CloseChecks{}.Depreciation(ctx, e.DE, ledger, 2026, int16(period), periodEnd(period))
		return err
	})
	if d.Run != nil {
		return "run " + d.Run.String(), err
	}
	return fmt.Sprintf("%d to depreciate", d.Selected), err
}

// mustDepreciation is closeDepreciation as the tenant's human, failing
// the test on an error.
func (e assetEnv) mustDepreciation(t *testing.T, ledger uuid.UUID, period int) string {
	t.Helper()
	got, err := e.closeDepreciation(t, e.Tenant.Human, ledger, period)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCloseDepreciation: the close's port finds nothing to depreciate in
// a company without assets and in a period before an asset's
// depreciation starts; it counts the assets a run would select while the
// period has no posted run, a queued, a failed and a cancelled run (whose
// batch posted) included; once a run is posted it names the latest one,
// in that ledger alone. A company the caller may not act in is an error.
func TestCloseDepreciation(t *testing.T) {
	e := newRuns(t, "platform.operation.read", "platform.operation.cancel")
	want(t, "no assets", e.mustDepreciation(t, e.l0, 6), "0 to depreciate")
	e.depreciable(t, "Mill")
	e.depreciable(t, "Lathe")
	want(t, "before the depreciation starts", e.mustDepreciation(t, e.l0, 5), "0 to depreciate")
	want(t, "two assets", e.mustDepreciation(t, e.l0, 6), "2 to depreciate")

	_, failed := e.start(t, e.l0, 6)
	want(t, "a queued run", e.mustDepreciation(t, e.l0, 6), "2 to depreciate")
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		return h.FailDepreciationRun(ctx, failed, problem.Conflict("ledger.period_not_open", "closed"))
	}); err != nil {
		t.Fatal(err)
	}
	want(t, "a failed run", e.mustDepreciation(t, e.l0, 6), "2 to depreciate")

	op, cancelled := e.start(t, e.l0, 6)
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		_, err := h.PlanDepreciationRun(ctx, cancelled)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		return h.PostDepreciationBatch(ctx, cancelled, 1)
	}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/operations/" + op["id"].(string)
	_, tag := e.Must(200, "GET", path, "", "")
	e.Must(200, "POST", path+":cancel", "", tag)
	want(t, "the cancelled run", runOf(e.carry(t, cancelled)), "failed 2 2 0 20.00; 1 posted 2 2 0 20.00")
	want(t, "a cancelled run whose batch posted", e.mustDepreciation(t, e.l0, 6), "2 to depreciate")

	_, posted := e.start(t, e.l0, 6)
	e.carry(t, posted)
	want(t, "a posted run", e.mustDepreciation(t, e.l0, 6), "run "+posted.String())
	_, repeat := e.start(t, e.l0, 6)
	e.carry(t, repeat)
	want(t, "the latest posted run", e.mustDepreciation(t, e.l0, 6), "run "+repeat.String())
	want(t, "another ledger", e.mustDepreciation(t, e.l2, 6), "2 to depreciate")
	want(t, "another period", e.mustDepreciation(t, e.l0, 7), "2 to depreciate")

	if _, err := e.closeDepreciation(t, e.Reader(t, "reader", e.US), e.l0, 6); err == nil || !strings.Contains(err.Error(), "not a company the caller may act in") {
		t.Errorf("a company the caller cannot act in: got %v, want an error", err)
	}
}

// TestCloseDepreciationFailures reads with each database statement
// failing in turn, with and without a posted run, and outside a unit of
// work.
func TestCloseDepreciationFailures(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	for name, ledger := range map[string]uuid.UUID{"a posted run": e.l0, "assets to depreciate": e.l2} {
		t.Run(name, func(t *testing.T) {
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, func(ctx context.Context) error {
				_, err := assets.CloseChecks{}.Depreciation(ctx, e.DE, ledger, 2026, 6, periodEnd(6))
				return err
			})
		})
	}
	if _, err := (assets.CloseChecks{}).Depreciation(context.Background(), e.DE, e.l0, 2026, 6, periodEnd(6)); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
}
