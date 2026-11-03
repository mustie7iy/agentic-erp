package assets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// postPermissions are what the acquisition tests' people hold besides
// reading and changing assets: posting them, and posting, reversing and
// reading journal entries and supplier invoices.
var postPermissions = []string{"assets.asset.post", "ledger.entry.post", "ledger.entry.read", "payables.invoice.capture", "payables.invoice.post", "payables.invoice.read"}

// newAcquisitions is newAssets whose people may post acquisitions and
// journal entries, with average rates GBP/USD 1.27 and USD/EUR 0.92 from
// 2026 (the group currency is EUR).
func newAcquisitions(t *testing.T) assetEnv {
	t.Helper()
	e := newAssets(t, postPermissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'GBP', 'USD', '2026-01-01', 1.27, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.92, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e
}

// line is a manual entry's line: an account of the company by number and
// an amount in the entry's currency.
type line struct{ account, amount string }

// post posts a manual entry of the company (1000 or 2000) on the day in
// the currency, to the ledger when one is given, and returns its id.
func (e assetEnv) post(t *testing.T, company, day, currency string, ledger *uuid.UUID, lines ...line) uuid.UUID {
	t.Helper()
	le := map[string]uuid.UUID{"1000": e.US, "2000": e.DE}[company]
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": %q}}`, e.Account(t, company, l.account), l.amount, currency)
	}
	only := ""
	if ledger != nil {
		only = fmt.Sprintf(`"ledger_id": %q, `, ledger)
	}
	je, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, %s"document_date": %q, "posting_date": %q, "currency": %q,
		"lines": [%s]}`, le, only, day, day, currency, strings.Join(parts, ", ")), "")
	return uuid.MustParse(je["id"].(string))
}

// acquire posts an acquisition on assetDay.
func (e assetEnv) acquire(t *testing.T, asset any, entry uuid.UUID, lineNo int, extra string) map[string]any {
	t.Helper()
	q, _ := e.clocked.Must(201, "POST", "/v1/asset-acquisitions", acquisition(asset, entry, lineNo, extra), "")
	return q
}

func acquisition(asset any, entry uuid.UUID, lineNo int, extra string) string {
	return fmt.Sprintf(`{"asset_id": "%v", "source": {"entry_id": %q, "line_no": %d}%s}`, asset, entry, lineNo, extra)
}

// money is a Money of a representation, as "amount currency".
func moneyOf(v any) string {
	m := v.(map[string]any)
	return fmt.Sprint(m["amount"], " ", m["currency"])
}

// balances are the account's balances per ledger in the transaction,
// local and group currency, as "ledger: tc lc gc", by ledger code.
func balances(t *testing.T, ten uuid.UUID, account uuid.UUID) string {
	t.Helper()
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT g.code, sum(l.amount_tc), sum(l.amount_lc), sum(l.amount_gc) FROM ledger.journal_line l
		JOIN ledger.ledger g ON g.id = l.ledger_id WHERE l.tenant_id = $1 AND l.account_id = $2 GROUP BY g.code ORDER BY g.code`, ten, account)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		var tc, lc, gc int64
		if err := rows.Scan(&code, &tc, &lc, &gc); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s: %d %d %d", code, tc, lc, gc))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "; ")
}

// TestAcquisitionSplitsALine: a GBP line on company 1000's clearing
// account (USD, group EUR) is split onto two assets; each takes its share
// of the local and group amounts, the second the rest, so the clearing
// account is at zero in all three currencies; the assets become active,
// capitalised on the line's posting date; each acquisition reads back,
// is audited and published.
func TestAcquisitionSplitsALine(t *testing.T) {
	e := newAcquisitions(t)
	entry := e.post(t, "1000", "2026-06-10", "GBP", nil, line{"169000", "1000.00"}, line{"100000", "-1000.00"})
	a, _ := e.create(t, e.body(e.US, "MACHINES", ""))
	b, _ := e.create(t, e.body(e.US, "MACHINES", ""))
	first := e.acquire(t, a["id"], entry, 1, `, "amount": {"amount": "333.33", "currency": "GBP"}, "text": "Milling machine, part"`)
	want(t, "the first share", fmt.Sprint(moneyOf(first["amount"]), ", ", moneyOf(first["amount_lc"]), ", ", moneyOf(first["amount_gc"])),
		"333.33 GBP, 423.33 USD, 389.46 EUR")
	second := e.acquire(t, b["id"], entry, 1, "")
	want(t, "the rest", fmt.Sprint(moneyOf(second["amount"]), ", ", moneyOf(second["amount_lc"]), ", ", moneyOf(second["amount_gc"])),
		"666.67 GBP, 846.67 USD, 778.94 EUR")
	want(t, "the clearing account", balances(t, e.Tenant.ID, e.Account(t, "1000", "169000")), "0L: 0 0 0")
	want(t, "the cost account", balances(t, e.Tenant.ID, e.Account(t, "1000", "160000")), "0L: 100000 127000 116840")

	want(t, "the first acquisition", fmt.Sprint(first["company_code"], " ", first["asset_number"], " ", first["asset_description"], " ", first["status"], " ",
		first["value_date"], " ", first["posting_date"], " ", first["fiscal_year"], " ", first["text"]),
		"1000 1 CNC milling machine posted 2026-06-10 2026-06-15 2026 Milling machine, part")
	src := first["source"].(map[string]any)
	want(t, "its source", fmt.Sprint(src["entry_id"], " ", src["line_no"], " ", src["fiscal_year"], " ", src["source_context"], " ", src["supplier_invoice"]),
		fmt.Sprint(entry, " 1 2026 ledger <nil>"))
	entries := first["entries"].([]any)
	want(t, "its entries", len(entries), 1)
	en := entries[0].(map[string]any)
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+en["journal_entry_id"].(string), "", "")
	want(t, "its entry", fmt.Sprint(en["ledger_code"], " ", en["document_number"], " ", je["document_type"], " ", je["source_context"], " ", je["currency"]),
		fmt.Sprint("0L ", je["document_number"], " AA assets GBP"))
	got, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+first["id"].(string), "", "")
	want(t, "read back", fmt.Sprint(got), fmt.Sprint(first))
	want(t, "ETag", tag, `"1"`)

	asset, _ := e.Must(200, "GET", "/v1/assets/"+a["id"].(string), "", "")
	want(t, "the asset", fmt.Sprint(asset["status"], " ", asset["capitalised_on"], " ", asset["version"]), "active 2026-06-10 2")
	var seq, last int
	pgtest.Scan(t, `SELECT q.seq, a.last_seq FROM assets.acquisition q JOIN assets.asset a ON a.id = q.asset_id WHERE q.id = $1`, []any{first["id"]}, &seq, &last)
	want(t, "the acquisition's seq and the asset's", fmt.Sprint(seq, " ", last), "2 2")
	var movement string
	pgtest.Scan(t, `SELECT concat_ws(' ', kind, cost, cost_gc, accumulated, value_date, fiscal_period, seq) FROM assets.movement WHERE document_id = $1`,
		[]any{first["id"]}, &movement)
	want(t, "the movement", movement, "acquisition 42333 38946 0 2026-06-10 6 2")

	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_posted"})
	want(t, "events", len(evs), 2)
	ledgers := evs[0]["ledgers"].([]any)[0].(map[string]any)
	want(t, "the first event", fmt.Sprint(evs[0]["document_kind"], " ", evs[0]["asset_number"], " ", evs[0]["asset_status"], " ", evs[0]["asset_version"], " ",
		moneyOf(ledgers["cost"]), " ", moneyOf(ledgers["cost_gc"])), "acquisition 1 active 2 423.33 USD 389.46 EUR")
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'assets.acquisition.post'`, []any{e.Tenant.ID}, &audits)
	want(t, "audit records", audits, 2)
	e.clocked.Problem(422, "assets.amount_exceeds_line", "POST", "/v1/asset-acquisitions", acquisition(a["id"], entry, 1, ""), "")
}

// TestAcquisitionInEveryLedger: company 2000 keeps 0L and 2L; an
// acquisition posts to both, with a movement and an entry in each, the
// cost line on the asset's profit center, and a later one keeps the
// earliest value date as the capitalisation date.
func TestAcquisitionInEveryLedger(t *testing.T) {
	e := newAcquisitions(t)
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "500.00"}, line{"100000", "-500.00"})
	later := e.post(t, "2000", "2026-06-12", "EUR", nil, line{"169000", "100.00"}, line{"100000", "-100.00"})
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	q := e.acquire(t, a["id"], later, 1, `, "value_date": "2026-06-30"`)
	e.acquire(t, a["id"], entry, 1, `, "value_date": "2026-05-20"`)
	var codes []string
	for _, en := range q["entries"].([]any) {
		codes = append(codes, en.(map[string]any)["ledger_code"].(string))
	}
	want(t, "the entries", strings.Join(codes, " "), "0L 2L")
	want(t, "the clearing account", balances(t, e.Tenant.ID, e.Account(t, "2000", "169000")), "0L: 0 0 0; 2L: 0 0 0")
	var movements, cost int
	pgtest.Scan(t, `SELECT count(*), count(DISTINCT ledger_id) FROM assets.movement WHERE asset_id = $1`, []any{a["id"]}, &movements, &cost)
	want(t, "movements in ledgers", fmt.Sprint(movements, " ", cost), "4 2")
	var pc uuid.UUID
	pgtest.Scan(t, `SELECT profit_center_id FROM ledger.journal_line WHERE journal_entry_id = $1 AND amount_tc > 0`,
		[]any{q["entries"].([]any)[0].(map[string]any)["journal_entry_id"]}, &pc)
	want(t, "the cost line's profit center", pc, e.c.Profit)
	asset, _ := e.Must(200, "GET", "/v1/assets/"+a["id"].(string), "", "")
	want(t, "the asset", fmt.Sprint(asset["status"], " ", asset["capitalised_on"]), "active 2026-05-20")
}

// TestAcquisitionRefused: lines that are not the asset's class's clearing
// lines of the leading ledger in its company, reversed lines and their
// reversals, ledger-specific lines, amounts beyond the line or of the
// other sign, value dates after the posting date's period, postings
// before the asset's latest center assignment or in a period not open,
// assets that take no documents, and reductions below zero cost are
// refused; a reduction within the asset's cost is a negative acquisition.
func TestAcquisitionRefused(t *testing.T) {
	e := newAcquisitions(t)
	cc := fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)
	good := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "500.00"}, line{"100000", "-500.00"})
	us := e.post(t, "1000", "2026-06-10", "USD", nil, line{"169000", "500.00"}, line{"100000", "-500.00"})
	only := e.post(t, "2000", "2026-06-10", "EUR", &e.l0, line{"169000", "500.00"}, line{"100000", "-500.00"})
	reversed := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "500.00"}, line{"100000", "-500.00"})
	_, jeTag := e.Must(200, "GET", "/v1/journal-entries/"+reversed.String(), "", "")
	rev, _ := e.Must(200, "POST", "/v1/journal-entries/"+reversed.String()+":reverse", "", jeTag)
	credit := e.post(t, "2000", "2026-06-11", "EUR", nil, line{"100000", "50.00"}, line{"169000", "-50.00"})
	var copy2L uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.journal_entry WHERE posting_group_id = $1 AND id <> $1`, []any{good}, &copy2L)
	asset, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	id := asset["id"]
	cancelled, tag := e.create(t, e.body(e.DE, "MACHINES", cc))
	e.Must(200, "POST", "/v1/assets/"+cancelled["id"].(string)+":cancel", "", tag)
	retired, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	pgtest.Exec(t, `UPDATE assets.asset SET status = 'active', capitalised_on = '2026-06-01', version = version + 1 WHERE id = $1`, retired["id"])
	pgtest.Exec(t, `UPDATE assets.asset SET status = 'retired', version = version + 1 WHERE id = $1`, retired["id"])
	moved, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = 2, version = version + 1 WHERE id = $1`, moved["id"])
	pgtest.Exec(t, `INSERT INTO assets.asset_assignment (tenant_id, asset_id, seq, valid_from, cost_center_id, profit_center_id, created_by)
		VALUES ($1, $2, 2, '2026-06-20', $3, $4, $5)`, e.Tenant.ID, moved["id"], e.c.Admin, e.c.Profit, e.Tenant.Human.ID)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 7`, e.Tenant.ID)
	for name, c := range map[string]struct {
		body        string
		status      int
		code, field string
	}{
		"another account's line":             {acquisition(id, good, 2, ""), 422, "assets.not_clearing_line", "source"},
		"a line the entry has not":           {acquisition(id, good, 9, ""), 422, "assets.not_clearing_line", "source"},
		"another company's line":             {acquisition(id, us, 1, ""), 422, "assets.not_clearing_line", "source"},
		"a parallel ledger's copy":           {acquisition(id, copy2L, 1, ""), 422, "assets.not_clearing_line", "source"},
		"a reversed line":                    {acquisition(id, reversed, 1, ""), 422, "assets.source_reversed", "source"},
		"a reversal's line":                  {acquisition(id, uuid.MustParse(rev["id"].(string)), 1, ""), 422, "assets.source_reversed", "source"},
		"a line of the leading ledger alone": {acquisition(id, only, 1, ""), 422, "assets.source_not_in_every_ledger", "source"},
		"more than the line":                 {acquisition(id, good, 1, `, "amount": {"amount": "500.01", "currency": "EUR"}`), 422, "assets.amount_exceeds_line", "amount"},
		"the other sign":                     {acquisition(id, good, 1, `, "amount": {"amount": "-1.00", "currency": "EUR"}`), 422, "assets.amount_exceeds_line", "amount"},
		"zero":                               {acquisition(id, good, 1, `, "amount": {"amount": "0.00", "currency": "EUR"}`), 422, "assets.amount_exceeds_line", "amount"},
		"another currency":                   {acquisition(id, good, 1, `, "amount": {"amount": "1.00", "currency": "USD"}`), 422, "request.validation_failed", "amount.currency"},
		"an amount of three decimals":        {acquisition(id, good, 1, `, "amount": {"amount": "1.001", "currency": "EUR"}`), 400, "request.invalid_amount", "amount"},
		"an unknown currency":                {acquisition(id, good, 1, `, "amount": {"amount": "1.00", "currency": "XXX"}`), 400, "request.invalid_amount", "amount"},
		"a value date after the period":      {acquisition(id, good, 1, `, "value_date": "2026-07-01"`), 422, "assets.value_date_in_future", "value_date"},
		"a period not open":                  {acquisition(id, good, 1, `, "posting_date": "2026-07-15"`), 409, "ledger.period_not_open", ""},
		"no fiscal year":                     {acquisition(id, good, 1, `, "posting_date": "1990-01-15"`), 422, "ledger.no_fiscal_period", ""},
		"a cancelled asset":                  {acquisition(cancelled["id"], good, 1, ""), 422, "assets.asset_cancelled", ""},
		"a retired asset":                    {acquisition(retired["id"], good, 1, ""), 422, "assets.asset_retired", ""},
		"before the latest assignment":       {acquisition(moved["id"], good, 1, ""), 409, "assets.posting_before_center_transfer", ""},
		"a reduction of no cost":             {acquisition(id, credit, 2, ""), 422, "assets.cost_negative", "amount"},
		"an unknown asset":                   {acquisition(uuid.Must(uuid.NewV7()), good, 1, ""), 404, "request.not_found", ""},
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
	e.acquire(t, moved["id"], good, 1, `, "amount": {"amount": "30.00", "currency": "EUR"}, "posting_date": "2026-06-20"`)
	e.clocked.Problem(422, "assets.cost_negative", "POST", "/v1/asset-acquisitions", acquisition(moved["id"], credit, 2, `, "posting_date": "2026-06-20"`), "")
	e.acquire(t, id, good, 1, `, "amount": {"amount": "200.00", "currency": "EUR"}`)
	reduction := e.acquire(t, id, credit, 2, "")
	want(t, "a negative acquisition", fmt.Sprint(moneyOf(reduction["amount"]), " ", moneyOf(reduction["amount_lc"])), "-50.00 EUR -50.00 EUR")
	var cost int64
	pgtest.Scan(t, `SELECT sum(cost) FROM assets.movement WHERE asset_id = $1 AND ledger_id = $2`, []any{id, e.l2}, &cost)
	want(t, "the cost after the reduction", cost, 15000)
	own := uuid.MustParse(reduction["entries"].([]any)[0].(map[string]any)["journal_entry_id"].(string))
	e.clocked.Problem(422, "assets.not_clearing_line", "POST", "/v1/asset-acquisitions", acquisition(id, own, 2, ""), "")

	reader := e.clocked
	reader.As = e.Reader(t, "reader", e.US)
	reader.Problem(404, "request.not_found", "POST", "/v1/asset-acquisitions", acquisition(id, good, 1, ""), "")
	reader.Problem(404, "request.not_found", "GET", "/v1/asset-acquisitions/"+reduction["id"].(string), "", "")
}

// TestCancelAssetWithMovements: a planned asset with movements (an
// acquisition reversed, T57d) is kept, not cancelled.
func TestCancelAssetWithMovements(t *testing.T) {
	e := newAcquisitions(t)
	a, tag := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	pgtest.Exec(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, cost, document_kind, document_id, created_by)
		VALUES ($1, gen_random_uuid(), $2, 2, $3, $4, 'MACHINES', 'acquisition', '2026-06-01', '2026-06-01', 2026, 6, 0, 'acquisition', gen_random_uuid(), $5)`,
		e.Tenant.ID, a["id"], e.DE, e.l0, e.Tenant.Human.ID)
	p := e.Problem(409, "assets.asset_not_planned", "POST", "/v1/assets/"+a["id"].(string)+":cancel", "", tag)
	if !strings.Contains(p["detail"].(string), "has movements") {
		t.Errorf("detail: got %v, want the movements named", p["detail"])
	}
}
