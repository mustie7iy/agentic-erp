package controlling_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Cycles by fixed amounts, and allocation runs reversed (T60k, ADR-0064
// As built T60k).

// amount is a receiver of a cycle body with its fixed amount in EUR.
type amount struct {
	center uuid.UUID
	value  string
}

// fixedBody is a new assessment cycle of the company from cost center
// 1000 on the assessment account whose receivers are charged fixed
// amounts.
func (p plan) fixedBody(code, kind string, receivers ...amount) string {
	rs := make([]string, len(receivers))
	for i, r := range receivers {
		rs[i] = fmt.Sprintf(`{"cost_center_id": %q, "amount": {"amount": %q, "currency": "EUR"}}`, r.center, r.value)
	}
	account := ""
	if kind == "assessment" {
		account = fmt.Sprintf(`"assessment_account_id": %q, `, p.assessed)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "Cycle %s", "kind": %q, %s"share_basis": "fixed_amounts", "sender_cost_center_id": %q,
		"receivers": [%s]}`, p.DE, code, code, kind, account, p.c.Admin, strings.Join(rs, ", "))
}

// amountsLine is a cycle's receivers as "center amount currency, ...".
func amountsLine(c map[string]any) string {
	var out []string
	for _, r := range c["receivers"].([]any) {
		m := r.(map[string]any)
		a, _ := m["amount"].(map[string]any)
		out = append(out, fmt.Sprint(m["cost_center_code"], " ", a["amount"], " ", a["currency"], " ", m["percentage"]))
	}
	return fmt.Sprint(c["share_basis"], ": ", strings.Join(out, ", "))
}

func TestFixedAmountCycles(t *testing.T) {
	p := setup(t)
	fourth := p.center(t, "4000", "2026-01-01")
	created, _ := p.Must(201, "POST", "/v1/allocation-cycles", p.fixedBody("FIXED", "assessment", amount{p.c.Sales, "250.00"}, amount{fourth, "150.00"}), "")
	want(t, "the new cycle", amountsLine(created), "fixed_amounts: 2000 250.00 EUR <nil>, 4000 150.00 EUR <nil>")
	read, _ := p.Must(200, "GET", fmt.Sprint("/v1/allocation-cycles/", created["id"]), "", "")
	want(t, "the cycle read", amountsLine(read), "fixed_amounts: 2000 250.00 EUR <nil>, 4000 150.00 EUR <nil>")
	p.cycle(t, "PCT", share{p.c.Sales, "100"})
	page, _ := p.Must(200, "GET", "/v1/allocation-cycles", "", "")
	var listed []string
	for _, c := range apitest.List(t, page, "items") {
		listed = append(listed, amountsLine(c.(map[string]any)))
	}
	wantList(t, "the cycles listed", listed, []string{"fixed_amounts: 2000 250.00 EUR <nil>, 4000 150.00 EUR <nil>",
		"fixed_percentages: 2000 <nil> <nil> 100.00"})
	t.Run("refusals of a create", func(t *testing.T) {
		const huge = "92233720368547758.07"
		for name, c := range map[string]struct {
			body string
			want []string
		}{
			"a distribution by fixed amounts": {p.fixedBody("X", "distribution", amount{p.c.Sales, "1.00"}), []string{"share_basis not_allowed"}},
			"an amount of nothing":            {p.fixedBody("X", "assessment", amount{p.c.Sales, "0.00"}), []string{"receivers[0].amount.amount invalid"}},
			"an amount without its cents":     {p.fixedBody("X", "assessment", amount{p.c.Sales, "5"}), []string{"receivers[0].amount.amount invalid"}},
			"an amount in another currency": {strings.Replace(p.fixedBody("X", "assessment", amount{p.c.Sales, "1.00"}), `"EUR"`, `"USD"`, 1),
				[]string{"receivers[0].amount.currency invalid"}},
			"a receiver without its amount": {strings.Replace(p.fixedBody("X", "assessment", amount{p.c.Sales, "1.00"}), `, "amount": {"amount": "1.00", "currency": "EUR"}`,
				"", 1), []string{"receivers[0].amount required"}},
			"a percentage by fixed amounts": {strings.Replace(p.fixedBody("X", "assessment", amount{p.c.Sales, "1.00"}), `"amount": {`, `"percentage": "100", "amount": {`, 1),
				[]string{"receivers[0].percentage not_allowed"}},
			"an amount on fixed percentages": {strings.Replace(cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "100"}), `"percentage": "100"`,
				`"percentage": "100", "amount": {"amount": "1.00", "currency": "EUR"}`, 1), []string{"receivers[0].amount not_allowed"}},
			"amounts beyond what an amount holds": {p.fixedBody("X", "assessment", amount{p.c.Sales, huge}, amount{fourth, huge}), []string{"receivers too_large"}},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.allocation_cycle_invalid", "POST", "/v1/allocation-cycles", c.body, "")
				wantList(t, "field errors", fieldErrors(m), c.want)
			})
		}
	})
	v2 := apitest.Published(t, p.Tenant, outbox.Event{Context: "controlling", Name: "allocation_cycle_changed", Version: 2})
	a := v2[0]["receivers"].([]any)[0].(map[string]any)["amount"].(map[string]any)
	want(t, "the v2 event's amount", fmt.Sprint(v2[0]["share_basis"], " ", a["amount"], " ", a["currency"]), "fixed_amounts 250.00 EUR")
	want(t, "v1 events: the cycle by fixed percentages alone", len(p.events(t, "allocation_cycle_changed")), 1)
	var audited string
	pgtest.Scan(t, `SELECT (detail->'receivers'->0->>'amount') || ' ' || (detail->'receivers'->0->>'currency') FROM audit.audit_event WHERE tenant_id = $1
		AND action = 'controlling.allocation_cycle.create' ORDER BY seq LIMIT 1`, []any{p.Tenant.ID}, &audited)
	want(t, "audited", audited, "250.00 EUR")
}

func TestFixedAmountRun(t *testing.T) {
	p := setup(t)
	fourth := p.center(t, "4000", "2026-01-01")
	c, _ := p.Must(201, "POST", "/v1/allocation-cycles", p.fixedBody("FIXED", "assessment", amount{p.c.Sales, "250.00"}, amount{fourth, "150.01"}), "")
	path := fmt.Sprint("/v1/allocation-cycles/", c["id"])
	// The sender has no costs: the amounts are charged as they are.
	got, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "the run", ranLine(got), "FIXED assessment 2026/5 on 2026-05-31 from 1000: 400.01 EUR on 0 account(s)")
	wantList(t, "the entry's lines", p.entryLines(t, apitest.Object(t, got, "run")["journal_entry_id"]), []string{"695000 -400.01 on center 1000",
		"695000 250.00 on center 2000", "695000 150.01 on center 4000"})
	p.Problem(409, "controlling.allocation_run_exists", "POST", path+":run", settleBody(2026, 5), `"1"`)
	// Costs posted to the sender change nothing of what it charges.
	p.post(t, "2026-06-10", on(p.office, 99999, &p.c.Admin), on(p.bank, -99999, nil))
	june, _ := p.Must(200, "POST", path+":run", settleBody(2026, 6), `"1"`)
	want(t, "June's run", ranLine(june), "FIXED assessment 2026/6 on 2026-06-30 from 1000: 400.01 EUR on 0 account(s)")
	wantList(t, "the sender's costs", p.centerCosts(t, p.c.Admin), []string{"520000 99999", "695000 -80002"})
}

// reverseRun reverses the run at the cycle's version 1.
func (p plan) reverseRun(t *testing.T, run any) map[string]any {
	t.Helper()
	r, _ := p.Must(200, "POST", fmt.Sprint("/v1/allocation-runs/", run, ":reverse"), "", `"1"`)
	return r
}

func TestAllocationRunReversal(t *testing.T) {
	p := setup(t)
	id := p.cycle(t, "SHARED", share{p.c.Sales, "100"})
	path := "/v1/allocation-cycles/" + id
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	ran, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	run := apitest.Object(t, ran, "run")
	runPath := fmt.Sprint("/v1/allocation-runs/", run["id"])
	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		p.Problem(428, "request.precondition_required", "POST", runPath+":reverse", "", "")
		p.Problem(412, "request.version_mismatch", "POST", runPath+":reverse", "", `"9"`)
		p.Problem(404, "request.not_found", "POST", "/v1/allocation-runs/"+uuid.NewString()+":reverse", "", `"1"`)
		_, etag := p.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", run["journal_entry_id"]), "", "")
		m := p.Problem(409, "ledger.entry_owned_elsewhere", "POST", fmt.Sprint("/v1/journal-entries/", run["journal_entry_id"], ":reverse"), "{}", etag)
		want(t, "the path the ledger names", strings.Contains(fmt.Sprint(m["detail"]), fmt.Sprint("POST /v1/allocation-runs/", run["id"], ":reverse")), true)
	})
	reversed := p.reverseRun(t, run["id"])
	want(t, "the reversed run", fmt.Sprint(runLine(reversed), " by ", reversed["reversed_by"]), fmt.Sprint(
		"SHARED distribution 2026/5 on 2026-05-31 from 1000: 10.00 EUR on 1 account(s) by ", p.Tenant.Human.ID))
	wantList(t, "the reversal's lines", p.entryLines(t, reversed["reversal_journal_entry_id"]), []string{"520000 10.00 on center 1000",
		"520000 -10.00 on center 2000"})
	entry, _ := p.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", reversed["reversal_journal_entry_id"]), "", "")
	want(t, "the reversal's number and date", fmt.Sprint(entry["document_number"], " ", entry["posting_date"]), fmt.Sprint(reversed["reversal_journal_entry_number"],
		" 2026-05-31"))
	wantList(t, "the sender holds its cost again", p.centerCosts(t, p.c.Admin), []string{"520000 1000"})
	wantList(t, "the receiver gave it back", p.centerCosts(t, p.c.Sales), nil)
	got, _ := p.Must(200, "GET", runPath, "", "")
	want(t, "the run read", fmt.Sprint(got["reversal_journal_entry_id"], " ", got["reversed_at"] != nil), fmt.Sprint(reversed["reversal_journal_entry_id"], " true"))
	m := p.Problem(409, "controlling.allocation_run_reversed", "POST", runPath+":reverse", "", `"1"`)
	want(t, "the second reversal's detail", m["detail"], any(fmt.Sprint("The allocation run is reversed already: journal entry ", reversed["reversal_journal_entry_number"], ".")))

	again, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "the period run again", ranLine(again), "SHARED distribution 2026/5 on 2026-05-31 from 1000: 10.00 EUR on 1 account(s)")
	p.Problem(409, "controlling.allocation_run_exists", "POST", path+":run", settleBody(2026, 5), `"1"`)
	var attempts string
	pgtest.Scan(t, `SELECT string_agg(attempt::text, ' ' ORDER BY attempt) FROM controlling.allocation_run WHERE tenant_id = $1`, []any{p.Tenant.ID}, &attempts)
	want(t, "the runs' attempts", attempts, "1 2")
	t.Run("the runs listed, reversed or in force", func(t *testing.T) {
		for name, c := range map[string]struct {
			query string
			want  int
		}{"every run": {"", 2}, "reversed": {"?reversed=true", 1}, "in force": {"?reversed=false", 1}} {
			t.Run(name, func(t *testing.T) {
				page, _ := p.in(t).Must(200, "GET", "/v1/allocation-runs"+c.query, "", "")
				want(t, "runs", len(apitest.List(t, page, "items")), c.want)
			})
		}
		page, _ := p.Must(200, "GET", "/v1/allocation-runs?reversed=true", "", "")
		want(t, "the reversed run listed", apitest.List(t, page, "items")[0].(map[string]any)["id"], run["id"])
	})
	t.Run("a run into a closed period is not reversed", func(t *testing.T) {
		periods, _ := p.Must(200, "GET", fmt.Sprintf("/v1/fiscal-periods?legal_entity_id=%s&fiscal_year=2026&order_by=period&page_size=5", p.DE), "", "")
		may := periods["items"].([]any)[4].(map[string]any)
		p.Must(200, "POST", "/v1/fiscal-periods/"+may["id"].(string)+":close", "", fmt.Sprintf(`"%v"`, may["version"]))
		p.Problem(409, "ledger.period_not_open", "POST", fmt.Sprint("/v1/allocation-runs/", apitest.Object(t, again, "run")["id"], ":reverse"), "", `"1"`)
		page, _ := p.Must(200, "GET", "/v1/allocation-runs?reversed=true", "", "")
		want(t, "runs reversed", len(apitest.List(t, page, "items")), 1)
	})
	var published []string
	for _, ev := range p.events(t, "allocation_run_reversed") {
		published = append(published, fmt.Sprint(ev["code"], " ", ev["fiscal_year"], "/", ev["fiscal_period"], " ", ev["amount"].(map[string]any)["amount"], " ",
			ev["allocation_run_id"] == run["id"], " ", ev["reversal_journal_entry_id"] == reversed["reversal_journal_entry_id"], " ", ev["sender_cost_center_code"]))
	}
	wantList(t, "published", published, []string{"SHARED 2026/5 10.00 true true 1000"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg((detail->>'code') || ' ' || (detail->>'amount') || ' ' || (detail->>'reversal_journal_entry_number'), '; ' ORDER BY seq)
		FROM audit.audit_event WHERE tenant_id = $1 AND action = 'controlling.allocation_cycle.reverse_run'`, []any{p.Tenant.ID}, &audited)
	want(t, "audited", audited, fmt.Sprint("SHARED 10.00 ", reversed["reversal_journal_entry_number"]))
}

// TestAssessmentReversedReleasesTheSender: a sender assessed for a period
// may be run for it again once the assessment is reversed.
func TestAssessmentReversedReleasesTheSender(t *testing.T) {
	p := setup(t)
	assessment := p.assessment(t, "ASSESS", share{p.c.Sales, "100"})
	distribution := p.cycle(t, "MOVE", share{p.c.Sales, "100"})
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	ran, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+assessment+":run", settleBody(2026, 5), `"1"`)
	p.Problem(409, "controlling.allocation_sender_assessed", "POST", "/v1/allocation-cycles/"+distribution+":run", settleBody(2026, 5), `"1"`)
	p.reverseRun(t, apitest.Object(t, ran, "run")["id"])
	moved, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+distribution+":run", settleBody(2026, 5), `"1"`)
	want(t, "the distribution once the assessment is reversed", ranLine(moved), "MOVE distribution 2026/5 on 2026-05-31 from 1000: 10.00 EUR on 1 account(s)")
	wantList(t, "the sender's costs", p.centerCosts(t, p.c.Admin), nil)
	wantList(t, "the receiver's costs", p.centerCosts(t, p.c.Sales), []string{"520000 1000"})
}

// TestAllocationRunReversalAccess: a run is reversed only in the companies
// a person may act in, by a person holding the run permission and the
// ledger's entry read.
func TestAllocationRunReversalAccess(t *testing.T) {
	p := setup(t)
	id := p.cycle(t, "SHARED", share{p.c.Sales, "100"})
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	ran, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+id+":run", settleBody(2026, 5), `"1"`)
	path := fmt.Sprint("/v1/allocation-runs/", apitest.Object(t, ran, "run")["id"], ":reverse")
	p.as(p.Reader(t, "Reader of 1000", p.US)).Problem(404, "request.not_found", "POST", path, "", `"1"`)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	p.As(p.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", path, "", `"1"`)
	for _, permission := range []string{"controlling.allocation.run", "ledger.entry.read"} {
		t.Run("without "+permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, permission)
			p.Problem(403, "identity.permission_missing", "POST", path, "", `"1"`)
		})
	}
}

// TestAllocationRunReversedOnce: two reversals of a run at once serialize
// on the sender's cycles' locks; the second finds the first's reversal and
// is refused, and one reversal entry is posted.
func TestAllocationRunReversedOnce(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id := p.cycle(t, "SHARED", share{p.c.Sales, "100"})
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	ran, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+id+":run", settleBody(2026, 5), `"1"`)
	run := uuid.MustParse(apitest.Object(t, ran, "run")["id"].(string))
	reverse := func(ctx context.Context) error {
		_, err := h.ReverseAllocationRun(ctx, gen.ReverseAllocationRunParams{ID: run, IfMatch: `"1"`})
		return err
	}
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, reverse, reverse)
	if first != nil {
		t.Fatalf("first reversal: %v", first)
	}
	want(t, "second reversal", problem.From(second).Code, "controlling.allocation_run_reversed")
	wantList(t, "the receiver's costs", p.centerCosts(t, p.c.Sales), nil)
	want(t, "reversals published", len(p.events(t, "allocation_run_reversed")), 1)
}

// TestAllocationRunReversalThenRun: a reversal and a run of the same
// cycle and period at once follow one another on the sender's cycles'
// locks: the run waits for the reversal, finds the run reversed and runs
// the period again.
func TestAllocationRunReversalThenRun(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id := uuid.MustParse(p.cycle(t, "SHARED", share{p.c.Sales, "100"}))
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	ran, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+id.String()+":run", settleBody(2026, 5), `"1"`)
	run := uuid.MustParse(apitest.Object(t, ran, "run")["id"].(string))
	var posted bool
	reverse, rerun := apitest.Serialized(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ReverseAllocationRun(ctx, gen.ReverseAllocationRunParams{ID: run, IfMatch: `"1"`})
		return err
	}, func(ctx context.Context) error {
		res, err := h.RunAllocationCycle(ctx, &gen.AllocationCycleRun{FiscalYear: 2026, FiscalPeriod: 5}, gen.RunAllocationCycleParams{ID: id, IfMatch: `"1"`})
		if err == nil {
			posted = res.(*gen.AllocationRunResultHeaders).Response.Posted
		}
		return err
	})
	if err := errors.Join(reverse, rerun); err != nil {
		t.Fatal(err)
	}
	want(t, "the run after the reversal posted", posted, true)
	wantList(t, "the receiver's costs", p.centerCosts(t, p.c.Sales), []string{"520000 1000"})
}

// TestAllocationRunReversalDatabaseFailures calls the reversal, a cycle
// and a run by fixed amounts and the runs' listing by reversal outside a
// unit of work and with each of their database statements failing in
// turn.
func TestAllocationRunReversalDatabaseFailures(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id := p.cycle(t, "SHARED", share{p.c.Sales, "100"})
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	ran, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+id+":run", settleBody(2026, 5), `"1"`)
	run := uuid.MustParse(apitest.Object(t, ran, "run")["id"].(string))
	// A reversed run of June, so that a run of the period is its second attempt.
	p.post(t, "2026-06-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	june, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+id+":run", settleBody(2026, 6), `"1"`)
	p.reverseRun(t, apitest.Object(t, june, "run")["id"])
	fixed, _ := p.Must(201, "POST", "/v1/allocation-cycles", p.fixedBody("FIXED", "assessment", amount{p.c.Sales, "1.00"}), "")
	fixedID := uuid.MustParse(fixed["id"].(string))
	cycle := uuid.MustParse(id)
	calls := map[string]func(ctx context.Context) error{
		"reverse a run": func(ctx context.Context) error {
			_, err := h.ReverseAllocationRun(ctx, gen.ReverseAllocationRunParams{ID: run, IfMatch: `"1"`})
			return err
		},
		"run a period again": func(ctx context.Context) error {
			_, err := h.RunAllocationCycle(ctx, &gen.AllocationCycleRun{FiscalYear: 2026, FiscalPeriod: 6}, gen.RunAllocationCycleParams{ID: cycle, IfMatch: `"1"`})
			return err
		},
		"create a cycle by fixed amounts": func(ctx context.Context) error {
			_, err := h.CreateAllocationCycle(ctx, &gen.AllocationCycleCreate{LegalEntityID: p.DE, Code: "MADE", Name: "M", Kind: "assessment",
				AssessmentAccountID: gen.NewOptUUID(p.assessed), ShareBasis: gen.NewOptString("fixed_amounts"), SenderCostCenterID: p.c.Admin,
				Receivers: []gen.AllocationCycleCreateReceiversItem{{CostCenterID: p.c.Sales, Amount: gen.NewOptMoney(gen.Money{Amount: "1.00", Currency: "EUR"})}}},
				gen.CreateAllocationCycleParams{})
			return err
		},
		"get a cycle by fixed amounts": func(ctx context.Context) error {
			_, err := h.GetAllocationCycle(ctx, gen.GetAllocationCycleParams{ID: fixedID})
			return err
		},
		"run a cycle by fixed amounts": func(ctx context.Context) error {
			_, err := h.RunAllocationCycle(ctx, &gen.AllocationCycleRun{FiscalYear: 2026, FiscalPeriod: 5}, gen.RunAllocationCycleParams{ID: fixedID, IfMatch: `"1"`})
			return err
		},
		"list reversed runs": func(ctx context.Context) error {
			_, err := h.ListAllocationRuns(ctx, gen.ListAllocationRunsParams{Reversed: gen.NewOptBool(true)})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, p.Tenant, p.Tenant.Human, name, call)
		})
	}
}
