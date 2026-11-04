package cash_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// keptSnapshot writes a snapshot of Maple of the date, of the kind, with
// lines "value date|bucket end|bank|currency|source|item|amount" (bank
// main, usd or none, item "" for none), and returns its id.
func (e actualsEnv) keptSnapshot(t *testing.T, kind, date string, lines ...string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, name, snapshot_date, time_zone, horizon_end, config_version, taken_at, taken_by)
		VALUES ($1, $2, $3, $4, CASE WHEN $4 = 'on_demand' THEN 'On demand' END, $5, 'America/Toronto', $5::date + 365, 1, clock_timestamp(), $6)`,
		e.Tenant.ID, id, e.le, kind, date, e.Tenant.Human.ID)
	banks := map[string]*uuid.UUID{"main": &e.main, "usd": &e.usd, "none": nil}
	for i, l := range lines {
		f := strings.Split(l, "|")
		e.exec(t, `INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, bucket_end, house_bank_id, currency, source, certainty,
				liquidity_item, amount, flow_count)
			VALUES ($1, $2, $3, $3, $4, nullif($5, '')::date, $6, $7, $8, 'expected', nullif($9, ''), $10::bigint, 1)`,
			e.Tenant.ID, id, i+1, f[0], f[1], banks[f[2]], f[3], f[4], f[5], f[6])
	}
	return id
}

// forecastOf renders shares as "snapshot date line value date +lag bank
// source/certainty blocked item (name) amount currency/exponent".
func forecastOf(shares []cash.ForecastShare, names map[uuid.UUID]string) []string {
	out := make([]string, len(shares))
	for i, s := range shares {
		out[i] = fmt.Sprintf("%s %s/%d %s +%d %s %s/%s blocked=%v %s (%s) %d %s/%d", names[s.SnapshotID], s.SnapshotDate.Format(time.DateOnly), s.LineNo,
			s.ValueDate.Format(time.DateOnly), s.LagDays, s.HouseBankCode, s.Source, s.Certainty, s.Blocked, s.Item, s.ItemName, s.Amount, s.Currency, s.Exponent)
	}
	return out
}

// TestForecastShares: the forecast the analytical store holds is each
// date's snapshot the variance compares against (the daily one, else the
// last taken on demand) at document level, each line with its lag, bank
// code and item name (ADR-0051 §6, T53j2): read as the caller in a
// consistent read, and as the extract in the context's transaction,
// alike. A bucket's sum is not a line of it. A legal entity the caller
// cannot act in is not found; one the directory does not know has none
// for the extract.
func TestForecastShares(t *testing.T) {
	e := setupActuals(t)
	names := map[uuid.UUID]string{
		e.keptSnapshot(t, "daily", "2026-04-10", "2026-04-10||main|CAD|payable|OP_RENT|-5000", "2026-04-17||none|CAD|payable_blocked|OP_SUPPLIERS|-300",
			"2026-04-17||usd|USD|payment_run||-70", "2026-05-10||main|CAD|memo|REC_CUSTOMERS|900", "2026-05-11|2026-05-17|none|CAD|payable||-1"): "daily",
		e.keptSnapshot(t, "on_demand", "2026-04-10", "2026-04-10||main|CAD|payable|OP_RENT|-1"): "same day",
		e.keptSnapshot(t, "on_demand", "2026-04-11", "2026-04-11||main|CAD|payable|OP_RENT|-2"): "early",
		e.keptSnapshot(t, "on_demand", "2026-04-11", "2026-04-18||main|CAD|payable|TAX|-4000"):  "late",
	}
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	endpoints.Consistent = inRequest{}
	var asCaller, asExtract, unknown []cash.ForecastShare
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		if asCaller, err = endpoints.ForecastShares(ctx, e.le); err != nil {
			return err
		}
		if asExtract, err = endpoints.ExtractForecastShares(ctx, e.le); err != nil {
			return err
		}
		unknown, err = endpoints.ExtractForecastShares(ctx, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"daily 2026-04-10/1 2026-04-10 +0 MAIN payable/expected blocked=false OP_RENT (Rent) -5000 CAD/2",
		"daily 2026-04-10/2 2026-04-17 +7  payable_blocked/expected blocked=true OP_SUPPLIERS (Supplier payments) -300 CAD/2",
		"daily 2026-04-10/3 2026-04-17 +7 USD payment_run/expected blocked=false  () -70 USD/2",
		"daily 2026-04-10/4 2026-05-10 +30 MAIN memo/expected blocked=false REC_CUSTOMERS (Customer receipts) 900 CAD/2",
		"late 2026-04-11/1 2026-04-18 +7 MAIN payable/expected blocked=false TAX (Taxes) -4000 CAD/2",
	}
	if got := forecastOf(asCaller, names); !slices.Equal(got, want) {
		t.Errorf("as the caller: got %q, want %q", got, want)
	}
	if got := forecastOf(asExtract, names); !slices.Equal(got, want) {
		t.Errorf("as the extract: got %q, want %q", got, want)
	}
	if len(unknown) != 0 {
		t.Errorf("an unknown legal entity: got %q, want none", forecastOf(unknown, names))
	}
	err := apitest.InRequest(t, e.Tenant, e.Reader(t, "reader of 1000", e.US), func(ctx context.Context) error {
		_, err := endpoints.ForecastShares(ctx, e.le)
		return err
	})
	if p, ok := errors.AsType[*problem.Problem](err); !ok || p.Status != 404 {
		t.Errorf("a legal entity the caller cannot act in: got %v, want 404", err)
	}
}

// TestForecastSharesFailures calls the forecast outside a unit of work and
// with each of its database statements failing in turn, as the caller and
// as the extract, and with a house bank the payments context does not
// know.
func TestForecastSharesFailures(t *testing.T) {
	e := setupActuals(t)
	e.keptSnapshot(t, "daily", "2026-04-10", "2026-04-10||main|CAD|payable|OP_RENT|-5000")
	endpoints := func(consistent cash.Consistency) *cash.Endpoints {
		c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
		c.Consistent = consistent
		return c
	}
	caller := func(ctx context.Context) error {
		_, err := endpoints(inRequest{}).ForecastShares(ctx, e.le)
		return err
	}
	extract := func(ctx context.Context) error {
		_, err := endpoints(nil).ExtractForecastShares(ctx, e.le)
		return err
	}
	for name, read := range map[string]func(ctx context.Context) error{"as the caller": caller, "as the extract": extract} {
		t.Run(name, func(t *testing.T) {
			if err := read(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, read)
		})
	}
	t.Run("house banks", func(t *testing.T) {
		c := endpoints(nil)
		c.Payments = failingBanks{c.Payments}
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := c.ExtractForecastShares(ctx, e.le)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "house banks of the forecast") {
			t.Errorf("got %v, want the house banks' error", err)
		}
	})
}
