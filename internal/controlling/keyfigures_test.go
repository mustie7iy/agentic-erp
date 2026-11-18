package controlling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/controlling"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Statistical key figures, their values, and cycles whose receivers'
// shares come from them (T60j, ADR-0064 As built T60j).

// keyFigureBody is a new key figure of the company.
func keyFigureBody(le uuid.UUID, code, unit string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "Figure %s", "unit": %q}`, le, code, code, unit)
}

// keyFigure creates a key figure of company 2000 counted in EA and
// returns its id.
func (p plan) keyFigure(t *testing.T, code string) string {
	t.Helper()
	k, _ := p.Must(201, "POST", "/v1/statistical-key-figures", keyFigureBody(p.DE, code, "EA"), "")
	return k["id"].(string)
}

// value is a line of a posting.
type value struct {
	center uuid.UUID
	value  string
}

func postingBody(year, period int, values ...value) string {
	ls := make([]string, len(values))
	for i, v := range values {
		ls[i] = fmt.Sprintf(`{"cost_center_id": %q, "value": %q}`, v.center, v.value)
	}
	return fmt.Sprintf(`{"fiscal_year": %d, "fiscal_period": %d, "lines": [%s]}`, year, period, strings.Join(ls, ", "))
}

// postValues posts the key figure's values for 2026/period and returns
// the posting.
func (p plan) postValues(t *testing.T, figure string, period int, values ...value) map[string]any {
	t.Helper()
	posted, _ := p.Must(201, "POST", "/v1/statistical-key-figures/"+figure+"/postings", postingBody(2026, period, values...), "")
	return posted
}

// valuesLine is a values page as "center value, ... = total", then "next"
// when a further page follows.
func valuesLine(t *testing.T, page map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range apitest.List(t, page, "items") {
		m := it.(map[string]any)
		out = append(out, fmt.Sprint(m["cost_center_code"], " ", m["value"]))
	}
	line := strings.Join(out, ", ") + " = " + fmt.Sprint(page["total"])
	if _, ok := page["next_page_token"].(string); ok {
		line += " next"
	}
	return line
}

func TestStatisticalKeyFigures(t *testing.T) {
	p := setup(t)
	created, tag := p.Must(201, "POST", "/v1/statistical-key-figures", keyFigureBody(p.DE, "EMPL", "EA"), "")
	id := created["id"].(string)
	line := func(k map[string]any) string {
		return fmt.Sprint(k["code"], " ", k["name"], " ", k["unit"], " ", k["legal_entity_id"])
	}
	want(t, "the new key figure", line(created), fmt.Sprint("EMPL Figure EMPL EA ", p.DE))
	want(t, "its etag", tag, `"1"`)
	got, tag := p.Must(200, "GET", "/v1/statistical-key-figures/"+id, "", "")
	want(t, "the key figure read", line(got), fmt.Sprint("EMPL Figure EMPL EA ", p.DE))
	want(t, "its etag read", tag, `"1"`)
	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		p.Problem(409, "controlling.statistical_key_figure_exists", "POST", "/v1/statistical-key-figures", keyFigureBody(p.DE, "EMPL", "M2"), "")
		m := p.Problem(422, "controlling.statistical_key_figure_invalid", "POST", "/v1/statistical-key-figures", keyFigureBody(p.DE, "AREA", "SQFT"), "")
		want(t, "an unknown unit", reason(t, m), "unit unknown")
		p.Blank("name", "POST", "/v1/statistical-key-figures", fmt.Sprintf(`{"legal_entity_id": %q, "code": "B", "name": " ", "unit": "EA"}`, p.DE), "")
		p.Problem(422, "request.reference_not_found", "POST", "/v1/statistical-key-figures", keyFigureBody(uuid.New(), "X", "EA"), "")
		p.Problem(400, "request.invalid_body", "POST", "/v1/statistical-key-figures", keyFigureBody(p.DE, "lower", "EA"), "")
		p.Problem(404, "request.not_found", "GET", "/v1/statistical-key-figures/"+uuid.NewString(), "", "")
	})
	p.Must(201, "POST", "/v1/statistical-key-figures", keyFigureBody(p.DE, "AREA", "M2"), "")
	p.Must(201, "POST", "/v1/statistical-key-figures", keyFigureBody(p.US, "EMPL", "EA"), "")
	t.Run("list", func(t *testing.T) {
		list := func(t *testing.T, query string) []string {
			t.Helper()
			page, _ := p.in(t).Must(200, "GET", "/v1/statistical-key-figures"+query, "", "")
			var out []string
			for _, it := range apitest.List(t, page, "items") {
				k := it.(map[string]any)
				out = append(out, fmt.Sprint(k["code"], " ", k["unit"]))
			}
			if tok, ok := page["next_page_token"].(string); ok {
				out = append(out, "next:"+tok)
			}
			return out
		}
		for name, c := range map[string]struct {
			query string
			want  []string
		}{
			"of a company, by code":  {"?legal_entity_id=" + p.DE.String(), []string{"AREA M2", "EMPL EA"}},
			"with a code":            {"?code=EMPL", []string{"EMPL EA", "EMPL EA"}},
			"the last first":         {"?legal_entity_id=" + p.DE.String() + "&order_by=code%20desc", []string{"EMPL EA", "AREA M2"}},
			"of a company with none": {"?legal_entity_id=" + uuid.NewString(), nil},
		} {
			t.Run(name, func(t *testing.T) {
				wantList(t, "key figures", list(t, c.query), c.want)
			})
		}
		first := list(t, "?legal_entity_id="+p.DE.String()+"&page_size=1")
		want(t, "the first page", first[0], "AREA M2")
		wantList(t, "the second page", list(t, "?legal_entity_id="+p.DE.String()+"&page_size=1&page_token="+strings.TrimPrefix(first[1], "next:")),
			[]string{"EMPL EA"})
		p.Problem(400, "request.invalid_order_by", "GET", "/v1/statistical-key-figures?order_by=name", "", "")
	})
	var published []string
	for _, ev := range p.events(t, "statistical_key_figure_changed") {
		published = append(published, fmt.Sprint(ev["code"], " ", ev["unit"], " ", ev["change"]))
	}
	wantList(t, "published", published, []string{"EMPL EA created", "AREA M2 created", "EMPL EA created"})
	want(t, "the event's key figure", p.events(t, "statistical_key_figure_changed")[0]["statistical_key_figure_id"], any(id))
	var audited string
	pgtest.Scan(t, `SELECT string_agg(action || ' ' || (detail->>'code') || ' ' || (detail->>'unit'), '; ' ORDER BY seq) FROM audit.audit_event
		WHERE tenant_id = $1 AND action LIKE 'controlling.statistical_key_figure.%'`, []any{p.Tenant.ID}, &audited)
	want(t, "audited", audited, "controlling.statistical_key_figure.create EMPL EA; controlling.statistical_key_figure.create AREA M2; "+
		"controlling.statistical_key_figure.create EMPL EA")
}

func TestStatisticalKeyFigurePostings(t *testing.T) {
	p := setup(t)
	figure := p.keyFigure(t, "EMPL")
	path := "/v1/statistical-key-figures/" + figure
	posted := p.postValues(t, figure, 5, value{p.c.Sales, "3.50"}, value{p.c.Admin, "2"})
	var lines []string
	for _, l := range apitest.List(t, posted, "lines") {
		m := l.(map[string]any)
		lines = append(lines, fmt.Sprint(m["cost_center_id"], " ", m["cost_center_code"], " ", m["value"]))
	}
	wantList(t, "the posting's lines, as the column keeps them", lines, []string{fmt.Sprint(p.c.Sales, " 2000 3.5"), fmt.Sprint(p.c.Admin, " 1000 2")})
	want(t, "the posting", fmt.Sprint(posted["statistical_key_figure_id"], " ", posted["statistical_key_figure_code"], " ", posted["unit"], " ",
		posted["legal_entity_id"], " ", posted["fiscal_year"], "/", posted["fiscal_period"], " ", posted["created_by"]),
		fmt.Sprint(figure, " EMPL EA ", p.DE, " 2026/5 ", p.Tenant.Human.ID))
	p.postValues(t, figure, 5, value{p.c.Sales, "-0.5"}, value{p.c.Locked, "1.000001"})
	p.postValues(t, figure, 6, value{p.c.Sales, "7"})
	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		waiting, _ := p.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "9000", "name": "Pending", "responsible": "J",
			"category": "ADMIN", "valid_from": "2026-01-01", "reason": "r"}`, p.DE), "")
		m := p.Problem(422, "controlling.statistical_key_figure_posting_invalid", "POST", path+"/postings", postingBody(2026, 5, value{uuid.New(), "1"},
			value{uuid.MustParse(waiting["id"].(string)), "1"}, value{p.c.Sales, "0.000"}, value{p.c.Sales, "1"}), "")
		wantList(t, "field errors", fieldErrors(m), []string{"lines[0].cost_center_id unknown", "lines[1].cost_center_id not_active", "lines[2].value zero",
			"lines[3].cost_center_id duplicate"})
		us, _ := p.Must(201, "POST", "/v1/statistical-key-figures", keyFigureBody(p.US, "EMPL", "EA"), "")
		m = p.Problem(422, "controlling.statistical_key_figure_posting_invalid", "POST", fmt.Sprint("/v1/statistical-key-figures/", us["id"], "/postings"),
			postingBody(2026, 5, value{p.c.Sales, "1"}), "")
		want(t, "another company's cost center", reason(t, m), "lines[0].cost_center_id not_in_company")
		m = p.Problem(422, "controlling.period_not_regular", "POST", path+"/postings", postingBody(2026, 13, value{p.c.Sales, "1"}), "")
		want(t, "a special period", reason(t, m), "fiscal_period not_regular")
		p.Problem(422, "ledger.no_fiscal_period", "POST", path+"/postings", postingBody(1999, 5, value{p.c.Sales, "1"}), "")
		p.Problem(400, "request.invalid_body", "POST", path+"/postings", postingBody(2026, 5, value{p.c.Sales, "1.0000001"}), "")
		p.Problem(400, "request.invalid_body", "POST", path+"/postings", postingBody(2026, 5), "")
		p.Problem(404, "request.not_found", "POST", "/v1/statistical-key-figures/"+uuid.NewString()+"/postings", postingBody(2026, 5, value{p.c.Sales, "1"}), "")
		p.Problem(404, "request.not_found", "GET", "/v1/statistical-key-figures/"+uuid.NewString()+"/values?fiscal_year=2026&fiscal_period=5", "", "")
	})
	t.Run("a period's values, summed per cost center, with the total of every page", func(t *testing.T) {
		values := func(t *testing.T, query string) string {
			t.Helper()
			page, _ := p.in(t).Must(200, "GET", path+"/values?fiscal_year=2026&"+query, "", "")
			want(t, "the page's key figure", fmt.Sprint(page["statistical_key_figure_id"], " ", page["statistical_key_figure_code"], " ", page["unit"], " ",
				page["legal_entity_id"], " ", page["fiscal_year"]), fmt.Sprint(figure, " EMPL EA ", p.DE, " 2026"))
			return valuesLine(t, page)
		}
		// Ordered by the center's id: the fixture's centers in the order created.
		want(t, "May", values(t, "fiscal_period=5"), "1000 2, 2000 3, 3000 1.000001 = 6.000001")
		want(t, "June", values(t, "fiscal_period=6"), "2000 7 = 7")
		want(t, "a period without postings", values(t, "fiscal_period=7"), " = 0")
		want(t, "one center", values(t, "fiscal_period=5&cost_center_id="+p.c.Sales.String()), "2000 3 = 3")
		want(t, "a first page", values(t, "fiscal_period=5&page_size=2"), "1000 2, 2000 3 = 6.000001 next")
		page, _ := p.Must(200, "GET", path+"/values?fiscal_year=2026&fiscal_period=5&page_size=2", "", "")
		want(t, "the second page", values(t, "fiscal_period=5&page_size=2&page_token="+page["next_page_token"].(string)), "3000 1.000001 = 6.000001")
		p.Problem(400, "request.invalid_page_token", "GET", path+"/values?fiscal_year=2026&fiscal_period=6&page_size=2&page_token="+
			page["next_page_token"].(string), "", "")
	})
	var published []string
	for _, ev := range p.events(t, "statistical_key_figure_posted") {
		var ls []string
		for _, l := range ev["lines"].([]any) {
			m := l.(map[string]any)
			ls = append(ls, fmt.Sprint(m["cost_center_code"], " ", m["value"]))
		}
		published = append(published, fmt.Sprint(ev["code"], " ", ev["unit"], " ", ev["fiscal_year"], "/", ev["fiscal_period"], ": ", strings.Join(ls, ", ")))
	}
	wantList(t, "published", published, []string{"EMPL EA 2026/5: 2000 3.5, 1000 2", "EMPL EA 2026/5: 2000 -0.5, 3000 1.000001", "EMPL EA 2026/6: 2000 7"})
	want(t, "the event's posting", p.events(t, "statistical_key_figure_posted")[0]["posting_id"], posted["id"])
	var audited string
	pgtest.Scan(t, `SELECT string_agg((detail->>'fiscal_period') || ' ' || jsonb_array_length(detail->'lines'), '; ' ORDER BY seq) FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'controlling.statistical_key_figure.post'`, []any{p.Tenant.ID}, &audited)
	want(t, "audited", audited, "5 2; 5 2; 6 1")
}

// keyFigureCycleBody is a new cycle of the company whose shares come from
// the key figure.
func keyFigureCycleBody(le uuid.UUID, code, kind, figure string, sender uuid.UUID, receivers ...uuid.UUID) string {
	rs := make([]string, len(receivers))
	for i, r := range receivers {
		rs[i] = fmt.Sprintf(`{"cost_center_id": %q}`, r)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "Cycle %s", "kind": %q, "share_basis": "statistical_key_figure",
		"statistical_key_figure_id": %q, "sender_cost_center_id": %q, "receivers": [%s]}`, le, code, code, kind, figure, sender, strings.Join(rs, ", "))
}

func TestKeyFigureCycles(t *testing.T) {
	p := setup(t)
	figure := p.keyFigure(t, "EMPL")
	fourth := p.center(t, "4000", "2026-01-01")
	created, _ := p.Must(201, "POST", "/v1/allocation-cycles", keyFigureCycleBody(p.DE, "BYEMPL", "distribution", figure, p.c.Admin, p.c.Sales, fourth), "")
	want(t, "the new cycle", fmt.Sprint(cycleLine(created), " ", created["share_basis"], " ", created["statistical_key_figure_id"], " ",
		created["statistical_key_figure_code"]), fmt.Sprint("BYEMPL distribution 1000 -> 2000 <nil>, 4000 <nil> statistical_key_figure ", figure, " EMPL"))
	read, _ := p.Must(200, "GET", fmt.Sprint("/v1/allocation-cycles/", created["id"]), "", "")
	want(t, "the cycle read", fmt.Sprint(read["share_basis"], " ", read["statistical_key_figure_code"]), "statistical_key_figure EMPL")
	fixed := p.cycle(t, "FIXED", share{p.c.Sales, "100"})
	read, _ = p.Must(200, "GET", "/v1/allocation-cycles/"+fixed, "", "")
	want(t, "a cycle by fixed percentages", fmt.Sprint(read["share_basis"], " ", read["statistical_key_figure_id"]), "fixed_percentages <nil>")
	t.Run("refusals of a create", func(t *testing.T) {
		other, _ := p.Must(201, "POST", "/v1/statistical-key-figures", keyFigureBody(p.US, "EMPL", "EA"), "")
		body := func(basis, figure string, pct string) string {
			b := fmt.Sprintf(`{"legal_entity_id": %q, "code": "X", "name": "X", "kind": "distribution", "sender_cost_center_id": %q, "receivers": [{"cost_center_id": %q%s}]`,
				p.DE, p.c.Admin, p.c.Sales, pct)
			if basis != "" {
				b += fmt.Sprintf(`, "share_basis": %q`, basis)
			}
			if figure != "" {
				b += fmt.Sprintf(`, "statistical_key_figure_id": %q`, figure)
			}
			return b + "}"
		}
		const hundred = `, "percentage": "100"`
		for name, c := range map[string]struct {
			body string
			want []string
		}{
			"a basis that is none":                {body("actual_values", "", hundred), []string{"share_basis unsupported"}},
			"a key figure basis without a figure": {body("statistical_key_figure", "", ""), []string{"statistical_key_figure_id required"}},
			"a key figure that is none":           {body("statistical_key_figure", uuid.NewString(), ""), []string{"statistical_key_figure_id unknown"}},
			"another company's key figure":        {body("statistical_key_figure", other["id"].(string), ""), []string{"statistical_key_figure_id unknown"}},
			"fixed percentages with a figure":     {body("fixed_percentages", figure, hundred), []string{"statistical_key_figure_id not_allowed"}},
			"a percentage by key figure":          {body("statistical_key_figure", figure, hundred), []string{"receivers[0].percentage not_allowed"}},
			"no percentage on fixed percentages":  {body("", "", ""), []string{"receivers[0].percentage required"}},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.allocation_cycle_invalid", "POST", "/v1/allocation-cycles", c.body, "")
				wantList(t, "field errors", fieldErrors(m), c.want)
			})
		}
	})
	// .v2 for every cycle, .v1 beside it for the cycle by fixed percentages alone.
	v2 := apitest.Published(t, p.Tenant, outbox.Event{Context: "controlling", Name: "allocation_cycle_changed", Version: 2})
	var published []string
	for _, ev := range v2 {
		published = append(published, fmt.Sprint(cycleLine(ev), " ", ev["share_basis"], " ", ev["statistical_key_figure_code"]))
	}
	wantList(t, "published as v2", published, []string{"BYEMPL distribution 1000 -> 2000 <nil>, 4000 <nil> statistical_key_figure EMPL",
		"FIXED distribution 1000 -> 2000 100.00 fixed_percentages <nil>"})
	want(t, "the v2 event's key figure", v2[0]["statistical_key_figure_id"], any(figure))
	published = nil
	for _, ev := range p.events(t, "allocation_cycle_changed") {
		published = append(published, cycleLine(ev))
	}
	wantList(t, "published as v1", published, []string{"FIXED distribution 1000 -> 2000 100.00"})
	var audited string
	pgtest.Scan(t, `SELECT detail->>'share_basis' || ' ' || (detail->>'statistical_key_figure_code') FROM audit.audit_event WHERE tenant_id = $1
		AND action = 'controlling.allocation_cycle.create' ORDER BY seq LIMIT 1`, []any{p.Tenant.ID}, &audited)
	want(t, "audited", audited, "statistical_key_figure EMPL")
}

func TestKeyFigureRun(t *testing.T) {
	p := setup(t)
	figure := p.keyFigure(t, "EMPL")
	fourth, fifth := p.center(t, "4000", "2026-01-01"), p.center(t, "5000", "2026-01-01")
	c, _ := p.Must(201, "POST", "/v1/allocation-cycles", keyFigureCycleBody(p.DE, "BYEMPL", "distribution", figure, p.c.Admin, p.c.Sales, fourth, fifth), "")
	path := fmt.Sprint("/v1/allocation-cycles/", c["id"])
	for _, m := range []int{4, 5, 6} {
		p.post(t, fmt.Sprintf("2026-%02d-10", m), on(p.office, 10001, &p.c.Admin), on(p.bank, -10001, nil))
	}
	// A sender without costs posts nothing, whatever the values.
	got, _ := p.Must(200, "POST", path+":run", settleBody(2026, 3), `"1"`)
	want(t, "a run without costs or values", ranLine(got), "nothing")
	t.Run("refused without values to share by", func(t *testing.T) {
		m := p.Problem(409, "controlling.allocation_key_figure_missing", "POST", path+":run", settleBody(2026, 4), `"1"`)
		want(t, "what it names", m["detail"], any("The receivers have no values of key figure EMPL in 2026/4, so the costs have nothing to be shared by; "+
			"post the period's values first."))
		// The sender's own value, and a value of another period, are none of the receivers'.
		p.postValues(t, figure, 4, value{p.c.Admin, "5"})
		p.postValues(t, figure, 3, value{p.c.Sales, "5"})
		p.Problem(409, "controlling.allocation_key_figure_missing", "POST", path+":run", settleBody(2026, 4), `"1"`)
		p.postValues(t, figure, 4, value{fourth, "2"}, value{fifth, "-1"})
		p.postValues(t, figure, 4, value{fourth, "-2"})
		m = p.Problem(409, "controlling.allocation_key_figure_negative", "POST", path+":run", settleBody(2026, 4), `"1"`)
		want(t, "which receiver", m["detail"], any("Receiver 5000's value of key figure EMPL in 2026/4 is -1: no share is taken from a negative value; "+
			"post a correction first."))
		wantList(t, "runs kept", runs(t, p.Client, ""), nil)
	})
	// May: 3 to 2000, 1 to 4000, none to 5000.
	p.postValues(t, figure, 5, value{p.c.Sales, "1.5"}, value{fourth, "0.5"})
	p.postValues(t, figure, 5, value{p.c.Sales, "1.5"}, value{fourth, "0.5"})
	may, _ := p.Must(200, "POST", path+":run", settleBody(2026, 5), `"1"`)
	want(t, "run for May", ranLine(may), "BYEMPL distribution 2026/5 on 2026-05-31 from 1000: 100.01 EUR on 1 account(s)")
	// 3/4 of 100.01 is 75.0075 and 1/4 is 25.0025: 75.01 and 25.00.
	wantList(t, "the entry's lines", p.entryLines(t, apitest.Object(t, may, "run")["journal_entry_id"]), []string{"520000 -100.01 on center 1000",
		"520000 75.01 on center 2000", "520000 25.00 on center 4000"})
	var values string
	pgtest.Scan(t, `SELECT string_agg((v->>'cost_center_code') || ' ' || (v->>'value'), ', ' ORDER BY n) FROM audit.audit_event,
		jsonb_array_elements(detail->'key_figure_values') WITH ORDINALITY AS x(v, n) WHERE tenant_id = $1 AND action = 'controlling.allocation_cycle.run'`,
		[]any{p.Tenant.ID}, &values)
	want(t, "the values the run used, in its audit record", values, "2000 3, 4000 1, 5000 0")
	t.Run("an assessment by key figure", func(t *testing.T) {
		a, _ := p.Must(201, "POST", "/v1/allocation-cycles", strings.Replace(keyFigureCycleBody(p.DE, "ASSESS", "assessment", figure, p.c.Admin, p.c.Sales, fourth),
			`"share_basis"`, fmt.Sprintf(`"assessment_account_id": %q, "share_basis"`, p.assessed), 1), "")
		p.postValues(t, figure, 6, value{p.c.Sales, "1"}, value{fourth, "2"})
		june, _ := p.Must(200, "POST", fmt.Sprint("/v1/allocation-cycles/", a["id"], ":run"), settleBody(2026, 6), `"1"`)
		// 1/3 of 100.01 is 33.3366… and 2/3 is 66.6733…: 33.34 and 66.67.
		wantList(t, "the entry's lines", p.entryLines(t, apitest.Object(t, june, "run")["journal_entry_id"]), []string{"695000 -100.01 on center 1000",
			"695000 33.34 on center 2000", "695000 66.67 on center 4000"})
	})
}

// TestKeyFigureAccess: key figures and their values are read, defined and
// posted only in the companies a person may act in, each by its
// permission; defining and posting are a person's.
func TestKeyFigureAccess(t *testing.T) {
	p := setup(t)
	figure := p.keyFigure(t, "EMPL")
	path := "/v1/statistical-key-figures/" + figure
	posting := postingBody(2026, 5, value{p.c.Sales, "1"})
	values := path + "/values?fiscal_year=2026&fiscal_period=5"
	body := keyFigureBody(p.DE, "OTHER", "EA")

	other := p.as(p.Reader(t, "Reader of 1000", p.US))
	other.Problem(404, "request.not_found", "GET", path, "", "")
	other.Problem(404, "request.not_found", "GET", values, "", "")
	other.Problem(404, "request.not_found", "POST", path+"/postings", posting, "")
	other.Problem(422, "request.reference_not_found", "POST", "/v1/statistical-key-figures", body, "")
	page, _ := other.Must(200, "GET", "/v1/statistical-key-figures", "", "")
	want(t, "key figures another company's reader sees", len(apitest.List(t, page, "items")), 0)

	agent := p.As(p.Tenant.Agent)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent.Must(200, "GET", path, "", "")
	agent.Must(200, "GET", values, "", "")
	agent.Must(200, "GET", "/v1/statistical-key-figures", "", "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/statistical-key-figures", body, "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+"/postings", posting, "")
	for _, c := range []struct{ permission, method, path, body string }{
		{"controlling.key_figure.write", "POST", "/v1/statistical-key-figures", body}, {"controlling.key_figure.write", "POST", path + "/postings", posting},
		{"controlling.key_figure.read", "GET", path, ""}, {"controlling.key_figure.read", "GET", values, ""},
		{"controlling.key_figure.read", "GET", "/v1/statistical-key-figures", ""},
	} {
		t.Run(c.method+" "+c.path[strings.LastIndex(c.path, "/"):]+" without "+c.permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, c.permission)
			p.Problem(403, "identity.permission_missing", c.method, c.path, c.body, "")
		})
	}
}

// TestKeyFigureDatabaseFailures calls every key figure handler outside a
// unit of work and with each of its database statements failing in turn.
func TestKeyFigureDatabaseFailures(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	figure := uuid.MustParse(p.keyFigure(t, "EMPL"))
	p.postValues(t, figure.String(), 5, value{p.c.Sales, "1"}, value{p.c.Locked, "2"})
	p.post(t, "2026-05-10", on(p.office, 1000, &p.c.Admin), on(p.bank, -1000, nil))
	c, _ := p.Must(201, "POST", "/v1/allocation-cycles", keyFigureCycleBody(p.DE, "BYEMPL", "distribution", figure.String(), p.c.Admin, p.c.Sales), "")
	cycle := uuid.MustParse(c["id"].(string))
	byCenter := gen.ListStatisticalKeyFigureValuesParams{ID: figure, FiscalYear: 2026, FiscalPeriod: 5, PageSize: gen.NewOptInt32(1)}
	calls := map[string]func(ctx context.Context) error{
		"create a key figure": func(ctx context.Context) error {
			_, err := h.CreateStatisticalKeyFigure(ctx, &gen.StatisticalKeyFigureCreate{LegalEntityID: p.DE, Code: "NEW", Name: "N", Unit: "EA"},
				gen.CreateStatisticalKeyFigureParams{})
			return err
		},
		"get a key figure": func(ctx context.Context) error {
			_, err := h.GetStatisticalKeyFigure(ctx, gen.GetStatisticalKeyFigureParams{ID: figure})
			return err
		},
		"list key figures": func(ctx context.Context) error {
			_, err := h.ListStatisticalKeyFigures(ctx, gen.ListStatisticalKeyFiguresParams{})
			return err
		},
		"post values": func(ctx context.Context) error {
			_, err := h.CreateStatisticalKeyFigurePosting(ctx, &gen.StatisticalKeyFigurePostingCreate{FiscalYear: 2026, FiscalPeriod: 5,
				Lines: []gen.StatisticalKeyFigurePostingCreateLinesItem{{CostCenterID: p.c.Sales, Value: "1"}}}, gen.CreateStatisticalKeyFigurePostingParams{ID: figure})
			return err
		},
		"list values, a page after the first": func(ctx context.Context) error {
			res, err := h.ListStatisticalKeyFigureValues(ctx, byCenter)
			if err != nil {
				return err
			}
			next := byCenter
			next.PageToken = gen.NewOptString(res.(*gen.StatisticalKeyFigureValuePage).NextPageToken.Or("none"))
			_, err = h.ListStatisticalKeyFigureValues(ctx, next)
			return err
		},
		"create a cycle by key figure": func(ctx context.Context) error {
			_, err := h.CreateAllocationCycle(ctx, &gen.AllocationCycleCreate{LegalEntityID: p.DE, Code: "MADE", Name: "M", Kind: "distribution",
				ShareBasis: gen.NewOptString(controlling.SharesKeyFigure), StatisticalKeyFigureID: gen.NewOptUUID(figure), SenderCostCenterID: p.c.Admin,
				Receivers: []gen.AllocationCycleCreateReceiversItem{{CostCenterID: p.c.Sales}}}, gen.CreateAllocationCycleParams{})
			return err
		},
		"run a cycle by key figure": func(ctx context.Context) error {
			_, err := h.RunAllocationCycle(ctx, &gen.AllocationCycleRun{FiscalYear: 2026, FiscalPeriod: 5}, gen.RunAllocationCycleParams{ID: cycle, IfMatch: `"1"`})
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
