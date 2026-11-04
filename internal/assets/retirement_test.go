package assets_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// retire posts a retirement of the asset on assetDay (posted on 15 June
// 2026 unless the body says otherwise) and returns it.
func (e assetEnv) retire(t *testing.T, asset any, body string) map[string]any {
	t.Helper()
	q, _ := e.clocked.Must(201, "POST", "/v1/asset-retirements", fmt.Sprintf(`{"asset_id": "%v", %s}`, asset, body), "")
	return q
}

// retired is what a retirement took, one ledger each: "ledger share
// catch-up cost accumulated residual book-value gain", then in group
// currency "catch-up cost accumulated gain".
func retired(q map[string]any) string {
	var out []string
	for _, l := range q["ledgers"].([]any) {
		l := l.(map[string]any)
		out = append(out, fmt.Sprint(l["ledger_code"], " ", l["share"], " ", amount(l["catch_up"]), " ", amount(l["cost"]), " ", amount(l["accumulated"]), " ",
			amount(l["residual"]), " ", amount(l["net_book_value"]), " ", amount(l["gain"]), " | ", amount(l["catch_up_gc"]), " ", amount(l["cost_gc"]), " ",
			amount(l["accumulated_gc"]), " ", amount(l["gain_gc"])))
	}
	return strings.Join(out, "; ")
}

// entryOf is a document's journal entry in the ledger (by code).
func entryOf(q map[string]any, ledger string) any {
	for _, en := range q["entries"].([]any) {
		if en := en.(map[string]any); en["ledger_code"] == ledger {
			return en["journal_entry_id"]
		}
	}
	return nil
}

// bookedThrough is the asset's booked-through period per ledger.
func bookedThrough(t *testing.T, asset any) string {
	t.Helper()
	var booked string
	pgtest.Scan(t, `SELECT string_agg(g.code || ' ' || coalesce(t.booked_through_year || '/' || t.booked_through_period, '-'), ', ' ORDER BY g.code)
		FROM assets.asset_terms t JOIN ledger.ledger g ON g.id = t.ledger_id WHERE t.asset_id = $1`, []any{asset}, &booked)
	return booked
}

// TestRetirementSaleAndScrapping: half of an asset whose June run posted
// in 0L is sold in June with a gain, the June depreciation posted back
// first (the catch-up through May less booked) in 0L and nothing in 2L,
// each ledger with an entry of its own and the gain on the asset's profit
// center; a repeat June run then depreciates the half left; the rest is
// scrapped with a loss, which retires the asset and clears its accounts
// in all currencies; it is published and audited, and a retired asset
// takes nothing more.
func TestRetirementSaleAndScrapping(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)

	sale := e.retire(t, mill, `"kind": "sale", "proceeds": {"amount": "700.00", "currency": "EUR"}, "share": {"percent": "50"}`)
	want(t, "the sale", fmt.Sprint(sale["kind"], " ", sale["status"], " ", sale["value_date"], " ", sale["posting_date"], " ", sale["fiscal_year"], "/",
		sale["period"], " ", sale["full"], " ", sale["share_percent"], " ", moneyOf(sale["proceeds"]), " ", moneyOf(sale["proceeds_gc"]), " ",
		sale["company_code"], " ", sale["asset_description"]), "sale posted 2026-06-15 2026-06-15 2026/6 false 50 700.00 EUR 760.87 USD 2000 Mill")
	want(t, "what it took", retired(sale), "0L 0.5 -10.00 600.00 0.00 0.00 600.00 100.00 | -10.87 652.18 0.00 108.69; "+
		"2L 0.5 0.00 600.00 0.00 0.00 600.00 100.00 | 0.00 652.18 0.00 108.69")
	want(t, "the 0L entry", entryLines(t, entryOf(sale, "0L")),
		"0L 620000 1000 PC1 -1000 -1087; 0L 160900 - PC1 1000 1087; 0L 160000 - PC1 -60000 -65218; 0L 169500 - PC1 70000 76087; 0L 490000 - PC1 -10000 -10869")
	want(t, "the 2L entry", entryLines(t, entryOf(sale, "2L")), "2L 160000 - PC1 -60000 -65218; 2L 169500 - PC1 70000 76087; 2L 490000 - PC1 -10000 -10869")
	want(t, "booked through", bookedThrough(t, mill), "0L 2026/6, 2L 2026/5")
	want(t, "the mill after the sale", e.asset(t, mill), "active 2026-06-10; 0L 600.00 EUR 652.17 USD 0.00 EUR 0.00 USD 600.00 EUR 652.17 USD; "+
		"2L 600.00 EUR 652.17 USD 0.00 EUR 0.00 USD 600.00 EUR 652.17 USD")

	_, again := e.start(t, e.l0, 6)
	want(t, "the repeat June run, the half left", runOf(e.carry(t, again)), "posted 1 1 0 5.00; 1 posted 1 1 0 5.00")

	scrap := e.retire(t, mill, `"kind": "scrapping", "text": "Scrapped"`)
	want(t, "the scrapping", fmt.Sprint(scrap["full"], " ", moneyOf(scrap["proceeds"]), " ", scrap["text"]), "true 0.00 EUR Scrapped")
	want(t, "what it took", retired(scrap), "0L 1 -5.00 600.00 0.00 0.00 600.00 -600.00 | -5.43 652.17 0.00 -652.17; "+
		"2L 1 0.00 600.00 0.00 0.00 600.00 -600.00 | 0.00 652.17 0.00 -652.17")
	want(t, "the 0L entry", entryLines(t, entryOf(scrap, "0L")),
		"0L 620000 1000 PC1 -500 -543; 0L 160900 - PC1 500 543; 0L 160000 - PC1 -60000 -65217; 0L 690000 1000 PC1 60000 65217")
	want(t, "the mill, retired", e.asset(t, mill), "retired 2026-06-10; 0L 0.00 EUR 0.00 USD 0.00 EUR 0.00 USD 0.00 EUR 0.00 USD; "+
		"2L 0.00 EUR 0.00 USD 0.00 EUR 0.00 USD 0.00 EUR 0.00 USD")
	want(t, "the cost account's balances", balances(t, e.Tenant.ID, e.Account(t, "2000", "160000")), "0L: 0 0 0; 2L: 0 0 0")
	want(t, "the accumulated depreciation account's balances", balances(t, e.Tenant.ID, e.Account(t, "2000", "160900")), "0L: 0 0 0")

	posted := slices.DeleteFunc(apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_posted"}),
		func(ev map[string]any) bool { return ev["document_kind"] != "retirement" })
	want(t, "AssetMovementPosted", fmt.Sprint(len(posted), " ", posted[0]["document_kind"], " ", posted[0]["asset_status"], " ", posted[1]["asset_status"], " ",
		posted[0]["ledgers"].([]any)[0].(map[string]any)["gain"]), "2 retirement active retired map[amount:100.00 currency:EUR]")
	changed := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_changed"})
	want(t, "AssetChanged", fmt.Sprint(changed[len(changed)-1]["change"], " ", changed[len(changed)-1]["status"]), "retired retired")
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'assets.retirement.post'`, []any{e.Tenant.ID}, &audited)
	want(t, "audited", audited, 2)

	e.clocked.Problem(422, "assets.asset_retired", "POST", "/v1/asset-retirements", fmt.Sprintf(`{"asset_id": %q, "kind": "scrapping"}`, mill), "")
	_, july := e.start(t, e.l0, 7)
	want(t, "July's run, the mill not selected", runOf(e.carry(t, july)), "posted 0 0 0 0.00")
}
