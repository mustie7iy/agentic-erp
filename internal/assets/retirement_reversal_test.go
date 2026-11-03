package assets_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// unretire reverses the retirement with its current ETag.
func (e assetEnv) unretire(t *testing.T, q map[string]any, body string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", fmt.Sprint("/v1/asset-retirements/", q["id"]), "", "")
	out, _ := e.Must(200, "POST", fmt.Sprint("/v1/asset-retirements/", q["id"], ":reverse"), body, tag)
	return out
}

// refuseRetirementReversal asks for the retirement's reversal with its
// current ETag and wants the problem.
func (e assetEnv) refuseRetirementReversal(t *testing.T, q map[string]any, body string, status int, code string) {
	t.Helper()
	_, tag := e.Must(200, "GET", fmt.Sprint("/v1/asset-retirements/", q["id"]), "", "")
	e.Problem(status, code, "POST", fmt.Sprint("/v1/asset-retirements/", q["id"], ":reverse"), body, tag)
}

// TestRetirementReversal: half of an asset scrapped in July, its June
// depreciation caught up in each ledger and the asset booked through
// June, is reversed on a later date: every ledger's reversal entry is
// dated that day, the movements are negated, booked_through returns to
// its value before the catch-up, so June's run posts June again; a
// reversal before the retirement and a second reversal are refused, and
// once anything was posted on the asset after a retirement (a run), so is
// its reversal; an acquisition is not reversed while a retirement posted
// after it stands.
func TestRetirementReversal(t *testing.T) {
	e := newRuns(t)
	mill, acquired := e.depreciable(t, "Mill")
	july := `"posting_date": "2026-07-15", "value_date": "2026-07-15"`
	q := e.retire(t, mill, `"kind": "scrapping", "share": {"percent": "50"}, `+july)
	want(t, "what it took", retired(q), "0L 0.5 10.00 600.00 -5.00 0.00 595.00 -595.00 | 10.87 652.18 -5.44 -646.74; "+
		"2L 0.5 20.00 600.00 -10.00 0.00 590.00 -590.00 | 21.74 652.18 -10.87 -641.31")
	want(t, "booked through", bookedThrough(t, mill), "0L 2026/6, 2L 2026/6")
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+acquired, "", "")
	e.clocked.Problem(409, "assets.later_movements", "POST", "/v1/asset-acquisitions/"+acquired+":reverse", `{"reason": "r"}`, tag)

	e.refuseRetirementReversal(t, q, `{"reason": "r", "posting_date": "2026-07-14"}`, 422, "assets.reversal_before_original")
	r := e.unretire(t, q, `{"reason": "Scrapped the wrong machine", "posting_date": "2026-07-20"}`)
	want(t, "the reversal", fmt.Sprint(r["status"], " ", r["reversal"]), "reversed map[posting_date:2026-07-20 reason:Scrapped the wrong machine]")
	var dates string
	pgtest.Scan(t, `SELECT string_agg(g.code || ' ' || j.posting_date, ', ' ORDER BY g.code) FROM ledger.journal_entry j JOIN ledger.ledger g ON g.id = j.ledger_id
		WHERE j.id IN (SELECT reversal_entry_id FROM assets.document_entry WHERE document_id = $1)`, []any{q["id"]}, &dates)
	want(t, "the reversal entries' dates", dates, "0L 2026-07-20, 2L 2026-07-20")
	for _, en := range r["entries"].([]any) {
		en := en.(map[string]any)
		want(t, "a reversal entry in "+en["ledger_code"].(string), en["reversal_journal_entry_id"] != nil && en["reversal_document_number"] != nil, true)
	}
	want(t, "booked through after the reversal", bookedThrough(t, mill), "0L -, 2L -")
	want(t, "the mill", e.asset(t, mill), "active 2026-06-10; 0L 1200.00 EUR 1304.35 USD 0.00 EUR 0.00 USD 1200.00 EUR 1304.35 USD; "+
		"2L 1200.00 EUR 1304.35 USD 0.00 EUR 0.00 USD 1200.00 EUR 1304.35 USD")
	reversed := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_reversed"})
	want(t, "AssetMovementReversed", fmt.Sprint(reversed[0]["document_kind"], " ", reversed[0]["asset_status"], " ",
		reversed[0]["ledgers"].([]any)[0].(map[string]any)["cost"]), "retirement active map[amount:600.00 currency:EUR]")
	e.refuseRetirementReversal(t, q, `{"reason": "r"}`, 409, "assets.retirement_not_posted")

	_, june := e.start(t, e.l0, 6)
	want(t, "June's run, the mill no longer booked through June", runOf(e.carry(t, june)), "posted 1 1 0 10.00; 1 posted 1 1 0 10.00")

	again := e.retire(t, mill, `"kind": "scrapping", "share": {"cost": {"amount": "300.00", "currency": "EUR"}}, `+july)
	want(t, "the second retirement's 0L share", retired(again)[:30], "0L 0.25 0.00 300.00 -2.50 0.00")
	_, jul := e.start(t, e.l0, 7)
	e.carry(t, jul)
	e.refuseRetirementReversal(t, again, `{"reason": "r"}`, 409, "assets.later_movements")
	e.Problem(404, "request.not_found", "GET", "/v1/asset-retirements/"+uuid.Must(uuid.NewV7()).String(), "", "")
}

// TestRetirementRefused: the problems a retirement meets.
func TestRetirementRefused(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	planned, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	cancelled, tag := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	e.Must(200, "POST", fmt.Sprint("/v1/assets/", cancelled["id"], ":cancel"), "", tag)
	late, _ := e.depreciable(t, "Late")
	entry := e.post(t, "2000", "2026-07-05", "EUR", nil, line{"169000", "100.00"}, line{"100000", "-100.00"})
	e.acquire(t, late, entry, 1, `, "posting_date": "2026-07-05"`)
	bare, _ := e.depreciable(t, "Bare")
	pgtest.Exec(t, `DELETE FROM assets.asset_terms WHERE asset_id = $1 AND ledger_id = $2`, bare, e.l2)
	body := func(asset any, rest string) string { return fmt.Sprintf(`{"asset_id": "%v", %s}`, asset, rest) }
	for name, c := range map[string]struct {
		status     int
		code, body string
	}{
		"a value date in the period before":   {422, "assets.value_date_outside_period", body(mill, `"kind": "scrapping", "value_date": "2026-05-31"`)},
		"a value date after the posting date": {422, "assets.value_date_outside_period", body(mill, `"kind": "scrapping", "value_date": "2026-06-16"`)},
		"a value date before capitalisation":  {422, "assets.value_date_before_capitalisation", body(mill, `"kind": "scrapping", "value_date": "2026-06-05"`)},
		"an acquisition valued later":         {409, "assets.later_value_date", body(late, `"kind": "scrapping", "posting_date": "2026-06-20"`)},
		"a cost above the asset's":            {422, "assets.share_exceeds", body(mill, `"kind": "scrapping", "share": {"cost": {"amount": "1200.01", "currency": "EUR"}}`)},
		"a planned asset":                     {422, "assets.asset_not_capitalised", body(planned["id"], `"kind": "scrapping"`)},
		"a cancelled asset":                   {422, "assets.asset_cancelled", body(cancelled["id"], `"kind": "scrapping"`)},
		"no terms in a ledger kept":           {422, "assets.ledger_terms_missing", body(bare, `"kind": "scrapping"`)},
		"an unknown asset":                    {404, "request.not_found", body(uuid.Must(uuid.NewV7()), `"kind": "scrapping"`)},
		"another kind":                        {422, "request.validation_failed", body(mill, `"kind": "donation"`)},
		"a sale without proceeds":             {422, "request.validation_failed", body(mill, `"kind": "sale"`)},
		"a scrapping with proceeds":           {422, "request.validation_failed", body(mill, `"kind": "scrapping", "proceeds": {"amount": "1.00", "currency": "EUR"}`)},
		"negative proceeds":                   {422, "request.validation_failed", body(mill, `"kind": "sale", "proceeds": {"amount": "-1.00", "currency": "EUR"}`)},
		"proceeds in another currency":        {422, "request.validation_failed", body(mill, `"kind": "sale", "proceeds": {"amount": "1.00", "currency": "USD"}`)},
		"proceeds of another precision":       {400, "request.invalid_amount", body(mill, `"kind": "sale", "proceeds": {"amount": "1.001", "currency": "EUR"}`)},
		"a percent and a cost":                {422, "request.validation_failed", body(mill, `"kind": "scrapping", "share": {"percent": "5", "cost": {"amount": "1.00", "currency": "EUR"}}`)},
		"an empty share":                      {422, "request.validation_failed", body(mill, `"kind": "scrapping", "share": {}`)},
		"a percent of zero":                   {422, "request.validation_failed", body(mill, `"kind": "scrapping", "share": {"percent": "0"}`)},
		"a percent above 100":                 {422, "request.validation_failed", body(mill, `"kind": "scrapping", "share": {"percent": "100.5"}`)},
		"a cost of zero":                      {422, "request.validation_failed", body(mill, `"kind": "scrapping", "share": {"cost": {"amount": "0.00", "currency": "EUR"}}`)},
		"a cost in another currency":          {422, "request.validation_failed", body(mill, `"kind": "scrapping", "share": {"cost": {"amount": "1.00", "currency": "USD"}}`)},
		"a cost of another precision":         {400, "request.invalid_amount", body(mill, `"kind": "scrapping", "share": {"cost": {"amount": "1.001", "currency": "EUR"}}`)},
	} {
		t.Run(name, func(t *testing.T) {
			e.clocked.In(t).Problem(c.status, c.code, "POST", "/v1/asset-retirements", c.body, "")
		})
	}

	// A run of a later period books an asset through it even when it
	// posts nothing for it (its residual value is all its cost in 2L).
	fixed, _ := e.depreciable(t, "Fixed")
	_, tag = e.Must(200, "GET", "/v1/assets/"+fixed, "", "")
	e.Must(200, "PATCH", "/v1/assets/"+fixed, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "residual_value": {"amount": "1200.00", "currency": "EUR"}}]}`, e.l2), tag)
	_, run := e.start(t, e.l2, 7)
	e.carry(t, run)
	var moved int
	pgtest.Scan(t, `SELECT count(*) FROM assets.movement WHERE asset_id = $1 AND kind = 'depreciation'`, []any{fixed}, &moved)
	want(t, "the run's movements of the asset", moved, 0)
	want(t, "booked through", bookedThrough(t, fixed), "0L -, 2L 2026/7")
	e.clocked.Problem(409, "assets.later_value_date", "POST", "/v1/asset-retirements", body(fixed, `"kind": "scrapping"`), "")
}

// TestRetirementOfZeroCost: an asset whose cost a credit memo brought to
// zero stays active, refuses a share, and is retired by a full
// retirement taking zero cost, which posts no entry and still writes its
// movements.
func TestRetirementOfZeroCost(t *testing.T) {
	e := newRuns(t)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	cost := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "100.00"}, line{"100000", "-100.00"})
	credit := e.post(t, "2000", "2026-06-11", "EUR", nil, line{"100000", "100.00"}, line{"169000", "-100.00"})
	e.acquire(t, a["id"], cost, 1, "")
	e.acquire(t, a["id"], credit, 2, "")
	want(t, "the asset at zero cost", e.asset(t, a["id"])[:6], "active")
	for _, share := range []string{`{"percent": "50"}`, `{"cost": {"amount": "1.00", "currency": "EUR"}}`} {
		e.clocked.Problem(422, "assets.share_of_zero_cost", "POST", "/v1/asset-retirements", fmt.Sprintf(`{"asset_id": "%v", "kind": "scrapping", "share": %s}`,
			a["id"], share), "")
	}
	q := e.retire(t, a["id"], `"kind": "scrapping", "share": {"percent": "100"}`)
	want(t, "the full retirement", fmt.Sprint(q["full"], " ", len(q["entries"].([]any)), " ", retired(q)), "true 0 "+
		"0L 1 0.00 0.00 0.00 0.00 0.00 0.00 | 0.00 0.00 0.00 0.00; 2L 1 0.00 0.00 0.00 0.00 0.00 0.00 | 0.00 0.00 0.00 0.00")
	var movements int
	pgtest.Scan(t, `SELECT count(*) FROM assets.movement WHERE document_id = $1 AND journal_entry_id IS NULL`, []any{q["id"]}, &movements)
	want(t, "its movements, without entries", movements, 4)
	want(t, "the asset", e.asset(t, a["id"])[:7], "retired")
	r := e.unretire(t, q, `{"reason": "r"}`)
	want(t, "the reversal", fmt.Sprint(r["status"], " ", e.asset(t, a["id"])[:6]), "reversed active")
}

// TestRetirementGroupAmounts: June's run posted at one rate is posted back
// by a retirement's catch-up at another, leaving the accumulated
// depreciation zero locally but not in group currency; a partial
// retirement takes all of that group amount, on a line without a local
// amount, so the account clears in group currency.
func TestRetirementGroupAmounts(t *testing.T) {
	e := newRuns(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-06-01', 0.80, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-06-20', 0.92, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	mill, _ := e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	q := e.retire(t, mill, `"kind": "sale", "proceeds": {"amount": "500.00", "currency": "EUR"}, "share": {"percent": "50"}`)
	want(t, "what it took in 0L", retired(q)[:66], "0L 0.5 -10.00 600.00 0.00 0.00 600.00 -100.00 | -12.50 750.00 1.63")
	want(t, "the 0L entry", entryLines(t, entryOf(q, "0L")),
		"0L 620000 1000 PC1 -1000 -1250; 0L 160900 - PC1 1000 1250; 0L 160900 - PC1 0 -163; 0L 160000 - PC1 -60000 -75000; 0L 169500 - PC1 50000 62500; "+
			"0L 690000 1000 PC1 10000 12663")
	want(t, "the accumulated depreciation account", balances(t, e.Tenant.ID, e.Account(t, "2000", "160900")), "0L: 0 0 0")
}

// TestRetirementsAtOnce: two full retirements of each of two assets at
// once serialise on each asset's lock and take the ledgers' number ranges
// in the same order (the leading ledger first, then by code), so none
// deadlocks: one retirement of each asset posts, the other finds it
// retired.
func TestRetirementsAtOnce(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i, asset := range []string{mill, press, mill, press} {
		wg.Go(func() {
			codes[i], _, _ = e.clocked.Do("POST", "/v1/asset-retirements", fmt.Sprintf(`{"asset_id": %q, "kind": "scrapping"}`, asset), "")
		})
	}
	wg.Wait()
	for i := range 2 {
		got := fmt.Sprint(min(codes[i], codes[i+2]), " ", max(codes[i], codes[i+2]))
		want(t, fmt.Sprint("the retirements of asset ", i+1), got, "201 422")
	}
	for _, a := range []string{mill, press} {
		want(t, "the asset", e.asset(t, a)[:7], "retired")
	}
}

// TestRetirementDatabaseFailures calls each retirement handler with each
// of its database statements failing in turn: a sale of a share after a
// run (with its catch-up), a full scrapping, a reversal, a read and the
// list.
func TestRetirementDatabaseFailures(t *testing.T) {
	e := newRuns(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages, Now: func() time.Time { return assetDay }})
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	q := e.retire(t, press, `"kind": "scrapping", "share": {"percent": "10"}`)
	id := uuid.MustParse(q["id"].(string))
	asset := uuid.MustParse(mill)
	for name, fn := range map[string]func(ctx context.Context) error{
		"a sale of a share": func(ctx context.Context) error {
			_, err := h.CreateAssetRetirement(ctx, &gen.AssetRetirementCreate{AssetID: asset, Kind: "sale",
				Proceeds: gen.NewOptMoney(gen.Money{Amount: "100.00", Currency: "EUR"}),
				Share:    gen.NewOptAssetRetirementShare(gen.AssetRetirementShare{Cost: gen.NewOptMoney(gen.Money{Amount: "100.00", Currency: "EUR"})})},
				gen.CreateAssetRetirementParams{})
			return err
		},
		"a full scrapping": func(ctx context.Context) error {
			_, err := h.CreateAssetRetirement(ctx, &gen.AssetRetirementCreate{AssetID: asset, Kind: "scrapping"}, gen.CreateAssetRetirementParams{})
			return err
		},
		"a reversal": func(ctx context.Context) error {
			_, err := h.ReverseAssetRetirement(ctx, &gen.AssetRetirementReverse{Reason: "r"}, gen.ReverseAssetRetirementParams{ID: id, IfMatch: "*"})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetAssetRetirement(ctx, gen.GetAssetRetirementParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListAssetRetirements(ctx, gen.ListAssetRetirementsParams{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
				t.Errorf("outside a unit of work: got %v, want it refused", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
}

// TestListAssetRetirements: the retirements of the caller's companies by
// posting date, filtered by company, asset, kind, posting dates and status,
// paged.
func TestListAssetRetirements(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	sale := e.retire(t, mill, `"kind": "sale", "proceeds": {"amount": "10.00", "currency": "EUR"}, "share": {"percent": "25"}`)
	scrap := e.retire(t, press, `"kind": "scrapping"`)
	reversed := e.retire(t, mill, `"kind": "scrapping", "share": {"percent": "10"}`)
	e.unretire(t, reversed, `{"reason": "r"}`)
	ids := func(query string) string {
		page, _ := e.Must(200, "GET", "/v1/asset-retirements"+query, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return strings.Join(out, " ")
	}
	for name, c := range map[string]struct{ query, want string }{
		"all":         {"", fmt.Sprint(sale["id"], " ", scrap["id"], " ", reversed["id"])},
		"by company":  {"?legal_entity_id=" + e.US.String(), ""},
		"by asset":    {"?asset_id=" + press, fmt.Sprint(scrap["id"])},
		"by kind":     {"?kind=sale", fmt.Sprint(sale["id"])},
		"by status":   {"?status=reversed", fmt.Sprint(reversed["id"])},
		"posted from": {"?posting_date_from=2026-06-16", ""},
		"posted to":   {"?posting_date_to=2026-06-15&order_by=created_at", fmt.Sprint(sale["id"], " ", scrap["id"], " ", reversed["id"])},
	} {
		t.Run(name, func(t *testing.T) { want(t, "the retirements", ids(c.query), c.want) })
	}
	byCreation, _ := e.Must(200, "GET", "/v1/asset-retirements?page_size=1&order_by=created_at", "", "")
	want(t, "a page by creation", byCreation["next_page_token"] != nil, true)
	first, _ := e.Must(200, "GET", "/v1/asset-retirements?page_size=2", "", "")
	next, _ := e.Must(200, "GET", "/v1/asset-retirements?page_size=2&page_token="+first["next_page_token"].(string), "", "")
	want(t, "the second page", next["items"].([]any)[0].(map[string]any)["id"], reversed["id"])
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/asset-retirements?order_by=kind", "", "")
}

// noGroupCurrency is a tenancy whose group currency cannot be read.
type noGroupCurrency struct{ assets.Companies }

func (noGroupCurrency) GroupCurrency(context.Context) (string, error) { return "", errRuns }

// TestRetirementReadsFail: a retirement whose group currency, company
// zone or schedule cannot be read fails rather than posting, and one
// posted in a year not set up is the ledger's problem.
func TestRetirementReadsFail(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	scrap := &gen.AssetRetirementCreate{AssetID: uuid.MustParse(mill), Kind: "scrapping"}
	for name, c := range map[string]struct {
		change func(h *assets.Endpoints)
		want   string
	}{
		"the group currency": {func(h *assets.Endpoints) { h.Companies = noGroupCurrency{h.Companies} }, errRuns.Error()},
		"the company's zone": {func(h *assets.Endpoints) { h.Settings = zoneLost{h.Settings} }, "Europe/Nowhere"},
		"the asset's class":  {func(h *assets.Endpoints) { h.Settings = classGone{h.Settings} }, "MACHINES"},
	} {
		t.Run(name, func(t *testing.T) {
			h := app.Assets(app.Config{})
			c.change(h)
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CreateAssetRetirement(ctx, scrap, gen.CreateAssetRetirementParams{})
				return err
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
	e.clocked.Problem(422, "ledger.no_fiscal_period", "POST", "/v1/asset-retirements",
		fmt.Sprintf(`{"asset_id": %q, "kind": "scrapping", "posting_date": "2031-06-15"}`, mill), "")
	_, tag := e.Must(200, "GET", "/v1/assets/"+mill, "", "")
	e.Must(200, "PATCH", "/v1/assets/"+mill, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 2, "declining_factor": "10"}]}`, e.l2), tag)
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1 WHERE id = $1`, mill)
	pgtest.Exec(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, cost, cost_gc, document_kind, document_id, created_by)
		SELECT tenant_id, gen_random_uuid(), id, last_seq, legal_entity_id, $2, class_code, 'acquisition', '2026-06-10', '2026-06-10', 2026, 6,
			2000000000000000000, 2000000000000000000, 'acquisition', gen_random_uuid(), $3
		FROM assets.asset WHERE id = $1`, mill, e.l2, e.Tenant.Human.ID)
	e.clocked.Problem(500, "platform.internal", "POST", "/v1/asset-retirements",
		fmt.Sprintf(`{"asset_id": %q, "kind": "scrapping", "posting_date": "2026-07-15"}`, mill), "")
}

// TestRetirementInTheFirstPeriod: a retirement in a fiscal year's first
// period catches up through the previous year's last and books the asset
// through it.
func TestRetirementInTheFirstPeriod(t *testing.T) {
	e := newRuns(t)
	entry := e.post(t, "2000", "2026-01-05", "EUR", nil, line{"169000", "1200.00"}, line{"100000", "-1200.00"})
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	e.acquire(t, a["id"], entry, 1, `, "posting_date": "2026-01-05"`)
	q := e.retire(t, a["id"], `"kind": "scrapping", "posting_date": "2026-01-20"`)
	want(t, "the retirement", fmt.Sprint(q["fiscal_year"], "/", q["period"], " ", q["full"]), "2026/1 true")
	want(t, "booked through", bookedThrough(t, a["id"]), "0L 2025/12, 2L 2025/12")
	stale := fmt.Sprintf(`"%v"`, q["version"].(float64)+1) // a version the retirement does not have
	e.Problem(412, "request.version_mismatch", "POST", fmt.Sprint("/v1/asset-retirements/", q["id"], ":reverse"), `{"reason": "r"}`, stale)
}
