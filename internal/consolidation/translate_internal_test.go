package consolidation

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestTranslateRefusals: a balance of an account that is not the
// company's is an error; one of an account without a group account fails
// the run (consolidation.account_unmapped, naming the account and the
// company); a balance too large to translate is an error; a trading
// partner tenancy no longer lists is ordered by its id; a chart without
// the role gives the computed row no group account.
func TestTranslateRefusals(t *testing.T) {
	bank := Account{ID: uuid.New(), Number: "100000", Name: "Bank", Type: config.Asset}
	base := func() figures {
		return figures{company: Company{Code: "1000"}, year: 2026, accounts: map[uuid.UUID]Account{bank.ID: bank},
			mapping: map[string]string{"100000": "G1000"}, partners: map[uuid.UUID]string{}, rate: big.NewRat(2, 1), expLC: 2, expGC: 2}
	}
	f := base()
	f.balances = []Balance{{AccountID: uuid.New(), LC: 100}}
	if _, err := translate(f); err == nil || !strings.Contains(err.Error(), "is not the company's") {
		t.Errorf("an account not the company's: got %v, want an error", err)
	}
	f = base()
	f.mapping = map[string]string{}
	f.balances = []Balance{{AccountID: bank.ID, LC: 100}}
	var p *problem.Problem
	if _, err := translate(f); !errors.As(err, &p) || p.Code != "consolidation.account_unmapped" || !strings.Contains(p.Detail, "100000 Bank of company 1000") {
		t.Errorf("an unmapped account: got %v, want consolidation.account_unmapped naming it", err)
	}
	f = base()
	f.balances = []Balance{{AccountID: bank.ID, LC: money.Minor(1 << 62)}}
	if _, err := translate(f); err == nil || !strings.Contains(err.Error(), "translate account 100000 of company 1000") {
		t.Errorf("a balance too large: got %v, want an error", err)
	}
	f = base()
	gone := uuid.New()
	f.balances = []Balance{{AccountID: bank.ID, TradingPartnerID: &gone, LC: 100, GC: 150}}
	rows, err := translate(f)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rows); got != 2 || rows[0].groupAccount != "" || rows[0].kind != kindCTA || rows[1].partnerCode != gone.String() || rows[1].gc != 200 {
		t.Errorf("a partner unknown and no cta account: got %+v, want the cta row without an account first, the balance at 2.00 with the partner's id", rows)
	}
}

// TestRevaluationParts (ADR-0059 §7, As built: the group-currency
// revaluation's translation part): a revaluation line's part is its
// booked group amount less its local amount at its entry's rate, rounded
// once per line, half away from zero; the year's parts come off their
// account's row, under its trading partner, and an earlier year's off the
// earlier results, each onto the translation difference; lines of an
// account without a balance make a row of their own unless their parts
// cancel; and a line is refused on an account that is not the company's,
// has no group account or is not a profit and loss account, or too large
// to translate.
func TestRevaluationParts(t *testing.T) {
	gain := Account{ID: uuid.New(), Number: "480000", Name: "Unrealised gains", Type: config.Revenue}
	loss := Account{ID: uuid.New(), Number: "680000", Name: "Unrealised losses", Type: config.Expense}
	bank := Account{ID: uuid.New(), Number: "100000", Name: "Bank", Type: config.Asset}
	partner := uuid.New()
	half, whole := big.NewRat(1, 2), big.NewRat(1, 1)
	base := func() figures {
		return figures{company: Company{Code: "1000"}, year: 2026, accounts: map[uuid.UUID]Account{gain.ID: gain, loss.ID: loss, bank.ID: bank},
			mapping:  map[string]string{"480000": "G4000", "680000": "G5000", "100000": "G1000"},
			chart:    config.Consolidation{Accounts: []config.GroupAccount{{Number: "G3800", Role: config.RoleRetainedEarnings}, {Number: "G3900", Role: config.RoleCTA}}},
			partners: map[uuid.UUID]string{partner: "2000"}, rate: big.NewRat(2, 1), expLC: 2, expGC: 2}
	}
	show := func(rows []row) string {
		var out []string
		for _, r := range rows {
			number, booked := "-", "-"
			if r.account != nil {
				number = r.account.Number
			}
			if r.gcBooked != nil {
				booked = fmt.Sprintf("%d/%d", *r.lc, *r.gcBooked)
			}
			line := fmt.Sprintf("%s %s %s tp%s %s -> %d moved %d", r.groupAccount, number, r.kind, r.partnerCode, booked, r.gc, r.moved)
			if r.net != 0 {
				line += fmt.Sprintf(" net %d", r.net)
			}
			out = append(out, line)
		}
		return strings.Join(out, " | ")
	}
	cases := []struct {
		name     string
		balances []Balance
		lines    []RevaluationLine
		want     string
		moved    money.Minor
	}{
		{"a gain booked beyond its local amount translated, and one under a trading partner",
			[]Balance{{AccountID: gain.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -90}, {AccountID: gain.ID, TradingPartnerID: &partner, FiscalYear: 2026, Period: 10, LC: -20, GC: 0}},
			[]RevaluationLine{{AccountID: gain.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -90, Rate: half},
				{AccountID: gain.ID, TradingPartnerID: &partner, FiscalYear: 2026, Period: 10, LC: -20, GC: 0, Rate: half}},
			"G3900 - cta tp - -> 60 moved 0 | G4000 480000 balance tp -100/-90 -> -50 moved -40 | G4000 480000 balance tp2000 -20/0 -> -10 moved 10", -30},
		{"each line rounded once, half away from zero: parts that cancel add no row",
			nil, []RevaluationLine{{AccountID: loss.ID, FiscalYear: 2026, Period: 9, LC: 5, GC: 0, Rate: half}, {AccountID: loss.ID, FiscalYear: 2026, Period: 10, LC: -5, GC: 0, Rate: half}},
			"", 0},
		{"lines of an account without a balance, a valuation and its reset at another rate",
			nil, []RevaluationLine{{AccountID: loss.ID, FiscalYear: 2026, Period: 9, LC: 5, GC: 0, Rate: half}, {AccountID: loss.ID, FiscalYear: 2026, Period: 10, LC: -5, GC: 0, Rate: whole}},
			"G3900 - cta tp - -> 2 moved 0 | G5000 680000 balance tp 0/0 -> -2 moved 2", 2},
		{"a line whose group amount is its local amount translated", []Balance{{AccountID: gain.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -50}},
			[]RevaluationLine{{AccountID: gain.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -50, Rate: half}},
			"G3900 - cta tp - -> 50 moved 0 | G4000 480000 balance tp -100/-50 -> -50 moved 0", 0},
		{"an earlier year's line, off the earlier results", []Balance{{AccountID: gain.ID, FiscalYear: 2025, LC: -100, GC: -90}},
			[]RevaluationLine{{AccountID: gain.ID, FiscalYear: 2025, LC: -100, GC: -90, Rate: half}},
			"G3800 - prior_results tp -100/-90 -> -50 moved -40 | G3900 - cta tp - -> 50 moved 0", 0},
		{"an earlier year's lines without earlier results", nil,
			[]RevaluationLine{{AccountID: gain.ID, FiscalYear: 2025, LC: -100, GC: -90, Rate: half}, {AccountID: gain.ID, FiscalYear: 2025, LC: 100, GC: 90, Rate: whole}},
			"G3800 - prior_results tp 0/0 -> 50 moved -50 | G3900 - cta tp - -> -50 moved 0", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := base()
			f.balances, f.revaluations = c.balances, c.lines
			rows, err := translate(f)
			if err != nil {
				t.Fatal(err)
			}
			if got := show(rows); got != c.want {
				t.Errorf("rows:\ngot  %s\nwant %s", got, c.want)
			}
			if got, net := movedInYear(rows); got != c.moved || net != 0 {
				t.Errorf("moved in the year: got %d and %d of net investments, want %d and 0", got, net, c.moved)
			}
		})
	}
	refusals := []struct {
		name string
		line RevaluationLine
		drop string // a mapping removed
		want string
	}{
		{"an account not the company's", RevaluationLine{AccountID: uuid.New(), LC: 1, Rate: half}, "", "is not the company's"},
		{"an account without a group account", RevaluationLine{AccountID: gain.ID, LC: 1, Rate: half}, "480000", "480000 Unrealised gains of company 1000"},
		{"a balance sheet account", RevaluationLine{AccountID: bank.ID, LC: 1, Rate: half}, "", "account 100000 of company 1000, which is not a profit and loss account"},
		{"a line too large", RevaluationLine{AccountID: gain.ID, LC: money.Minor(1 << 62), Rate: big.NewRat(4, 1)}, "", "translate a revaluation line of company 1000"},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			f := base()
			delete(f.mapping, c.drop)
			f.revaluations = []RevaluationLine{c.line}
			if _, err := translate(f); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// TestNetInvestmentParts (ADR-0059 As built: a net investment's exchange
// differences; IAS 21.32): a revaluation line that offsets a difference
// on a designated account's balance with its trading partner leaves the
// profit whole, its translation part as every line's and the rest, its
// local amount translated, beside it; a line valuing the account with
// another trading partner or none, or naming no valued account, keeps
// the rest in the profit; an earlier year's comes off the earlier
// results; and a valuation and its reset that cancel add no row. A row
// the run takes whole out of the profit is zero, so these figures, which
// leave the balance sheet side out, need no translation difference.
func TestNetInvestmentParts(t *testing.T) {
	gain := Account{ID: uuid.New(), Number: "480000", Name: "Unrealised gains", Type: config.Revenue}
	loan := Account{ID: uuid.New(), Number: "130000", Name: "Loans to group companies", Type: config.Asset}
	partner, other := uuid.New(), uuid.New()
	half := big.NewRat(1, 2)
	base := func() figures {
		return figures{company: Company{Code: "1000"}, year: 2026, accounts: map[uuid.UUID]Account{gain.ID: gain, loan.ID: loan},
			mapping:  map[string]string{"480000": "G4000", "130000": "G1000"},
			chart:    config.Consolidation{Accounts: []config.GroupAccount{{Number: "G3800", Role: config.RoleRetainedEarnings}, {Number: "G3900", Role: config.RoleCTA}}},
			partners: map[uuid.UUID]string{partner: "2000", other: "3000"}, rate: big.NewRat(2, 1), expLC: 2, expGC: 2,
			netInvestments: map[netItem]bool{{account: loan.ID, partner: partner}: true}}
	}
	show := func(rows []row) string {
		var out []string
		for _, r := range rows {
			number := "-"
			if r.account != nil {
				number = r.account.Number
			}
			out = append(out, fmt.Sprintf("%s %s %s tp%s -> %d moved %d net %d", r.groupAccount, number, r.kind, r.partnerCode, r.gc, r.moved, r.net))
		}
		return strings.Join(out, " | ")
	}
	cases := []struct {
		name       string
		balances   []Balance
		lines      []RevaluationLine
		want       string
		moved, net money.Minor
	}{
		{"a gain on the designated balance leaves the profit whole",
			[]Balance{{AccountID: gain.ID, TradingPartnerID: &partner, FiscalYear: 2026, Period: 10, LC: -100, GC: -90}},
			[]RevaluationLine{{AccountID: gain.ID, TradingPartnerID: &partner, ValuedAccountID: &loan.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -90, Rate: half}},
			"G4000 480000 balance tp2000 -> 0 moved -40 net -50", -40, -50},
		{"a gain booked at its local amount translated has no translation part",
			[]Balance{{AccountID: gain.ID, TradingPartnerID: &partner, FiscalYear: 2026, Period: 10, LC: -100, GC: -50}},
			[]RevaluationLine{{AccountID: gain.ID, TradingPartnerID: &partner, ValuedAccountID: &loan.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -50, Rate: half}},
			"G4000 480000 balance tp2000 -> 0 moved 0 net -50", 0, -50},
		{"the account's balance with another trading partner, with none, and a line naming no valued account keep the rest",
			[]Balance{{AccountID: gain.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -90}, {AccountID: gain.ID, TradingPartnerID: &partner, FiscalYear: 2026, Period: 10, LC: -100, GC: -90},
				{AccountID: gain.ID, TradingPartnerID: &other, FiscalYear: 2026, Period: 10, LC: -100, GC: -90}},
			[]RevaluationLine{{AccountID: gain.ID, ValuedAccountID: &loan.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -90, Rate: half},
				{AccountID: gain.ID, TradingPartnerID: &partner, FiscalYear: 2026, Period: 10, LC: -100, GC: -90, Rate: half},
				{AccountID: gain.ID, TradingPartnerID: &other, ValuedAccountID: &loan.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -90, Rate: half}},
			"G3900 - cta tp -> 150 moved 0 net 0 | G4000 480000 balance tp -> -50 moved -40 net 0 | G4000 480000 balance tp2000 -> -50 moved -40 net 0 | " +
				"G4000 480000 balance tp3000 -> -50 moved -40 net 0", -120, 0},
		{"an earlier year's, off the earlier results", []Balance{{AccountID: gain.ID, TradingPartnerID: &partner, FiscalYear: 2025, LC: -100, GC: -90}},
			[]RevaluationLine{{AccountID: gain.ID, TradingPartnerID: &partner, ValuedAccountID: &loan.ID, FiscalYear: 2025, LC: -100, GC: -90, Rate: half}},
			"G3800 - prior_results tp -> 0 moved -40 net -50", 0, 0},
		{"an earlier year's without earlier results or a translation part", nil,
			[]RevaluationLine{{AccountID: gain.ID, TradingPartnerID: &partner, ValuedAccountID: &loan.ID, FiscalYear: 2025, LC: -100, GC: -50, Rate: half}},
			"G3800 - prior_results tp -> 50 moved 0 net -50 | G3900 - cta tp -> -50 moved 0 net 0", 0, 0},
		{"a valuation and its reset without a balance cancel: no row", nil,
			[]RevaluationLine{{AccountID: gain.ID, TradingPartnerID: &partner, ValuedAccountID: &loan.ID, FiscalYear: 2026, Period: 9, LC: -100, GC: -50, Rate: half},
				{AccountID: gain.ID, TradingPartnerID: &partner, ValuedAccountID: &loan.ID, FiscalYear: 2026, Period: 10, LC: 100, GC: 50, Rate: half}},
			"", 0, 0},
		{"a valuation alone without a balance makes a row of its own", nil,
			[]RevaluationLine{{AccountID: gain.ID, TradingPartnerID: &partner, ValuedAccountID: &loan.ID, FiscalYear: 2026, Period: 10, LC: -100, GC: -50, Rate: half}},
			"G3900 - cta tp -> -50 moved 0 net 0 | G4000 480000 balance tp2000 -> 50 moved 0 net -50", 0, -50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := base()
			f.balances, f.revaluations = c.balances, c.lines
			rows, err := translate(f)
			if err != nil {
				t.Fatal(err)
			}
			if got := show(rows); got != c.want {
				t.Errorf("rows:\ngot  %s\nwant %s", got, c.want)
			}
			if moved, net := movedInYear(rows); moved != c.moved || net != c.net {
				t.Errorf("moved in the year: got %d and %d of net investments, want %d and %d", moved, net, c.moved, c.net)
			}
		})
	}
}

// TestNetInvestmentsOfARun: a company's designations are those of the
// run's group naming it, by the ids of its accounts and of the trading
// partners; one counts from the period the run consolidates its trading
// partner in (the parent always, a member from its first period), and
// one naming an account the ledger does not give the company, or a
// trading partner tenancy does not list (company 4000), designates
// nothing. The accounts the ledger is asked to name are each once, in
// one order.
func TestNetInvestmentsOfARun(t *testing.T) {
	loan, payable, second := uuid.New(), uuid.New(), uuid.New()
	parent, us, late := uuid.New(), uuid.New(), uuid.New()
	cs := config.Consolidation{Groups: []config.ConsolidationGroup{
		{Code: "OTHER", Parent: "2000", NetInvestments: []config.NetInvestment{{Company: "2000", Account: "131000", Partner: "1000"}}},
		{Code: "ACME", Parent: "2000",
			Members: []config.GroupMember{{Company: "1000", FirstPeriod: "2026-11"}, {Company: "3000", FirstPeriod: "2027-01"}, {Company: "4000", FirstPeriod: "2026-01"}},
			NetInvestments: []config.NetInvestment{{Company: "2000", Account: "130000", Partner: "1000"}, {Company: "2000", Account: "130000", Partner: "3000"},
				{Company: "2000", Account: "999999", Partner: "1000"}, {Company: "1000", Account: "230000", Partner: "2000"},
				{Company: "2000", Account: "131000", Partner: "3000"}, {Company: "2000", Account: "131000", Partner: "4000"}}}}}
	accounts := map[uuid.UUID]Account{loan: {ID: loan, Number: "130000"}, payable: {ID: payable, Number: "230000"}, second: {ID: second, Number: "131000"}}
	partners := map[uuid.UUID]string{parent: "2000", us: "1000", late: "3000"}
	show := func(company string, year, period int16) string {
		f := figures{netInvestments: netInvestments(cs, Run{GroupCode: "ACME", FiscalYear: year, Period: period}, company, accounts, partners)}
		var out []string
		for _, id := range f.valuedAccounts() {
			var with []string
			for _, code := range []string{"1000", "2000", "3000"} {
				for tp, c := range partners {
					if c == code && f.netInvestments[netItem{account: id, partner: tp}] {
						with = append(with, code)
					}
				}
			}
			out = append(out, accounts[id].Number+" with "+strings.Join(with, ","))
		}
		slices.Sort(out)
		return fmt.Sprintf("%d: %s", len(f.netInvestments), strings.Join(out, "; "))
	}
	for _, c := range []struct {
		name         string
		company      string
		year, period int16
		want         string
	}{
		{"the parent before any member's first period", "2000", 2026, 10, "0: "},
		{"the parent in a member's first period", "2000", 2026, 11, "1: 130000 with 1000"},
		{"the parent once both members are consolidated", "2000", 2027, 1, "3: 130000 with 1000,3000; 131000 with 3000"},
		{"a member, with its parent", "1000", 2026, 11, "1: 230000 with 2000"},
		{"a company without designations", "3000", 2027, 1, "0: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := show(c.company, c.year, c.period); got != c.want {
				t.Errorf("designations: got %s, want %s", got, c.want)
			}
		})
	}
	ids := figures{netInvestments: netInvestments(cs, Run{GroupCode: "ACME", FiscalYear: 2027, Period: 1}, "2000", accounts, partners)}.valuedAccounts()
	if !slices.IsSortedFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }) || len(ids) != 2 {
		t.Errorf("the valued accounts: got %v, want the two accounts once each, in order", ids)
	}
}
