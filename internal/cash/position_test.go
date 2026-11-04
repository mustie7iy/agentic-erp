package cash_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// readAt is the position's clock: Friday 17 April 2026. The working day
// before it is Thursday the 16th; Monday the 20th is Maple Inc's holiday.
var readAt = time.Date(2026, 4, 17, 15, 0, 0, 0, time.UTC)

// withPosition is Maple Inc (fixture.Maple: a CAD company with a CAD house
// bank MAIN and a USD house bank USD) keeping a parallel ledger 2L, with
// its USD bank's payments a day at the bank, a USD 5,000.00 credit line
// on it, and a holiday on Monday 20 April.
func withPosition(doc *config.Config) {
	fixture.Maple(doc)
	doc.Ledgers = []config.Ledger{{Code: "0L", Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "2L", Name: "Local", AccountingPrinciple: "ASPE"}}
	co := doc.Company("3000")
	co.Ledgers = []string{"0L", "2L"}
	co.Cash = config.CompanyCash{ValueDays: []config.ValueDays{{HouseBank: "USD", Days: 1}}, CreditLines: []config.CreditLine{{HouseBank: "USD", Amount: "5000.00"}}}
	doc.Calendars = append(doc.Calendars, config.Calendar{Company: "3000", Holidays: []config.Holiday{{Date: "2026-04-20", Name: "Bank holiday"}}})
}

// positionEnv is Maple Inc with its accounts, banks and ledgers, the API
// reading at readAt, and an admin connection that writes the position's
// sources directly.
type positionEnv struct {
	fixture.Env
	le, usd, main                               uuid.UUID
	clearing, bankAccount, cadClearing, expense uuid.UUID
	leading, parallel                           uuid.UUID
	admin                                       *pgx.Conn
}

func setupPosition(t *testing.T) positionEnv {
	t.Helper()
	return setupPositionWith(t, withPosition, "cash.position.read")
}

// setupPositionWith is setupPosition with the document extended by extend
// (withPosition and more) and the people holding the permissions.
func setupPositionWith(t *testing.T, extend func(*config.Config), permissions ...string) positionEnv {
	t.Helper()
	return setupPositionOf(t, nil, extend, permissions...)
}

// setupPositionOf is setupPositionWith with further legal entities beside
// Maple Inc, which extend configures.
func setupPositionOf(t *testing.T, more []fixture.Company, extend func(*config.Config), permissions ...string) positionEnv {
	t.Helper()
	f := fixture.NewWith(t, append([]fixture.Company{fixture.MapleCompany}, more...), extend, permissions...)
	f.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return readAt } })
	e := positionEnv{Env: f, admin: pgtest.Admin(t), clearing: f.Account(t, "3000", "102000"), bankAccount: f.Account(t, "3000", "103000"),
		cadClearing: f.Account(t, "3000", "100000"), expense: f.Account(t, "3000", "520000")}
	pgtest.Scan(t, `SELECT le.id, u.id, m.id, l0.id, l2.id FROM tenancy.legal_entity le
		JOIN payments.house_bank u ON u.legal_entity_id = le.id AND u.code = 'USD' JOIN payments.house_bank m ON m.legal_entity_id = le.id AND m.code = 'MAIN'
		JOIN ledger.ledger l0 ON l0.tenant_id = le.tenant_id AND l0.code = '0L' JOIN ledger.ledger l2 ON l2.tenant_id = le.tenant_id AND l2.code = '2L'
		WHERE le.tenant_id = $1 AND le.code = '3000'`, []any{f.Tenant.ID}, &e.le, &e.usd, &e.main, &e.leading, &e.parallel)
	e.rate(t, "average", "USD", "CAD", "2026-01-01", "1.35")
	return e
}

func (e positionEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func (e positionEnv) rate(t *testing.T, rateType, from, to, day, rate string) {
	t.Helper()
	e.exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, $6, 'BOC', $7)`, e.Tenant.ID, rateType, from, to, day, rate, e.Tenant.Human.ID)
}

// entry writes a balanced journal entry straight into a ledger: amount on
// the account against the expense account, without the checks a posting
// runs, posted for the source's document.
func (e positionEnv) entry(t *testing.T, ledger, account uuid.UUID, day, currency string, tc, lc int64, source string, document *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `SET session_replication_role = replica`)
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by)
		VALUES ($1, 2026, $2, $3, $4, extract(month FROM $5::date), 'ZP', $10, $5, $5, $6, 'CAD', 'EUR', 1, 1, $7, $8, $9)`,
		e.Tenant.ID, id, e.le, ledger, day, currency, source, document, e.Tenant.Human.ID, id.String())
	e.exec(t, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc)
		SELECT $1, 2026, $2, n, $3, $4, extract(month FROM $5::date), $5, CASE WHEN n = 1 THEN $6::uuid ELSE $7::uuid END,
			CASE WHEN n = 1 THEN $8::bigint ELSE -$8::bigint END, CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END, CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END
		FROM generate_series(1, 2) n`, e.Tenant.ID, id, e.le, ledger, day, account, e.expense, tc, lc)
	return id
}

// run writes a payment run of the bank in a status, decided by the
// approver once released.
func (e positionEnv) run(t *testing.T, bank uuid.UUID, day, currency, status string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	var decided any
	if status == "released" {
		decided = e.Approver.ID
	}
	e.exec(t, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status,
			proposed_by, decided_by, decided_at, created_by, updated_by)
		VALUES ($1, $2, $10, $3, $4, $5, $5, $6, 0, $7, $8, $9::uuid, CASE WHEN $9::uuid IS NULL THEN NULL ELSE now() END, $8, $8)`,
		e.Tenant.ID, id, e.le, bank, day, currency, status, e.Tenant.Human.ID, decided, id.String()[24:])
	return id
}

// payment writes a payment of the run, posted when the entry is given.
func (e positionEnv) payment(t *testing.T, run uuid.UUID, amount int64, currency string, entry *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	var year any
	if entry != nil {
		year = 2026
	}
	e.exec(t, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id,
			journal_entry_year, journal_entry_id, created_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, $6, $10, $7, $8, $9)`,
		e.Tenant.ID, id, run, e.Supplier, amount, currency, year, entry, e.Tenant.Human.ID, endToEnd(id))
	return id
}

// released is a released payment from the USD bank on the run date,
// posted to the clearing account in the leading ledger and copied to the
// parallel one, as a payment's posting goes to every ledger (ADR-0045).
func (e positionEnv) released(t *testing.T, day string, amount int64) (payment, entry uuid.UUID) {
	t.Helper()
	payment = uuid.Must(uuid.NewV7())
	entry = e.entry(t, e.leading, e.clearing, day, "USD", -amount, -amount*135/100, "payments", &payment)
	e.entry(t, e.parallel, e.clearing, day, "USD", -amount, -amount*135/100, "payments", &payment)
	run := e.run(t, e.usd, day, "USD", "released")
	e.exec(t, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id,
			journal_entry_year, journal_entry_id, created_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, 'USD', $8, 2026, $6, $7)`,
		e.Tenant.ID, payment, run, e.Supplier, amount, entry, e.Tenant.Human.ID, endToEnd(payment))
	return payment, entry
}

func endToEnd(id uuid.UUID) string { return strings.ReplaceAll(id.String(), "-", "")[:32] }

// statement writes a statement of the bank.
func (e positionEnv) statement(t *testing.T, bank uuid.UUID, currency, opened, closed string, opening, closing int64, prev *uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO payments.bank_statement (tenant_id, id, house_bank_id, legal_entity_id, format, file_sha256, file_position, file_ref, statement_key,
			statement_ref, sequence, account, currency, opening_balance, opening_date, closing_balance, closing_date, line_count, previous_statement_id, status,
			created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'camt.053.001.02', sha256($13::bytea), 1, 'statements/' || $13, $13, $13, '1', 'ACCOUNT', $5, $6, $7, $8, $9, 0,
			$10, $11, $12, $12)`, e.Tenant.ID, id, bank, e.le, currency, opening, opened, closing, closed, prev, status, e.Tenant.Human.ID, id.String())
	return id
}

// line writes a statement line in a status, matched to the payment when
// one is given.
func (e positionEnv) line(t *testing.T, statement uuid.UUID, no int, day string, amount int64, status string, payment *uuid.UUID) {
	t.Helper()
	e.exec(t, `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code, bank_reference,
			owner_reference, text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $5, 'PMNT-ICDT-ESCT', 'B', 'O', 'T', $6, $7, $7)`, e.Tenant.ID, statement, no, amount, day, status, e.Tenant.Human.ID)
	if payment == nil {
		return
	}
	match := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, rule, reason, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'rule', 'end_to_end_id', 'the end-to-end id', 'matched', $5, $5)`, e.Tenant.ID, match, statement, no, e.Tenant.Human.ID)
	e.exec(t, `INSERT INTO payments.statement_match_target (tenant_id, match_id, candidate, target_no, payment_id, created_by) VALUES ($1, $2, 1, 1, $3, $4)`,
		e.Tenant.ID, match, *payment, e.Tenant.Human.ID)
}

// scenario writes Maple Inc's sources:
//   - USD: statements of 14 April (USD 5,000.00) and 16 April (opening
//     4,900.00, closing 4,800.00: a gap) with an unmatched line of -100.00
//     and one of -300.00 matched to a payment awaiting its posting's
//     approval; USD 4,850.00 on the main bank account by 10 April, in both
//     ledgers; payments in transit of 1,000.00 and 150.00 (run on the
//     17th, at the bank the 21st after the weekend and the holiday) and of 200.00 (run
//     on the 14th: late, also the 21st), the booked one of 300.00, one of
//     50.00 reversed and one of 70.00 cleared, all on the clearing account,
//     where a revaluation's CAD line of 25.00 is open too;
//   - MAIN (CAD): a statement of 15 April (CAD 10,000.00), stale, and a
//     proposed run paying CAD 500.00 on the 17th.
func (e positionEnv) scenario(t *testing.T) {
	t.Helper()
	first := e.statement(t, e.usd, "USD", "2026-04-13", "2026-04-14", 500000, 500000, nil, "imported")
	second := e.statement(t, e.usd, "USD", "2026-04-15", "2026-04-16", 490000, 480000, &first, "gap")
	e.entry(t, e.leading, e.bankAccount, "2026-04-10", "USD", 485000, 654750, "ledger", nil)
	e.entry(t, e.parallel, e.bankAccount, "2026-04-10", "USD", 485000, 654750, "ledger", nil)
	e.released(t, "2026-04-17", 100000)
	e.released(t, "2026-04-17", 15000)
	e.released(t, "2026-04-14", 20000)
	booked, _ := e.released(t, "2026-04-15", 30000)
	e.line(t, second, 1, "2026-04-16", -10000, "unmatched", nil)
	e.line(t, second, 2, "2026-04-16", -30000, "pending_approval", &booked)
	_, reversed := e.released(t, "2026-04-15", 5000)
	_, cleared := e.released(t, "2026-04-15", 7000)
	// The reversal and the clearing name entries and clearings not written.
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `INSERT INTO ledger.reversal (tenant_id, original_year, original_entry_id, reversal_year, reversal_entry_id) VALUES ($1, 2026, $2, 2026, gen_random_uuid())`,
		e.Tenant.ID, reversed)
	e.exec(t, `INSERT INTO ledger.clearing_item (tenant_id, fiscal_year, journal_entry_id, line_no, clearing_id) VALUES ($1, 2026, $2, 1, gen_random_uuid())`,
		e.Tenant.ID, cleared)
	e.exec(t, `RESET session_replication_role`)
	e.entry(t, e.leading, e.clearing, "2026-03-31", "CAD", 2500, 2500, "ledger", nil)
	e.statement(t, e.main, "CAD", "2026-04-15", "2026-04-15", 1000000, 1000000, nil, "imported")
	e.payment(t, e.run(t, e.main, "2026-04-17", "CAD", "proposed"), 50000, "CAD", nil)
}

// positionOf reads the position as the human.
func (e positionEnv) positionOf(t *testing.T, query string) map[string]any {
	t.Helper()
	p, _ := e.Must(200, "GET", "/v1/cash-position?legal_entity_id="+e.le.String()+query, "", "")
	return p
}

// days renders days as "date opening>closing (available) flows".
func days(t *testing.T, v any) []string {
	t.Helper()
	var out []string
	for _, d := range v.([]any) {
		m := d.(map[string]any)
		s := fmt.Sprintf("%s %s>%s (%s)", m["date"], amount(m["opening"]), amount(m["closing"]), amount(m["available"]))
		flows, _ := m["flows"].([]any)
		for _, f := range flows {
			fm := f.(map[string]any)
			s += fmt.Sprintf(" %s/%s/late=%v %s x%v", fm["source"], fm["certainty"], fm["late"], amount(fm["amount"]), fm["count"])
		}
		out = append(out, s)
	}
	return out
}

func amount(v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return "-"
	}
	return fmt.Sprint(m["amount"], " ", m["currency"])
}

func want(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s:\n got %v\nwant %v", what, got, want)
	}
}

// bankOf finds a bank by house_bank_code; the deprecated code it still
// gives within /v1 must hold the same value (ADR-0038 As built).
func bankOf(t *testing.T, p map[string]any, code string) map[string]any {
	t.Helper()
	for _, b := range p["banks"].([]any) {
		if m := b.(map[string]any); m["house_bank_code"] == code {
			if m["code"] != code {
				t.Errorf("bank %s: deprecated code got %v, want %s", code, m["code"], code)
			}
			return m
		}
	}
	t.Fatalf("bank %s: not in %v", code, p["banks"])
	return nil
}

// TestCashPosition: Maple Inc's position per house bank, in its currency,
// and day (ADR-0051 §8): the USD bank opens at its latest statement, which
// is in gap but not stale, its payments in transit land on the next
// working day after their value day, late ones the working day after the
// read, and neither the booked, reversed or cleared payments nor the
// revaluation's line nor the parallel ledger's copies count; the CAD bank
// opens stale with a proposed run's payment; the book balance check shows
// the statement against the leading ledger; the currencies are summed and
// converted into CAD.
func TestCashPosition(t *testing.T) {
	e := setupPosition(t)
	e.scenario(t)
	p := e.positionOf(t, "&days=5")
	want(t, "header", fmt.Sprint(p["read_date"], " ", p["time_zone"], " ", p["from"], " ", p["days"]), "2026-04-17 UTC 2026-04-17 5")

	usd := bankOf(t, p, "USD")
	want(t, "USD bank", fmt.Sprint(usd["currency"], " ", usd["account_last4"], " ", amount(usd["credit_line"])), "USD 0001 5000.00 USD")
	want(t, "USD opening", usd["opening"], map[string]any{"balance": map[string]any{"amount": "4800.00", "currency": "USD"}, "gap": true, "stale": false,
		"statement_date": "2026-04-16"})
	want(t, "USD days", days(t, usd["days"]), []string{
		"2026-04-17 4800.00 USD>4800.00 USD (9800.00 USD)", "2026-04-18 4800.00 USD>4800.00 USD (9800.00 USD)",
		"2026-04-19 4800.00 USD>4800.00 USD (9800.00 USD)", "2026-04-20 4800.00 USD>4800.00 USD (9800.00 USD)",
		"2026-04-21 4800.00 USD>3450.00 USD (8450.00 USD) in_transit/confirmed/late=false -1150.00 USD x2 in_transit/confirmed/late=true -200.00 USD x1"})
	book := usd["book_balance"].(map[string]any)
	figures := book["figures"].(map[string]any)
	want(t, "USD book balance", fmt.Sprint(figures["as_of"], " closing ", amount(figures["closing_balance"]), " ledger ", amount(figures["ledger_balance"]),
		" not posted ", amount(figures["in_transit"]), " unexplained ", amount(figures["unexplained"]), " payments in transit ", amount(book["payments_in_transit"])),
		"2026-04-16 closing 4800.00 USD ledger 4850.00 USD not posted -400.00 USD unexplained 350.00 USD payments in transit -1350.00 USD")

	main := bankOf(t, p, "MAIN")
	want(t, "MAIN opening", main["opening"], map[string]any{"balance": map[string]any{"amount": "10000.00", "currency": "CAD"}, "gap": false, "stale": true,
		"statement_date": "2026-04-15"})
	want(t, "MAIN days", days(t, main["days"])[:2], []string{
		"2026-04-17 10000.00 CAD>9500.00 CAD (9500.00 CAD) payment_run/confirmed/late=false -500.00 CAD x1", "2026-04-18 9500.00 CAD>9500.00 CAD (9500.00 CAD)"})
	want(t, "MAIN has no credit line nor ledger balance", fmt.Sprint(amount(main["credit_line"]), " ",
		amount(main["book_balance"].(map[string]any)["figures"].(map[string]any)["ledger_balance"])), "- -")

	cur := p["currencies"].([]any)
	want(t, "currencies", fmt.Sprint(cur[0].(map[string]any)["currency"], " ", cur[1].(map[string]any)["currency"], " ", amount(cur[1].(map[string]any)["credit_line"])),
		"CAD USD 5000.00 USD")
	want(t, "CAD days", days(t, cur[0].(map[string]any)["days"])[0], "2026-04-17 10000.00 CAD>9500.00 CAD (9500.00 CAD) payment_run/confirmed/late=false -500.00 CAD x1")

	conv := p["converted"].(map[string]any)
	// No planning rate: the average of 1 January, older than 7 days.
	want(t, "rates", conv["rates"], []any{map[string]any{"currency": "USD", "rate": "1.350000000000", "rate_type": "average", "stale": true, "valid_from": "2026-01-01"}})
	want(t, "converted days", []string{days(t, conv["days"])[0], days(t, conv["days"])[4]}, []string{
		"2026-04-17 16480.00 CAD>15980.00 CAD (22730.00 CAD)", "2026-04-21 15980.00 CAD>14157.50 CAD (20907.50 CAD)"})
	want(t, "complete", conv["incomplete_currencies"], []any{})
}

// TestCashPositionConversion: the company's planning rate, when there is
// one, converts the totals, and is fresh; a currency without any rate into
// the display currency is left out and named; an unknown display currency
// is refused.
func TestCashPositionConversion(t *testing.T) {
	e := setupPosition(t)
	e.scenario(t)
	e.rate(t, "planning", "CAD", "USD", "2026-04-16", "0.8")
	p := e.positionOf(t, "&days=1&display_currency=USD")
	conv := p["converted"].(map[string]any)
	want(t, "rates", conv["rates"], []any{map[string]any{"currency": "CAD", "rate": "0.800000000000", "rate_type": "planning", "stale": false, "valid_from": "2026-04-16"}})
	want(t, "converted", days(t, conv["days"]), []string{"2026-04-17 12800.00 USD>12400.00 USD (17400.00 USD)"})

	p = e.positionOf(t, "&days=1&display_currency=EUR")
	conv = p["converted"].(map[string]any)
	want(t, "incomplete", fmt.Sprint(conv["incomplete_currencies"], " ", conv["rates"], " ", days(t, conv["days"])),
		"[CAD USD] [] [2026-04-17 0.00 EUR>0.00 EUR (0.00 EUR)]")
	e.Problem(422, "cash.unknown_currency", "GET", "/v1/cash-position?legal_entity_id="+e.le.String()+"&display_currency=XXZ", "", "")
}

// TestCashPositionDays: a later first day opens with the flows before it;
// a first day before the read date is refused; the working day before a
// Monday is the Friday; days are 1 to 31.
func TestCashPositionDays(t *testing.T) {
	e := setupPosition(t)
	e.scenario(t)
	usd := bankOf(t, e.positionOf(t, "&from=2026-04-22&days=1"), "USD")
	want(t, "later first day", days(t, usd["days"]), []string{"2026-04-22 3450.00 USD>3450.00 USD (8450.00 USD)"})
	want(t, "later first day not stale", usd["opening"].(map[string]any)["stale"], false)

	e.Problem(422, "request.validation_failed", "GET", "/v1/cash-position?legal_entity_id="+e.le.String()+"&from=2026-04-16", "", "")

	// Read on Monday the 13th, Friday the 10th's statement is fresh, the
	// 9th's stale.
	monday := e.Client
	monday.API = apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Now = func() time.Time { return time.Date(2026, 4, 13, 9, 0, 0, 0, time.UTC) }
	})
	stale := func() any {
		p, _ := monday.Must(200, "GET", "/v1/cash-position?days=1&legal_entity_id="+e.le.String(), "", "")
		return bankOf(t, p, "USD")["opening"].(map[string]any)["stale"]
	}
	e.statement(t, e.usd, "USD", "2026-04-09", "2026-04-09", 500000, 500000, nil, "imported")
	want(t, "Monday after Thursday's statement", stale(), true)
	e.statement(t, e.usd, "USD", "2026-04-10", "2026-04-10", 500000, 500000, nil, "imported")
	want(t, "Monday after Friday's statement", stale(), false)

	p := e.positionOf(t, "")
	want(t, "default days", len(p["banks"].([]any)[0].(map[string]any)["days"].([]any)), 14)
	for _, q := range []string{"&days=0", "&days=32"} {
		if code, _, _ := e.Do("GET", "/v1/cash-position?legal_entity_id="+e.le.String()+q, "", ""); code != 400 {
			t.Errorf("%s: got %d, want 400", q, code)
		}
	}
}

// TestCashPositionTimeZone: the read date is the company's date in its
// time zone (T72n1). At 22:00 on Friday 17 April in Toronto it is already
// the 18th in UTC; at 01:00 on the 17th in Dubai it is still the 16th. In
// both the read date is the 17th, shown with the zone: the position starts
// on it by default, the 16th is refused as past, and Thursday's statement
// is not stale. Read in UTC, Toronto's would have started on Saturday with
// the statement stale, and Dubai's on the 16th.
func TestCashPositionTimeZone(t *testing.T) {
	for zone, at := range map[string]time.Time{
		"America/Toronto": time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC),  // 22:00 EDT on the 17th
		"Asia/Dubai":      time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC), // 01:00 GST on the 17th
	} {
		t.Run(zone, func(t *testing.T) {
			e := setupPositionWith(t, func(doc *config.Config) {
				withPosition(doc)
				doc.Company("3000").TimeZone = zone
			}, "cash.position.read")
			e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return at } })
			e.scenario(t)
			p := e.positionOf(t, "&days=1")
			want(t, "header", fmt.Sprint(p["read_date"], " ", p["time_zone"], " ", p["from"]), "2026-04-17 "+zone+" 2026-04-17")
			usd := bankOf(t, p, "USD")
			want(t, "Thursday's statement on Friday", usd["opening"].(map[string]any)["stale"], false)
			want(t, "the first day", days(t, usd["days"]), []string{"2026-04-17 4800.00 USD>4800.00 USD (9800.00 USD)"})
			e.Problem(422, "request.validation_failed", "GET", "/v1/cash-position?legal_entity_id="+e.le.String()+"&from=2026-04-16", "", "")
		})
	}
}

// zoneLost is the active configuration with company 3000's time zone one
// this build cannot load, as a document stored under a zone database that
// later lost the name would read (validation refuses it).
type zoneLost struct{ cash.Settings }

func (z zoneLost) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, n, err := z.Settings.Document(ctx)
	if err == nil {
		doc.Company("3000").TimeZone = "America/Nowhere"
	}
	return doc, n, err
}

// TestCashPositionZoneLost: a company whose stored zone does not load
// fails the read rather than reading the date in UTC.
func TestCashPositionZoneLost(t *testing.T) {
	e := setupPosition(t)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	endpoints.Settings = zoneLost{endpoints.Settings}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints.Position(ctx, cash.Query{LegalEntity: e.le})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "America/Nowhere") {
		t.Errorf("got %v, want the company's zone refused", err)
	}
}

// TestCashPositionWithoutStatements: a bank without a statement opens at
// zero, stale, with no book balance check; a company without house banks
// has an empty position.
func TestCashPositionWithoutStatements(t *testing.T) {
	e := setupPosition(t)
	p := e.positionOf(t, "&days=1")
	usd := bankOf(t, p, "USD")
	want(t, "opening", usd["opening"], map[string]any{"balance": map[string]any{"amount": "0.00", "currency": "USD"}, "gap": false, "stale": true})
	want(t, "book balance", usd["book_balance"], nil)

	pgtest.Exec(t, `UPDATE payments.house_bank SET status = 'inactive', active_gl_account_id = NULL, version = version + 1 WHERE legal_entity_id = $1`, e.le)
	p = e.positionOf(t, "&days=1")
	want(t, "no banks", fmt.Sprint(p["banks"], " ", p["currencies"], " ", days(t, p["converted"].(map[string]any)["days"])), "[] [] [2026-04-17 0.00 CAD>0.00 CAD (0.00 CAD)]")
}

// TestCashPositionTooLarge: a position whose sums are out of the range
// of an amount is refused.
func TestCashPositionTooLarge(t *testing.T) {
	e := setupPosition(t)
	e.statement(t, e.usd, "USD", "2026-04-16", "2026-04-16", math.MaxInt64-1, math.MaxInt64-1, nil, "imported")
	e.Problem(422, "cash.amount_too_large", "GET", "/v1/cash-position?legal_entity_id="+e.le.String(), "", "")
}

// TestCashPositionCompanyWithoutSettings: a legal entity the active
// document has no company for has the defaults and no banks.
func TestCashPositionCompanyWithoutSettings(t *testing.T) {
	e := setupPosition(t)
	le := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '9000', 'Not configured', 'GB', 'GBP', gen_random_uuid(), 'active', $3, $3)`, e.Tenant.ID, le, e.Tenant.Human.ID)
	p, _ := e.Must(200, "GET", "/v1/cash-position?days=1&legal_entity_id="+le.String(), "", "")
	want(t, "position", fmt.Sprint(p["banks"], " ", days(t, p["converted"].(map[string]any)["days"])), "[] [2026-04-17 0.00 GBP>0.00 GBP (0.00 GBP)]")
}

// TestCashPositionAccess: only the caller's companies (fail closed): a
// legal entity the caller cannot act in, or none, is not found; an agent
// reads the position as a person does, bank accounts by their last four
// characters only.
func TestCashPositionAccess(t *testing.T) {
	e := setupPosition(t)
	e.scenario(t)
	other := e.Reader(t, "reader of 1000", e.US)
	e.As(other).Problem(404, "request.not_found", "GET", "/v1/cash-position?legal_entity_id="+e.le.String(), "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/cash-position?legal_entity_id="+uuid.NewString(), "", "")
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	code, p, _ := e.As(e.Tenant.Agent).Do("GET", "/v1/cash-position?legal_entity_id="+e.le.String()+"&days=1", "", "")
	if code != 200 {
		t.Fatalf("agent: got %d %v, want 200", code, p)
	}
	if body := fmt.Sprint(p); strings.Contains(body, "4000001") || strings.Contains(body, "1000001") {
		t.Errorf("agent: got %s, want no account numbers", body)
	}
}

// TestPositionFailures calls the position outside a unit of work and with
// each of its database statements failing in turn.
func TestPositionFailures(t *testing.T) {
	e := setupPosition(t)
	e.scenario(t)
	e.rate(t, "planning", "CAD", "USD", "2026-04-16", "0.8")
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	for name, q := range map[string]cash.Query{
		"position":               {LegalEntity: e.le},
		"converted elsewhere":    {LegalEntity: e.le, Display: "USD"},
		"display without a rate": {LegalEntity: e.le, Display: "EUR"},
	} {
		call := func(ctx context.Context) error {
			_, err := endpoints.Position(ctx, q)
			return err
		}
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}
