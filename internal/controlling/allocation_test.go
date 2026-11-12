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

// Allocation cycles and their distribution runs (T60f, ADR-0064 §5).

// share is a receiver of a cycle body with its percentage.
type share struct {
	center uuid.UUID
	pct    string
}

// cycleBody is a new distribution cycle of the company.
func cycleBody(le uuid.UUID, code, kind string, sender uuid.UUID, receivers ...share) string {
	rs := make([]string, len(receivers))
	for i, r := range receivers {
		rs[i] = fmt.Sprintf(`{"cost_center_id": %q, "percentage": %q}`, r.center, r.pct)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "Cycle %s", "kind": %q, "sender_cost_center_id": %q, "receivers": [%s]}`,
		le, code, code, kind, sender, strings.Join(rs, ", "))
}

// center creates an active cost center of company 2000 valid from the day
// and returns its id.
func (p plan) center(t *testing.T, code, from string) uuid.UUID {
	t.Helper()
	created, _ := p.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "Center %s", "responsible": "J",
		"category": "ADMIN", "valid_from": %q, "reason": "r"}`, p.DE, code, code, from), "")
	p.Approve(t, created["pending_change_id"].(string))
	return uuid.MustParse(created["id"].(string))
}

// cycle creates a distribution cycle of company 2000 from cost center
// 1000 to the receivers and returns its id.
func (p plan) cycle(t *testing.T, code string, receivers ...share) string {
	t.Helper()
	c, _ := p.Must(201, "POST", "/v1/allocation-cycles", cycleBody(p.DE, code, "distribution", p.c.Admin, receivers...), "")
	return c["id"].(string)
}

// cycleLine is a cycle as "code kind sender -> receiver percentage, ...".
func cycleLine(v any) string {
	c := v.(map[string]any)
	var rs []string
	for _, r := range c["receivers"].([]any) {
		m := r.(map[string]any)
		rs = append(rs, fmt.Sprint(m["cost_center_code"], " ", m["percentage"]))
	}
	return fmt.Sprintf("%v %v %v -> %s", c["code"], c["kind"], c["sender_cost_center_code"], strings.Join(rs, ", "))
}

// cycles lists the cycles of a query as cycleLine, then "next" when there
// is a further page.
func cycles(t *testing.T, c apitest.Client, query string) []string {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/allocation-cycles"+query, "", "")
	var out []string
	for _, it := range apitest.List(t, page, "items") {
		out = append(out, cycleLine(it))
	}
	if _, ok := page["next_page_token"].(string); ok {
		out = append(out, "next")
	}
	return out
}

// fieldErrors are a problem's field errors as "field code", in order.
func fieldErrors(m map[string]any) []string {
	var out []string
	errs, _ := m["errors"].([]any)
	for _, e := range errs {
		fe, _ := e.(map[string]any)
		out = append(out, fmt.Sprint(fe["field"], " ", fe["code"]))
	}
	return out
}

func TestAllocationCycles(t *testing.T) {
	p := setup(t)
	created, tag := p.Must(201, "POST", "/v1/allocation-cycles", cycleBody(p.DE, "SHARED", "distribution", p.c.Admin, share{p.c.Sales, "60"},
		share{p.c.Locked, "40.0"}), "")
	id := created["id"].(string)
	want(t, "the new cycle", cycleLine(created), "SHARED distribution 1000 -> 2000 60.00, 3000 40.00")
	want(t, "the new cycle's etag", tag, `"1"`)
	want(t, "the new cycle's centers", fmt.Sprint(created["sender_cost_center_id"], " ", created["receivers"].([]any)[1].(map[string]any)["cost_center_id"]),
		fmt.Sprint(p.c.Admin, " ", p.c.Locked))
	got, tag := p.Must(200, "GET", "/v1/allocation-cycles/"+id, "", "")
	want(t, "the cycle read", cycleLine(got), "SHARED distribution 1000 -> 2000 60.00, 3000 40.00")
	want(t, "the cycle's etag read", tag, `"1"`)

	t.Run("refusals of a create", func(t *testing.T) {
		p := p.in(t)
		whole := []share{{p.c.Sales, "100"}}
		p.Problem(409, "controlling.allocation_cycle_exists", "POST", "/v1/allocation-cycles", cycleBody(p.DE, "SHARED", "distribution", p.c.Admin, whole...), "")
		p.Blank("name", "POST", "/v1/allocation-cycles", fmt.Sprintf(`{"legal_entity_id": %q, "code": "BLANK", "name": "  ", "kind": "distribution",
			"sender_cost_center_id": %q, "receivers": [{"cost_center_id": %q, "percentage": "100"}]}`, p.DE, p.c.Admin, p.c.Sales), "")
		p.Problem(422, "request.reference_not_found", "POST", "/v1/allocation-cycles", cycleBody(uuid.New(), "X", "distribution", p.c.Admin, whole...), "")
		p.Problem(400, "request.invalid_body", "POST", "/v1/allocation-cycles", cycleBody(p.DE, "X", "distribution", p.c.Admin), "")
		p.Problem(400, "request.invalid_body", "POST", "/v1/allocation-cycles", cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "100.001"}), "")
		p.Problem(404, "request.not_found", "GET", "/v1/allocation-cycles/"+uuid.NewString(), "", "")
		pending, _ := p.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "9000", "name": "Pending", "responsible": "J",
			"category": "ADMIN", "valid_from": "2026-01-01", "reason": "r"}`, p.DE), "")
		waiting := uuid.MustParse(pending["id"].(string))
		for name, c := range map[string]struct {
			body string
			want []string
		}{
			"a kind that is none":   {cycleBody(p.DE, "X", "assessment", p.c.Admin, whole...), []string{"kind unsupported"}},
			"a sender that is none": {cycleBody(p.DE, "X", "distribution", uuid.New(), whole...), []string{"sender_cost_center_id unknown"}},
			"a sender of another company": {cycleBody(p.US, "X", "distribution", p.c.Admin, whole...),
				[]string{"sender_cost_center_id not_in_company", "receivers[0].cost_center_id not_in_company"}},
			"a receiver waiting for approval": {cycleBody(p.DE, "X", "distribution", p.c.Admin, share{waiting, "100"}), []string{"receivers[0].cost_center_id not_active"}},
			"a receiver that is none": {cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "50"}, share{uuid.New(), "50"}),
				[]string{"receivers[1].cost_center_id unknown"}},
			"the sender as a receiver": {cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "50"}, share{p.c.Admin, "50"}),
				[]string{"receivers[1].cost_center_id is_sender"}},
			"a receiver twice": {cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "50"}, share{p.c.Sales, "50"}),
				[]string{"receivers[1].cost_center_id duplicate"}},
			"a percentage of nothing": {cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "100"}, share{p.c.Locked, "0"}),
				[]string{"receivers[1].percentage invalid"}},
			"percentages short of the whole": {cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "60"}, share{p.c.Locked, "39.99"}),
				[]string{"receivers not_100"}},
			"percentages beyond the whole": {cycleBody(p.DE, "X", "distribution", p.c.Admin, share{p.c.Sales, "60"}, share{p.c.Locked, "40.01"}),
				[]string{"receivers not_100"}},
			"every field that cannot stand": {cycleBody(p.DE, "X", "assessment", uuid.New(), share{p.c.Sales, "60"}, share{p.c.Sales, "30"}),
				[]string{"kind unsupported", "sender_cost_center_id unknown", "receivers[1].cost_center_id duplicate", "receivers not_100"}},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.allocation_cycle_invalid", "POST", "/v1/allocation-cycles", c.body, "")
				wantList(t, "field errors", fieldErrors(m), c.want)
			})
		}
		wantList(t, "cycles after refusals", cycles(t, p.Client, ""), []string{"SHARED distribution 1000 -> 2000 60.00, 3000 40.00"})
	})

	t.Run("list", func(t *testing.T) {
		p := p.in(t)
		p.Must(201, "POST", "/v1/allocation-cycles", cycleBody(p.DE, "ADMIN", "distribution", p.c.Sales, share{p.c.Admin, "33.33"}, share{p.c.Locked, "66.67"}), "")
		shared, admin := "SHARED distribution 1000 -> 2000 60.00, 3000 40.00", "ADMIN distribution 2000 -> 1000 33.33, 3000 66.67"
		for name, c := range map[string]struct {
			query string
			want  []string
		}{
			"every cycle by code":     {"", []string{admin, shared}},
			"of a company":            {"?legal_entity_id=" + p.DE.String(), []string{admin, shared}},
			"of a company with none":  {"?legal_entity_id=" + p.US.String(), nil},
			"with a code":             {"?code=SHARED", []string{shared}},
			"of a kind":               {"?kind=distribution&code=ADMIN", []string{admin}},
			"of a kind with none":     {"?kind=assessment", nil},
			"of a sender":             {"?sender_cost_center_id=" + p.c.Sales.String(), []string{admin}},
			"a first page":            {"?page_size=1", []string{admin, "next"}},
			"by code, the last first": {"?order_by=code%20desc", []string{shared, admin}},
		} {
			t.Run(name, func(t *testing.T) {
				wantList(t, "cycles", cycles(t, p.in(t).Client, c.query), c.want)
			})
		}
		page, _ := p.Must(200, "GET", "/v1/allocation-cycles?page_size=1", "", "")
		wantList(t, "the second page", cycles(t, p.Client, "?page_size=1&page_token="+page["next_page_token"].(string)), []string{shared})
		p.Problem(400, "request.invalid_order_by", "GET", "/v1/allocation-cycles?order_by=name", "", "")
	})

	var published []string
	for _, ev := range p.events(t, "allocation_cycle_changed") {
		published = append(published, cycleLine(ev)+" "+fmt.Sprint(ev["change"]))
	}
	wantList(t, "published", published, []string{"SHARED distribution 1000 -> 2000 60.00, 3000 40.00 created", "ADMIN distribution 2000 -> 1000 33.33, 3000 66.67 created"})
	want(t, "the event's cycle", p.events(t, "allocation_cycle_changed")[0]["allocation_cycle_id"], any(id))
	var audited string
	pgtest.Scan(t, `SELECT string_agg(action || ' ' || (detail->>'code') || ' ' || (detail->>'sender_cost_center_code') || ' ' ||
			jsonb_array_length(detail->'receivers'), '; ' ORDER BY seq)
		FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'controlling.allocation_cycle.%'`, []any{p.Tenant.ID}, &audited)
	want(t, "audited", audited, "controlling.allocation_cycle.create SHARED 1000 2; controlling.allocation_cycle.create ADMIN 2000 2")
}

// TestAllocationAccess: cycles and runs are read, defined and run only in
// the companies a person may act in, each by its permission; defining
// and running are a person's, and the runs' amounts need
// ledger.entry.read beside.
func TestAllocationAccess(t *testing.T) {
	p := setup(t)
	id := p.cycle(t, "SHARED", share{p.c.Sales, "100"})
	path := "/v1/allocation-cycles/" + id
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	ran, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	run := "/v1/allocation-runs/" + apitest.Object(t, ran, "run")["id"].(string)
	body := cycleBody(p.DE, "OTHER", "distribution", p.c.Admin, share{p.c.Sales, "100"})

	other := p.as(p.Reader(t, "Reader of 1000", p.US))
	other.Problem(404, "request.not_found", "GET", path, "", "")
	other.Problem(404, "request.not_found", "GET", run, "", "")
	other.Problem(404, "request.not_found", "POST", path+":run", settleBody(2026, 6), `"1"`)
	other.Problem(422, "request.reference_not_found", "POST", "/v1/allocation-cycles", body, "")
	wantList(t, "cycles another company's reader sees", cycles(t, other.Client, ""), nil)
	page, _ := other.Must(200, "GET", "/v1/allocation-runs", "", "")
	want(t, "runs another company's reader sees", len(apitest.List(t, page, "items")), 0)

	agent := p.As(p.Tenant.Agent)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent.Must(200, "GET", path, "", "")
	agent.Must(200, "GET", "/v1/allocation-cycles", "", "")
	agent.Must(200, "GET", run, "", "")
	agent.Must(200, "GET", "/v1/allocation-runs", "", "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/allocation-cycles", body, "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":run", settleBody(2026, 6), `"1"`)
	for _, c := range []struct{ permission, method, path, body string }{
		{"controlling.allocation.write", "POST", "/v1/allocation-cycles", body}, {"controlling.allocation.run", "POST", path + ":run", settleBody(2026, 6)},
		{"ledger.entry.read", "POST", path + ":run", settleBody(2026, 6)}, {"ledger.entry.read", "GET", run, ""},
		{"ledger.entry.read", "GET", "/v1/allocation-runs", ""}, {"controlling.allocation.read", "GET", path, ""},
		{"controlling.allocation.read", "GET", "/v1/allocation-cycles", ""}, {"controlling.allocation.read", "GET", run, ""},
		{"controlling.allocation.read", "GET", "/v1/allocation-runs", ""},
	} {
		t.Run(c.method+" "+c.path[strings.LastIndex(c.path, "/"):]+" without "+c.permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, c.permission)
			p.Problem(403, "identity.permission_missing", c.method, c.path, c.body, `"1"`)
		})
	}
}

// runLine is a run as "cycle kind year/period on date from sender: amount
// currency on n account(s)".
func runLine(v any) string {
	r := v.(map[string]any)
	a := r["amount"].(map[string]any)
	return fmt.Sprintf("%v %v %v/%v on %v from %v: %v %v on %v account(s)", r["allocation_cycle_code"], r["kind"], r["fiscal_year"], r["fiscal_period"],
		r["posting_date"], r["sender_cost_center_code"], a["amount"], a["currency"], r["account_count"])
}

// ranLine is what a run answered: the run posted, or "nothing".
func ranLine(m map[string]any) string {
	if m["posted"] != true {
		if _, ok := m["run"]; ok {
			return "a run although nothing was posted"
		}
		return "nothing"
	}
	return runLine(m["run"])
}

// runs lists the runs of a query as runLine, then "next" when there is a
// further page.
func runs(t *testing.T, c apitest.Client, query string) []string {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/allocation-runs"+query, "", "")
	var out []string
	for _, it := range apitest.List(t, page, "items") {
		out = append(out, runLine(it))
	}
	if _, ok := page["next_page_token"].(string); ok {
		out = append(out, "next")
	}
	return out
}

func TestDistributionRun(t *testing.T) {
	p := setup(t)
	it := p.Account(t, "2000", "530000")
	fourth := p.center(t, "4000", "2026-01-01")
	id := p.cycle(t, "SHARED", share{p.c.Sales, "60"}, share{fourth, "40"})
	path := "/v1/allocation-cycles/" + id
	p.post(t, "2026-05-10", on(p.office, 10001, &p.c.Admin), on(p.bank, -10001, nil))
	p.post(t, "2026-05-20", on(it, 5000, &p.c.Admin), on(p.bank, -5000, nil))
	p.post(t, "2026-06-15", on(p.office, 700, &p.c.Admin), on(p.bank, -700, nil))
	// Another center's cost is none of the sender's.
	p.post(t, "2026-05-10", on(p.office, 99900, &p.c.Sales), on(p.bank, -99900, nil))

	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		p.Problem(428, "request.precondition_required", "POST", path+":run", settleBody(2026, 5), "")
		p.Problem(412, "request.version_mismatch", "POST", path+":run", settleBody(2026, 5), `"9"`)
		p.Problem(404, "request.not_found", "POST", "/v1/allocation-cycles/"+uuid.NewString()+":run", settleBody(2026, 5), `"1"`)
		p.Problem(404, "request.not_found", "GET", "/v1/allocation-runs/"+uuid.NewString(), "", "")
		p.Problem(422, "ledger.no_fiscal_period", "POST", path+":run", settleBody(1999, 5), `"1"`)
		m := p.Problem(422, "controlling.period_not_regular", "POST", path+":run", settleBody(2026, 13), `"1"`)
		want(t, "field error of a special period", reason(t, m), "fiscal_period not_regular")
		p.Problem(400, "request.invalid_body", "POST", path+":run", settleBody(2026, 17), `"1"`)
		wantList(t, "runs after refusals", runs(t, p.Client, ""), nil)
	})
	t.Run("a sender without costs posts nothing, and no run is kept", func(t *testing.T) {
		for range 2 {
			got, _ := p.Must(200, "POST", path+":run", settleBody(2026, 4), `"1"`)
			want(t, "run for April", ranLine(got), "nothing")
			want(t, "the cycle and period named", fmt.Sprint(got["allocation_cycle_id"], " ", got["allocation_cycle_code"], " ", got["allocation_cycle_name"], " ",
				got["legal_entity_id"], " ", got["fiscal_year"], "/", got["fiscal_period"]), fmt.Sprint(id, " SHARED Cycle SHARED ", p.DE, " 2026/4"))
		}
		wantList(t, "runs kept", runs(t, p.Client, ""), nil)
	})

	may, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	t.Run("the sender's costs of the period move to the receivers by their shares", func(t *testing.T) {
		want(t, "run for May", ranLine(may), "SHARED distribution 2026/5 on 2026-05-31 from 1000: 150.01 EUR on 2 account(s)")
		r := apitest.Object(t, may, "run")
		want(t, "the run's cycle, sender and maker", fmt.Sprint(r["allocation_cycle_id"], " ", r["allocation_cycle_name"], " ", r["legal_entity_id"], " ",
			r["sender_cost_center_id"], " ", r["created_by"]), fmt.Sprint(id, " Cycle SHARED ", p.DE, " ", p.c.Admin, " ", p.Tenant.Human.ID))
		// 60 % of 100.01 is 60.006 and 40 % is 40.004: the shares are 60.01
		// and 40.00, the cost to the cent.
		wantList(t, "the entry's lines", p.entryLines(t, r["journal_entry_id"]), []string{"520000 -100.01 on center 1000", "520000 60.01 on center 2000",
			"520000 40.00 on center 4000", "530000 -50.00 on center 1000", "530000 30.00 on center 2000", "530000 20.00 on center 4000"})
		entry, _ := p.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", r["journal_entry_id"]), "", "")
		want(t, "the entry's number and date", fmt.Sprint(entry["document_number"], " ", entry["posting_date"], " ", entry["fiscal_period"]),
			fmt.Sprint(r["journal_entry_number"], " 2026-05-31 5"))
		wantList(t, "the sender's costs: June's alone", p.centerCosts(t, p.c.Admin), []string{"520000 700"})
		wantList(t, "the first receiver's costs", p.centerCosts(t, p.c.Sales), []string{"520000 105901", "530000 3000"})
		wantList(t, "the second receiver's costs", p.centerCosts(t, fourth), []string{"520000 4000", "530000 2000"})
		got, _ := p.Must(200, "GET", fmt.Sprint("/v1/allocation-runs/", r["id"]), "", "")
		want(t, "the run read", runLine(got), "SHARED distribution 2026/5 on 2026-05-31 from 1000: 150.01 EUR on 2 account(s)")
		want(t, "the run read's entry", got["journal_entry_id"], r["journal_entry_id"])
	})
	t.Run("one run per cycle and period", func(t *testing.T) {
		m := p.in(t).Problem(409, "controlling.allocation_run_exists", "POST", path+":run", settleBody(2026, 5), `"1"`)
		want(t, "which run it names", m["detail"], any(fmt.Sprintf("The allocation cycle was already run for 2026/5: journal entry %v.",
			apitest.Object(t, may, "run")["journal_entry_number"])))
		// A cost posted to the sender after its run stays on the sender.
		p.post(t, "2026-05-25", on(p.office, 300, &p.c.Admin), on(p.bank, -300, nil))
		p.in(t).Problem(409, "controlling.allocation_run_exists", "POST", path+":run", settleBody(2026, 5), `"1"`)
		wantList(t, "the sender's costs", p.centerCosts(t, p.c.Admin), []string{"520000 1000"})
	})
	june, _ := p.Must(200, "POST", path+":run", settleBody(2026, 6), `"1"`)
	want(t, "run for June", ranLine(june), "SHARED distribution 2026/6 on 2026-06-30 from 1000: 7.00 EUR on 1 account(s)")
	other := p.cycle(t, "OTHER", share{fourth, "100"})
	p.post(t, "2026-07-10", on(p.office, 900, &p.c.Admin), on(p.bank, -900, nil))
	july, _ := p.Must(200, "POST", "/v1/allocation-cycles/"+other+":run", settleBody(2026, 7), `"1"`)
	want(t, "another cycle's run for July", ranLine(july), "OTHER distribution 2026/7 on 2026-07-31 from 1000: 9.00 EUR on 1 account(s)")

	t.Run("the runs listed, latest first", func(t *testing.T) {
		p := p.in(t)
		all := []string{"OTHER distribution 2026/7 on 2026-07-31 from 1000: 9.00 EUR on 1 account(s)",
			"SHARED distribution 2026/6 on 2026-06-30 from 1000: 7.00 EUR on 1 account(s)",
			"SHARED distribution 2026/5 on 2026-05-31 from 1000: 150.01 EUR on 2 account(s)"}
		for name, c := range map[string]struct {
			query string
			want  []string
		}{
			"every run":              {"", all},
			"of a company":           {"?legal_entity_id=" + p.DE.String(), all},
			"of a company with none": {"?legal_entity_id=" + p.US.String(), nil},
			"of a cycle":             {"?allocation_cycle_id=" + id, all[1:]},
			"of a year":              {"?fiscal_year=2026", all},
			"of a year with none":    {"?fiscal_year=2025", nil},
			"of a period":            {"?fiscal_year=2026&fiscal_period=5", all[2:]},
			"a first page":           {"?page_size=2", append(slicesClone(all[:2]), "next")},
			"the earliest first":     {"?order_by=posting_date%20asc&page_size=1", []string{all[2], "next"}},
		} {
			t.Run(name, func(t *testing.T) {
				wantList(t, "runs", runs(t, p.in(t).Client, c.query), c.want)
			})
		}
		page, _ := p.Must(200, "GET", "/v1/allocation-runs?page_size=2", "", "")
		wantList(t, "the second page", runs(t, p.Client, "?page_size=2&page_token="+page["next_page_token"].(string)), all[2:])
		p.Problem(400, "request.invalid_order_by", "GET", "/v1/allocation-runs?order_by=amount", "", "")
	})
	t.Run("published and audited", func(t *testing.T) {
		var got []string
		for _, ev := range p.events(t, "allocation_run_posted") {
			got = append(got, fmt.Sprint(ev["code"], " ", ev["kind"], " ", ev["fiscal_year"], "/", ev["fiscal_period"], " ", ev["posting_date"], " ",
				ev["sender_cost_center_code"], " ", ev["amount"].(map[string]any)["amount"], " ", ev["amount"].(map[string]any)["currency"], " ", ev["account_count"]))
		}
		wantList(t, "runs published", got, []string{"SHARED distribution 2026/5 2026-05-31 1000 150.01 EUR 2", "SHARED distribution 2026/6 2026-06-30 1000 7.00 EUR 1",
			"OTHER distribution 2026/7 2026-07-31 1000 9.00 EUR 1"})
		first, r := p.events(t, "allocation_run_posted")[0], apitest.Object(t, may, "run")
		want(t, "the event's run, cycle and entry", fmt.Sprint(first["allocation_run_id"], " ", first["allocation_cycle_id"], " ", first["name"], " ",
			first["journal_entry_id"], " ", first["journal_entry_number"], " ", first["sender_cost_center_id"]),
			fmt.Sprint(r["id"], " ", id, " Cycle SHARED ", r["journal_entry_id"], " ", r["journal_entry_number"], " ", p.c.Admin))
		var audited string
		pgtest.Scan(t, `SELECT string_agg((detail->>'code') || ' ' || (detail->>'fiscal_period') || ' ' || (detail->>'amount') || ' ' || (detail->>'currency'), '; '
				ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'controlling.allocation_cycle.run'`, []any{p.Tenant.ID}, &audited)
		want(t, "audited", audited, "SHARED 5 150.01 EUR; SHARED 6 7.00 EUR; OTHER 7 9.00 EUR")
	})
}

func slicesClone(in []string) []string { return append([]string(nil), in...) }

// TestDistributionRunCosts: a run moves the sender's primary costs only:
// revenue posted to the sender stays, and a credit balance on a cost
// account moves as one.
func TestDistributionRunCosts(t *testing.T) {
	p := setup(t)
	c, _ := p.Must(201, "POST", "/v1/allocation-cycles", cycleBody(p.DE, "SALES", "distribution", p.c.Sales, share{p.c.Admin, "100"}), "")
	path := "/v1/allocation-cycles/" + c["id"].(string)
	p.post(t, "2026-05-10", on(p.revenue, -50000, &p.c.Sales), on(p.bank, 50000, nil))
	got, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "a sender with revenue alone", ranLine(got), "nothing")
	p.post(t, "2026-05-11", on(p.office, -1200, &p.c.Sales), on(p.bank, 1200, nil))
	got, _ = p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "a credit on a cost account", ranLine(got), "SALES distribution 2026/5 on 2026-05-31 from 2000: -12.00 EUR on 1 account(s)")
	wantList(t, "the entry's lines", p.entryLines(t, apitest.Object(t, got, "run")["journal_entry_id"]), []string{"520000 12.00 on center 2000",
		"520000 -12.00 on center 1000"})
	wantList(t, "the sender keeps its revenue", p.centerCosts(t, p.c.Sales), []string{"400000 -50000"})
}

// TestDistributionRunCenters: the sender and the receivers are checked on
// the period's last day before anything is posted, and what the ledger
// refuses of the entry is the run's refusal; no run is kept.
func TestDistributionRunCenters(t *testing.T) {
	p := setup(t)
	later := p.center(t, "9300", "2026-06-01")
	path := "/v1/allocation-cycles/" + p.cycle(t, "LATER", share{p.c.Sales, "50"}, share{later, "50"})
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	m := p.Problem(409, "controlling.allocation_center_not_valid", "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "which center is refused", m["detail"], any("Cost center 9300 of the cycle is not valid on 2026-05-31, the period's last day."))
	t.Run("a receiver locked for costs is the ledger's refusal", func(t *testing.T) {
		locked := "/v1/allocation-cycles/" + p.cycle(t, "LOCKED", share{p.c.Locked, "100"})
		p.Problem(422, "ledger.cost_center_invalid", "POST", locked+":run", settleBody(2026, 5), `"1"`)
	})
	wantList(t, "runs kept", runs(t, p.Client, ""), nil)
	p.post(t, "2026-06-10", on(p.office, 500, &p.c.Admin), on(p.bank, -500, nil))
	june, _ := p.Must(200, "POST", path+":run", settleBody(2026, 6), `"1"`)
	want(t, "run once the receiver is valid", ranLine(june), "LATER distribution 2026/6 on 2026-06-30 from 1000: 5.00 EUR on 1 account(s)")
}

// TestDistributionRunsOfOneSender: two cycles of one sender run for one
// period at once follow one another on the sender's cycles' row locks:
// the second waits, finds the cost gone and posts nothing, so the cost is
// moved once.
func TestDistributionRunsOfOneSender(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	fourth := p.center(t, "4000", "2026-01-01")
	first := uuid.MustParse(p.cycle(t, "FIRST", share{p.c.Sales, "100"}))
	second := uuid.MustParse(p.cycle(t, "SECOND", share{fourth, "100"}))
	p.post(t, "2026-05-10", on(p.office, 99999, &p.c.Admin), on(p.bank, -99999, nil))
	var posted []bool
	run := func(id uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			res, err := h.RunAllocationCycle(ctx, &gen.AllocationCycleRun{FiscalYear: 2026, FiscalPeriod: 5}, gen.RunAllocationCycleParams{ID: id, IfMatch: `"1"`})
			if err == nil {
				posted = append(posted, res.(*gen.AllocationRunResultHeaders).Response.Posted)
			}
			return err
		}
	}
	// The second cycle has the lower lock neither run holds first: each
	// run locks both cycles in id order, whichever it is for.
	a, b := apitest.Serialized(t, p.Tenant, p.Tenant.Human, run(second), run(first))
	if a != nil || b != nil {
		t.Fatalf("runs: got %v and %v, want both answered", a, b)
	}
	want(t, "first and second posted", fmt.Sprint(posted), "[true false]")
	wantList(t, "runs kept", runs(t, p.Client, ""), []string{"SECOND distribution 2026/5 on 2026-05-31 from 1000: 999.99 EUR on 1 account(s)"})
	wantList(t, "the sender's costs", p.centerCosts(t, p.c.Admin), nil)
	wantList(t, "the receiver of the cycle run first", p.centerCosts(t, fourth), []string{"520000 99999"})
	wantList(t, "the receiver of the cycle run second", p.centerCosts(t, p.c.Sales), nil)
}

// TestDistributionRunOnce: two runs of a cycle for one period at once
// serialize on the row locks of the sender's cycles; the second finds the first's run and
// is refused, and one entry is posted.
func TestDistributionRunOnce(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id := uuid.MustParse(p.cycle(t, "SHARED", share{p.c.Sales, "100"}))
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	run := func(ctx context.Context) error {
		_, err := h.RunAllocationCycle(ctx, &gen.AllocationCycleRun{FiscalYear: 2026, FiscalPeriod: 5}, gen.RunAllocationCycleParams{ID: id, IfMatch: `"1"`})
		return err
	}
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, run, run)
	if first != nil {
		t.Fatalf("first run: %v", first)
	}
	want(t, "second run", problem.From(second).Code, "controlling.allocation_run_exists")
	wantList(t, "runs kept", runs(t, p.Client, ""), []string{"SHARED distribution 2026/5 on 2026-05-31 from 1000: 10.00 EUR on 1 account(s)"})
	wantList(t, "the receiver's costs", p.centerCosts(t, p.c.Sales), []string{"520000 1000"})
}
