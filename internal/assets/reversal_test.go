package assets_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// manualAcquisition is a request for an acquisition against an offset
// account.
func manualAcquisition(asset any, account uuid.UUID, amount, extra string) string {
	return fmt.Sprintf(`{"asset_id": "%v", "offset_account_id": %q, "amount": %s%s}`, asset, account, amount, extra)
}

// manual posts an acquisition against an offset account on assetDay.
func (e assetEnv) manual(t *testing.T, asset any, account uuid.UUID, amount, extra string) map[string]any {
	t.Helper()
	q, _ := e.clocked.Must(201, "POST", "/v1/asset-acquisitions", manualAcquisition(asset, account, amount, extra), "")
	return q
}

// reverse reverses an acquisition with the body's fields besides its
// reason, reading its ETag first.
func (e assetEnv) reverse(t *testing.T, q map[string]any, extra string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+q["id"].(string), "", "")
	out, _ := e.Must(200, "POST", "/v1/asset-acquisitions/"+q["id"].(string)+":reverse", `{"reason": "Capitalised in error"`+extra+`}`, tag)
	return out
}

// refuseReversal expects the acquisition's reversal refused with the
// problem.
func (e assetEnv) refuseReversal(t *testing.T, q map[string]any, extra string, status int, code string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+q["id"].(string), "", "")
	return e.Problem(status, code, "POST", "/v1/asset-acquisitions/"+q["id"].(string)+":reverse", `{"reason": "Capitalised in error"`+extra+`}`, tag)
}

// asset reads an asset as "status capitalised_on; values".
func (e assetEnv) asset(t *testing.T, id any) string {
	t.Helper()
	a, _ := e.Must(200, "GET", fmt.Sprint("/v1/assets/", id), "", "")
	return fmt.Sprint(a["status"], " ", a["capitalised_on"], "; ", values(a))
}

// amounts is an acquisition's amount in its three currencies.
func amounts(q map[string]any) string {
	return fmt.Sprint(moneyOf(q["amount"]), ", ", moneyOf(q["amount_lc"]), ", ", moneyOf(q["amount_gc"]))
}

// TestManualAcquisition: a GBP amount against company 1000's share
// capital (USD, group EUR) is converted at the posting date's rate, its
// movement takes the cost line's local and group amounts, the asset
// becomes active on its posting date, and it reads back with its offset
// account, audited and published. Its reversal, and that of a GBP line's
// part, negate the local and group amounts exactly and give the line
// back in all three currencies.
func TestManualAcquisition(t *testing.T) {
	e := newAcquisitions(t)
	a, _ := e.create(t, e.body(e.US, "MACHINES", ""))
	capital := e.Account(t, "1000", "300000")
	q := e.manual(t, a["id"], capital, `{"amount": "100.00", "currency": "GBP"}`, `, "posting_date": "2026-06-12", "text": "Contribution in kind"`)
	want(t, "the amounts", amounts(q), "100.00 GBP, 127.00 USD, 116.84 EUR")
	want(t, "the dates", fmt.Sprint(q["value_date"], " ", q["posting_date"]), "2026-06-12 2026-06-12")
	off := q["offset_account"].(map[string]any)
	want(t, "the offset account", fmt.Sprint(off["id"], " ", off["number"], " ", off["name"]), fmt.Sprint(capital, " 300000 Share Capital"))
	if _, ok := q["source"]; ok {
		t.Errorf("source: got %v, want none", q["source"])
	}
	got, _ := e.Must(200, "GET", "/v1/asset-acquisitions/"+q["id"].(string), "", "")
	want(t, "read back", fmt.Sprint(got), fmt.Sprint(q))
	want(t, "the cost account", balances(t, e.Tenant.ID, e.Account(t, "1000", "160000")), "0L: 10000 12700 11684")
	want(t, "the offset account's balance", balances(t, e.Tenant.ID, capital), "0L: -10000 -12700 -11684")
	want(t, "the asset", e.asset(t, a["id"]), "active 2026-06-12; 0L 127.00 USD 116.84 EUR 0.00 USD 0.00 EUR 127.00 USD 116.84 EUR")
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_posted"})
	ledgers := evs[0]["ledgers"].([]any)[0].(map[string]any)
	want(t, "the event", fmt.Sprint(evs[0]["asset_status"], " ", moneyOf(ledgers["cost"]), " ", moneyOf(ledgers["cost_gc"])), "active 127.00 USD 116.84 EUR")
	var offset string
	pgtest.Scan(t, `SELECT detail->>'offset_account_id' FROM audit.audit_event WHERE tenant_id = $1 AND action = 'assets.acquisition.post'`,
		[]any{e.Tenant.ID}, &offset)
	want(t, "the audited offset account", offset, capital)
	page, _ := e.Must(200, "GET", "/v1/asset-acquisitions?page_size=1&asset_id="+a["id"].(string), "", "")
	want(t, "listed with its offset account", page["items"].([]any)[0].(map[string]any)["offset_account"].(map[string]any)["number"], "300000")

	entry := e.post(t, "1000", "2026-06-10", "GBP", nil, line{"169000", "1000.00"}, line{"100000", "-1000.00"})
	part := e.acquire(t, a["id"], entry, 1, `, "amount": {"amount": "333.33", "currency": "GBP"}`)
	e.reverse(t, q, "")
	e.reverse(t, part, "")
	var negated string
	pgtest.Scan(t, `SELECT string_agg(concat_ws(' ', m.cost, m.cost_gc), '; ' ORDER BY m.seq) FROM assets.movement m WHERE m.asset_id = $1 AND m.reverses_id IS NOT NULL`,
		[]any{a["id"]}, &negated)
	want(t, "the negated movements in local and group currency", negated, "-12700 -11684; -42333 -38946")
	var used string
	pgtest.Scan(t, `SELECT concat_ws(' ', used, used_lc, used_gc) FROM assets.clearing_use WHERE entry_id = $1`, []any{entry}, &used)
	want(t, "the line given back in all three currencies", used, "0 0 0")
	want(t, "the cost account", balances(t, e.Tenant.ID, e.Account(t, "1000", "160000")), "0L: 0 0 0")
	evs = apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_reversed"})
	ledgers = evs[0]["ledgers"].([]any)[0].(map[string]any)
	want(t, "the reversal's event", fmt.Sprint(evs[0]["asset_status"], " ", moneyOf(ledgers["cost"]), " ", moneyOf(ledgers["cost_gc"])),
		"active -127.00 USD -116.84 EUR")
}

// TestManualAcquisitionRefused: an offset account that is a
// reconciliation account of either kind, an asset clearing account, not
// one of the company or unknown is refused, as is a request naming both a
// line and an offset account or neither, and one against an offset
// account without a positive amount.
func TestManualAcquisitionRefused(t *testing.T) {
	e := newAcquisitions(t)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	id := a["id"]
	capital := e.Account(t, "2000", "300000")
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "500.00"}, line{"100000", "-500.00"})
	eur := `{"amount": "10.00", "currency": "EUR"}`
	for name, c := range map[string]struct {
		body        string
		status      int
		code, field string
	}{
		"a supplier reconciliation account": {manualAcquisition(id, e.Account(t, "2000", "211000"), eur, ""), 422, "assets.offset_account_invalid", "offset_account_id"},
		"an asset reconciliation account":   {manualAcquisition(id, e.Account(t, "2000", "160000"), eur, ""), 422, "assets.offset_account_invalid", "offset_account_id"},
		"the asset clearing account":        {manualAcquisition(id, e.Account(t, "2000", "169000"), eur, ""), 422, "assets.offset_account_invalid", "offset_account_id"},
		"an unknown account":                {manualAcquisition(id, uuid.Must(uuid.NewV7()), eur, ""), 422, "assets.offset_account_invalid", "offset_account_id"},
		"a line and an offset account": {fmt.Sprintf(`{"asset_id": "%v", "source": {"entry_id": %q, "line_no": 1}, "offset_account_id": %q}`, id, entry, capital),
			422, "request.validation_failed", "source"},
		"neither":                       {fmt.Sprintf(`{"asset_id": "%v"}`, id), 422, "request.validation_failed", "source"},
		"no amount":                     {fmt.Sprintf(`{"asset_id": "%v", "offset_account_id": %q}`, id, capital), 422, "request.validation_failed", "amount"},
		"a negative amount":             {manualAcquisition(id, capital, `{"amount": "-1.00", "currency": "EUR"}`, ""), 422, "request.validation_failed", "amount"},
		"zero":                          {manualAcquisition(id, capital, `{"amount": "0.00", "currency": "EUR"}`, ""), 422, "request.validation_failed", "amount"},
		"a value date after the period": {manualAcquisition(id, capital, eur, `, "value_date": "2026-07-01"`), 422, "assets.value_date_in_future", "value_date"},
		"a currency without a rate":     {manualAcquisition(id, capital, `{"amount": "10", "currency": "JPY"}`, ""), 422, "ledger.no_exchange_rate", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.clocked.Problem(c.status, c.code, "POST", "/v1/asset-acquisitions", c.body, "")
			if c.field == "" {
				return
			}
			errs, _ := p["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("errors: got %v, want one on %s", p["errors"], c.field)
			}
			want(t, "field", errs[0].(map[string]any)["field"], c.field)
		})
	}
	if e.Account(t, "1000", "300000") != capital {
		e.clocked.Problem(422, "assets.offset_account_invalid", "POST", "/v1/asset-acquisitions", manualAcquisition(id, e.Account(t, "1000", "300000"), eur, ""), "")
	}
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE legal_entity_id = $1 AND account_id = $2`, e.DE, capital)
	e.clocked.Problem(422, "assets.offset_account_invalid", "POST", "/v1/asset-acquisitions", manualAcquisition(id, capital, eur, ""), "")
}

// TestReverseAcquisition: company 2000 keeps 0L and 2L. Reversing the
// earliest of three acquisitions on a later date reverses its entry in
// both ledgers on that date, negates its movements with the asset's next
// seq, gives its amount back to the line, moves the capitalisation date
// to the earliest left, is audited and published; reversing the others
// returns the asset to planned. A reversal dated before the acquisition,
// in a period not open, of one already reversed or with a stale ETag is
// refused, and the ledger's own :reverse names the acquisition's path.
func TestReverseAcquisition(t *testing.T) {
	e := newAcquisitions(t)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	early := e.post(t, "2000", "2026-06-05", "EUR", nil, line{"169000", "300.00"}, line{"100000", "-300.00"})
	late := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "200.00"}, line{"100000", "-200.00"})
	first := e.acquire(t, a["id"], early, 1, `, "amount": {"amount": "100.00", "currency": "EUR"}`)
	second := e.acquire(t, a["id"], late, 1, "")
	third := e.manual(t, a["id"], e.Account(t, "2000", "300000"), `{"amount": "50.00", "currency": "EUR"}`, `, "value_date": "2026-06-20"`)
	var pc uuid.UUID
	pgtest.Scan(t, `SELECT profit_center_id FROM ledger.journal_line WHERE journal_entry_id = $1 AND amount_tc < 0`,
		[]any{third["entries"].([]any)[0].(map[string]any)["journal_entry_id"]}, &pc)
	want(t, "the offset line's profit center, the asset's", pc, e.c.Profit)
	want(t, "the asset before", e.asset(t, a["id"]), "active 2026-06-05; 0L 350.00 EUR 350.00 EUR 0.00 EUR 0.00 EUR 350.00 EUR 350.00 EUR; "+
		"2L 350.00 EUR 350.00 EUR 0.00 EUR 0.00 EUR 350.00 EUR 350.00 EUR")

	e.refuseReversal(t, first, `, "posting_date": "2026-06-14"`, 422, "assets.reversal_before_original")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 7`, e.Tenant.ID)
	e.refuseReversal(t, first, `, "posting_date": "2026-07-15"`, 409, "ledger.period_not_open")
	e.Problem(412, "request.version_mismatch", "POST", "/v1/asset-acquisitions/"+first["id"].(string)+":reverse", `{"reason": "r"}`, `"9"`)
	reader := e.Client
	reader.As = e.Reader(t, "reader", e.US)
	reader.Problem(404, "request.not_found", "POST", "/v1/asset-acquisitions/"+first["id"].(string)+":reverse", `{"reason": "r"}`, "*")
	je := second["entries"].([]any)[0].(map[string]any)["journal_entry_id"].(string)
	_, jeTag := e.Must(200, "GET", "/v1/journal-entries/"+je, "", "")
	p := e.Problem(409, "ledger.entry_owned_elsewhere", "POST", "/v1/journal-entries/"+je+":reverse", "", jeTag)
	if !strings.Contains(p["detail"].(string), "/v1/asset-acquisitions/"+second["id"].(string)+":reverse") {
		t.Errorf("detail: got %v, want the acquisition's path", p["detail"])
	}

	r := e.reverse(t, first, `, "posting_date": "2026-06-20"`)
	want(t, "the reversed acquisition", fmt.Sprint(r["status"], " ", r["version"], " ", r["reversal"]), "reversed 2 map[posting_date:2026-06-20 reason:Capitalised in error]")
	var reversals []string
	for _, en := range r["entries"].([]any) {
		en := en.(map[string]any)
		rev, _ := e.Must(200, "GET", "/v1/journal-entries/"+en["reversal_journal_entry_id"].(string), "", "")
		want(t, "the reversal entry's number", en["reversal_document_number"], rev["document_number"])
		reversals = append(reversals, fmt.Sprint(en["ledger_code"], " ", rev["posting_date"], " ", rev["reversal_of_id"] == en["journal_entry_id"], " ", en["reversal_fiscal_year"]))
	}
	want(t, "the reversal entries", strings.Join(reversals, "; "), "0L 2026-06-20 true 2026; 2L 2026-06-20 true 2026")
	want(t, "the asset after", e.asset(t, a["id"]), "active 2026-06-10; 0L 250.00 EUR 250.00 EUR 0.00 EUR 0.00 EUR 250.00 EUR 250.00 EUR; "+
		"2L 250.00 EUR 250.00 EUR 0.00 EUR 0.00 EUR 250.00 EUR 250.00 EUR")
	var movements string
	pgtest.Scan(t, `SELECT string_agg(concat_ws(' ', m.cost, m.cost_gc, m.seq, m.seq = a.last_seq, m.posting_date, m.value_date, m.fiscal_period), '; ' ORDER BY m.ledger_id)
		FROM assets.movement m JOIN assets.asset a ON a.id = m.asset_id WHERE m.document_id = $1 AND m.reverses_id IS NOT NULL`, []any{first["id"]}, &movements)
	want(t, "the negated movements", movements, "-10000 -10000 5 t 2026-06-20 2026-06-05 6; -10000 -10000 5 t 2026-06-20 2026-06-05 6")
	got, _ := e.worklist(t, "&remaining=true")
	want(t, "the line given back", got[0], e.number(t, early)+"/1 300.00 EUR used 0.00 EUR left 300.00 EUR 300.00 EUR 300.00 EUR reversed false [1:100.00:reversed]")
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_reversed"})
	ledger := evs[0]["ledgers"].([]any)[0].(map[string]any)
	want(t, "the event", fmt.Sprint(evs[0]["document_id"], " ", evs[0]["posting_date"], " ", evs[0]["reason"], " ", evs[0]["asset_status"], " ",
		moneyOf(ledger["cost"]), " ", ledger["reversed_entry_id"] == first["entries"].([]any)[0].(map[string]any)["journal_entry_id"]),
		fmt.Sprint(first["id"], " 2026-06-20 Capitalised in error active -100.00 EUR true"))
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'assets.acquisition.reverse'`, []any{e.Tenant.ID}, &audits)
	want(t, "audit records", audits, 1)
	e.refuseReversal(t, first, "", 409, "assets.acquisition_not_posted")

	e.reverse(t, second, "")
	want(t, "with the manual acquisition left", e.asset(t, a["id"]), "active 2026-06-20; 0L 50.00 EUR 50.00 EUR 0.00 EUR 0.00 EUR 50.00 EUR 50.00 EUR; "+
		"2L 50.00 EUR 50.00 EUR 0.00 EUR 0.00 EUR 50.00 EUR 50.00 EUR")
	e.reverse(t, third, "")
	want(t, "with none left", e.asset(t, a["id"]), "planned <nil>; 0L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR; "+
		"2L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR")
	want(t, "the offset account", balances(t, e.Tenant.ID, e.Account(t, "2000", "300000")), "0L: 0 0 0; 2L: 0 0 0")
	var ids []string
	for token := ""; ; {
		page, _ := e.Must(200, "GET", "/v1/asset-acquisitions?status=reversed&page_size=1"+token, "", "")
		for _, it := range page["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		token = "&page_token=" + next
	}
	want(t, "the reversed acquisitions, a page each", strings.Join(ids, " "), fmt.Sprint(first["id"], " ", second["id"], " ", third["id"]))
}

// laterRetirement writes a retirement movement on the asset in ledger 0L
// with its next seq, as a retirement (T57g) would, and returns it.
func (e assetEnv) laterRetirement(t *testing.T, asset any) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1 WHERE id = $1`, asset)
	pgtest.Exec(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, document_kind, document_id, created_by)
		SELECT tenant_id, $2, id, last_seq, legal_entity_id, $3, class_code, 'retirement', '2026-06-15', '2026-06-15', 2026, 6, 'retirement', gen_random_uuid(), $4
		FROM assets.asset WHERE id = $1`, asset, id, e.l0, e.Tenant.Human.ID)
	return id
}

// reverseRetirement writes the reversal of laterRetirement's movement with
// the asset's next seq.
func (e assetEnv) reverseRetirement(t *testing.T, asset any, movement uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1 WHERE id = $1`, asset)
	pgtest.Exec(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, document_kind, document_id, reverses_id, created_by)
		SELECT m.tenant_id, gen_random_uuid(), m.asset_id, a.last_seq, m.legal_entity_id, m.ledger_id, m.class_code, m.kind, m.value_date, m.posting_date,
			m.fiscal_year, m.fiscal_period, m.document_kind, m.document_id, m.id, $3
		FROM assets.movement m JOIN assets.asset a ON a.id = m.asset_id WHERE m.id = $2 AND a.id = $1`, asset, movement, e.Tenant.Human.ID)
}

// TestReversalOrder: an acquisition is not reversed while a reduction
// capitalised since holds cost it brought (assets.cost_negative), nor while
// a retirement or transfer posted after it stands (assets.later_movements);
// once those are reversed, it is.
func TestReversalOrder(t *testing.T) {
	e := newAcquisitions(t)
	cc := fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "100.00"}, line{"100000", "-100.00"})
	credit := e.post(t, "2000", "2026-06-11", "EUR", nil, line{"100000", "60.00"}, line{"169000", "-60.00"})
	cost := e.acquire(t, a["id"], entry, 1, "")
	reduction := e.acquire(t, a["id"], credit, 2, "")
	e.refuseReversal(t, cost, "", 422, "assets.cost_negative")
	e.reverse(t, reduction, "")
	e.reverse(t, cost, "")

	b, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	other := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "80.00"}, line{"100000", "-80.00"})
	q := e.acquire(t, b["id"], other, 1, "")
	retirement := e.laterRetirement(t, b["id"])
	e.refuseReversal(t, q, "", 409, "assets.later_movements")
	e.reverseRetirement(t, b["id"], retirement)
	e.reverse(t, q, "")
	want(t, "the asset", e.asset(t, b["id"]), "planned <nil>; 0L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR; "+
		"2L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR")
}

// invoiceOf posts a supplier invoice of company 2000 on the day with an
// item of EUR 100.00 on the clearing account and returns its id and entry.
func (e assetEnv) invoiceOf(t *testing.T, ref, day string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	entry := e.invoice(t, ref, day)
	var id uuid.UUID
	pgtest.Scan(t, `SELECT source_document_id FROM ledger.journal_entry WHERE id = $1`, []any{entry}, &id)
	return id, entry
}

// reversalOf reads a worklist item's reversal's line as its acquisitions
// and offers.
func reversalOf(item map[string]any) string {
	r := item["reversal"].(map[string]any)
	var acqs, offers []string
	for _, a := range r["acquisitions"].([]any) {
		a := a.(map[string]any)
		acqs = append(acqs, fmt.Sprint(a["asset_number"], ":", a["amount"].(map[string]any)["amount"], ":", a["status"]))
	}
	for _, o := range r["offers"].([]any) {
		o := o.(map[string]any)
		offers = append(offers, fmt.Sprint(o["asset_number"], " ", o["asset_description"], ":", moneyOf(o["amount"])))
	}
	return fmt.Sprintf("line %v [%s] offers [%s]", r["line_no"], strings.Join(acqs, " "), strings.Join(offers, ", "))
}

// worklistItem is the item of the entry's line 1 on company 2000's
// worklist with the query, nil when it is not listed.
func (e assetEnv) worklistItem(t *testing.T, entry uuid.UUID, query string) map[string]any {
	t.Helper()
	_, page := e.worklist(t, query)
	for _, it := range page["items"].([]any) {
		if it := it.(map[string]any); it["entry_id"] == entry.String() {
			return it
		}
	}
	return nil
}

// TestReversalLine: a supplier invoice's line is capitalised in part onto
// two machines and the invoice is reversed in payables. The worklist
// shows the line with its reversal's line beside it, offering each
// machine what it holds. The reversal's line takes cost off a machine
// whose acquisition can no longer be reversed (a retirement followed it),
// at most what it holds, then what is left of that, and onto no other
// asset; the machine's acquisition is then not reversed until the
// take-offs are. A reversed entry's line nothing was capitalised from
// gives its reversal's line to no asset.
func TestReversalLine(t *testing.T) {
	e := newAcquisitions(t)
	cc := fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)
	mill, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	lathe, _ := e.create(t, strings.Replace(e.body(e.DE, "MACHINES", cc), "CNC milling machine", "CNC lathe", 1))
	other, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	invoice, entry := e.invoiceOf(t, "MILL-9", "2026-06-04")
	qm := e.acquire(t, mill["id"], entry, 1, `, "amount": {"amount": "60.00", "currency": "EUR"}`)
	e.acquire(t, lathe["id"], entry, 1, `, "amount": {"amount": "30.00", "currency": "EUR"}`)
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+invoice.String(), "", "")
	inv, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+invoice.String()+":reverse", `{"reason": "Duplicate", "posting_date": "2026-06-12"}`, tag)
	rev := uuid.MustParse(inv["reversal_entry_id"].(string))
	retirement := e.laterRetirement(t, mill["id"])
	e.refuseReversal(t, qm, "", 409, "assets.later_movements")

	item := e.worklistItem(t, entry, "")
	want(t, "the reversed line", fmt.Sprint(item["source_reversed"], " ", item["reversal"].(map[string]any)["entry_id"], " ", reversalOf(item)),
		fmt.Sprint("true ", rev, " line 1 [] offers [1 CNC milling machine:-60.00 EUR, 2 CNC lathe:-30.00 EUR]"))
	want(t, "the reversal's entry", item["reversal"].(map[string]any)["document_number"], e.number(t, rev))
	if e.worklistItem(t, entry, "&remaining=true") == nil {
		t.Errorf("remaining=true: got no item for the reversed line, want it while its reversal's line offers something")
	}
	takeOff := func(asset any, amount string) string {
		if amount == "" {
			return acquisition(asset, rev, 1, "")
		}
		return acquisition(asset, rev, 1, `, "amount": {"amount": "`+amount+`", "currency": "EUR"}`)
	}
	e.clocked.Problem(422, "assets.source_reversed", "POST", "/v1/asset-acquisitions", takeOff(other["id"], ""), "")
	e.clocked.Problem(422, "assets.amount_exceeds_line", "POST", "/v1/asset-acquisitions", takeOff(mill["id"], "-70.00"), "")
	e.clocked.Problem(422, "assets.amount_exceeds_line", "POST", "/v1/asset-acquisitions", takeOff(mill["id"], "50.00"), "")
	e.clocked.Problem(422, "request.validation_failed", "POST", "/v1/asset-acquisitions", acquisition(mill["id"], rev, 1,
		`, "amount": {"amount": "-500.00", "currency": "USD"}`), "")
	e.clocked.Problem(422, "assets.source_reversed", "POST", "/v1/asset-acquisitions", acquisition(mill["id"], entry, 1, ""), "")
	part, _ := e.clocked.Must(201, "POST", "/v1/asset-acquisitions", takeOff(mill["id"], "-50.00"), "")
	want(t, "the take-off", amounts(part), "-50.00 EUR, -50.00 EUR, -50.00 EUR")
	e.clocked.Problem(422, "assets.amount_exceeds_line", "POST", "/v1/asset-acquisitions", takeOff(mill["id"], "-20.00"), "")
	rest, _ := e.clocked.Must(201, "POST", "/v1/asset-acquisitions", takeOff(mill["id"], ""), "")
	want(t, "the rest of the bound", amounts(rest), "-10.00 EUR, -10.00 EUR, -10.00 EUR")
	e.clocked.Problem(422, "assets.amount_exceeds_line", "POST", "/v1/asset-acquisitions", takeOff(mill["id"], ""), "")
	var reverses uuid.UUID
	pgtest.Scan(t, `SELECT reverses_entry_id FROM assets.clearing_use WHERE entry_id = $1`, []any{rev}, &reverses)
	want(t, "the reversal's use names the line reversed", reverses, entry)
	want(t, "the milling machine", e.asset(t, mill["id"]), "active 2026-06-04; 0L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR; "+
		"2L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR")

	item = e.worklistItem(t, entry, "")
	want(t, "the worklist after the take-offs", reversalOf(item), "line 1 [1:-50.00:posted 1:-10.00:posted] offers [2 CNC lathe:-30.00 EUR]")
	e.reverseRetirement(t, mill["id"], retirement)
	e.refuseReversal(t, qm, "", 409, "assets.reversal_line_capitalised")
	e.clocked.Must(201, "POST", "/v1/asset-acquisitions", takeOff(lathe["id"], ""), "")
	if e.worklistItem(t, entry, "&remaining=true") != nil {
		t.Errorf("remaining=true: got the reversed line, want it left out once nothing is offered")
	}
	want(t, "the clearing account", balances(t, e.Tenant.ID, e.Account(t, "2000", "169000")), "0L: 0 0 0; 2L: 0 0 0")
	var all []string
	for token := ""; ; {
		got, page := e.worklist(t, "&page_size=1"+token)
		all = append(all, got...)
		if it := page["items"].([]any); len(it) == 1 && it[0].(map[string]any)["entry_id"] == entry.String() {
			want(t, "the reversed line on its page", reversalOf(it[0].(map[string]any)), "line 1 [1:-50.00:posted 1:-10.00:posted 2:-30.00:posted] offers []")
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		token = "&page_token=" + next
	}
	full, _ := e.worklist(t, "")
	want(t, "the pages", strings.Join(all, "\n"), strings.Join(full, "\n"))

	e.reverse(t, part, "")
	e.reverse(t, rest, "")
	e.reverse(t, qm, "")
	want(t, "the milling machine once its acquisition is reversed", e.asset(t, mill["id"]), "planned <nil>; 0L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR "+
		"0.00 EUR 0.00 EUR; 2L 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR 0.00 EUR")

	untouched := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "40.00"}, line{"100000", "-40.00"})
	_, tag = e.Must(200, "GET", "/v1/journal-entries/"+untouched.String(), "", "")
	back, _ := e.Must(200, "POST", "/v1/journal-entries/"+untouched.String()+":reverse", "", tag)
	e.clocked.Problem(422, "assets.source_reversed", "POST", "/v1/asset-acquisitions", acquisition(lathe["id"], uuid.MustParse(back["id"].(string)), 1, ""), "")
}
