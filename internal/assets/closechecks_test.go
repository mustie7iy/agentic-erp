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
// depreciation of 2026's period in the ledger, as the actor, naming at
// most limit late assets: "run <id>", or "<n> to depreciate" with, after
// a later period's run, "; later <id> <year>/<period>" and each late
// asset named as "; <description> from <year>/<period> catch-up <cents>
// <currency>".
func (e assetEnv) closeDepreciation(t *testing.T, as pgtest.Actor, ledger uuid.UUID, period, limit int) (string, error) {
	t.Helper()
	var d assets.PeriodDepreciation
	err := apitest.InRequest(t, e.Tenant, as, func(ctx context.Context) error {
		var err error
		d, err = assets.CloseChecks{}.Depreciation(ctx, e.DE, ledger, 2026, int16(period), periodEnd(period), limit)
		return err
	})
	if d.Run != nil {
		return "run " + d.Run.String(), err
	}
	out := fmt.Sprintf("%d to depreciate", d.Selected)
	if d.Later != nil {
		out += fmt.Sprintf("; later %s %d/%d", d.Later.ID, d.Later.Of.FiscalYear, d.Later.Of.Period)
	}
	for _, a := range d.Late {
		from := "<nil>"
		if a.MissingFrom != nil {
			from = fmt.Sprintf("%d/%d", a.MissingFrom.FiscalYear, a.MissingFrom.Period)
		}
		out += fmt.Sprintf("; %s from %s catch-up %d %s", a.Description, from, a.CatchUp, d.Later.Currency)
	}
	return out, err
}

// mustDepreciation is closeDepreciation as the tenant's human, failing
// the test on an error.
func (e assetEnv) mustDepreciation(t *testing.T, ledger uuid.UUID, period int) string {
	t.Helper()
	got, err := e.closeDepreciation(t, e.Tenant.Human, ledger, period, 20)
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

	if _, err := e.closeDepreciation(t, e.Reader(t, "reader", e.US), e.l0, 6, 20); err == nil || !strings.Contains(err.Error(), "not a company the caller may act in") {
		t.Errorf("a company the caller cannot act in: got %v, want an error", err)
	}
}

// TestCloseDepreciationLate: after July's run posted, a machine
// capitalised in June is selected by a run of June, which is refused as
// out of order; the port names July's run and the machine, its
// depreciation missing from June, and the catch-up a run of July posts
// for it (June's and July's 10.00). It names at most limit late assets,
// counting them all (none with a limit of 0), each missing from the
// period after the one it is booked through (2025/12: 2026/1), else from
// its depreciation start's (2025/11; its cost arrives in June, so June
// and July take 1200.00 over the 113 periods left, 10.62 each), or from none
// for an asset with neither. A repeat run of July posts exactly the
// catch-ups named (20.00 + 20.00 - 5.00 + 21.24), after which nothing is
// late. Without a later run nothing is late, and one ledger's later run
// does not count for another.
func TestCloseDepreciationLate(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	_, july := e.start(t, e.l0, 7)
	e.carry(t, july)
	want(t, "nothing late", e.mustDepreciation(t, e.l0, 6), "0 to depreciate")
	e.depreciable(t, "Lathe")
	e.Must(409, "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": 6}`, e.DE), "")
	later := "later " + july.String() + " 2026/7"
	want(t, "a late machine", e.mustDepreciation(t, e.l0, 6), "1 to depreciate; "+later+"; Lathe from 2026/6 catch-up 2000 EUR")
	want(t, "another ledger, no later run", e.mustDepreciation(t, e.l2, 6), "2 to depreciate")

	press, _ := e.depreciable(t, "Press")
	pgtest.Exec(t, `UPDATE assets.asset_terms SET booked_through_year = 2025, booked_through_period = 12, version = version + 1
		WHERE asset_id = $1 AND ledger_id = $2`, press, e.l0)
	a, _ := e.create(t, fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": "Drill", "cost_center_id": %q}`, e.DE, e.c.Admin))
	e.depreciate(t, a["id"].(string), e.l0, 7, 500)
	saw, _ := e.depreciable(t, "Saw")
	pgtest.Exec(t, `UPDATE assets.asset_terms SET depreciation_start = '2025-11-01', version = version + 1 WHERE asset_id = $1 AND ledger_id = $2`, saw, e.l0)
	want(t, "four late, none named", e.mustDepreciationOf(t, 6, 0), "4 to depreciate; "+later)
	want(t, "four late, one named", e.mustDepreciationOf(t, 6, 1), "4 to depreciate; "+later+"; Lathe from 2026/6 catch-up 2000 EUR")
	want(t, "four late, all named", e.mustDepreciationOf(t, 6, 20), "4 to depreciate; "+later+"; Lathe from 2026/6 catch-up 2000 EUR; "+
		"Press from 2026/1 catch-up 2000 EUR; Drill from <nil> catch-up -500 EUR; Saw from 2025/11 catch-up 2124 EUR")

	_, repeat := e.start(t, e.l0, 7)
	want(t, "the repeat run of July, the catch-ups", runOf(e.carry(t, repeat)), "posted 5 4 0 56.24; 1 posted 5 4 0 56.24")
	want(t, "caught up", e.mustDepreciation(t, e.l0, 6), "0 to depreciate")
}

// mustDepreciationOf is mustDepreciation in 0L naming at most limit late
// assets.
func (e assetEnv) mustDepreciationOf(t *testing.T, period, limit int) string {
	t.Helper()
	got, err := e.closeDepreciation(t, e.Tenant.Human, e.l0, period, limit)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCloseDepreciationFailures reads with each database statement
// failing in turn, with and without a posted run, and outside a unit of
// work.
func TestCloseDepreciationFailures(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	_, july := e.start(t, e.l2, 7)
	e.carry(t, july)
	e.depreciable(t, "Lathe")
	cases := []struct {
		name   string
		ledger uuid.UUID
		period int16
	}{{"a posted run", e.l0, 6}, {"assets to depreciate", e.l0, 7}, {"late assets", e.l2, 6}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, c.name, func(ctx context.Context) error {
				_, err := assets.CloseChecks{}.Depreciation(ctx, e.DE, c.ledger, 2026, c.period, periodEnd(int(c.period)), 20)
				return err
			})
		})
	}
	if _, err := (assets.CloseChecks{}).Depreciation(context.Background(), e.DE, e.l0, 2026, 6, periodEnd(6), 20); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
}
