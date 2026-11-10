package consolidation_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Exchange differences on a net investment in a foreign operation
// (ADR-0059 As built: a net investment's exchange differences; the
// accounting specialist's B4 correction 2, po:2026-11-09; identification
// per account and trading partner in the group's configuration,
// po:2026-11-10; IAS 21.32).

// The two sides of netInvestmentFigures' loans, as a group designates
// them: company 2000's loans to company 1000 and company 1000's loans
// from company 2000.
var (
	lenderSide   = config.NetInvestment{Company: "2000", Account: "130000", Partner: "1000"}
	borrowerSide = config.NetInvestment{Company: "1000", Account: "230000", Partner: "2000"}
)

// netInvestmentEnv is a tenant whose group ACME consolidates company
// 1000 (USD) from October 2026 with the designations given; company 2000
// (EUR, the group currency) keeps its loans on 130000 and company 1000
// its borrowings on 230000, both revalued, the differences to 480000 and
// 680000; the average USD/EUR rates are 0.80 from 1 January 2025, 0.64
// from 30 September 2026 and 0.50 from 31 October 2026; and
// netInvestmentFigures are posted.
func netInvestmentEnv(t *testing.T, designations ...config.NetInvestment) fixture.Env {
	t.Helper()
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withRuns(doc)
		g := &doc.Consolidation.Groups[0]
		g.Members[0].FirstPeriod, g.NetInvestments = "2026-10", designations
		for _, c := range []struct {
			code    string
			account config.Account
		}{
			{"2000", config.Account{Number: "130000", Name: "Loans to group companies", Type: config.Asset, GroupAccount: "G1000", RevalueForeignCurrency: true}},
			{"1000", config.Account{Number: "230000", Name: "Loans from group companies", Type: config.Liability, GroupAccount: "G2000", RevalueForeignCurrency: true}},
		} {
			co := doc.Company(c.code)
			co.ChartOfAccounts = append(co.ChartOfAccounts, c.account,
				config.Account{Number: "480000", Name: "Unrealised exchange gains", Type: config.Revenue, GroupAccount: "G4000"},
				config.Account{Number: "680000", Name: "Unrealised exchange losses", Type: config.Expense, GroupAccount: "G5000"})
			co.FXValuation.Accounts = append(co.FXValuation.Accounts,
				config.FXValuationAccount{Account: c.account.Number, UnrealisedGain: "480000", UnrealisedLoss: "680000"})
		}
	}, runPermissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.8, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-09-30', 0.64, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-31', 0.5, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	sameCalendar(t, e)
	netInvestmentFigures(t, e)
	return e
}

// netInvestmentFigures posts what TestNetInvestmentExchangeDifferences
// computes by hand.
//
// Company 1000 (USD) has share capital of USD 500.00 from January 2026
// (400.00 EUR at 0.80), company 2000's investment of 400.00 EUR. Company
// 2000 funds it with two loans it does not expect back, each company
// naming the other as trading partner on its loan line:
//
//   - EUR 1,000.00 on 15 August: in company 2000's own currency; company
//     1000 books USD 1,250.00 at 1/0.80 and 1,000.00 EUR in group
//     currency.
//   - USD 1,000.00 on 5 October, at 0.64: 640.00 EUR in company 2000's
//     books; in company 1000's own currency.
//
// Company 2000 also lent USD 500.00 to a third party on 5 October,
// 320.00 EUR, on the same account without a trading partner.
//
// Both companies revalue at 31 October, at 0.50:
//
//   - company 2000's USD 1,000.00 is worth 500.00 EUR, a loss of 140.00
//     with trading partner 1000, and its USD 500.00 worth 250.00, a loss
//     of 70.00 without one; its books are in the group currency, so both
//     are booked alike in it.
//   - company 1000 owes EUR 1,000.00, USD 2,000.00 at 1/0.50: a loss of
//     USD 750.00 with trading partner 2000. The euros are 1,000.00 EUR
//     whatever the dollar does, so the loss books no group amount; at
//     the entry's 0.50 it is 375.00 EUR.
func netInvestmentFigures(t *testing.T, e fixture.Env) {
	t.Helper()
	post(t, e, e.US, "1000", "USD", "2026-01-05", "100000", "300000", "500.00", nil)
	post(t, e, e.DE, "2000", "EUR", "2026-01-05", "150000", "100000", "400.00", nil)
	postIC(t, e, e.DE, "2000", "EUR", "2026-08-15", icLine{"130000", "1000.00", &e.US}, icLine{"100000", "-1000.00", nil})
	postIC(t, e, e.US, "1000", "EUR", "2026-08-15", icLine{"100000", "1000.00", nil}, icLine{"230000", "-1000.00", &e.DE})
	postIC(t, e, e.DE, "2000", "USD", "2026-10-05", icLine{"130000", "1000.00", &e.US}, icLine{"100000", "-1000.00", nil})
	postIC(t, e, e.US, "1000", "USD", "2026-10-05", icLine{"100000", "1000.00", nil}, icLine{"230000", "-1000.00", &e.DE})
	postIC(t, e, e.DE, "2000", "USD", "2026-10-05", icLine{"130000", "500.00", nil}, icLine{"100000", "-500.00", nil})
	for _, company := range []uuid.UUID{e.DE, e.US} {
		e.Must(201, "POST", "/v1/revaluation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "key_date": "2026-10-31"}`, company), "")
	}
}

// lossRows are the totals of a company's rows on its unrealised loss
// account 680000 in a run: booked and translated, in minor units.
func lossRows(t *testing.T, e fixture.Env, run, company uuid.UUID, code string) string {
	t.Helper()
	p, _ := e.Must(200, "GET", fmt.Sprintf("%s/%s/balances?legal_entity_id=%s&account_id=%s", runsPath, run, company, e.Account(t, code, "680000")), "", "")
	return fmt.Sprintf("booked %d, translated %d", minorOf(t, p["totals"], "group_booked"), minorOf(t, p["totals"], "group"))
}

// netInvestmentLines shows what a profit and loss says a run moved out
// of the profit: per company of its translation reserve, and in its
// totals.
func netInvestmentLines(pl map[string]any) string {
	var out []string
	for _, it := range pl["translation_reserve"].([]any) {
		v := it.(map[string]any)
		out = append(out, fmt.Sprintf("%s %s + %s = %s, translation part %s, net investment %s", v["company_code"], moneyOf(v["opening"]),
			moneyOf(v["difference"]), moneyOf(v["closing"]), moneyOf(v["revaluation_translation"]), moneyOf(v["net_investment"])))
	}
	tot := pl["totals"].(map[string]any)
	return strings.Join(out, "; ") + "; totals: translation part " + columnsOf(tot["revaluation_translation"]) + "; net investment " +
		columnsOf(tot["net_investment"]) + "; profit " + columnsOf(tot["profit"]) + "; other comprehensive income " +
		columnsOf(tot["other_comprehensive_income"])
}

// TestNetInvestmentExchangeDifferences: netInvestmentFigures' October
// run, each step run twice, under three configurations.
//
// As booked the group's profit holds three losses: company 2000's 140.00
// EUR on its dollar loan to company 1000 and 70.00 EUR on its loan to a
// third party, and company 1000's USD 750.00 on its euro borrowing,
// which books no group amount: the run keeps it at its entry's 0.50,
// 375.00 EUR, and takes that out of company 1000's translation
// difference (the group-currency revaluation's translation part, -375.00
// debit positive). Company 1000's reserve is what balances it: its bank
// of USD 2,750.00 (1,375.00 EUR at 0.50) against its borrowings of USD
// 3,000.00 (1,500.00), its share capital of 400.00 and the loss of
// 375.00: 150.00 debit. So without a designation the profit is a loss of
// 585.00 and other comprehensive income 150.00.
//
// With company 2000's loans to company 1000 designated, its 140.00
// leaves the profit for its translation reserve; the 70.00 on the third
// party's loan, on the same account without the trading partner, stays,
// and so does company 1000's loss, on its own account, which that
// designation does not name: 445.00 and 290.00.
//
// With company 1000's borrowings from company 2000 designated too, the
// 375.00 the profit kept leaves it as well: 70.00 and 665.00. Total
// comprehensive income is 735.00 throughout, the balance sheet sums to
// zero, and both companies' ledgers are as they were.
func TestNetInvestmentExchangeDifferences(t *testing.T) {
	const parent = "2000 0.00 EUR + 0.00 EUR = 0.00 EUR, translation part 0.00 EUR, net investment 0.00 EUR"
	const designated = "2000 0.00 EUR + 140.00 EUR = 140.00 EUR, translation part 0.00 EUR, net investment 140.00 EUR"
	cases := []struct {
		name           string
		designations   []config.NetInvestment
		lender, member string // their rows on 680000
		statement      string
	}{
		{"no designation", nil, "booked 21000, translated 21000", "booked 0, translated 37500",
			"1000 0.00 EUR + 150.00 EUR = 150.00 EUR, translation part -375.00 EUR, net investment 0.00 EUR; " + parent +
				"; totals: translation part -375.00 EUR / 0.00 EUR / -375.00 EUR; net investment 0.00 EUR / 0.00 EUR / 0.00 EUR; " +
				"profit 585.00 EUR / 0.00 EUR / 585.00 EUR; other comprehensive income 150.00 EUR / 0.00 EUR / 150.00 EUR"},
		{"the lender's loans designated", []config.NetInvestment{lenderSide}, "booked 21000, translated 7000", "booked 0, translated 37500",
			"1000 0.00 EUR + 150.00 EUR = 150.00 EUR, translation part -375.00 EUR, net investment 0.00 EUR; " + designated +
				"; totals: translation part -375.00 EUR / 0.00 EUR / -375.00 EUR; net investment 140.00 EUR / 0.00 EUR / 140.00 EUR; " +
				"profit 445.00 EUR / 0.00 EUR / 445.00 EUR; other comprehensive income 290.00 EUR / 0.00 EUR / 290.00 EUR"},
		{"both sides designated", []config.NetInvestment{lenderSide, borrowerSide}, "booked 21000, translated 7000", "booked 0, translated 0",
			"1000 0.00 EUR + 525.00 EUR = 525.00 EUR, translation part -375.00 EUR, net investment 375.00 EUR; " + designated +
				"; totals: translation part -375.00 EUR / 0.00 EUR / -375.00 EUR; net investment 515.00 EUR / 0.00 EUR / 515.00 EUR; " +
				"profit 70.00 EUR / 0.00 EUR / 70.00 EUR; other comprehensive income 665.00 EUR / 0.00 EUR / 665.00 EUR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := netInvestmentEnv(t, c.designations...)
			h := app.Consolidation(app.Config{})
			before := ledgerOf(t, e, e.DE) + " | " + ledgerOf(t, e, e.US)
			run := startPeriod(t, e, 2026, 10)
			carryOutTwice(t, e, h, run)
			if got := lossRows(t, e, run, e.DE, "2000"); got != c.lender {
				t.Errorf("company 2000's unrealised losses: got %s, want %s", got, c.lender)
			}
			if got := lossRows(t, e, run, e.US, "1000"); got != c.member {
				t.Errorf("company 1000's unrealised losses: got %s, want %s", got, c.member)
			}
			bs, pl := statements(t, e, run)
			balanced(t, bs, pl)
			if got := netInvestmentLines(pl); got != c.statement {
				t.Errorf("the profit and loss:\ngot  %s\nwant %s", got, c.statement)
			}
			if got, w := minorOf(t, pl["totals"].(map[string]any)["total_comprehensive_income"], "consolidated"), int64(73500); got != w {
				t.Errorf("total comprehensive income: got %d, want %d whatever is designated", got, w)
			}
			if _, ok := bs["totals"].(map[string]any)["net_investment"]; ok {
				t.Error("the balance sheet's totals: got net_investment, want none")
			}
			if got := ledgerOf(t, e, e.DE) + " | " + ledgerOf(t, e, e.US); got != before {
				t.Errorf("the companies' ledgers after the run:\ngot  %s\nwant %s", got, before)
			}
		})
	}
}
