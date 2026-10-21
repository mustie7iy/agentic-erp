package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// reader is a person of the tenant who may act in the legal entities only.
func (c centers) reader(t *testing.T, legalEntities ...uuid.UUID) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'reader', 'active', $2, $2)`, c.ten.ID, a.ID, pgtest.Issuer(t), a.Subject)
	for _, le := range legalEntities {
		pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, c.ten.ID, a.ID, le)
	}
	return a
}

// TestAssignmentSimulation: a change to the controlling rules replays the
// window's lines on the P&L accounts it assigns differently, per company
// and account, and each reader is shown the companies they may access.
func TestAssignmentSimulation(t *testing.T) {
	c := setupCenters(t)
	bank, revenue := c.account(t, "2000", "100000"), c.account(t, "2000", "400000")
	supplies, it := c.account(t, "2000", "520000"), c.account(t, "2000", "530000")
	c.Must(201, "POST", "/v1/journal-entries", entry(c.de, "2026-03-15", "EUR", line{bank, "100.00", ""}, line{revenue, "-100.00", ""}), "")
	c.Must(201, "POST", "/v1/journal-entries", entry(c.de, "2026-03-16", "EUR", line{supplies, "30.00", ""}, line{it, "20.00", ""}, line{bank, "-50.00", ""}), "")
	c.Must(201, "POST", "/v1/journal-entries", entry(c.de, "2026-03-17", "EUR", line{supplies, "5.00", ""}, line{bank, "-5.00", ""}), "")
	c.Must(201, "POST", "/v1/journal-entries", entry(c.de, "2026-01-02", "EUR", line{supplies, "5.00", ""}, line{bank, "-5.00", ""}), "") // before the window
	sim := ledger.AssignmentSimulation{LegalEntities: tenancy.Directory{}, Now: at(t, "2026-04-15")}
	var got configuration.Simulation
	simulate := func(t *testing.T, base, proposed *config.Config) configuration.Simulation {
		t.Helper()
		if err := apitest.InRequest(t, c.ten, c.ten.Human, func(ctx context.Context) (err error) {
			got, err = sim.Simulate(ctx, base, proposed)
			return err
		}); err != nil {
			t.Fatalf("simulate: %v", err)
		}
		return got
	}
	if r := simulate(t, document(t), document(t)); !r.Passed || r.Summary != "" || len(r.Detail) != 0 {
		t.Errorf("no change: got %+v, want a pass recording nothing", r)
	}
	proposed := controlling(t, func(c *config.Config, _ *config.Company) {
		c.Companies = append(c.Companies, config.Company{Code: "3000"}) // new, no lines
	})
	r := simulate(t, document(t), proposed)
	if !r.Passed || r.Summary != "Journal lines of the last 90 days replayed under the proposed controlling rules." {
		t.Errorf("recorded: got passed %v, summary %q", r.Passed, r.Summary)
	}
	detail, err := json.Marshal(r.Detail)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"assignment_window_days":90,"journal_lines_by_company":[{"company":"2000","legal_entity_id":%q,"accounts":[`+
		`{"account":"400000","base":"no cost center; no profit center","proposed":"no cost center; default profit center DUMMY","refused":false,"lines":1,"entries":1,"examples":["SA 100000"]},`+
		`{"account":"520000","base":"no cost center; no profit center","proposed":"default cost center 1000; profit center of the cost center","refused":false,"lines":2,"entries":2,"examples":["SA 100001","SA 100002"]},`+
		`{"account":"530000","base":"no cost center; no profit center","proposed":"cost center required; profit center of the cost center","refused":true,"lines":1,"entries":1,"examples":["SA 100001"]}]}]}`, c.de)
	if string(detail) != want {
		t.Errorf("detail:\ngot  %s\nwant %s", detail, want)
	}
	evidence := configuration.Evidence{Kind: "simulation", Passed: true, Summary: "Invoices replayed. " + r.Summary, Detail: detail}
	for name, tc := range map[string]struct {
		as          pgtest.Actor
		wantSummary string
		wantShown   int
	}{
		"a reader of every company": {c.ten.Human, "Invoices replayed. 4 journal lines of the last 90 days on 3 accounts would be assigned differently " +
			"under the proposed controlling rules, 1 of them refused without a cost center.", 1},
		"a reader of company 1000": {c.reader(t, c.us), "Invoices replayed. 0 journal lines of the last 90 days on 0 accounts would be assigned " +
			"differently under the proposed controlling rules, 0 of them refused without a cost center.", 0},
	} {
		t.Run(name, func(t *testing.T) {
			var shown configuration.Evidence
			if err := apitest.InRequest(t, c.ten, tc.as, func(ctx context.Context) (err error) {
				shown, err = ledger.AssignmentDisclosure{}.Disclose(ctx, evidence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if shown.Summary != tc.wantSummary {
				t.Errorf("summary: got %q, want %q", shown.Summary, tc.wantSummary)
			}
			var fields struct {
				Companies []ledger.CompanyAssignments `json:"journal_lines_by_company"`
			}
			if err := json.Unmarshal(shown.Detail, &fields); err != nil || len(fields.Companies) != tc.wantShown {
				t.Errorf("companies shown: got %v (%v), want %d", fields.Companies, err, tc.wantShown)
			}
		})
	}
	t.Run("other evidence is shown as it is", func(t *testing.T) {
		for _, e := range []configuration.Evidence{{Kind: "regression", Detail: json.RawMessage(`[1]`)},
			{Kind: "simulation", Summary: "Invoices.", Detail: json.RawMessage(`{"invoices_by_company": []}`)}} {
			if err := apitest.InRequest(t, c.ten, c.ten.Human, func(ctx context.Context) error {
				shown, err := ledger.AssignmentDisclosure{}.Disclose(ctx, e)
				if err == nil && (shown.Summary != e.Summary || string(shown.Detail) != string(e.Detail)) {
					t.Errorf("%s: got %+v, want it unchanged", e.Kind, shown)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("a detail that does not read fails", func(t *testing.T) {
		for _, detail := range []string{`[1]`, `{"journal_lines_by_company": 1}`} {
			err := apitest.InRequest(t, c.ten, c.ten.Human, func(ctx context.Context) error {
				_, err := ledger.AssignmentDisclosure{}.Disclose(ctx, configuration.Evidence{Kind: "simulation", Detail: json.RawMessage(detail)})
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "ledger: simulated assignments") {
				t.Errorf("%s: got %v, want a simulated assignments error", detail, err)
			}
		}
	})
	t.Run("failing statements", func(t *testing.T) {
		apitest.EveryFailure(t, c.ten, c.ten.Human, "simulate", func(ctx context.Context) error {
			_, err := sim.Simulate(ctx, document(t), proposed)
			return err
		})
		apitest.EveryFailure(t, c.ten, c.ten.Human, "disclose", func(ctx context.Context) error {
			_, err := ledger.AssignmentDisclosure{}.Disclose(ctx, evidence)
			return err
		})
		if _, err := sim.Simulate(context.Background(), document(t), proposed); err == nil {
			t.Error("simulated outside a request: got no error, want one")
		}
		if _, err := (ledger.AssignmentDisclosure{}).Disclose(context.Background(), evidence); err == nil {
			t.Error("disclosed outside a request: got no error, want one")
		}
	})
}
