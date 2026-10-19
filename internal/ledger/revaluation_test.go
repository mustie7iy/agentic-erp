package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// revaluationSetup values companies 1000 and 2000 at bank_selling rates
// (ADR-0042): the payables reconciliation account 211000 into adjustment
// 211099, and in 2000 also the flagged bank account 100000; 2000 has the
// down payment account 159000. Without payables, 211000 has no valuation.
func revaluationSetup(t *testing.T, e env, payables bool) {
	t.Helper()
	doc := document(t)
	for _, code := range []string{"1000", "2000"} {
		co := doc.Company(code)
		co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "211099", Name: "Payables valuation", Type: config.Liability})
		co.FXValuation = config.FXValuation{RateType: "bank_selling"}
		if payables {
			co.FXValuation.Accounts = append(co.FXValuation.Accounts,
				config.FXValuationAccount{Account: "211000", Adjustment: "211099", UnrealisedGain: "400000", UnrealisedLoss: "610000"})
		}
	}
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	de.Account("100000").RevalueForeignCurrency = true
	de.FXValuation.Accounts = append(de.FXValuation.Accounts, config.FXValuationAccount{Account: "100000", UnrealisedGain: "400000", UnrealisedLoss: "610000"})
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
}

// revaluationRate records a bank_selling rate.
func revaluationRate(t *testing.T, e env, from, to, validFrom, rate string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'bank_selling', $2, $3, $4, $5, 'ECB', $6)`, e.ten.ID, from, to, validFrom, rate, e.ten.Human.ID)
}

// revaluationPost posts an entry of lines in a currency on a date.
func revaluationPost(t *testing.T, e env, le uuid.UUID, day, currency string, lines ...ledger.PostingLine) ledger.Entry {
	t.Helper()
	var entry ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: le, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day),
			Currency: currency, SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: lines})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entry
}

func preview(le uuid.UUID, keyDate string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "key_date": %q}`, le, keyDate)
}

// TestRevaluationPreview values the open items of the payables
// reconciliation account and the flagged bank account's balance at the key
// date (ADR-0042): items posted after it, cleared or reversed on or
// before it, in local currency or down payments are not valued; an item
// cleared or reversed after it is; the rate is the legal entity's rate
// type's; group currency differences follow from local at the average
// rate; and a key date that is not a period end, a missing, stale or
// unrecordable rate and an account without a valuation are refused.
func TestRevaluationPreview(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	expense, ap, bank, dp := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "100000"),
		settleAccount(t, e, "2000", "159000")
	names := map[string]string{ap.String(): "211000", bank.String(): "100000"}
	a := "A"
	pl := func(account uuid.UUID, amount money.Minor, partner *uuid.UUID) ledger.PostingLine {
		return ledger.PostingLine{AccountID: account, Amount: amount, PartnerID: partner}
	}
	inv1 := revaluationPost(t, e, e.de, "2026-03-02", "USD", pl(expense, 10000, nil), pl(ap, -10000, &e.supplier))
	inv2 := revaluationPost(t, e, e.de, "2026-03-02", "USD", pl(expense, 3000, nil), pl(ap, -3000, &e.vendor))
	pay2 := revaluationPost(t, e, e.de, "2026-03-02", "USD", pl(ap, 3000, &e.vendor), pl(bank, -3000, nil))
	inv3 := revaluationPost(t, e, e.de, "2026-03-02", "USD", pl(expense, 2000, nil), pl(ap, -2000, &e.vendor))
	pay3 := revaluationPost(t, e, e.de, "2026-04-03", "USD", pl(ap, 2000, &e.vendor), pl(bank, -2000, nil))
	revaluationPost(t, e, e.de, "2026-04-02", "USD", pl(expense, 700, nil), pl(ap, -700, &e.supplier))
	revaluationPost(t, e, e.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: dp, Amount: 4000, PartnerID: &e.supplier, SpecialGL: &a}, pl(bank, -4000, nil))
	revaluationPost(t, e, e.de, "2026-03-02", "EUR", pl(expense, 1000, nil), pl(ap, -1000, &e.supplier))
	revaluationPost(t, e, e.de, "2026-03-02", "USD", pl(bank, 50000, nil), pl(expense, -50000, nil))
	clear := func(day string, x, y ledger.Entry) {
		e.Must(201, "POST", "/v1/clearings", clearing(e.de, day, ref{map[string]any{"fiscal_year": 2026, "id": x.ID.String()}, 2},
			ref{map[string]any{"fiscal_year": 2026, "id": y.ID.String()}, 1}), "")
	}
	reverse := func(x ledger.Entry, day string) {
		on := date(day)
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := postings().Reverse(ctx, x.ID, &on, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	reverse(revaluationPost(t, e, e.de, "2026-03-02", "USD", pl(expense, 5000, nil), pl(ap, -5000, &e.supplier)), "2026-03-10")
	inv6 := revaluationPost(t, e, e.de, "2026-03-02", "USD", pl(expense, 6000, nil), pl(ap, -6000, &e.supplier))
	reverse(inv6, "2026-04-02")
	clear("2026-03-20", inv2, pay2)
	clear("2026-04-05", inv3, pay3)
	usExpense, usAP := settleAccount(t, e, "1000", "520000"), settleAccount(t, e, "1000", "211000")
	revaluationPost(t, e, e.us, "2026-03-02", "EUR", pl(usExpense, 10000, nil), pl(usAP, -10000, &e.supplier))

	path := "/v1/revaluation-runs/preview"
	t.Run("no rate of the rate type", func(t *testing.T) {
		e.in(t).Problem(422, "ledger.no_exchange_rate", "POST", path, preview(e.de, "2026-03-31"), "")
	})
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	t.Run("valued items and totals", func(t *testing.T) {
		got, _ := e.in(t).Must(200, "POST", path, preview(e.de, "2026-03-31"), "")
		if h := fmt.Sprint(got["fiscal_year"], " ", got["fiscal_period"], " ", got["rate_type"], " ", got["key_date"]); h != "2026 3 bank_selling 2026-03-31" {
			t.Errorf("header: got %s, want 2026 3 bank_selling 2026-03-31", h)
		}
		amount := func(m map[string]any, k string) any { return m[k].(map[string]any)["amount"] }
		var items []string
		for _, x := range got["items"].([]any) {
			it := x.(map[string]any)
			line := ""
			if it["journal_entry_id"] != nil {
				line = fmt.Sprintf(" %s/%v", map[string]string{inv1.ID.String(): "inv1", inv3.ID.String(): "inv3", inv6.ID.String(): "inv6"}[it["journal_entry_id"].(string)], it["line_no"])
			}
			items = append(items, fmt.Sprintf("%s%s %v %v %v %v %v %v %v %v", names[it["account_id"].(string)], line, amount(it, "amount"), amount(it, "amount_lc"),
				it["rate"], it["rate_valid_from"], amount(it, "valued_lc"), amount(it, "difference_lc"), amount(it, "difference_gc"), it["direction"]))
		}
		want := []string{
			"100000 430.00 344.00 0.900000000000 2026-03-15 387.00 43.00 43.00 gain",
			"211000 inv1/2 -100.00 -80.00 0.900000000000 2026-03-15 -90.00 -10.00 -10.00 loss",
			"211000 inv3/2 -20.00 -16.00 0.900000000000 2026-03-15 -18.00 -2.00 -2.00 loss",
			"211000 inv6/2 -60.00 -48.00 0.900000000000 2026-03-15 -54.00 -6.00 -6.00 loss",
		}
		if strings.Join(items, "\n") != strings.Join(want, "\n") {
			t.Errorf("items:\ngot  %s\nwant %s", strings.Join(items, "\n     "), strings.Join(want, "\n     "))
		}
		var totals []string
		for _, x := range got["totals"].([]any) {
			s := x.(map[string]any)
			totals = append(totals, fmt.Sprintf("%s %v %v %v %v %v %v %v %v", names[s["account_id"].(string)], s["currency"], s["direction"], s["item_count"],
				amount(s, "amount"), amount(s, "amount_lc"), amount(s, "valued_lc"), amount(s, "difference_lc"), amount(s, "difference_gc")))
		}
		wantTotals := []string{"100000 USD gain 1 430.00 344.00 387.00 43.00 43.00", "211000 USD loss 3 -180.00 -144.00 -162.00 -18.00 -18.00"}
		if strings.Join(totals, "\n") != strings.Join(wantTotals, "\n") {
			t.Errorf("totals:\ngot  %s\nwant %s", strings.Join(totals, "\n     "), strings.Join(wantTotals, "\n     "))
		}
	})
	t.Run("group currency from local at the average rate", func(t *testing.T) {
		got, _ := e.in(t).Must(200, "POST", path, preview(e.us, "2026-03-31"), "")
		it := got["items"].([]any)[0].(map[string]any)
		s := fmt.Sprint(it["rate"], " ", it["amount_gc"], " ", it["valued_lc"], " ", it["difference_lc"], " ", it["difference_gc"], " ", it["partner_id"] == e.supplier.String())
		if want := "1.111111111111 map[amount:-100.00 currency:EUR] map[amount:-111.11 currency:USD] map[amount:13.89 currency:USD] map[amount:11.11 currency:EUR] true"; s != want {
			t.Errorf("item of 1000: got %s, want %s", s, want)
		}
	})
	for _, c := range []struct{ name, body, code string }{
		{"a key date that is not a period end", preview(e.de, "2026-03-30"), "ledger.key_date_not_period_end"},
		{"a stale rate", preview(e.de, "2026-04-30"), "ledger.rate_stale"},
		{"an unknown legal entity", preview(uuid.New(), "2026-03-31"), "request.reference_not_found"},
		{"an unknown ledger", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "key_date": "2026-03-31"}`, e.de, uuid.New()), "request.reference_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) { e.in(t).Problem(422, c.code, "POST", path, c.body, "") })
	}
	t.Run("a rate that rounds to zero", func(t *testing.T) {
		revaluationRate(t, e, "EUR", "USD", "2026-03-31", "3000000000000")
		e.in(t).Problem(422, "ledger.rate_not_recordable", "POST", path, preview(e.de, "2026-03-31"), "")
	})
	t.Run("an account without a valuation", func(t *testing.T) {
		revaluationSetup(t, e, false)
		p := e.in(t).Problem(422, "ledger.revaluation_account_missing", "POST", path, preview(e.de, "2026-03-31"), "")
		if d, _ := p["detail"].(string); !strings.Contains(d, "211000") {
			t.Errorf("detail: got %q, want it to name account 211000", d)
		}
	})
}

// TestRevaluationPreviewFailures fails each statement of a preview that
// reaches every one, in company 1000, whose local currency is not the
// group's, and calls it outside a unit of work.
func TestRevaluationPreviewFailures(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	revaluationPost(t, e, e.us, "2026-03-02", "EUR", ledger.PostingLine{AccountID: settleAccount(t, e, "1000", "520000"), Amount: 100},
		ledger.PostingLine{AccountID: settleAccount(t, e, "1000", "211000"), Amount: -100, PartnerID: &e.supplier})
	h := endpoints()
	call := func(ctx context.Context) error {
		_, err := h.PreviewRevaluation(ctx, &gen.RevaluationPreviewRequest{LegalEntityID: e.us, KeyDate: date("2026-03-31")}, gen.PreviewRevaluationParams{})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "preview revaluation", call)
}

// TestRevaluationPreviewTooLarge: items whose totals, or an item whose
// valued amount, are out of range refuse the preview.
func TestRevaluationPreviewTooLarge(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	expense, ap := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000")
	// In two periods, so that no period's balance, which the projector
	// keeps, is out of range.
	for _, day := range []string{"2026-02-02", "2026-03-02"} {
		revaluationPost(t, e, e.de, day, "USD", ledger.PostingLine{AccountID: expense, Amount: 5e18},
			ledger.PostingLine{AccountID: ap, Amount: -5e18, PartnerID: &e.supplier})
	}
	t.Run("totals", func(t *testing.T) {
		e.in(t).Problem(422, "ledger.amount_too_large", "POST", "/v1/revaluation-runs/preview", preview(e.de, "2026-03-31"), "")
	})
	t.Run("valued amount", func(t *testing.T) {
		revaluationRate(t, e, "USD", "EUR", "2026-03-20", "2")
		e.in(t).Problem(422, "ledger.amount_too_large", "POST", "/v1/revaluation-runs/preview", preview(e.de, "2026-03-31"), "")
	})
}

// TestRevaluationPreviewBudget checks a preview against 01-requirements'
// trial balance budget (p95 1 s), a report over one company and period of
// the same weight: 10,000 open USD supplier items and 40,000 cleared ones
// on the reconciliation account, and 10,000 lines on the flagged bank
// account. The entries are written straight into the journal, balanced,
// without the row checks a posting runs (as the payment run's budget does).
func TestRevaluationPreviewBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	expense, ap, bank := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "100000")
	ctx, cancel := context.WithTimeout(context.Background(), seedingDeadline)
	defer cancel()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	start := time.Now()
	exec(`SET session_replication_role = replica`)
	exec(`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
		SELECT $1, 2026, md5('rv' || n)::uuid, $2, l.id, 1 + n % 3, 'KR', 'RV' || n, DATE '2026-01-01' + n % 89, DATE '2026-01-01' + n % 89,
			'USD', 'EUR', 'EUR', 0.8, 1, 'payables', $3
		FROM generate_series(1, 60000) n, ledger.ledger l WHERE l.tenant_id = $1`, e.ten.ID, e.de, e.ten.Human.ID)
	// Entries 1-50,000 are invoices (expense to the supplier), the rest
	// bank receipts; invoices 10,001-50,000 are cleared, in 10,000 clearings.
	exec(`INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc, partner_id)
		SELECT j.tenant_id, 2026, j.id, i, $2, j.ledger_id, j.fiscal_period, j.posting_date,
			CASE WHEN i = 1 THEN CASE WHEN n <= 50000 THEN $3::uuid ELSE $5::uuid END WHEN n <= 50000 THEN $4::uuid ELSE $3::uuid END,
			CASE WHEN i = 1 THEN 1000 ELSE -1000 END, CASE WHEN i = 1 THEN 800 ELSE -800 END, CASE WHEN i = 1 THEN 800 ELSE -800 END,
			CASE WHEN i = 2 AND n <= 50000 THEN $6::uuid END
		FROM generate_series(1, 60000) n JOIN ledger.journal_entry j ON j.tenant_id = $1 AND j.fiscal_year = 2026 AND j.id = md5('rv' || n)::uuid,
			generate_series(1, 2) i`, e.ten.ID, e.de, expense, ap, bank, e.supplier)
	exec(`INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
		SELECT $1, md5('rc' || c)::uuid, $2, '2026-03-20', 2026, md5('rv' || c)::uuid, $3 FROM generate_series(1, 10000) c`, e.ten.ID, e.de, e.ten.Human.ID)
	exec(`INSERT INTO ledger.clearing_item (tenant_id, fiscal_year, journal_entry_id, line_no, clearing_id)
		SELECT $1, 2026, md5('rv' || n)::uuid, 2, md5('rc' || (n % 10000 + 1))::uuid FROM generate_series(10001, 50000) n`, e.ten.ID)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE ledger.journal_entry, ledger.journal_line, ledger.clearing, ledger.clearing_item`)
	t.Logf("seeded 60,000 entries in %v", time.Since(start))
	var times []time.Duration
	for range 10 {
		start := time.Now()
		got, _ := e.Must(200, "POST", "/v1/revaluation-runs/preview", preview(e.de, "2026-03-31"), "")
		times = append(times, time.Since(start))
		if n := len(got["items"].([]any)); n != 10001 {
			t.Fatalf("items: got %d, want 10,000 open items and the bank's balance", n)
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("revaluation preview over 10,000 open items: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(time.Second); p95 > budget {
		t.Errorf("revaluation preview: p95 %v exceeds the %v budget (1 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
