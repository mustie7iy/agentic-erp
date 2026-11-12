package controlling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Assessment cycles and their runs (T60g, ADR-0064 §4-§5).

// assessmentBody is a new assessment cycle of the company on the account.
func assessmentBody(le uuid.UUID, code string, sender, account uuid.UUID, receivers ...share) string {
	body := cycleBody(le, code, "assessment", sender, receivers...)
	return strings.TrimSuffix(body, "}") + fmt.Sprintf(`, "assessment_account_id": %q}`, account)
}

// assessment creates an assessment cycle of company 2000 from cost center
// 1000 to the receivers on account 695000 and returns its id.
func (p plan) assessment(t *testing.T, code string, receivers ...share) string {
	t.Helper()
	c, _ := p.Must(201, "POST", "/v1/allocation-cycles", assessmentBody(p.DE, code, p.c.Admin, p.assessed, receivers...), "")
	return c["id"].(string)
}

// accountOf is the cycle's assessment account as "id number", or "none".
func accountOf(c map[string]any) string {
	id, has := c["assessment_account_id"]
	number, named := c["assessment_account_number"]
	if !has && !named {
		return "none"
	}
	return fmt.Sprint(id, " ", number)
}

func TestAssessmentCycles(t *testing.T) {
	p := setup(t)
	created, _ := p.Must(201, "POST", "/v1/allocation-cycles", assessmentBody(p.DE, "ASSESS", p.c.Admin, p.assessed, share{p.c.Sales, "60"}, share{p.c.Locked, "40"}), "")
	id := created["id"].(string)
	line, account := "ASSESS assessment 1000 -> 2000 60.00, 3000 40.00", fmt.Sprint(p.assessed, " 695000")
	want(t, "the new cycle", cycleLine(created), line)
	want(t, "the new cycle's assessment account", accountOf(created), account)
	got, _ := p.Must(200, "GET", "/v1/allocation-cycles/"+id, "", "")
	want(t, "the cycle read", cycleLine(got), line)
	want(t, "the cycle read's assessment account", accountOf(got), account)
	p.cycle(t, "DISTRIB", share{p.c.Sales, "100"})
	wantList(t, "the assessment cycles", cycles(t, p.Client, "?kind=assessment"), []string{line})
	page, _ := p.Must(200, "GET", "/v1/allocation-cycles", "", "")
	var accounts []string
	for _, c := range apitest.List(t, page, "items") {
		accounts = append(accounts, fmt.Sprint(c.(map[string]any)["code"], " ", accountOf(c.(map[string]any))))
	}
	wantList(t, "the accounts of the cycles listed", accounts, []string{"ASSESS " + account, "DISTRIB none"})

	t.Run("refusals of an assessment account", func(t *testing.T) {
		whole := []share{{p.c.Sales, "100"}}
		for name, c := range map[string]struct {
			body string
			want []string
		}{
			"an assessment without an account": {cycleBody(p.DE, "X", "assessment", p.c.Admin, whole...), []string{"assessment_account_id required"}},
			"a distribution with an account": {strings.Replace(assessmentBody(p.DE, "X", p.c.Admin, p.assessed, whole...), "assessment", "distribution", 1),
				[]string{"assessment_account_id not_allowed"}},
			"an account that is none":            {assessmentBody(p.DE, "X", p.c.Admin, uuid.New(), whole...), []string{"assessment_account_id unknown"}},
			"an account of another company":      {assessmentBody(p.DE, "X", p.c.Admin, p.Account(t, "1000", "520000"), whole...), []string{"assessment_account_id unknown"}},
			"a primary cost account":             {assessmentBody(p.DE, "X", p.c.Admin, p.office, whole...), []string{"assessment_account_id not_secondary"}},
			"a balance sheet account":            {assessmentBody(p.DE, "X", p.c.Admin, p.bank, whole...), []string{"assessment_account_id not_secondary"}},
			"the account and the shares at once": {assessmentBody(p.DE, "X", p.c.Admin, p.office, share{p.c.Sales, "60"}), []string{"assessment_account_id not_secondary", "receivers not_100"}},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.allocation_cycle_invalid", "POST", "/v1/allocation-cycles", c.body, "")
				wantList(t, "field errors", fieldErrors(m), c.want)
			})
		}
	})

	var published []string
	for _, ev := range p.events(t, "allocation_cycle_changed") {
		published = append(published, cycleLine(ev)+" "+accountOf(ev))
	}
	wantList(t, "published", published, []string{line + " " + account, "DISTRIB distribution 1000 -> 2000 100.00 none"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg((detail->>'code') || ' ' || coalesce((detail->>'assessment_account_id') || ' ' || (detail->>'assessment_account_number'), 'none'),
			'; ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'controlling.allocation_cycle.create'`, []any{p.Tenant.ID}, &audited)
	want(t, "audited", audited, "ASSESS "+account+"; DISTRIB none")

	t.Run("an account that is no longer active", func(t *testing.T) {
		// The chart no longer has the account: the activation deactivates it.
		pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1 WHERE tenant_id = $1 AND id = $2`, p.Tenant.ID, p.assessed)
		m := p.in(t).Problem(422, "controlling.allocation_cycle_invalid", "POST", "/v1/allocation-cycles",
			assessmentBody(p.DE, "LATE", p.c.Admin, p.assessed, share{p.c.Sales, "100"}), "")
		wantList(t, "field errors", fieldErrors(m), []string{"assessment_account_id not_active"})
	})
}

func TestAssessmentRun(t *testing.T) {
	p := setup(t)
	it := p.Account(t, "2000", "530000")
	fourth := p.center(t, "4000", "2026-01-01")
	id := p.assessment(t, "ASSESS", share{p.c.Sales, "60"}, share{fourth, "40"})
	path := "/v1/allocation-cycles/" + id
	p.post(t, "2026-05-10", on(p.office, 10001, &p.c.Admin), on(p.bank, -10001, nil))
	p.post(t, "2026-05-20", on(it, 5000, &p.c.Admin), on(p.bank, -5000, nil))
	p.post(t, "2026-06-15", on(p.office, 700, &p.c.Admin), on(p.bank, -700, nil))

	t.Run("a sender without costs posts nothing, and no run is kept", func(t *testing.T) {
		got, _ := p.Must(200, "POST", path+":run", settleBody(2026, 4), `"1"`)
		want(t, "run for April", ranLine(got), "nothing")
		wantList(t, "runs kept", runs(t, p.Client, ""), nil)
	})

	may, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	r := apitest.Object(t, may, "run")
	t.Run("the receivers are charged the sender's costs in one amount on the assessment account", func(t *testing.T) {
		want(t, "run for May", ranLine(may), "ASSESS assessment 2026/5 on 2026-05-31 from 1000: 150.01 EUR on 2 account(s)")
		// 60 % of 150.01 is 90.006 and 40 % is 60.004: the shares are 90.01
		// and 60.00, the total to the cent.
		wantList(t, "the entry's lines", p.entryLines(t, r["journal_entry_id"]), []string{"695000 -150.01 on center 1000", "695000 90.01 on center 2000",
			"695000 60.00 on center 4000"})
		entry, _ := p.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", r["journal_entry_id"]), "", "")
		want(t, "the entry's number, date and text", fmt.Sprint(entry["document_number"], " ", entry["posting_date"], " ", entry["header_text"]),
			fmt.Sprint(r["journal_entry_number"], " 2026-05-31 Assessment ASSESS"))
		wantList(t, "the sender: its primary costs stay, and the assessment relieves it", p.centerCosts(t, p.c.Admin),
			[]string{"520000 10701", "530000 5000", "695000 -15001"})
		wantList(t, "the first receiver's costs", p.centerCosts(t, p.c.Sales), []string{"695000 9001"})
		wantList(t, "the second receiver's costs", p.centerCosts(t, fourth), []string{"695000 6000"})
		var balance int64
		pgtest.Scan(t, `SELECT coalesce(sum(amount_lc), 0)::bigint FROM ledger.journal_line WHERE tenant_id = $1 AND account_id = $2`,
			[]any{p.Tenant.ID, p.assessed}, &balance)
		want(t, "the assessment account's balance in the company", balance, 0)
		got, _ := p.Must(200, "GET", fmt.Sprint("/v1/allocation-runs/", r["id"]), "", "")
		want(t, "the run read", runLine(got), "ASSESS assessment 2026/5 on 2026-05-31 from 1000: 150.01 EUR on 2 account(s)")
	})
	t.Run("a sender assessed for a period is not run for it again", func(t *testing.T) {
		p := p.in(t)
		p.Problem(409, "controlling.allocation_run_exists", "POST", path+":run", settleBody(2026, 5), `"1"`)
		detail := fmt.Sprintf("Cost center 1000 was already assessed for 2026/5 by cycle ASSESS (journal entry %v): its primary costs stay on their accounts, "+
			"so another run of the period would charge them again.", r["journal_entry_number"])
		for name, other := range map[string]string{"by a distribution": p.cycle(t, "DISTRIB", share{p.c.Sales, "100"}),
			"by another assessment": p.assessment(t, "AGAIN", share{fourth, "100"})} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(409, "controlling.allocation_sender_assessed", "POST", "/v1/allocation-cycles/"+other+":run", settleBody(2026, 5), `"1"`)
				want(t, "which assessment it names", m["detail"], any(detail))
			})
		}
		wantList(t, "the sender's costs", p.centerCosts(t, p.c.Admin), []string{"520000 10701", "530000 5000", "695000 -15001"})
		// Another sender's cycle, and the sender's cycles in another period, run as before.
		sales, _ := p.Must(201, "POST", "/v1/allocation-cycles", cycleBody(p.DE, "SALES", "distribution", p.c.Sales, share{fourth, "100"}), "")
		got, _ := p.Must(200, "POST", fmt.Sprint("/v1/allocation-cycles/", sales["id"], ":run"), settleBody(2026, 5), `"1"`)
		want(t, "a receiver's own cycle moves its primary costs alone: none", ranLine(got), "nothing")
	})
	june, _ := p.Must(200, "POST", path+":run", settleBody(2026, 6), `"1"`)
	want(t, "run for June", ranLine(june), "ASSESS assessment 2026/6 on 2026-06-30 from 1000: 7.00 EUR on 1 account(s)")

	t.Run("published and audited", func(t *testing.T) {
		var got []string
		for _, ev := range p.events(t, "allocation_run_posted") {
			got = append(got, fmt.Sprint(ev["code"], " ", ev["kind"], " ", ev["fiscal_year"], "/", ev["fiscal_period"], " ", ev["posting_date"], " ",
				ev["sender_cost_center_code"], " ", ev["amount"].(map[string]any)["amount"], " ", ev["amount"].(map[string]any)["currency"], " ", ev["account_count"]))
		}
		wantList(t, "runs published", got, []string{"ASSESS assessment 2026/5 2026-05-31 1000 150.01 EUR 2", "ASSESS assessment 2026/6 2026-06-30 1000 7.00 EUR 1"})
		var audited string
		pgtest.Scan(t, `SELECT string_agg((detail->>'code') || ' ' || (detail->>'kind') || ' ' || (detail->>'fiscal_period') || ' ' || (detail->>'amount') || ' ' ||
				(detail->>'currency'), '; ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'controlling.allocation_cycle.run'`,
			[]any{p.Tenant.ID}, &audited)
		want(t, "audited", audited, "ASSESS assessment 5 150.01 EUR; ASSESS assessment 6 7.00 EUR")
	})
}

// TestAssessmentRunCosts: an assessment charges the total of the sender's
// primary costs: revenue posted to the sender is none of it, costs that
// add up to nothing post nothing, and a credit total is charged as one.
func TestAssessmentRunCosts(t *testing.T) {
	p := setup(t)
	it := p.Account(t, "2000", "530000")
	c, _ := p.Must(201, "POST", "/v1/allocation-cycles", assessmentBody(p.DE, "SALES", p.c.Sales, p.assessed, share{p.c.Admin, "100"}), "")
	path := "/v1/allocation-cycles/" + c["id"].(string)
	p.post(t, "2026-05-10", on(p.revenue, -50000, &p.c.Sales), on(p.bank, 50000, nil))
	p.post(t, "2026-05-11", on(p.office, 1200, &p.c.Sales), on(it, -1200, &p.c.Sales))
	got, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "costs that add up to nothing", ranLine(got), "nothing")
	wantList(t, "runs kept", runs(t, p.Client, ""), nil)
	p.post(t, "2026-05-12", on(it, -300, &p.c.Sales), on(p.bank, 300, nil))
	got, _ = p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "a credit total", ranLine(got), "SALES assessment 2026/5 on 2026-05-31 from 2000: -3.00 EUR on 2 account(s)")
	wantList(t, "the entry's lines", p.entryLines(t, apitest.Object(t, got, "run")["journal_entry_id"]), []string{"695000 3.00 on center 2000",
		"695000 -3.00 on center 1000"})
	wantList(t, "the sender keeps its revenue and its primary costs", p.centerCosts(t, p.c.Sales), []string{"400000 -50000", "520000 1200", "530000 -1500", "695000 300"})
}

// TestAssessmentRunCenters: what the ledger refuses of the entry is the
// run's refusal, a receiver locked for actual costs among it (an
// assessment charges a center's costs), and no run is kept.
func TestAssessmentRunCenters(t *testing.T) {
	p := setup(t)
	path := "/v1/allocation-cycles/" + p.assessment(t, "LOCKED", share{p.c.Locked, "100"})
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	m := p.Problem(422, "ledger.cost_center_invalid", "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "why the receiver is refused", reason(t, m), "lines[1].cost_center_id locked_for_costs")
	wantList(t, "runs kept", runs(t, p.Client, ""), nil)
	wantList(t, "the sender's costs", p.centerCosts(t, p.c.Admin), []string{"520000 1000"})
}

// TestAssessmentRunsOfOneSender: an assessment and a distribution of one
// sender run for one period at once follow one another on the sender's
// cycles' row locks: the second waits, finds the sender assessed and is
// refused, so the cost is charged once.
func TestAssessmentRunsOfOneSender(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	fourth := p.center(t, "4000", "2026-01-01")
	distribution := uuid.MustParse(p.cycle(t, "FIRST", share{p.c.Sales, "100"}))
	assessment := uuid.MustParse(p.assessment(t, "SECOND", share{fourth, "100"}))
	p.post(t, "2026-05-10", on(p.office, 99999, &p.c.Admin), on(p.bank, -99999, nil))
	run := func(id uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.RunAllocationCycle(ctx, &gen.AllocationCycleRun{FiscalYear: 2026, FiscalPeriod: 5}, gen.RunAllocationCycleParams{ID: id, IfMatch: `"1"`})
			return err
		}
	}
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, run(assessment), run(distribution))
	if first != nil {
		t.Fatalf("the assessment's run: %v", first)
	}
	want(t, "the distribution's run", problem.From(second).Code, "controlling.allocation_sender_assessed")
	wantList(t, "runs kept", runs(t, p.Client, ""), []string{"SECOND assessment 2026/5 on 2026-05-31 from 1000: 999.99 EUR on 1 account(s)"})
	wantList(t, "the sender's costs", p.centerCosts(t, p.c.Admin), []string{"520000 99999", "695000 -99999"})
	wantList(t, "the assessment's receiver", p.centerCosts(t, fourth), []string{"695000 99999"})
	wantList(t, "the distribution's receiver", p.centerCosts(t, p.c.Sales), nil)
}
