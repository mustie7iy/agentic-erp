package assets_test

import (
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withNextYear is withLedgers with company 2000's number ranges, and so
// its open periods, in 2027 too: a second fiscal year whose sheet opens
// with 2026's closing values.
func withNextYear(doc *config.Config) {
	withLedgers(doc)
	for _, dt := range append(slices.Sorted(maps.Keys(fixture.DocumentTypes)), config.AssetRanges...) {
		doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: dt, FiscalYear: 2027,
			Prefix: fixture.DocumentTypes[dt], First: 1000000, Last: 1999999})
	}
}

// sheetColumns are the history sheet's columns in the order sheetLine
// shows them.
var sheetColumns = []string{"opening_cost", "acquisitions", "retirements", "transfers", "closing_cost", "opening_accumulated", "depreciation",
	"retirements_accumulated", "transfers_accumulated", "closing_accumulated", "opening_net_book_value", "closing_net_book_value"}

// sheetLine is a row's (or the totals') columns, space separated.
func sheetLine(v map[string]any) string {
	out := make([]string, len(sheetColumns))
	for i, c := range sheetColumns {
		out[i] = amount(v[c])
	}
	return strings.Join(out, " ")
}

// sheetRows are the sheet's rows: "class: columns", or by asset
// "number class: columns".
func sheetRows(s map[string]any) []string {
	var out []string
	for _, it := range s["items"].([]any) {
		r := it.(map[string]any)
		key := fmt.Sprint(r["class_code"])
		if n, ok := r["asset_number"]; ok {
			key = fmt.Sprint(n, " ", key)
		}
		out = append(out, key+": "+sheetLine(r))
	}
	return out
}

// sheet reads the history sheet of company 2000 with the query.
func (e assetEnv) sheet(t *testing.T, query string) map[string]any {
	t.Helper()
	s, _ := e.Must(200, "GET", "/v1/asset-history-sheet?legal_entity_id="+e.DE.String()+query, "", "")
	return s
}

// sheetScene is company 2000's assets through every kind of movement in
// both ledgers over two fiscal years:
//   - 2026: the mill, press, lathe and saw acquired (1,200.00 each, June);
//     June's runs in both ledgers; in July half the press sold and the
//     rest of it scrapped, a quarter of the mill moved to a plot of class
//     LAND (a reclassification), each taking its share of the June
//     depreciation, and the mill moved to a cost center of another profit
//     center (a center transfer); in June the lathe's acquisition
//     reversed, the saw scrapped (its catch-up posting June back) and the
//     scrapping reversed, then the saw moved to the other cost center and
//     that reversed;
//   - 2027: January's runs in both ledgers (July 2026 to January 2027
//     caught up) and a second acquisition of the saw (600.00, February).
type sheetScene struct {
	assetEnv
	machines, land []string
}

func newSheetScene(t *testing.T) sheetScene {
	t.Helper()
	e := newRunsWith(t, withNextYear)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	lathe, lathed := e.depreciable(t, "Lathe")
	saw, _ := e.depreciable(t, "Saw")
	plot := e.land(t)
	for _, l := range []uuid.UUID{e.l0, e.l2} {
		_, run := e.start(t, l, 6)
		e.carry(t, run)
	}
	e.retire(t, press, `"kind": "sale", "proceeds": {"amount": "700.00", "currency": "EUR"}, "share": {"percent": "50"}, "posting_date": "2026-07-15"`)
	e.retire(t, press, `"kind": "scrapping", "posting_date": "2026-07-15"`)
	e.transfer(t, mill, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "25"}, "posting_date": "2026-07-15"`, plot))
	other := e.otherCenter(t)
	e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-07-20"`, other))
	e.reverse(t, map[string]any{"id": lathed}, "")
	e.unretire(t, e.retire(t, saw, `"kind": "scrapping"`), `{"reason": "Scrapped in error"}`)
	e.untransfer(t, e.transfer(t, saw, fmt.Sprintf(`"kind": "center", "cost_center_id": %q`, other)), `{"reason": "Moved in error"}`)
	for _, l := range []uuid.UUID{e.l0, e.l2} {
		_, run := e.startIn(t, l, 2027, 1)
		e.carry(t, run)
	}
	entry := e.post(t, "2000", "2027-02-10", "EUR", nil, line{"169000", "600.00"}, line{"100000", "-600.00"})
	e.acquire(t, saw, entry, 1, `, "posting_date": "2027-02-10"`)
	return sheetScene{assetEnv: e, machines: []string{mill, press, lathe, saw}, land: []string{plot}}
}

// startIn starts a run of company 2000 in the ledger, year and period.
func (e assetEnv) startIn(t *testing.T, ledger uuid.UUID, year, period int) (map[string]any, uuid.UUID) {
	t.Helper()
	op, _ := e.Must(202, "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": %d, "period": %d}`,
		e.DE, ledger, year, period), "")
	return op, uuid.MustParse(strings.TrimPrefix(op["resource"].(string), "/v1/depreciation-runs/"))
}

// cents is an amount's minor units.
func cents(t *testing.T, a string) int64 {
	t.Helper()
	r, ok := new(big.Rat).SetString(a)
	if !ok {
		t.Fatalf("amount %q", a)
	}
	return new(big.Rat).Mul(r, big.NewRat(100, 1)).Num().Int64()
}

// decimal shows minor units as an amount with two decimals.
func decimal(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// expected computes the sheet line of assets in the ledger and year from
// the movements they list: the subledger read by another path than the
// year totals, each movement in its column by kind.
func (e assetEnv) expected(t *testing.T, ledger uuid.UUID, year int, assets ...string) string {
	t.Helper()
	var openCost, acq, ret, tr, openAcc, dep, retAcc, trAcc int64
	for _, a := range assets {
		page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/assets/%s/movements?ledger_id=%s&page_size=500", a, ledger), "", "")
		for _, it := range page["items"].([]any) {
			m := it.(map[string]any)
			y, cost, acc := int(m["fiscal_year"].(float64)), cents(t, amount(m["cost"])), cents(t, amount(m["accumulated"]))
			switch {
			case y > year:
			case y < year:
				openCost, openAcc = openCost+cost, openAcc+acc
			case m["kind"] == "acquisition":
				acq += cost
			case m["kind"] == "retirement":
				ret, retAcc = ret+cost, retAcc+acc
			case m["kind"] == "depreciation":
				dep += acc
			default: // transfer_in, transfer_out
				tr, trAcc = tr+cost, trAcc+acc
			}
		}
	}
	closeCost, closeAcc := openCost+acq+ret+tr, openAcc+dep+retAcc+trAcc
	out := make([]string, 0, len(sheetColumns))
	for _, c := range []int64{openCost, acq, ret, tr, closeCost, openAcc, dep, retAcc, trAcc, closeAcc, openCost + openAcc, closeCost + closeAcc} {
		out = append(out, decimal(c))
	}
	return strings.Join(out, " ")
}

// TestAssetHistorySheet: the sheet of each fiscal year and ledger by class
// matches the movements of every kind by column (acquisitions and their
// reversal, runs and catch-ups, retirements and their reversal, asset and
// center transfers and a reversal), 2027 opening with 2026's closing
// values; the year totals equal the movements' sums, per asset and per
// class.
func TestAssetHistorySheet(t *testing.T) {
	s := newSheetScene(t)
	all := append(slices.Clone(s.machines), s.land...)
	for _, c := range []struct {
		ledger uuid.UUID
		year   int
	}{{s.l0, 2026}, {s.l0, 2027}, {s.l2, 2026}, {s.l2, 2027}} {
		t.Run(fmt.Sprint(c.ledger, " ", c.year), func(t *testing.T) {
			sheet := s.sheet(t, fmt.Sprintf("&ledger_id=%s&fiscal_year=%d", c.ledger, c.year))
			want(t, "the rows", strings.Join(sheetRows(sheet), "; "), "LAND: "+s.expected(t, c.ledger, c.year, s.land...)+"; MACHINES: "+
				s.expected(t, c.ledger, c.year, s.machines...))
			want(t, "the totals", sheetLine(sheet["totals"].(map[string]any)), s.expected(t, c.ledger, c.year, all...))
		})
	}
	sheet := s.sheet(t, "&fiscal_year=2026")
	want(t, "the head", fmt.Sprint(sheet["company_code"], " ", sheet["company_name"], " ", sheet["ledger_code"], " ", sheet["ledger_name"], " ",
		sheet["start_date"], " ", sheet["end_date"], " ", sheet["currency"], " ", sheet["group_by"]), "2000 Acme GmbH 0L Leading ledger 2026-01-01 2026-12-31 EUR class")
	items := sheet["items"].([]any)
	want(t, "the class names", fmt.Sprint(items[0].(map[string]any)["class_name"], ", ", items[1].(map[string]any)["class_name"]), "Land, Machinery and equipment")
	closing := sheet["totals"].(map[string]any)
	want(t, "2026's totals in 0L: four machines bought, one reversed; the press sold and scrapped with June's depreciation; the run's",
		sheetLine(closing), "0.00 3600.00 -1200.00 0.00 2400.00 0.00 -40.00 10.00 0.00 -30.00 0.00 2370.00")
	opening := s.sheet(t, "&fiscal_year=2027")["totals"].(map[string]any)
	want(t, "2027 opens with 2026's closing values", fmt.Sprint(amount(opening["opening_cost"]), " ", amount(opening["opening_accumulated"]), " ",
		amount(opening["opening_net_book_value"])), fmt.Sprint(amount(closing["closing_cost"]), " ", amount(closing["closing_accumulated"]), " ",
		amount(closing["closing_net_book_value"])))
	want(t, "year totals that differ from their movements' sums", totalsDiffer(t, s.Tenant.ID), 0)
	s.closesOnTheLedger(t)
	s.byAsset(t)
}

// totalsDiffer counts the tenant's asset year totals and class year totals
// that differ from the sums of the movements they total (ADR-0057 §5),
// either way, and the terms whose depreciation booked differs from minus
// the sum of their asset's depreciation movements in their ledger (T57f3).
func totalsDiffer(t *testing.T, tenant uuid.UUID) int {
	t.Helper()
	var differences int
	pgtest.Scan(t, `WITH m AS (SELECT ledger_id, asset_id, legal_entity_id, class_code, fiscal_year,
			coalesce(sum(cost) FILTER (WHERE kind = 'acquisition'), 0) AS a, coalesce(sum(cost) FILTER (WHERE kind = 'retirement'), 0) AS r,
			coalesce(sum(cost) FILTER (WHERE kind LIKE 'transfer%'), 0) AS t, coalesce(sum(accumulated) FILTER (WHERE kind = 'depreciation'), 0) AS d,
			coalesce(sum(accumulated) FILTER (WHERE kind = 'retirement'), 0) AS ra, coalesce(sum(accumulated) FILTER (WHERE kind LIKE 'transfer%'), 0) AS ta
		FROM assets.movement WHERE tenant_id = $1 GROUP BY 1, 2, 3, 4, 5),
		y AS (SELECT ledger_id, asset_id, legal_entity_id, class_code, fiscal_year, acquisitions AS a, retired_cost AS r, transferred_cost AS t,
			depreciation AS d, retired_accumulated AS ra, transferred_accumulated AS ta FROM assets.year_total WHERE tenant_id = $1),
		c AS (SELECT legal_entity_id, ledger_id, fiscal_year, class_code, sum(a) AS a, sum(r) AS r, sum(t) AS t, sum(d) AS d, sum(ra) AS ra, sum(ta) AS ta
			FROM m GROUP BY 1, 2, 3, 4),
		k AS (SELECT legal_entity_id, ledger_id, fiscal_year, class_code, acquisitions::numeric AS a, retired_cost::numeric AS r,
			transferred_cost::numeric AS t, depreciation::numeric AS d, retired_accumulated::numeric AS ra, transferred_accumulated::numeric AS ta
			FROM assets.class_year_total WHERE tenant_id = $1)
		SELECT (SELECT count(*) FROM ((SELECT * FROM m EXCEPT SELECT * FROM y) UNION ALL (SELECT * FROM y EXCEPT SELECT * FROM m)) a)
			+ (SELECT count(*) FROM ((SELECT * FROM c EXCEPT SELECT * FROM k) UNION ALL (SELECT * FROM k EXCEPT SELECT * FROM c)) b)
			+ (SELECT count(*) FROM assets.asset_terms t WHERE t.tenant_id = $1 AND t.booked_depreciation <> -(SELECT coalesce(sum(d.accumulated), 0)
				FROM assets.movement d WHERE d.tenant_id = t.tenant_id AND d.asset_id = t.asset_id AND d.ledger_id = t.ledger_id AND d.kind = 'depreciation'))`,
		[]any{tenant}, &differences)
	return differences
}

// ledgerBalances sums the ledger's balances of the accounts of company
// 2000 (by number) over its fiscal years through year, read through the
// ledger's own API (GET /v1/balances): the ledger carries no balance into
// a new year, so the closing balance is every year's so far.
func (e assetEnv) ledgerBalances(t *testing.T, ledger uuid.UUID, year int, numbers ...string) string {
	t.Helper()
	var out []string
	for _, n := range numbers {
		account := e.Account(t, "2000", n).String()
		var sum int64
		for y := 2026; y <= year; y++ {
			page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/balances?legal_entity_id=%s&ledger_id=%s&fiscal_year=%d&page_size=500", e.DE, ledger, y), "", "")
			if page["next_page_token"] != nil {
				t.Fatalf("balances of %d: more than a page", y)
			}
			for _, it := range page["items"].([]any) {
				b := it.(map[string]any)
				if b["account_id"] == account {
					sum += cents(t, amount(b["debit_lc"])) - cents(t, amount(b["credit_lc"]))
				}
			}
		}
		out = append(out, decimal(sum))
	}
	return strings.Join(out, " ")
}

// closesOnTheLedger: in each ledger and year, the sheet's closing cost and
// accumulated depreciation, both classes' together since they share the
// accounts, are the ledger's balances of the acquisition cost and
// accumulated depreciation accounts (ADR-0057 §3, §9): only assets post
// to them, and no migration loaded balances on them.
func (s sheetScene) closesOnTheLedger(t *testing.T) {
	t.Helper()
	for _, l := range []uuid.UUID{s.l0, s.l2} {
		for _, y := range []int{2026, 2027} {
			totals := s.sheet(t, fmt.Sprintf("&ledger_id=%s&fiscal_year=%d", l, y))["totals"].(map[string]any)
			want(t, fmt.Sprint("the closing values in ", l, " ", y), amount(totals["closing_cost"])+" "+amount(totals["closing_accumulated"]),
				s.ledgerBalances(t, l, y, "160000", "160900"))
		}
	}
}

// byAsset: by asset, the assets with a movement in the year or a value
// at its opening, by number, each with its class, in pages; a class
// filter keeps its assets and their class's totals; the rows add up to
// the totals.
func (s sheetScene) byAsset(t *testing.T) {
	t.Helper()
	var rows []string
	query := "&fiscal_year=2027&group_by=asset&page_size=2"
	page := s.sheet(t, query)
	for {
		rows = append(rows, sheetRows(page)...)
		want(t, "a page's totals", sheetLine(page["totals"].(map[string]any)), s.expected(t, s.l0, 2027, append(slices.Clone(s.machines), s.land...)...))
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		page = s.sheet(t, query+"&page_token="+next)
	}
	mill, lathe, saw, plot := s.machines[0], s.machines[2], s.machines[3], s.land[0]
	want(t, "2027's assets, the retired press without a value left out", strings.Join(rows, "; "), strings.Join([]string{
		"1 MACHINES: " + s.expected(t, s.l0, 2027, mill), "3 MACHINES: " + s.expected(t, s.l0, 2027, lathe),
		"4 MACHINES: " + s.expected(t, s.l0, 2027, saw), "5 LAND: " + s.expected(t, s.l0, 2027, plot)}, "; "))
	first := s.sheet(t, "&fiscal_year=2026&group_by=asset")["items"].([]any)[0].(map[string]any)
	want(t, "an asset's row", fmt.Sprint(first["asset_id"], " ", first["asset_number"], " ", first["description"], " ", first["status"], " ",
		first["class_code"], " ", first["class_name"]), mill+" 1 Mill active MACHINES Machinery and equipment")
	land := s.sheet(t, "&fiscal_year=2026&group_by=asset&class=LAND")
	want(t, "the class's assets", strings.Join(sheetRows(land), "; "), "5 LAND: "+s.expected(t, s.l0, 2026, plot))
	want(t, "the class's totals", sheetLine(land["totals"].(map[string]any)), s.expected(t, s.l0, 2026, plot))
	byClass := s.sheet(t, "&fiscal_year=2026&class=LAND")
	want(t, "a class by class", strings.Join(sheetRows(byClass), "; "), "LAND: "+s.expected(t, s.l0, 2026, plot))
}

// TestAssetHistorySheetRefused: a ledger the company does not keep, a
// fiscal year not set up, a company the caller cannot act in, page
// tokens not issued for the request, a grouping of neither class nor
// asset.
func TestAssetHistorySheetRefused(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	path := "/v1/asset-history-sheet?legal_entity_id=" + e.DE.String() + "&fiscal_year=2026"
	e.Problem(422, "ledger.ledger_not_kept", "GET", path+"&ledger_id="+e.l3.String(), "", "")
	e.Problem(422, "ledger.no_fiscal_period", "GET", "/v1/asset-history-sheet?legal_entity_id="+e.DE.String()+"&fiscal_year=2030", "", "")
	reader := e.Client
	reader.As = e.Reader(t, "reader", e.US)
	reader.Problem(404, "request.not_found", "GET", path, "", "")
	e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	page, _ := e.Must(200, "GET", path+"&group_by=asset&page_size=1", "", "")
	want(t, "the one asset with a value, the planned one left out", fmt.Sprint(len(page["items"].([]any)), " ", page["items"].([]any)[0].(map[string]any)["asset_id"],
		" ", page["next_page_token"]), "1 "+mill+" <nil>")
	two, _ := e.depreciable(t, "Press")
	page, _ = e.Must(200, "GET", path+"&group_by=asset&page_size=1", "", "")
	token := page["next_page_token"].(string)
	want(t, "the second page", e.sheet(t, "&fiscal_year=2026&group_by=asset&page_size=1&page_token="+token)["items"].([]any)[0].(map[string]any)["asset_id"], two)
	e.Problem(400, "request.invalid_page_token", "GET", path+"&page_token="+token, "", "")
	e.Problem(400, "request.invalid_page_token", "GET", path+"&group_by=asset&class=LAND&page_token="+token, "", "")
	e.Problem(400, "request.invalid_parameter", "GET", path+"&group_by=ledger", "", "")
}

// reconcileScene is company 2000's machines as IAS 16.73(e) names their
// lines (po:2026-11-09, B14), in 0L over two fiscal years, 1,200.00 each
// from June 2026 at 10.00 a month:
//   - the mill, with a change in estimate from September 2026 (T57j: 60
//     months, residual 180.00, so 17.37 a month from then);
//   - the press, held for sale from September 2026 (T57k: nothing from
//     then);
//   - the lathe, sold in July 2026 (disposed of in the year);
//   - the saw, depreciating as planned;
//   - the drill, acquired in February 2027 (600.00).
//
// June's run, then December's (July to December caught up on the terms
// in force in each period), then January 2027's.
type reconcileScene struct {
	assetEnv
	mill, press, lathe, saw, drill string
}

func newReconcileScene(t *testing.T) reconcileScene {
	t.Helper()
	s := reconcileScene{assetEnv: newRunsWith(t, withNextYear)}
	s.mill, _ = s.depreciable(t, "Mill")
	s.press, _ = s.depreciable(t, "Press")
	s.lathe, _ = s.depreciable(t, "Lathe")
	s.saw, _ = s.depreciable(t, "Saw")
	_, run := s.start(t, s.l0, 6)
	s.carry(t, run)
	_, tag := s.Must(200, "GET", "/v1/assets/"+s.mill, "", "")
	s.Must(200, "PATCH", "/v1/assets/"+s.mill, estimate(s.l0, 9, `, "useful_life_months": 60, "residual_value": {"amount": "180.00", "currency": "EUR"}`), tag)
	_, tag = s.Must(200, "GET", "/v1/assets/"+s.press, "", "")
	s.Must(200, "PATCH", "/v1/assets/"+s.press, heldForSale(s.l0, 9), tag)
	s.retire(t, s.lathe, `"kind": "sale", "proceeds": {"amount": "700.00", "currency": "EUR"}, "posting_date": "2026-07-15"`)
	_, run = s.start(t, s.l0, 12)
	s.carry(t, run)
	_, run = s.startIn(t, s.l0, 2027, 1)
	s.carry(t, run)
	entry := s.post(t, "2000", "2027-02-10", "EUR", nil, line{"169000", "600.00"}, line{"100000", "-600.00"})
	drill, _ := s.create(t, fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": "Drill", "cost_center_id": %q}`, s.DE, s.c.Admin))
	s.drill = drill["id"].(string)
	s.acquire(t, s.drill, entry, 1, `, "posting_date": "2027-02-10"`)
	return s
}

// reconciles checks a row's (or the totals') reconciliation from opening
// to closing: cost, accumulated depreciation and the net book values.
func reconciles(t *testing.T, what string, v map[string]any) {
	t.Helper()
	c := func(column string) int64 { return cents(t, amount(v[column])) }
	want(t, what+": closing cost, the opening cost plus the year's movements", c("closing_cost"),
		c("opening_cost")+c("acquisitions")+c("retirements")+c("transfers"))
	want(t, what+": closing accumulated depreciation, the opening plus the year's movements", c("closing_accumulated"),
		c("opening_accumulated")+c("depreciation")+c("retirements_accumulated")+c("transfers_accumulated"))
	want(t, what+": opening net book value, cost plus accumulated depreciation", c("opening_net_book_value"), c("opening_cost")+c("opening_accumulated"))
	want(t, what+": closing net book value, cost plus accumulated depreciation", c("closing_net_book_value"), c("closing_cost")+c("closing_accumulated"))
}

// TestAssetHistorySheetReconciles (po:2026-11-09, B14; IAS 16.73(d), (e)):
// every row of the sheet, by class and by asset, and its totals reconcile
// from opening to closing for cost, for accumulated depreciation and for
// the carrying amount, with an asset acquired in the year, one disposed
// of in it (the cost and the accumulated depreciation removed), one with
// a change in estimate and one held for sale; the rows are the movements'
// by column, and a year opens with the closing values of the one before.
func TestAssetHistorySheetReconciles(t *testing.T) {
	s := newReconcileScene(t)
	for _, year := range []int{2026, 2027} {
		for _, by := range []string{"class", "asset"} {
			t.Run(fmt.Sprint(year, " by ", by), func(t *testing.T) {
				sheet := s.sheet(t, fmt.Sprintf("&fiscal_year=%d&group_by=%s", year, by))
				for i, it := range sheet["items"].([]any) {
					reconciles(t, fmt.Sprint("row ", i+1), it.(map[string]any))
				}
				reconciles(t, "the totals", sheet["totals"].(map[string]any))
			})
		}
	}
	first := s.sheet(t, "&fiscal_year=2026&group_by=asset")
	want(t, "2026 by asset: all acquired in the year; the mill's change in estimate, the press held for sale, the lathe sold",
		strings.Join(sheetRows(first), "; "), strings.Join([]string{
			"1 MACHINES: 0.00 1200.00 0.00 0.00 1200.00 0.00 -99.48 0.00 0.00 -99.48 0.00 1100.52",
			"2 MACHINES: 0.00 1200.00 0.00 0.00 1200.00 0.00 -30.00 0.00 0.00 -30.00 0.00 1170.00",
			"3 MACHINES: 0.00 1200.00 -1200.00 0.00 0.00 0.00 -10.00 10.00 0.00 0.00 0.00 0.00",
			"4 MACHINES: 0.00 1200.00 0.00 0.00 1200.00 0.00 -70.00 0.00 0.00 -70.00 0.00 1130.00"}, "; "))
	second := s.sheet(t, "&fiscal_year=2027&group_by=asset")
	want(t, "2027 by asset: opening with 2026's closing values, the lathe gone, the press not depreciated, the drill acquired",
		strings.Join(sheetRows(second), "; "), strings.Join([]string{
			"1 MACHINES: 1200.00 0.00 0.00 0.00 1200.00 -99.48 -17.37 0.00 0.00 -116.85 1100.52 1083.15",
			"2 MACHINES: 1200.00 0.00 0.00 0.00 1200.00 -30.00 0.00 0.00 0.00 -30.00 1170.00 1170.00",
			"4 MACHINES: 1200.00 0.00 0.00 0.00 1200.00 -70.00 -10.00 0.00 0.00 -80.00 1130.00 1120.00",
			"5 MACHINES: 0.00 600.00 0.00 0.00 600.00 0.00 0.00 0.00 0.00 0.00 0.00 600.00"}, "; "))
	for year, sheet := range map[int]map[string]any{2026: first, 2027: second} {
		rows := sheetRows(sheet)
		for i, a := range s.shown(year) {
			want(t, fmt.Sprint(year, ": row ", i+1, ", its movements by column"), rows[i][strings.Index(rows[i], ": ")+2:], s.expected(t, s.l0, year, a))
		}
		want(t, fmt.Sprint(year, ": the totals, every asset's movements"), sheetLine(sheet["totals"].(map[string]any)),
			s.expected(t, s.l0, year, s.mill, s.press, s.lathe, s.saw, s.drill))
	}
	want(t, "year totals that differ from their movements' sums", totalsDiffer(t, s.Tenant.ID), 0)
}

// shown are the assets on the year's sheet by number: all but the drill
// in 2026, all but the lathe, sold, in 2027.
func (s reconcileScene) shown(year int) []string {
	if year == 2026 {
		return []string{s.mill, s.press, s.lathe, s.saw}
	}
	return []string{s.mill, s.press, s.saw, s.drill}
}
