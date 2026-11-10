package consolidation

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Currency translation (ADR-0059 §7; the method confirmed by an
// accounting specialist with additions, the ADR's question 1,
// po:2026-11-09). Each company of a run is
// translated from its leading-ledger figures into the group currency:
// assets and liabilities at the closing rate, each balance per account
// and trading partner rounded once, half away from zero; income, expenses
// and equity at their booked group amounts, the profit and loss accounts
// less the translation part of the company's group-currency revaluation
// differences (revaluationPart) and less the exchange differences on a
// monetary item designated as part of a net investment in a foreign
// operation (netInvestment); the results of earlier fiscal
// years, and for a member in the fiscal year of its first period those of
// the periods before it, as one row on the retained earnings account; and
// the translation difference, minus the sum of the other rows, on the cta
// account, so each company's rows sum to zero.

// Kinds of translated rows.
const (
	kindBalance      = "balance"
	kindPriorResults = "prior_results"
	kindCTA          = "cta"
)

// one is the rate of a company in the group currency.
func one() *big.Rat { return big.NewRat(1, 1) }

// row is a translated row of a company: on its group account, of a
// company account and trading partner (kind balance) or computed (prior
// results, the translation difference, whose local and booked amounts are
// nil). moved is the part of its booked group amount that is a
// group-currency revaluation's translation effect (revaluationPart), and
// net the rest of the booked group amount of its revaluation lines on a
// net investment (netInvestment): its translated amount leaves both to
// the translation difference. revaluationOnly marks a row revaluation
// lines alone made, of an account and trading partner without a balance.
type row struct {
	groupAccount   string
	account        *Account
	partner        *uuid.UUID
	partnerCode    string
	kind           string
	lc, gcBooked   *money.Minor
	gc, moved, net money.Minor
	atClosingRate  bool

	revaluationOnly bool
}

// figures are what a company's translation reads: its role in the run,
// its accounts by id with their group accounts (the configuration's
// mapping), the group chart, the trading partners' codes, the closing rate
// and both currencies' minor units, its balances through the run's
// period, the lines of its revaluation runs' entries on profit and
// loss accounts through it, and its accounts' balances with trading
// partners designated as part of a net investment (netInvestments).
type figures struct {
	company           Company
	year              int16 // the run's fiscal year
	firstYear, firstP int16 // a member's first period; 0 for the parent
	accounts          map[uuid.UUID]Account
	mapping           map[string]string // company account number -> group account
	chart             config.Consolidation
	partners          map[uuid.UUID]string // legal entity -> company code
	rate              *big.Rat
	expLC, expGC      int
	balances          []Balance
	revaluations      []RevaluationLine
	netInvestments    map[netItem]bool
}

// netItem is a company account's balance with a trading partner.
type netItem struct{ account, partner uuid.UUID }

// netInvestments are the company's accounts with trading partners that
// the run's group designates as part of a net investment in a foreign
// operation (config.NetInvestment), by the ids of the accounts given and
// of the companies by their codes. A designation counts only while its
// trading partner is a company the run consolidates, its parent or a
// member whose first period has come (included): before that the group's
// statements hold no foreign operation the item could be an investment
// in. One naming an account the ledger does not give the company, or a
// trading partner tenancy does not list, designates nothing, since no
// line can name it.
func netInvestments(cs config.Consolidation, r Run, company string, accounts map[uuid.UUID]Account, partners map[uuid.UUID]string) map[netItem]bool {
	byNumber, byCode := map[string]uuid.UUID{}, map[string]uuid.UUID{}
	for id, a := range accounts {
		byNumber[a.Number] = id
	}
	for id, code := range partners {
		byCode[code] = id
	}
	out := map[netItem]bool{}
	for _, g := range cs.Groups {
		if g.Code != r.GroupCode {
			continue
		}
		for _, n := range g.NetInvestments {
			account, known := byNumber[n.Account]
			partner, listed := byCode[n.Partner]
			if n.Company == company && known && listed && included(g, n.Partner, r.FiscalYear, r.Period) {
				out[netItem{account: account, partner: partner}] = true
			}
		}
	}
	return out
}

// included reports whether a run of the group's period consolidates the
// company: its parent, or a member from its first period on (include).
func included(g config.ConsolidationGroup, company string, year, period int16) bool {
	for _, m := range g.Members {
		if m.Company == company {
			return m.ConsolidatedIn(int(year), int(period))
		}
	}
	return company == g.Parent
}

// valuedAccounts are the accounts the company's designations name: those
// whose revaluation lines the ledger is asked to name (Ledger).
func (f figures) valuedAccounts() []uuid.UUID {
	var out []uuid.UUID
	for n := range f.netInvestments {
		if !slices.Contains(out, n.account) {
			out = append(out, n.account)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// netInvestment reports whether a revaluation line offsets an exchange
// difference on a designated net investment: on the valued account's
// balance with the line's trading partner.
func (f figures) netInvestment(l RevaluationLine) bool {
	return l.ValuedAccountID != nil && l.TradingPartnerID != nil && f.netInvestments[netItem{account: *l.ValuedAccountID, partner: *l.TradingPartnerID}]
}

// priorResults reports whether a profit and loss figure of the fiscal
// year and period belongs to the company's earlier results: an earlier
// fiscal year's, or, for a member in the fiscal year of its first period,
// a period's before it.
func (f figures) priorResults(year, period int16) bool {
	return year < f.year || (f.firstYear == f.year && period < f.firstP)
}

// unmapped is the run's failure for a company account with a balance and
// no group account: a configuration activated before the rule (ADR-0059
// §13, consolidation.account_unmapped).
func unmapped(co Company, a Account) *problem.Problem {
	return problem.Unprocessable("consolidation.account_unmapped", fmt.Sprintf("Account %s %s of company %s has a balance and no group account; map it "+
		"(companies[].chart_of_accounts[].group_account) and run the group again.", a.Number, a.Name, co.Code),
		problem.FieldError{Field: "group_code", Code: "account_unmapped", Message: "account " + a.Number + " of company " + co.Code + " mapped"})
}

// translation is a company's translation under way: its balance rows by
// company account and trading partner, in the order first seen; its
// earlier results, local and booked group; and the translation part of
// its revaluation lines (revaluationPart) and the rest of those on a net
// investment (netInvestment), per row for the fiscal year's and summed
// for the earlier results'.
type translation struct {
	f                    figures
	byKey                map[string]*row
	rows                 []*row
	prior                [2]money.Minor
	priorMoved, priorNet money.Minor
}

// mapped is a company account with figures and its group account.
func (f figures) mapped(id uuid.UUID) (Account, string, error) {
	a, ok := f.accounts[id]
	if !ok {
		return Account{}, "", fmt.Errorf("consolidation: account %s of company %s has a balance but is not the company's", id, f.company.Code)
	}
	group := f.mapping[a.Number]
	if group == "" {
		return Account{}, "", unmapped(f.company, a)
	}
	return a, group, nil
}

// row is the balance row of a company account and trading partner, new
// with zero amounts when the account has none yet, as revaluationOnly
// says then.
func (t *translation) row(a Account, group string, partner *uuid.UUID, revaluationOnly bool) *row {
	key := a.ID.String() + "/" + partnerKey(partner)
	r, seen := t.byKey[key]
	if !seen {
		r = &row{groupAccount: group, account: &a, partner: partner, partnerCode: t.f.partnerCode(partner), kind: kindBalance,
			lc: new(money.Minor), gcBooked: new(money.Minor), atClosingRate: a.Type == config.Asset || a.Type == config.Liability,
			revaluationOnly: revaluationOnly}
		t.byKey[key], t.rows = r, append(t.rows, r)
	}
	return r
}

// balance adds a balance to its row, or to the earlier results.
func (t *translation) balance(b Balance) error {
	a, group, err := t.f.mapped(b.AccountID)
	if err != nil {
		return err
	}
	if (a.Type == config.Revenue || a.Type == config.Expense) && t.f.priorResults(b.FiscalYear, b.Period) {
		t.prior[0], t.prior[1] = t.prior[0]+b.LC, t.prior[1]+b.GC
		return nil
	}
	r := t.row(a, group, b.TradingPartnerID, false)
	*r.lc, *r.gcBooked = *r.lc+b.LC, *r.gcBooked+b.GC
	return nil
}

// revaluationPart is the translation part of a revaluation run's line on
// a profit and loss account (ADR-0042, the group-currency difference in
// consolidation; the accounting specialist's B4 correction 1,
// po:2026-11-09; IAS 21.39(c)). The run values group currency on its own,
// so the line's booked group amount is not its local amount translated:
// the profit keeps the local amount at the entry's local-to-group rate,
// the rate a posting of its date converts at (§7: income and expenses at
// each posting date's rate), rounded once to the group currency's minor
// unit, half away from zero (ADR-0005), and the rest of the booked group
// amount, this, belongs to the translation difference.
func (f figures) revaluationPart(l RevaluationLine) (money.Minor, error) {
	local, err := money.ConvertAt(l.LC, l.Rate, f.expLC, f.expGC)
	if err != nil {
		return 0, fmt.Errorf("consolidation: translate a revaluation line of company %s: %w", f.company.Code, err)
	}
	return l.GC - local, nil
}

// revaluation takes a revaluation line's translation part off its row, or
// off the earlier results, and, of a line on a designated net investment
// (the accounting specialist's B4 correction 2, po:2026-11-09; IAS
// 21.32), the rest of its booked group amount too: its local amount
// translated, the exchange difference the profit would keep. So the
// whole difference on a net investment leaves the profit for the
// translation difference, where the company's ledger books it. A line
// that moves nothing adds no row, and a row the lines alone made is left
// out once what they move cancels (translate).
func (t *translation) revaluation(l RevaluationLine) error {
	a, group, err := t.f.mapped(l.AccountID)
	if err != nil {
		return err
	}
	if a.Type != config.Revenue && a.Type != config.Expense {
		return fmt.Errorf("consolidation: revaluation line on account %s of company %s, which is not a profit and loss account", a.Number, t.f.company.Code)
	}
	part, err := t.f.revaluationPart(l)
	if err != nil {
		return err
	}
	var net money.Minor
	if t.f.netInvestment(l) {
		net = l.GC - part
	}
	switch {
	case part == 0 && net == 0:
	case t.f.priorResults(l.FiscalYear, l.Period):
		t.priorMoved, t.priorNet = t.priorMoved+part, t.priorNet+net
	default:
		r := t.row(a, group, l.TradingPartnerID, true)
		r.moved, r.net = r.moved+part, r.net+net
	}
	return nil
}

// translate translates the company's balances into its rows, in line
// order (sortRows).
func translate(f figures) ([]row, error) {
	t := &translation{f: f, byKey: map[string]*row{}}
	for _, b := range f.balances {
		if err := t.balance(b); err != nil {
			return nil, err
		}
	}
	for _, l := range f.revaluations {
		if err := t.revaluation(l); err != nil {
			return nil, err
		}
	}
	out := make([]row, 0, len(t.rows)+2)
	for _, r := range t.rows {
		if err := f.value(r); err != nil {
			return nil, err
		}
		if !r.revaluationOnly || r.moved != 0 || r.net != 0 {
			out = append(out, *r)
		}
	}
	if t.prior != [2]money.Minor{} || t.priorMoved != 0 || t.priorNet != 0 {
		lc, gc := t.prior[0], t.prior[1]
		out = append(out, row{groupAccount: f.role(config.RoleRetainedEarnings), kind: kindPriorResults, lc: &lc, gcBooked: &gc,
			gc: gc - t.priorMoved - t.priorNet, moved: t.priorMoved, net: t.priorNet})
	}
	var sum money.Minor
	for _, r := range out {
		sum += r.gc
	}
	if sum != 0 {
		out = append(out, row{groupAccount: f.role(config.RoleCTA), kind: kindCTA, gc: -sum})
	}
	sortRows(out)
	return out, nil
}

// movedInYear is what the rows of the fiscal year's revaluation lines
// leave to the translation difference, which the run moves from the
// year's profit to other comprehensive income: their translation part,
// and the rest of those on a net investment.
func movedInYear(rows []row) (moved, net money.Minor) {
	for _, r := range rows {
		if r.kind == kindBalance {
			moved, net = moved+r.moved, net+r.net
		}
	}
	return moved, net
}

// value sets a balance row's translated amount: its local amount at the
// closing rate for an asset or liability, rounded once; its booked group
// amount otherwise, less the translation part of its revaluation lines
// and the rest of those on a net investment.
func (f figures) value(r *row) error {
	if !r.atClosingRate {
		r.gc = *r.gcBooked - r.moved - r.net
		return nil
	}
	gc, err := money.ConvertAt(*r.lc, f.rate, f.expLC, f.expGC)
	if err != nil {
		return fmt.Errorf("consolidation: translate account %s of company %s: %w", r.account.Number, f.company.Code, err)
	}
	r.gc = gc
	return nil
}

// role is the group chart's account with the role; validation keeps each
// role on one account once a group exists.
func (f figures) role(role string) string { return roleAccount(f.chart, role) }

// roleAccount is a group chart's account with the role.
func roleAccount(chart config.Consolidation, role string) string {
	for _, a := range chart.Accounts {
		if a.Role == role {
			return a.Number
		}
	}
	return ""
}

// partnerKey is a trading partner's part of a row's key.
func partnerKey(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// partnerCode is a trading partner's company code; "" for none, and its
// id for a legal entity tenancy no longer lists (none is ever removed).
func (f figures) partnerCode(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	if code, ok := f.partners[*id]; ok {
		return code
	}
	return id.String()
}

// kindOrder orders a company's rows of one group account and company
// account by kind.
var kindOrder = map[string]int{kindBalance: 0, kindPriorResults: 1, kindCTA: 2}

// sortRows orders a company's rows by group account number, company
// account number (none first), trading partner code (none first) and kind
// (ADR-0059 §5), so two runs of the same figures number them alike.
func sortRows(rows []row) {
	number := func(r row) string {
		if r.account == nil {
			return ""
		}
		return r.account.Number
	}
	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Or(strings.Compare(a.groupAccount, b.groupAccount), strings.Compare(number(a), number(b)),
			strings.Compare(a.partnerCode, b.partnerCode), cmp.Compare(kindOrder[a.kind], kindOrder[b.kind]))
	})
}
