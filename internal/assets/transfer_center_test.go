package assets_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// transfer posts a transfer of the asset (on assetDay unless the body says
// otherwise) and returns it.
func (e assetEnv) transfer(t *testing.T, asset any, body string) map[string]any {
	t.Helper()
	q, _ := e.clocked.Must(201, "POST", "/v1/asset-transfers", fmt.Sprintf(`{"asset_id": "%v", %s}`, asset, body), "")
	return q
}

// untransfer reverses the transfer with its current ETag.
func (e assetEnv) untransfer(t *testing.T, q map[string]any, body string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", fmt.Sprint("/v1/asset-transfers/", q["id"]), "", "")
	out, _ := e.Must(200, "POST", fmt.Sprint("/v1/asset-transfers/", q["id"], ":reverse"), body, tag)
	return out
}

// refuseTransferReversal asks for the transfer's reversal with its current
// ETag and wants the problem.
func (e assetEnv) refuseTransferReversal(t *testing.T, q map[string]any, body string, status int, code string) {
	t.Helper()
	_, tag := e.Must(200, "GET", fmt.Sprint("/v1/asset-transfers/", q["id"]), "", "")
	e.Problem(status, code, "POST", fmt.Sprint("/v1/asset-transfers/", q["id"], ":reverse"), body, tag)
}

// otherCenter creates profit center PC3 of company 2000 and cost center
// 4000 on it, and returns the cost center's id.
func (e assetEnv) otherCenter(t *testing.T) uuid.UUID {
	t.Helper()
	pc, _ := e.Must(201, "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PC3", "name": "PC 3", "responsible": "Jane Doe",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, e.DE), "")
	e.Approve(t, pc["pending_change_id"].(string))
	return e.centerOn(t, "4000", uuid.MustParse(pc["id"].(string)))
}

// centerOn creates cost center code of company 2000 on the profit center
// and returns its id.
func (e assetEnv) centerOn(t *testing.T, code string, pc uuid.UUID) uuid.UUID {
	t.Helper()
	cc, _ := e.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "CC %s", "responsible": "J",
		"category": "ADMIN", "valid_from": "2026-01-01", "profit_center_id": %q, "reason": "r"}`, e.DE, code, code, pc), "")
	e.Approve(t, cc["pending_change_id"].(string))
	return uuid.MustParse(cc["id"].(string))
}

// january is a machine of company 2000 on the administration cost center
// whose 1,200.00 EUR line, posted on 10 January 2026, was capitalised
// then: 10.00 a month in 0L from January, declining balance in 2L.
func (e assetEnv) january(t *testing.T) string {
	t.Helper()
	entry := e.post(t, "2000", "2026-01-10", "EUR", nil, line{"169000", "1200.00"}, line{"100000", "-1200.00"})
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	e.acquire(t, a["id"], entry, 1, `, "posting_date": "2026-01-10"`)
	return a["id"].(string)
}

// moved is what a transfer moved, one ledger each: "ledger catch-up
// catch-up-date cost accumulated residual | catch-up cost accumulated" (the
// last three in group currency).
func moved(q map[string]any) string {
	var out []string
	for _, l := range q["ledgers"].([]any) {
		l := l.(map[string]any)
		out = append(out, fmt.Sprint(l["ledger_code"], " ", amount(l["catch_up"]), " ", l["catch_up_posting_date"], " ", amount(l["cost"]), " ",
			amount(l["accumulated"]), " ", amount(l["residual"]), " | ", amount(l["catch_up_gc"]), " ", amount(l["cost_gc"]), " ", amount(l["accumulated_gc"])))
	}
	return strings.Join(out, "; ")
}

// parts are a document's entries as "ledger part", in order.
func parts(q map[string]any) string {
	var out []string
	for _, en := range q["entries"].([]any) {
		en := en.(map[string]any)
		out = append(out, fmt.Sprint(en["ledger_code"], " ", en["part"]))
	}
	return strings.Join(out, ", ")
}

// partOf is a document's entry of the part in the ledger, and its
// reversal entry.
func partOf(q map[string]any, ledger, part string) (any, any) {
	for _, en := range q["entries"].([]any) {
		if en := en.(map[string]any); en["ledger_code"] == ledger && en["part"] == part {
			return en["journal_entry_id"], en["reversal_journal_entry_id"]
		}
	}
	return nil, nil
}

// entryDate is a journal entry's posting date.
func entryDate(t *testing.T, entry any) string {
	t.Helper()
	var d string
	pgtest.Scan(t, `SELECT posting_date::text FROM ledger.journal_entry WHERE id = $1`, []any{entry}, &d)
	return d
}

// centersOf is an asset's centers by code and its latest assignment's
// valid_from.
func (e assetEnv) centersOf(t *testing.T, asset any) string {
	t.Helper()
	a, _ := e.Must(200, "GET", fmt.Sprint("/v1/assets/", asset), "", "")
	var from string
	pgtest.Scan(t, `SELECT valid_from::text FROM assets.asset_assignment WHERE asset_id = $1 ORDER BY seq DESC LIMIT 1`, []any{asset}, &from)
	return fmt.Sprint(a["cost_center_code"], " ", a["profit_center_code"], " from ", from)
}

// TestCenterTransferWithTheMonthBeforeOpen: January and February run in
// 0L; with March open, a transfer to a cost center of another profit
// center valued 1 April posts March's depreciation (all three months' in
// 2L) on the old centers in an entry of its own dated 31 March, and
// reposts the balances after it to the new profit center in its April
// entry; the asset is booked through March. The March run then passes
// over the asset, a March-dated acquisition is refused (one dated April
// valued in March is not, and its reversal is not dated March), the
// acquisition from before the transfer is not reversed, and the April run
// posts on the new centers, the April acquisition's March depreciation
// included (planned 10 + 10 + 11.02 + 11.02 less 30 booked).
func TestCenterTransferWithTheMonthBeforeOpen(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill := e.january(t)
	acquired := e.acquisitionOf(t, mill)
	for _, p := range []int{1, 2} {
		_, run := e.start(t, e.l0, p)
		e.carry(t, run)
	}
	q := e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-10"`, cc))
	want(t, "the transfer", fmt.Sprint(q["kind"], " ", q["status"], " ", q["value_date"], " ", q["posting_date"], " ", q["profit_center_change"], " ",
		q["from"].(map[string]any)["cost_center_code"], "/", q["from"].(map[string]any)["profit_center_code"], " -> ",
		q["to"].(map[string]any)["cost_center_code"], "/", q["to"].(map[string]any)["profit_center_code"]), "center posted 2026-04-01 2026-04-10 true 1000/PC1 -> 4000/PC3")
	want(t, "what it moved", moved(q), "0L 10.00 2026-03-31 1200.00 -30.00 0.00 | 10.87 1304.35 -32.61; "+
		"2L 59.01 2026-03-31 1200.00 -59.01 0.00 | 64.14 1304.35 -64.14")
	want(t, "its entries", parts(q), "0L catch_up, 0L main, 2L catch_up, 2L main")
	catchUp, _ := partOf(q, "0L", "catch_up")
	main, _ := partOf(q, "0L", "main")
	want(t, "the catch-up's date", entryDate(t, catchUp), "2026-03-31")
	want(t, "the catch-up on the old centers", entryLines(t, catchUp), "0L 620000 1000 PC1 1000 1087; 0L 160900 - PC1 -1000 -1087")
	want(t, "the balances reposted", entryLines(t, main),
		"0L 160000 - PC1 -120000 -130435; 0L 160000 - PC3 120000 130435; 0L 160900 - PC1 3000 3261; 0L 160900 - PC3 -3000 -3261")
	want(t, "booked through", bookedThrough(t, mill), "0L 2026/3, 2L 2026/3")
	want(t, "the asset's centers", e.centersOf(t, mill), "4000 PC3 from 2026-04-01")
	_, jeTag := e.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", main), "", "")
	p := e.Problem(409, "ledger.entry_owned_elsewhere", "POST", fmt.Sprint("/v1/journal-entries/", main, ":reverse"), "", jeTag)
	if !strings.Contains(p["detail"].(string), fmt.Sprint("/v1/asset-transfers/", q["id"], ":reverse")) {
		t.Errorf("detail: got %v, want the transfer's path", p["detail"])
	}

	_, march := e.start(t, e.l0, 3)
	want(t, "the March run, the asset passed over", runOf(e.carry(t, march)), "posted 0 0 0 0.00")
	march2 := e.post(t, "2000", "2026-03-20", "EUR", nil, line{"169000", "120.00"}, line{"100000", "-120.00"})
	e.clocked.Problem(409, "assets.posting_before_center_transfer", "POST", "/v1/asset-acquisitions",
		acquisition(mill, march2, 1, `, "posting_date": "2026-03-20"`), "")
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+acquired, "", "")
	e.clocked.Problem(409, "assets.later_movements", "POST", "/v1/asset-acquisitions/"+acquired+":reverse", `{"reason": "r"}`, tag)
	late := e.acquire(t, mill, march2, 1, `, "posting_date": "2026-04-12", "value_date": "2026-03-20"`)
	e.refuseReversal(t, late, `, "posting_date": "2026-03-31"`, 422, "assets.reversal_before_original")

	_, april := e.start(t, e.l0, 4)
	r := e.carry(t, april)
	want(t, "the April run", runOf(r), "posted 1 1 0 12.04; 1 posted 1 1 0 12.04")
	batch := r["batches"].([]any)[0].(map[string]any)
	want(t, "the April run on the new centers", entryLines(t, batch["journal_entry_id"]), "0L 620000 4000 PC3 1204 1309; 0L 160900 - PC3 -1204 -1309")
	e.refuseTransferReversal(t, q, `{"reason": "r"}`, 409, "assets.later_movements")
}

// acquisitionOf is the asset's (first) acquisition's id.
func (e assetEnv) acquisitionOf(t *testing.T, asset any) string {
	t.Helper()
	var id string
	pgtest.Scan(t, `SELECT id::text FROM assets.acquisition WHERE asset_id = $1 ORDER BY seq LIMIT 1`, []any{asset}, &id)
	return id
}

// TestCenterTransferReversed: a transfer whose catch-up was dated 31 March
// is reversed on 20 April while March is open: its catch-up's reversal is
// dated 31 March and its main entry's 20 April on the restoring row's
// centers; booked_through returns, the previous centers are the asset's
// again from -infinity, so a March run posts March on them and a
// March-dated acquisition is accepted. A reversal dated before the
// transfer's posting date and a second reversal are refused.
func TestCenterTransferReversed(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill := e.january(t)
	q := e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-10", "text": "To CC 4000"`, cc))
	e.refuseTransferReversal(t, q, `{"reason": "r", "posting_date": "2026-04-09"}`, 422, "assets.reversal_before_original")
	stale := fmt.Sprintf(`"%v"`, q["version"].(float64)+1) // a version the transfer does not have
	e.Problem(412, "request.version_mismatch", "POST", fmt.Sprint("/v1/asset-transfers/", q["id"], ":reverse"), `{"reason": "r"}`, stale)
	r := e.untransfer(t, q, `{"reason": "Wrong cost center", "posting_date": "2026-04-20"}`)
	want(t, "the reversal", fmt.Sprint(r["status"], " ", r["reversal"]), "reversed map[posting_date:2026-04-20 reason:Wrong cost center]")
	_, catchUp := partOf(r, "0L", "catch_up")
	_, main := partOf(r, "0L", "main")
	want(t, "the catch-up's reversal date", entryDate(t, catchUp), "2026-03-31")
	want(t, "the main entry's reversal date", entryDate(t, main), "2026-04-20")
	want(t, "the main entry's reversal on the restoring row's centers", entryLines(t, main),
		"0L 160000 - PC1 120000 130435; 0L 160000 - PC3 -120000 -130435; 0L 160900 - PC1 -3000 -3261; 0L 160900 - PC3 3000 3261")
	want(t, "booked through", bookedThrough(t, mill), "0L -, 2L -")
	want(t, "the asset's centers", e.centersOf(t, mill), "1000 PC1 from -infinity")
	e.refuseTransferReversal(t, q, `{"reason": "r"}`, 409, "assets.transfer_not_posted")

	_, march := e.start(t, e.l0, 3)
	r = e.carry(t, march)
	want(t, "the March run", runOf(r), "posted 1 1 0 30.00; 1 posted 1 1 0 30.00")
	want(t, "the March run on the previous centers", entryLines(t, r["batches"].([]any)[0].(map[string]any)["journal_entry_id"]),
		"0L 620000 1000 PC1 3000 3261; 0L 160900 - PC1 -3000 -3261")
	march2 := e.post(t, "2000", "2026-03-20", "EUR", nil, line{"169000", "120.00"}, line{"100000", "-120.00"})
	e.acquire(t, mill, march2, 1, `, "posting_date": "2026-03-20"`)
}

// TestCenterTransferWithTheMonthBeforeClosed: with March closed, the
// catch-up is posted in the transfer's April entry on the old centers.
func TestCenterTransferWithTheMonthBeforeClosed(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill := e.january(t)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 3`,
		e.Tenant.ID)
	q := e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-10"`, cc))
	want(t, "its entries", parts(q), "0L main, 2L main")
	want(t, "the 0L catch-up", moved(q)[:22], "0L 30.00 2026-04-10 12")
	main, _ := partOf(q, "0L", "main")
	want(t, "the catch-up on the old centers, then the balances", entryLines(t, main), "0L 620000 1000 PC1 3000 3261; 0L 160900 - PC1 -3000 -3261; "+
		"0L 160000 - PC1 -120000 -130435; 0L 160000 - PC3 120000 130435; 0L 160900 - PC1 3000 3261; 0L 160900 - PC3 -3000 -3261")
	want(t, "booked through", bookedThrough(t, mill), "0L 2026/3, 2L 2026/3")
}

// TestCenterTransferCatchUpReversedAfterTheClose: a transfer whose
// catch-up was dated 31 March, reversed after March closed: the
// catch-up's reversal is posted on the reversal's April date on the old
// centers, and booked_through comes back.
func TestCenterTransferCatchUpReversedAfterTheClose(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill := e.january(t)
	q := e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-10"`, cc))
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 3`,
		e.Tenant.ID)
	r := e.untransfer(t, q, `{"reason": "r", "posting_date": "2026-04-15"}`)
	_, catchUp := partOf(r, "0L", "catch_up")
	want(t, "the catch-up's reversal date", entryDate(t, catchUp), "2026-04-15")
	want(t, "the catch-up's reversal on the old centers", entryLines(t, catchUp), "0L 620000 1000 PC1 -3000 -3261; 0L 160900 - PC1 3000 3261")
	want(t, "booked through", bookedThrough(t, mill), "0L -, 2L -")
	var moves string
	pgtest.Scan(t, `SELECT string_agg(posting_date || ' ' || fiscal_period || ' ' || accumulated, ', ' ORDER BY seq) FROM assets.movement m
		JOIN ledger.ledger g ON g.id = m.ledger_id WHERE m.document_id = $1 AND g.code = '0L'`, []any{q["id"]}, &moves)
	want(t, "the 0L catch-up and its reversal", moves, "2026-03-31 3 -3000, 2026-04-15 4 3000")
}

// TestTwoCenterTransfersInOnePeriod: two center transfers valued 1 April;
// between them an acquisition valued in February is capitalised, so the
// second's catch-up (that acquisition's February and March depreciation)
// is posted in its April entry on the first transfer's centers, its
// previous row being valid from April only.
func TestTwoCenterTransfersInOnePeriod(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill := e.january(t)
	e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-10"`, cc))
	feb := e.post(t, "2000", "2026-04-12", "EUR", nil, line{"169000", "119.00"}, line{"100000", "-119.00"})
	e.acquire(t, mill, feb, 1, `, "posting_date": "2026-04-12", "value_date": "2026-02-15"`)
	second := e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-14"`, e.c.Admin))
	want(t, "its entries", parts(second), "0L main, 2L main")
	want(t, "the 0L catch-up", moved(second)[:22], "0L 2.00 2026-04-14 131")
	main, _ := partOf(second, "0L", "main")
	want(t, "the catch-up on the first transfer's centers", strings.Join(strings.Split(entryLines(t, main), "; ")[:2], "; "),
		"0L 620000 4000 PC3 200 217; 0L 160900 - PC3 -200 -217")
	want(t, "the asset's centers", e.centersOf(t, mill), "1000 PC1 from 2026-04-01")
}

// TestCenterTransferAfterTheRun: June's run posted in 0L; a center
// transfer valued 1 June posts June's depreciation back on the old
// centers in its June entry (the asset booked through June stays so), so
// a repeat June run posts June on the new centers. A transfer to a cost
// center of the same profit center reposts nothing.
func TestCenterTransferAfterTheRun(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill, _ := e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	q := e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q`, cc))
	want(t, "what it moved", moved(q), "0L -10.00 2026-06-15 1200.00 0.00 0.00 | -10.87 1304.35 0.00; 2L 0.00 2026-06-15 1200.00 0.00 0.00 | 0.00 1304.35 0.00")
	main, _ := partOf(q, "0L", "main")
	want(t, "the June run posted back on the old centers", strings.Join(strings.Split(entryLines(t, main), "; ")[:2], "; "),
		"0L 620000 1000 PC1 -1000 -1087; 0L 160900 - PC1 1000 1087")
	want(t, "booked through", bookedThrough(t, mill), "0L 2026/6, 2L 2026/5")
	_, again := e.start(t, e.l0, 6)
	r := e.carry(t, again)
	want(t, "the repeat June run on the new centers", entryLines(t, r["batches"].([]any)[0].(map[string]any)["journal_entry_id"]),
		"0L 620000 4000 PC3 1000 1087; 0L 160900 - PC3 -1000 -1087")

	press, _ := e.depreciable(t, "Press")
	same := e.transfer(t, press, fmt.Sprintf(`"kind": "center", "cost_center_id": %q`, e.centerOn(t, "5000", e.c.Profit)))
	want(t, "a transfer within the profit center", fmt.Sprint(same["profit_center_change"], " ", parts(same), " ", moved(same)),
		"false  0L 0.00 2026-06-15 0.00 0.00 0.00 | 0.00 0.00 0.00; 2L 0.00 2026-06-15 0.00 0.00 0.00 | 0.00 0.00 0.00")
}
