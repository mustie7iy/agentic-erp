package controlling_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPlanBatches: a batch writes its lines; the same batch again, in any
// order, changes and publishes nothing; a new batch replaces the plan of
// the centers it names and leaves the others'; a late replay of an earlier
// batch does not undo a later one.
func TestPlanBatches(t *testing.T) {
	p := setup(t)
	cat := p.category(t, "PLAN")
	qty := `{"cost_center_id": %q, "account_id": %q, "fiscal_period": 2, "amount": {"amount": "200.00", "currency": "EUR"}, "quantity": {"value": "10.50", "unit": "H"}}`
	b1 := []string{line(p.c.Admin, p.office, 1, "100.00"), fmt.Sprintf(qty, p.c.Admin, p.office), line(p.c.Sales, p.office, 1, "50.00"),
		pcLine(p.c.Profit, p.revenue, 1, "-300.00")}
	first := p.batch(t, cat, "b1", b1...)
	want(t, "first batch's lines", first["line_count"], any(float64(4)))
	want(t, "first batch's replaced lines", first["replaced_count"], any(float64(0)))
	want(t, "first batch replayed", first["replayed"], any(false))
	all := []string{"1000/520000/1 100.00", "1000/520000/2 200.00 10.5 H", "2000/520000/1 50.00", "PC1/400000/1 -300.00"}
	wantList(t, "plan after the first batch", sorted(p.lines(t, "")), all)

	again := p.batch(t, cat, "b1", b1[3], b1[2], b1[1], b1[0])
	want(t, "the same batch again", again["replayed"], any(true))
	want(t, "the same batch's id", again["id"], first["id"])
	wantList(t, "plan after the same batch", sorted(p.lines(t, "")), all)
	p.Problem(409, "controlling.plan_batch_reused", "POST", "/v1/plan-batches", p.batchBody(cat, "b1", b1[0]), "")

	second := p.batch(t, cat, "b2", line(p.c.Admin, p.office, 3, "400.00"))
	want(t, "second batch's replaced lines", second["replaced_count"], any(float64(2)))
	later := []string{"1000/520000/3 400.00", "2000/520000/1 50.00", "PC1/400000/1 -300.00"}
	wantList(t, "plan after the second batch", sorted(p.lines(t, "")), later)
	want(t, "the first batch late", p.batch(t, cat, "b1", b1...)["replayed"], any(true))
	wantList(t, "plan after the first batch late", sorted(p.lines(t, "")), later)
	third := p.batch(t, cat, "b3", pcLine(p.c.Profit, p.revenue, 2, "-10.00"))
	want(t, "a profit center's batch replaces its plan", third["replaced_count"], any(float64(1)))

	for name, c := range map[string]struct {
		query string
		want  []string
	}{
		"the first batch's lines still current": {"&plan_batch_id=" + first["id"].(string), []string{"2000/520000/1 50.00"}},
		"the cost center":                       {"&cost_center_id=" + p.c.Admin.String(), []string{"1000/520000/3 400.00"}},
		"the profit center":                     {"&profit_center_id=" + p.c.Profit.String(), []string{"PC1/400000/2 -10.00"}},
		"the account":                           {"&account_id=" + p.revenue.String(), []string{"PC1/400000/2 -10.00"}},
		"the category":                          {"&plan_category_id=" + cat, []string{"1000/520000/3 400.00", "2000/520000/1 50.00", "PC1/400000/2 -10.00"}},
		"another category":                      {"&plan_category_id=" + uuid.NewString(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			wantList(t, "lines", sorted(p.in(t).lines(t, c.query)), c.want)
		})
	}
	page := p.lines(t, "&page_size=2")
	want(t, "a page of two and its token", len(page), 3)
	rest := p.lines(t, "&page_size=2&page_token="+strings.TrimPrefix(page[2], "next:"))
	wantList(t, "both pages", sorted(append(page[:2], rest...)), []string{"1000/520000/3 400.00", "2000/520000/1 50.00", "PC1/400000/2 -10.00"})
	p.Problem(400, "request.invalid_order_by", "GET", "/v1/plan-lines?legal_entity_id="+p.DE.String()+"&fiscal_year=2026&order_by=amount", "", "")
	want(t, "lines of a company the reader cannot act in", len(p.as(p.Reader(t, "outsider")).lines(t, "")), 0)

	var applied []string
	for _, ev := range p.events(t, "plan_batch_applied") {
		applied = append(applied, fmt.Sprint(ev["batch_key"], " ", ev["line_count"], " ", ev["replaced_count"], " ", len(ev["cost_center_ids"].([]any)),
			" ", len(ev["profit_center_ids"].([]any))))
	}
	wantList(t, "events, once per batch applied", applied, []string{"b1 4 0 2 1", "b2 1 2 1 0", "b3 1 1 0 1"})
	want(t, "audit", p.audited(t), "controlling.plan_category.create controlling.plan_batch.apply controlling.plan_batch.apply controlling.plan_batch.apply")
}

// TestLockedPlanBatches: a locked category refuses a new batch, and
// answers one it already applied as it was.
func TestLockedPlanBatches(t *testing.T) {
	p := setup(t)
	cat := p.category(t, "PLAN")
	p.batch(t, cat, "b1", line(p.c.Admin, p.office, 1, "100.00"))
	p.Must(200, "POST", "/v1/plan-categories/"+cat+":lock", "", `"1"`)
	p.Problem(409, "controlling.plan_category_locked", "POST", "/v1/plan-batches", p.batchBody(cat, "b2", line(p.c.Admin, p.office, 1, "1.00")), "")
	want(t, "the applied batch again", p.batch(t, cat, "b1", line(p.c.Admin, p.office, 1, "100.00"))["replayed"], any(true))
	wantList(t, "plan", p.lines(t, ""), []string{"1000/520000/1 100.00"})
}

// TestPlanBatchRefusals: every wrong line is named, and nothing is
// written.
func TestPlanBatchRefusals(t *testing.T) {
	p := setup(t)
	cat := p.category(t, "PLAN")
	ok := line(p.c.Admin, p.office, 1, "1.00")
	withQty := func(value, unit string) string {
		return fmt.Sprintf(`{"cost_center_id": %q, "account_id": %q, "fiscal_period": 1, "amount": {"amount": "1.00", "currency": "EUR"}, "quantity": {"value": %q, "unit": %q}}`,
			p.c.Admin, p.office, value, unit)
	}
	for name, c := range map[string]struct {
		body string
		want string
	}{
		"both centers": {p.batchBody(cat, "k", fmt.Sprintf(`{"cost_center_id": %q, "profit_center_id": %q, "account_id": %q, "fiscal_period": 1,
			"amount": {"amount": "1.00", "currency": "EUR"}}`, p.c.Admin, p.c.Profit, p.office)), "lines[0].cost_center_id"},
		"no center": {p.batchBody(cat, "k", fmt.Sprintf(`{"account_id": %q, "fiscal_period": 1, "amount": {"amount": "1.00", "currency": "EUR"}}`, p.office)),
			"lines[0].cost_center_id"},
		"another currency":                {p.batchBody(cat, "k", strings.Replace(ok, "EUR", "USD", 1)), "lines[0].amount.currency"},
		"too many decimals":               {p.batchBody(cat, "k", line(p.c.Admin, p.office, 1, "1.005")), "lines[0].amount.amount"},
		"a quantity that is not positive": {p.batchBody(cat, "k", withQty("0", "H")), "lines[0].quantity.value"},
		"an unknown unit":                 {p.batchBody(cat, "k", withQty("1", "XX")), "lines[0].quantity.unit"},
		"a line twice":                    {p.batchBody(cat, "k", ok, line(p.c.Admin, p.office, 1, "2.00")), "lines[1].account_id"},
		"an unknown cost center":          {p.batchBody(cat, "k", line(uuid.New(), p.office, 1, "1.00")), "lines[0].cost_center_id"},
		"an unknown profit center":        {p.batchBody(cat, "k", pcLine(uuid.New(), p.revenue, 1, "1.00")), "lines[0].profit_center_id"},
		"a balance sheet account":         {p.batchBody(cat, "k", line(p.c.Admin, p.bank, 1, "1.00")), "lines[0].account_id"},
		"a year not set up": {strings.Replace(p.batchBody(cat, "k", ok), `"fiscal_year": 2026`, `"fiscal_year": 2030`, 1),
			"lines[0].fiscal_period"},
	} {
		t.Run(name, func(t *testing.T) {
			got := p.in(t).Problem(422, "controlling.plan_batch_invalid", "POST", "/v1/plan-batches", c.body, "")
			var fields []string
			for _, e := range got["errors"].([]any) {
				fields = append(fields, e.(map[string]any)["field"].(string))
			}
			wantList(t, "fields", fields, []string{c.want})
		})
	}
	t.Run("at most a hundred errors", func(t *testing.T) {
		lines := make([]string, 101)
		for i := range lines {
			lines[i] = line(uuid.New(), p.office, 1, "1.00")
		}
		got := p.in(t).Problem(422, "controlling.plan_batch_invalid", "POST", "/v1/plan-batches", p.batchBody(cat, "k", lines...), "")
		want(t, "errors", len(got["errors"].([]any)), 100)
	})
	p.Problem(422, "request.reference_not_found", "POST", "/v1/plan-batches", p.batchBody(uuid.NewString(), "k", ok), "")
	p.Problem(422, "request.reference_not_found", "POST", "/v1/plan-batches", strings.Replace(p.batchBody(cat, "k", ok), p.DE.String(), uuid.NewString(), 1), "")
	want(t, "lines after the refusals", len(p.lines(t, "")), 0)
	want(t, "events after the refusals", len(p.events(t, "plan_batch_applied")), 0)
}

func sorted(rows []string) []string {
	out := slices.Clone(rows)
	slices.Sort(out)
	return out
}
