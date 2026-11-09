package consolidation

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// acquired is a member's acquisition as a run eliminates it: measured at
// the period before its first, or read from the frozen acquisition, with
// what that froze (origin, ownership, first period, investment account and
// the run that froze it).
type acquired struct {
	company           uuid.UUID
	code              string
	origin            string
	ownership         string
	firstYear, firstP int16
	investmentAccount uuid.UUID
	netLC, netGC      money.Minor
	rate              string
	rateFrom          *time.Time
	investment        money.Minor
	equity            map[string]money.Minor // its share capital: group account -> as booked, credit negative
	allEquity         bool                   // measured without a retained earnings account: every equity account counted
	frozenBy          *uuid.UUID
}

// investmentRow is a parent's translated row on an investment account.
type investmentRow struct {
	line             int32
	group            string
	account, partner *uuid.UUID
	gc               money.Minor
}

// investing is what a run's investment entries are made from: the run's
// configuration version, its parent and members, the parent's accounts by
// number and its rows on the members' investment accounts by account, the
// frozen acquisitions by member and the group currency's minor units.
type investing struct {
	doc      *config.Config
	parent   Member
	members  []Member
	accounts map[string]Account
	rows     map[uuid.UUID][]investmentRow
	frozen   map[uuid.UUID]acquired
	exp      int
}

// investments measures or reads each member's acquisition and makes its
// investment entry, in member code order.
func (e *Endpoints) investments(ctx context.Context, tx pgx.Tx, r Run, doc *config.Config) ([]entry, []acquired, error) {
	in, err := e.investingOf(ctx, tx, r, doc)
	if err != nil {
		return nil, nil, err
	}
	var entries []entry
	var out []acquired
	for _, m := range in.members {
		gm := in.member(r.GroupCode, m.CompanyCode)
		acc, ok := in.accounts[gm.InvestmentAccount]
		if !ok {
			return nil, nil, fmt.Errorf("consolidation: investment account %s of member %s is not an account of the parent", gm.InvestmentAccount, m.CompanyCode)
		}
		var invested money.Minor
		for _, row := range in.rows[acc.ID] {
			invested += row.gc
		}
		a, err := e.acquisitionOf(ctx, tx, r, in, m, gm, acc, invested)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, a)
		if en, ok := investmentEntry(a, in.parent, in.rows[acc.ID]); ok {
			entries = append(entries, en)
		}
	}
	return entries, out, nil
}

// investingOf reads what a run's investment entries are made from under
// the configuration version the run read.
func (e *Endpoints) investingOf(ctx context.Context, tx pgx.Tx, r Run, doc *config.Config) (investing, error) {
	ms, err := members(ctx, tx, []uuid.UUID{r.ID})
	if err != nil {
		return investing{}, err
	}
	in := investing{doc: doc, accounts: map[string]Account{}, rows: map[uuid.UUID][]investmentRow{}}
	for _, m := range ms[r.ID] {
		if m.Role == "parent" {
			in.parent = m
		} else {
			in.members = append(in.members, m)
		}
	}
	if in.exp, err = currency.Exponent(ctx, tx, r.GroupCurrency, nil); err != nil {
		return investing{}, fmt.Errorf("consolidation: currency %s: %w", r.GroupCurrency, err)
	}
	if len(in.members) == 0 {
		return in, nil
	}
	accounts, err := e.Ledger.Accounts(ctx, r.ParentID)
	if err != nil {
		return investing{}, fmt.Errorf("consolidation: accounts of %s: %w", in.parent.CompanyCode, err)
	}
	for _, a := range accounts {
		in.accounts[a.Number] = a
	}
	if err := in.readRows(ctx, tx, r); err != nil {
		return investing{}, err
	}
	in.frozen, err = frozenAcquisitions(ctx, tx, r.GroupCode)
	return in, err
}

// readRows reads the parent's translated rows on its members' investment
// accounts, by account and line.
func (in *investing) readRows(ctx context.Context, tx pgx.Tx, r Run) error {
	var ids []uuid.UUID
	for _, m := range in.members {
		if a, ok := in.accounts[in.member(r.GroupCode, m.CompanyCode).InvestmentAccount]; ok {
			ids = append(ids, a.ID)
		}
	}
	rows, _ := tx.Query(ctx, `SELECT line_no, group_account, account_id, trading_partner_id, amount_gc FROM consolidation.run_balance
		WHERE run_id = $1 AND legal_entity_id = $2 AND account_id = ANY($3) ORDER BY line_no`, r.ID, r.ParentID, ids)
	var x investmentRow
	_, err := pgx.ForEachRow(rows, []any{&x.line, &x.group, &x.account, &x.partner, &x.gc}, func() error {
		in.rows[*x.account] = append(in.rows[*x.account], x)
		return nil
	})
	return errs.Wrapf(err, "consolidation: investments of run %s", r.ID)
}

// member is a group member of the run's version; validation keeps every
// member of the run in it.
func (in investing) member(group, code string) config.GroupMember {
	g := in.doc.Consolidation.Group(group)
	return g.Members[slices.IndexFunc(g.Members, func(m config.GroupMember) bool { return m.Company == code })]
}

// frozenAcquisitions are the group's frozen acquisitions by member, with
// their equity.
func frozenAcquisitions(ctx context.Context, tx pgx.Tx, group string) (map[uuid.UUID]acquired, error) {
	// One statement, so an acquisition another finalization freezes
	// meanwhile is read whole or not at all (read committed).
	rows, _ := tx.Query(ctx, `SELECT a.legal_entity_id, a.origin, a.ownership_percent::text, a.first_fiscal_year, a.first_fiscal_period,
			a.investment_account_id, a.net_assets_lc, a.closing_rate::text, a.rate_valid_from, a.net_assets_gc, a.investment_gc, a.run_id,
			coalesce((SELECT jsonb_object_agg(e.group_account, e.amount_gc) FROM consolidation.acquisition_equity e
				WHERE e.tenant_id = a.tenant_id AND e.group_code = a.group_code AND e.legal_entity_id = a.legal_entity_id), '{}')
		FROM consolidation.acquisition a WHERE a.group_code = $1`, group)
	out := map[uuid.UUID]acquired{}
	var a acquired
	var by uuid.UUID
	var equity map[string]int64
	_, err := pgx.ForEachRow(rows, []any{&a.company, &a.origin, &a.ownership, &a.firstYear, &a.firstP, &a.investmentAccount, &a.netLC, &a.rate,
		&a.rateFrom, &a.netGC, &a.investment, &by, &equity}, func() error {
		x, frozenBy := a, by
		x.rate, x.frozenBy, x.equity = plainRate(a.rate), &frozenBy, make(map[string]money.Minor, len(equity))
		if a.rateFrom != nil {
			from := *a.rateFrom
			x.rateFrom = &from
		}
		for account, amount := range equity {
			x.equity[account] = money.Minor(amount)
		}
		out[x.company] = x
		equity = nil
		return nil
	})
	return out, errs.Wrapf(err, "consolidation: frozen acquisitions of %s", group)
}

// acquisitionOf is a member's acquisition: the frozen one, checked against
// the member's configuration and the investment, or measured at the
// period before its first and checked against the investment.
func (e *Endpoints) acquisitionOf(ctx context.Context, tx pgx.Tx, r Run, in investing, m Member, gm config.GroupMember, acc Account,
	invested money.Minor) (acquired, error) {
	if a, ok := in.frozen[m.LegalEntityID]; ok {
		a.code = m.CompanyCode
		return a, frozenChecks(a, m, gm, acc, invested, in.exp, r.GroupCurrency)
	}
	a, err := e.measure(ctx, tx, r, in, m)
	if err != nil {
		return acquired{}, err
	}
	a.origin, a.investmentAccount, a.investment = gm.Origin, acc.ID, invested
	if capital := a.shareCapital(); invested != capital {
		counted := ""
		if a.allEquity {
			counted = " (it has no retained earnings account (companies[].year_end.retained_earnings_account), so every equity account counts)"
		}
		return acquired{}, problem.Unprocessable("consolidation.investment_mismatch", fmt.Sprintf("The parent's investment in company %s, %s %s on "+
			"account %s, differs from its share capital, %s %s as booked%s; for a member the parent incorporated and holds whole the investment is "+
			"eliminated against its share capital without goodwill (B5(a)), and a difference waits for the accounting specialist: %s.", m.CompanyCode,
			invested.Plain(in.exp), r.GroupCurrency, acc.Number, capital.Plain(in.exp), r.GroupCurrency, counted, nonPilot), problem.FieldError{Field: "group_code",
			Code: "investment_mismatch", Message: "company " + m.CompanyCode + "'s investment equal to its share capital"})
	}
	return a, nil
}

// shareCapital is the member's share capital its investment entry
// eliminates, debit positive: minus its acquisition equity.
func (a acquired) shareCapital() money.Minor {
	var sum money.Minor
	for _, amount := range a.equity {
		sum -= amount
	}
	return sum
}

// frozenChecks refuses a run whose member's configuration or parent's
// investment differs from the frozen acquisition's (ADR-0059 §9).
func frozenChecks(a acquired, m Member, gm config.GroupMember, acc Account, invested money.Minor, exp int, group string) error {
	ownership, _ := money.ParseDecimal(gm.OwnershipPercent, 4) // validated
	frozen, _ := money.ParseDecimal(a.ownership, 4)            // a numeric(7,4)
	if gm.Origin != a.origin || ownership != frozen || *m.FirstYear != a.firstYear || *m.FirstPeriod != a.firstP || acc.ID != a.investmentAccount {
		return problem.Unprocessable("consolidation.acquisition_changed", fmt.Sprintf("Company %s's origin, ownership, first period or investment "+
			"account differs from its acquisition frozen by run %s (%s, %s%%, %d-%02d); changes after first consolidation are not consolidated "+
			"(ADR-0059 §1, §9).", m.CompanyCode, a.frozenBy, a.origin, plainRate(a.ownership), a.firstYear, a.firstP),
			problem.FieldError{Field: "group_code", Code: "acquisition_changed", Message: "company " + m.CompanyCode + " as first consolidated"})
	}
	if invested != a.investment {
		return problem.Unprocessable("consolidation.investment_changed", fmt.Sprintf("The parent's investment in company %s, %s %s, differs from "+
			"%s %s at its acquisition frozen by run %s; changes of the investment after first consolidation are not consolidated (ADR-0059 §1, §9).",
			m.CompanyCode, invested.Plain(exp), group, a.investment.Plain(exp), group, a.frozenBy), problem.FieldError{Field: "group_code",
			Code: "investment_changed", Message: "company " + m.CompanyCode + "'s investment as first consolidated"})
	}
	return nil
}

// measure measures a member's acquisition: its net assets at the period
// before its first, its asset and liability rows translated through it at
// its closing rate (§9), and its share capital (shareCapital), its
// acquisition equity.
func (e *Endpoints) measure(ctx context.Context, tx pgx.Tx, r Run, in investing, m Member) (acquired, error) {
	co := Company{ID: m.LegalEntityID, Code: m.CompanyCode, LocalCurrency: m.LocalCurrency}
	_, rate, err := e.measurePeriod(ctx, r.RateType, r.GroupCurrency, co, *m.FirstYear, *m.FirstPeriod)
	if err != nil {
		return acquired{}, err
	}
	year, per := previousPeriod(*m.FirstYear, *m.FirstPeriod)
	f, err := e.figuresOf(ctx, tx, r, m, year, per, rate.Rate)
	if err != nil {
		return acquired{}, err
	}
	rows, err := translate(f)
	if err != nil {
		return acquired{}, err
	}
	a := acquired{company: m.LegalEntityID, code: m.CompanyCode, ownership: *m.OwnershipPercent, firstYear: *m.FirstYear, firstP: *m.FirstPeriod,
		rate: rate.Shown}
	if co.LocalCurrency != r.GroupCurrency {
		a.rate, a.rateFrom = plainRate(rate.Shown), &rate.ValidFrom
	}
	for _, row := range rows {
		if row.atClosingRate {
			a.netLC, a.netGC = a.netLC+*row.lc, a.netGC+row.gc
		}
	}
	var retained string
	if c := in.doc.Company(m.CompanyCode); c != nil {
		retained = c.YearEnd.RetainedEarningsAccount
	}
	a.allEquity = retained == ""
	a.equity, err = shareCapital(ctx, tx, r.ID, m, f.accounts, retained)
	return a, err
}

// shareCapital is a member's share capital as the run translated it, per
// group account, credit negative: its rows through the run's period on
// its equity accounts other than its retained earnings account (ADR-0060
// §4), at their booked group amounts, each posting's rate (§7; B3's
// historical rates; po:2026-11-05, a migrated opening entry's at its
// date's). Only it is eliminated against the investment (B5(a),
// po:2026-11-09): the member's results and retained earnings stay the
// group's, its income counted from its incorporation (B8), and so does its
// translation reserve. It is read through the run's period, as the
// investment is, so that share capital an opening entry migrated into the
// first consolidated period carries is eliminated; the first final run
// freezes both.
func shareCapital(ctx context.Context, tx pgx.Tx, run uuid.UUID, m Member, accounts map[uuid.UUID]Account, retained string) (map[string]money.Minor, error) {
	var ids []uuid.UUID
	for id, a := range accounts {
		if a.Type == config.Equity && a.Number != retained {
			ids = append(ids, id)
		}
	}
	rows, _ := tx.Query(ctx, `SELECT group_account, sum(amount_gc)::bigint FROM consolidation.run_balance
		WHERE run_id = $1 AND legal_entity_id = $2 AND kind = 'balance' AND account_id = ANY($3) GROUP BY group_account`, run, m.LegalEntityID, ids)
	out := map[string]money.Minor{}
	var group string
	var amount int64
	_, err := pgx.ForEachRow(rows, []any{&group, &amount}, func() error {
		out[group] = money.Minor(amount)
		return nil
	})
	return out, errs.Wrapf(err, "consolidation: share capital of %s in run %s", m.CompanyCode, run)
}

// investmentEntry is a member's investment entry: its acquisition equity,
// its share capital, reversed per group account, of the member and no
// company account, and the parent's rows on its investment account
// reversed, lines of zero left out; none when no line is left. Goodwill
// and the interest are zero (a run refuses a member that needs them), so
// it sums to zero.
func investmentEntry(a acquired, parent Member, rows []investmentRow) (entry, bool) {
	en := entry{kind: EntryInvestment, company: a.company}
	member, parentID := a.company, parent.LegalEntityID
	for _, group := range slices.Sorted(maps.Keys(a.equity)) {
		if amount := a.equity[group]; amount != 0 {
			en.lines = append(en.lines, entryLine{group: group, company: &member, companyCode: a.code, amount: -amount, component: componentEquity})
		}
	}
	for _, row := range rows {
		if row.gc != 0 {
			en.lines = append(en.lines, entryLine{group: row.group, company: &parentID, account: row.account, partner: row.partner,
				companyCode: parent.CompanyCode, line: row.line, amount: -row.gc, component: componentInvestment})
		}
	}
	slices.SortFunc(en.lines, func(x, y entryLine) int {
		return cmp.Or(strings.Compare(x.group, y.group), strings.Compare(x.companyCode, y.companyCode), cmp.Compare(x.line, y.line))
	})
	return en, len(en.lines) > 0
}

// writeAcquisitions writes a run's acquisitions in one statement, without
// goodwill or an interest: a run includes only members the parent
// incorporated and holds whole (000246).
func writeAcquisitions(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run uuid.UUID, acqs []acquired) error {
	n := len(acqs)
	companies, accounts, origins, rates := make([]uuid.UUID, n), make([]uuid.UUID, n), make([]string, n), make([]string, n)
	froms, frozen := make([]pgtype.Date, n), make([]bool, n)
	lc, gc, invested := make([]int64, n), make([]int64, n), make([]int64, n)
	for i, a := range acqs {
		companies[i], accounts[i], origins[i], rates[i], frozen[i] = a.company, a.investmentAccount, a.origin, a.rate, a.frozenBy != nil
		lc[i], gc[i], invested[i] = int64(a.netLC), int64(a.netGC), int64(a.investment)
		if a.rateFrom != nil {
			froms[i] = pgtype.Date{Time: *a.rateFrom, Valid: true}
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO consolidation.run_acquisition (tenant_id, run_id, legal_entity_id, origin, investment_account_id, net_assets_lc,
			closing_rate, rate_valid_from, net_assets_gc, investment_gc, goodwill_gc, nci_gc, frozen)
		SELECT $1, $2, x.c, x.o, x.a, x.lc, x.r::numeric, x.f, x.gc, x.i, 0, 0, x.z
		FROM unnest($3::uuid[], $4::text[], $5::uuid[], $6::bigint[], $7::text[], $8::date[], $9::bigint[], $10::bigint[], $11::boolean[])
			AS x(c, o, a, lc, r, f, gc, i, z)`, p.TenantID, run, companies, origins, accounts, lc, rates, froms, gc, invested, frozen)
	return errs.Wrapf(err, "consolidation: acquisitions of run %s", run)
}
