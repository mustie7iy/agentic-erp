package cash_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// documents are the active document and a copy of it to change.
func documents(ctx context.Context) (base, proposed *config.Config, err error) {
	base, _, err = (configuration.Settings{}).Document(ctx)
	if err != nil {
		return nil, nil, err
	}
	b, err := json.Marshal(base)
	if err != nil {
		return nil, nil, err
	}
	proposed = &config.Config{}
	return base, proposed, json.Unmarshal(b, proposed)
}

// moved renders a simulation's companies: each with its count, its listed
// flows and its totals by item.
func moved(t *testing.T, detail map[string]any) []string {
	t.Helper()
	b, err := json.Marshal(detail[cash.SimulationKey])
	if err != nil {
		t.Fatal(err)
	}
	var companies []cash.CompanyMoves
	if err := json.Unmarshal(b, &companies); err != nil {
		t.Fatal(err)
	}
	shares := func(ss []cash.ShareJSON) string {
		var out []string
		for _, s := range ss {
			out = append(out, s.Item+" "+s.Amount.Amount)
		}
		return strings.Join(out, ", ")
	}
	var out []string
	for _, c := range companies {
		out = append(out, fmt.Sprintf("%s: %d moved", c.Company, c.Moved))
		for _, f := range c.Flows {
			out = append(out, fmt.Sprintf("  %s %s/%s %s %s: [%s] -> [%s]", f.ValueDate, f.Source, f.Certainty, f.Amount.Amount, f.Amount.Currency, shares(f.Base),
				shares(f.Proposed)))
		}
		for _, x := range c.Totals {
			out = append(out, fmt.Sprintf("  %s %s: %s -> %s", x.Base.Currency, x.Item, x.Base.Amount, x.Proposed.Amount))
		}
	}
	return out
}

// TestDerivationSimulation: a change of the derivation lists the open
// flows whose items it moves, per company, without documents or
// suppliers, with what they give each item before and after (ADR-0051 §4,
// T53e3); each reader is shown the companies they may access; a change
// leaving the derivation records nothing; the requester needs
// cash.forecast.read.
func TestDerivationSimulation(t *testing.T) {
	e := setupForecast(t, withItems)
	e.itemsScenario(t)
	sim := cash.Simulation{Endpoints: app.Cash(app.Config{Now: func() time.Time { return readAt }})}
	var got cash.SimulationResult
	simulateBoth := func(t *testing.T, as pgtest.Actor, change func(base, proposed *config.Config)) error {
		t.Helper()
		return apitest.InRequest(t, e.Tenant, as, func(ctx context.Context) error {
			base, proposed, err := documents(ctx)
			if err != nil {
				return err
			}
			change(base, proposed)
			got, err = sim.Simulate(ctx, base, proposed)
			return err
		})
	}
	simulate := func(t *testing.T, as pgtest.Actor, change func(*config.Config)) error {
		t.Helper()
		return simulateBoth(t, as, func(_, proposed *config.Config) { change(proposed) })
	}
	if err := simulate(t, e.Tenant.Human, func(c *config.Config) { c.Cash.LiquidityItems[2].Name = "Rents" }); err != nil {
		t.Fatal(err)
	}
	if !got.Passed || got.Summary != "" || got.Detail != nil {
		t.Errorf("the derivation unchanged: got %+v, want a pass recording nothing", got)
	}
	rentIsCapex := func(c *config.Config) { c.Cash.Derivation.Accounts[0].Item = "CAPEX" }
	if err := simulate(t, e.Tenant.Human, rentIsCapex); err != nil {
		t.Fatal(err)
	}
	want(t, "recorded", fmt.Sprint(got.Passed, " ", got.Summary), "true Open flows of the liquidity forecast classified under the proposed liquidity item derivation.")
	want(t, "moved", moved(t, got.Detail), []string{
		"1000: 0 moved", "2000: 0 moved",
		"3000: 4 moved",
		"  2026-04-17 payment_run/confirmed -700.00 CAD: [CAPEX -800.00, OP_RENT 100.00] -> [CAPEX -700.00]",
		"  2026-04-30 payable/expected -1152.00 CAD: [OP_RENT -595.00, CAPEX -357.00, OP_SUPPLIERS -200.00] -> [CAPEX -952.00, OP_SUPPLIERS -200.00]",
		"  2026-05-07 payable/expected -900.00 CAD: [OP_RENT -900.00] -> [CAPEX -900.00]",
		"  2026-05-14 invoice_unposted/planned -200.00 CAD: [OP_RENT -200.00] -> [CAPEX -200.00]",
		"  CAD CAPEX: -1157.00 -> -2752.00",
		"  CAD OP_RENT: -1595.00 -> 0.00",
		"  CAD OP_SUPPLIERS: -200.00 -> -200.00",
	})

	t.Run("each reader is shown their companies", func(t *testing.T) {
		detail, err := json.Marshal(got.Detail)
		if err != nil {
			t.Fatal(err)
		}
		ev := configuration.Evidence{Kind: "simulation", Summary: "Invoices replayed. " + got.Summary, Detail: detail}
		shown := func(access authz.Access) configuration.Evidence {
			t.Helper()
			out := ev
			for _, d := range app.Configuration(app.Config{}).Disclosures {
				if out, err = d.Disclose(context.Background(), out, access); err != nil {
					t.Fatal(err)
				}
			}
			return out
		}
		maple := shown(authz.Access{IDs: []uuid.UUID{e.le}})
		want(t, "a reader of Maple", maple.Summary,
			"Invoices replayed. Of the open flows of the liquidity forecast of the companies shown (1), 4 would be classified under other liquidity items by the proposed derivation.")
		var fields map[string]any
		if err := json.Unmarshal(maple.Detail, &fields); err != nil {
			t.Fatal(err)
		}
		want(t, "Maple's flows", len(moved(t, fields)), 8)
		none := shown(authz.Access{})
		want(t, "a reader of no company", none.Summary,
			"Invoices replayed. Of the open flows of the liquidity forecast of the companies shown (0), 0 would be classified under other liquidity items by the proposed derivation.")
		other := configuration.Evidence{Kind: "regression", Summary: "x", Detail: detail}
		for _, d := range app.Configuration(app.Config{}).Disclosures {
			if got, err := d.Disclose(context.Background(), other, authz.Access{}); err != nil || got.Summary != "x" {
				t.Errorf("another kind of evidence: got %q, %v; want it as it is", got.Summary, err)
			}
		}
		if _, _, err := cash.Disclose(json.RawMessage(`[]`), "", authz.Access{}); err == nil {
			t.Error("a detail that is not an object: got no error, want the read failed")
		}
		if _, _, err := cash.Disclose(json.RawMessage(`{"`+cash.SimulationKey+`": 1}`), "", authz.Access{}); err == nil {
			t.Error("companies that are not a list: got no error, want the read failed")
		}
		if d, s, err := cash.Disclose(json.RawMessage(`{"rules": []}`), "s", authz.Access{}); err != nil || s != "s" || string(d) != `{"rules": []}` {
			t.Errorf("another context's simulation: got %s, %q, %v; want it as it is", d, s, err)
		}
	})
	t.Run("as a change request's simulation runs it", func(t *testing.T) {
		var r configuration.Simulation
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			base, proposed, err := documents(ctx)
			if err != nil {
				return err
			}
			rentIsCapex(proposed)
			r, err = app.Configuration(app.Config{Now: func() time.Time { return readAt }}).Simulator.Simulate(ctx, base, proposed, 0)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		want(t, "moved", moved(t, r.Detail)[2], "3000: 4 moved")
	})
	t.Run("the requester needs cash.forecast.read", func(t *testing.T) {
		nobody := e.Reader(t, "no role")
		pgtest.Exec(t, `DELETE FROM identity.principal_role WHERE principal_id = $1`, nobody.ID)
		var p *problem.Problem
		if err := simulate(t, nobody, rentIsCapex); !errors.As(err, &p) || p.Code != "identity.permission_missing" {
			t.Errorf("got %v, want identity.permission_missing", err)
		}
	})
	t.Run("only the companies the requester may act in", func(t *testing.T) {
		reader := e.Reader(t, "reader of 1000", e.US)
		if err := simulate(t, reader, rentIsCapex); err != nil {
			t.Fatal(err)
		}
		want(t, "moved", moved(t, got.Detail), []string{"1000: 0 moved"})
	})
	t.Run("a company without a legal entity", func(t *testing.T) {
		if err := simulateBoth(t, e.Tenant.Human, func(base, proposed *config.Config) {
			base.Companies = append(base.Companies, config.Company{Code: "9999"})
			rentIsCapex(proposed)
		}); err != nil {
			t.Fatal(err)
		}
		want(t, "moved", moved(t, got.Detail)[:3], []string{"1000: 0 moved", "2000: 0 moved", "3000: 4 moved"})
		want(t, "companies", len(got.Detail[cash.SimulationKey].([]cash.CompanyMoves)), 3)
	})
}

// TestDerivationSimulationFailures: the simulation fails with the ports
// it reads, and with sums out of the range of an amount.
func TestDerivationSimulationFailures(t *testing.T) {
	e := setupForecast(t, withItems)
	e.itemsScenario(t)
	sim := cash.Simulation{Endpoints: app.Cash(app.Config{Now: func() time.Time { return readAt }})}
	run := func(ctx context.Context) error {
		base, proposed, err := documents(ctx)
		if err != nil {
			return err
		}
		proposed.Cash.Derivation.Accounts[0].Item = "CAPEX"
		_, err = sim.Simulate(ctx, base, proposed)
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "simulate", run)
	e.item(t, e.Supplier, "2026-04-01", "CAD", -9_000_000_000_000_000_000, nil)
	e.item(t, e.Supplier, "2026-04-02", "CAD", -9_000_000_000_000_000_000, nil)
	var p *problem.Problem
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, run); !errors.As(err, &p) || p.Code != "cash.amount_too_large" {
		t.Errorf("sums out of range: got %v, want cash.amount_too_large", err)
	}
}
