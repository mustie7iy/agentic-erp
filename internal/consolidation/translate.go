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

// Currency translation (ADR-0059 §7; the method is to be confirmed by an
// accounting specialist, the ADR's question 1). Each company of a run is
// translated from its leading-ledger figures into the group currency:
// assets and liabilities at the closing rate, each balance per account
// and trading partner rounded once, half away from zero; income, expenses
// and equity at their booked group amounts; the results of earlier fiscal
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
// nil).
type row struct {
	groupAccount  string
	account       *Account
	partner       *uuid.UUID
	partnerCode   string
	kind          string
	lc, gcBooked  *money.Minor
	gc            money.Minor
	atClosingRate bool
}

// figures are what a company's translation reads: its role in the run,
// its accounts by id with their group accounts (the configuration's
// mapping), the group chart, the trading partners' codes, the closing rate
// and both currencies' minor units, and its balances through the run's
// period.
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
}

// priorResults reports whether a profit and loss balance belongs to the
// company's earlier results: an earlier fiscal year's, or, for a member
// in the fiscal year of its first period, a period's before it.
func (f figures) priorResults(b Balance) bool {
	return b.FiscalYear < f.year || (f.firstYear == f.year && b.Period < f.firstP)
}

// unmapped is the run's failure for a company account with a balance and
// no group account: a configuration activated before the rule (ADR-0059
// §13, consolidation.account_unmapped).
func unmapped(co Company, a Account) *problem.Problem {
	return problem.Unprocessable("consolidation.account_unmapped", fmt.Sprintf("Account %s %s of company %s has a balance and no group account; map it "+
		"(companies[].chart_of_accounts[].group_account) and run the group again.", a.Number, a.Name, co.Code),
		problem.FieldError{Field: "group_code", Code: "account_unmapped", Message: "account " + a.Number + " of company " + co.Code + " mapped"})
}

// translate translates the company's balances into its rows, in line
// order (sortRows).
func translate(f figures) ([]row, error) {
	byKey := map[string]*row{}
	var rows []*row
	var prior [2]money.Minor // local, booked group
	for _, b := range f.balances {
		a, ok := f.accounts[b.AccountID]
		if !ok {
			return nil, fmt.Errorf("consolidation: account %s of company %s has a balance but is not the company's", b.AccountID, f.company.Code)
		}
		group := f.mapping[a.Number]
		if group == "" {
			return nil, unmapped(f.company, a)
		}
		profitAndLoss := a.Type == config.Revenue || a.Type == config.Expense
		if profitAndLoss && f.priorResults(b) {
			prior[0], prior[1] = prior[0]+b.LC, prior[1]+b.GC
			continue
		}
		key := b.AccountID.String() + "/" + partnerKey(b.TradingPartnerID)
		r, seen := byKey[key]
		if !seen {
			r = &row{groupAccount: group, account: &a, partner: b.TradingPartnerID, partnerCode: f.partnerCode(b.TradingPartnerID), kind: kindBalance,
				lc: new(money.Minor), gcBooked: new(money.Minor), atClosingRate: a.Type == config.Asset || a.Type == config.Liability}
			byKey[key], rows = r, append(rows, r)
		}
		*r.lc, *r.gcBooked = *r.lc+b.LC, *r.gcBooked+b.GC
	}
	out := make([]row, 0, len(rows)+2)
	for _, r := range rows {
		if err := f.value(r); err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if prior != [2]money.Minor{} {
		lc, gc := prior[0], prior[1]
		out = append(out, row{groupAccount: f.role(config.RoleRetainedEarnings), kind: kindPriorResults, lc: &lc, gcBooked: &gc, gc: gc})
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

// value sets a balance row's translated amount: its local amount at the
// closing rate for an asset or liability, rounded once; its booked group
// amount otherwise.
func (f figures) value(r *row) error {
	if !r.atClosingRate {
		r.gc = *r.gcBooked
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
func (f figures) role(role string) string {
	for _, a := range f.chart.Accounts {
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
