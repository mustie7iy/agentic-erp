package consolidation

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Intercompany matching and eliminations (ADR-0059 §8). For each
// elimination and each pair of the run's companies {A, B}, A's code before
// B's, the run sums A's translated rows on the elimination's group
// accounts naming B (amount) and B's naming A (partner amount), in the
// group currency, debit positive: their sum is the difference, matched at
// zero, within_tolerance up to the elimination's tolerance in absolute
// value, open above it. Rows on those accounts naming a company outside
// the run are outside_group per company and partner, rows naming none
// no_trading_partner per company: listed, not eliminated. Each pair with
// rows gets one intercompany entry reversing them, its difference on the
// elimination's difference account, so it sums to zero.

// Intercompany difference statuses (ADR-0059 §5), in the order the
// totals list them.
const (
	DiffMatched          = "matched"
	DiffWithinTolerance  = "within_tolerance"
	DiffOpen             = "open"
	DiffAccepted         = "accepted"
	DiffOutsideGroup     = "outside_group"
	DiffNoTradingPartner = "no_trading_partner"
)

// differenceStatuses are the statuses in their order.
var differenceStatuses = []string{DiffMatched, DiffWithinTolerance, DiffOpen, DiffAccepted, DiffOutsideGroup, DiffNoTradingPartner}

// Elimination entry kinds (ADR-0059 §5), in entry order: T68f writes the
// intercompany ones, T68h the others.
const (
	EntryIntercompany = "intercompany"
	EntryInvestment   = "investment"
	EntryNCI          = "nci"
)

// entryKinds are the kinds in their order.
var entryKinds = []string{EntryIntercompany, EntryInvestment, EntryNCI}

// icRow is a run's translated row on an elimination's group account.
type icRow struct {
	company          uuid.UUID
	line             int32
	group            string
	account, partner *uuid.UUID
	gc               money.Minor
}

// difference is a difference as matched: a pair's (company, partner, both
// sides, their sum and the tolerance, and the pair's rows) or a company's
// rows naming a partner outside the run or none (its amount alone).
type difference struct {
	elimination              string
	company                  uuid.UUID
	partner                  *uuid.UUID
	status                   string
	amount, partnerAmount    money.Minor
	diff, tolerance          money.Minor
	pair                     bool
	rows                     []icRow
	companyCode, partnerCode string
}

// entryLine is an elimination entry's line: a reversed row's, with its
// company's code and line for the order, a difference's, of no company,
// or an acquisition equity's, of the member and no line; with its
// component on an investment entry.
type entryLine struct {
	group                     string
	company, account, partner *uuid.UUID
	companyCode               string
	line                      int32
	amount                    money.Minor
	component                 string
}

// entry is an elimination entry: an intercompany entry of an elimination
// and a pair, or a member's investment entry (no elimination, partner
// uuid.Nil).
type entry struct {
	kind             string
	elimination      string
	company, partner uuid.UUID
	lines            []entryLine
}

// matching is what the matching reads: the run's companies' codes by
// legal entity, every legal entity's code (a trading partner outside the
// run's), and for each elimination its accounts, tolerance in the group
// currency's minor units and difference account.
type matching struct {
	codes, partners map[uuid.UUID]string
	eliminationOf   map[string]string // group account -> elimination code
	tolerance       map[string]money.Minor
	differenceTo    map[string]string // elimination code -> difference account
}

// match matches the rows into differences, in their list order, and the
// pairs' entries, in entry order.
func (m matching) match(rows []icRow) ([]difference, []entry) {
	type key struct {
		elimination string
		company     uuid.UUID
		partner     uuid.UUID // uuid.Nil: none
	}
	byKey := map[key]*difference{}
	var out []*difference
	at := func(k key, status string, partner *uuid.UUID) *difference {
		d, ok := byKey[k]
		if !ok {
			d = &difference{elimination: k.elimination, company: k.company, partner: partner, status: status, companyCode: m.codes[k.company],
				pair: status == DiffMatched, partnerCode: m.partnerCode(partner)}
			byKey[k] = d
			out = append(out, d)
		}
		return d
	}
	for _, r := range rows {
		elimination := m.eliminationOf[r.group]
		switch _, member := m.codes[derefID(r.partner)]; {
		case r.partner == nil:
			at(key{elimination, r.company, uuid.Nil}, DiffNoTradingPartner, nil).amount += r.gc
		case !member:
			at(key{elimination, r.company, *r.partner}, DiffOutsideGroup, r.partner).amount += r.gc
		case m.codes[r.company] < m.codes[*r.partner]:
			d := at(key{elimination, r.company, *r.partner}, DiffMatched, r.partner)
			d.amount, d.rows = d.amount+r.gc, append(d.rows, r)
		default:
			company := *r.partner
			d := at(key{elimination, company, r.company}, DiffMatched, &r.company)
			d.partnerAmount, d.rows = d.partnerAmount+r.gc, append(d.rows, r)
		}
	}
	slices.SortFunc(out, func(a, b *difference) int {
		// No partner's code is "", before every company code.
		return cmp.Or(strings.Compare(a.elimination, b.elimination), strings.Compare(a.companyCode, b.companyCode),
			strings.Compare(a.partnerCode, b.partnerCode))
	})
	diffs := make([]difference, len(out))
	var entries []entry
	for i, d := range out {
		if d.pair {
			d.diff, d.tolerance = d.amount+d.partnerAmount, m.tolerance[d.elimination]
			d.status = statusOf(d.diff, d.tolerance)
			if en, ok := m.entryOf(*d); ok {
				entries = append(entries, en)
			}
		}
		diffs[i] = *d
	}
	return diffs, entries
}

// statusOf is a pair's status by its difference and tolerance.
func statusOf(diff, tolerance money.Minor) string {
	switch {
	case diff == 0:
		return DiffMatched
	case diff.Abs() <= tolerance:
		return DiffWithinTolerance
	}
	return DiffOpen
}

// entryOf is a pair's intercompany entry: each of its rows reversed and
// its difference on the elimination's difference account, lines of zero
// left out; none when no line is left.
func (m matching) entryOf(d difference) (entry, bool) {
	en := entry{kind: EntryIntercompany, elimination: d.elimination, company: d.company, partner: *d.partner}
	for _, r := range d.rows {
		if r.gc != 0 {
			en.lines = append(en.lines, entryLine{group: r.group, company: &r.company, account: r.account, partner: r.partner,
				companyCode: m.codes[r.company], line: r.line, amount: -r.gc})
		}
	}
	if d.diff != 0 {
		en.lines = append(en.lines, entryLine{group: m.differenceTo[d.elimination], amount: d.diff})
	}
	slices.SortFunc(en.lines, func(a, b entryLine) int {
		return cmp.Or(strings.Compare(a.group, b.group), strings.Compare(a.companyCode, b.companyCode), cmp.Compare(a.line, b.line))
	})
	return en, len(en.lines) > 0
}

// partnerCode is a trading partner's company code (a company of the
// run's as the run read it), "" for none, and its id for a legal entity
// tenancy no longer lists (none is ever removed).
func (m matching) partnerCode(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	if code, ok := m.codes[*id]; ok {
		return code
	}
	if code, ok := m.partners[*id]; ok {
		return code
	}
	return id.String()
}

// derefID is the id, uuid.Nil for none.
func derefID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// EliminateConsolidationRun matches and eliminates a running run's
// intercompany balances in one transaction (ADR-0059 §8): it reads the
// rows on the eliminations' accounts of the configuration version the run
// read and writes their differences and the pairs' entries. A run that
// has differences or entries already, or is no longer running, is left
// alone, so the step run again writes nothing twice; one with a company
// not translated yet is refused, so its figures are matched only whole.
func (e *Endpoints) EliminateConsolidationRun(ctx context.Context, id uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := e.liveRun(ctx, tx, id, false)
	if err != nil || r.Status != RunRunning {
		return err
	}
	var done bool
	var pending int
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM consolidation.run_difference WHERE run_id = $1)
			OR EXISTS (SELECT FROM consolidation.run_entry WHERE run_id = $1) OR EXISTS (SELECT FROM consolidation.run_acquisition WHERE run_id = $1),
			(SELECT count(*) FROM consolidation.run_member WHERE run_id = $1 AND translated_at IS NULL)`, id).Scan(&done, &pending); err != nil {
		return fmt.Errorf("consolidation: eliminations of run %s: %w", id, err)
	}
	switch {
	case done:
		return nil
	case pending > 0:
		return fmt.Errorf("consolidation: %d of the companies of run %s are not translated yet; it is eliminated after them", pending, id)
	}
	doc, err := e.version(ctx, r.ConfigVersion)
	if err != nil {
		return fmt.Errorf("consolidation: configuration version %d of run %s: %w", r.ConfigVersion, r.ID, err)
	}
	m, accounts, err := e.matchingOf(ctx, tx, r, doc)
	if err != nil {
		return err
	}
	rows, _ := tx.Query(ctx, `SELECT legal_entity_id, line_no, group_account, account_id, trading_partner_id, amount_gc FROM consolidation.run_balance
		WHERE run_id = $1 AND group_account = ANY($2) ORDER BY legal_entity_id, line_no`, id, accounts)
	ic, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (icRow, error) {
		var x icRow
		return x, row.Scan(&x.company, &x.line, &x.group, &x.account, &x.partner, &x.gc)
	})
	if err != nil {
		return fmt.Errorf("consolidation: intercompany rows of run %s: %w", id, err)
	}
	diffs, entries := m.match(ic)
	investments, acqs, err := e.investments(ctx, tx, r, doc)
	if err != nil {
		return err
	}
	if err := writeDifferences(ctx, tx, p, id, diffs); err != nil {
		return err
	}
	if err := writeEntries(ctx, tx, p, id, append(entries, investments...)); err != nil {
		return err
	}
	return writeAcquisitions(ctx, tx, p, id, acqs)
}

// matchingOf reads what a run's matching needs, and the group accounts of
// its eliminations.
func (e *Endpoints) matchingOf(ctx context.Context, tx pgx.Tx, r Run, doc *config.Config) (matching, []string, error) {
	m := matching{codes: map[uuid.UUID]string{}, partners: map[uuid.UUID]string{}, eliminationOf: map[string]string{},
		tolerance: map[string]money.Minor{}, differenceTo: map[string]string{}}
	exp, err := currency.Exponent(ctx, tx, r.GroupCurrency, nil)
	if err != nil {
		return matching{}, nil, fmt.Errorf("consolidation: currency %s: %w", r.GroupCurrency, err)
	}
	var accounts []string
	for _, el := range doc.Consolidation.Eliminations {
		// Setup refused a tolerance finer than the group currency when the
		// version was submitted; one activated before is the run's failure.
		if m.tolerance[el.Code], err = money.ParseDecimal(el.Tolerance, exp); err != nil {
			return matching{}, nil, fmt.Errorf("consolidation: tolerance %q of elimination %s in %s: %w", el.Tolerance, el.Code, r.GroupCurrency, err)
		}
		m.differenceTo[el.Code] = el.DifferenceAccount
		for _, a := range el.Accounts {
			m.eliminationOf[a] = el.Code
			accounts = append(accounts, a)
		}
	}
	rows, _ := tx.Query(ctx, `SELECT legal_entity_id, company_code FROM consolidation.run_member WHERE run_id = $1`, r.ID)
	var company uuid.UUID
	var code string
	if _, err := pgx.ForEachRow(rows, []any{&company, &code}, func() error { m.codes[company] = code; return nil }); err != nil {
		return matching{}, nil, fmt.Errorf("consolidation: companies of run %s: %w", r.ID, err)
	}
	entities, err := e.Companies.Entities(ctx)
	if err != nil {
		return matching{}, nil, fmt.Errorf("consolidation: companies: %w", err)
	}
	for _, c := range entities {
		m.partners[c.ID] = c.Code
	}
	return m, accounts, nil
}

// optMinor is a pair's amount, NULL for a difference that has none.
func optMinor(d difference, m money.Minor) pgtype.Int8 {
	return pgtype.Int8{Int64: int64(m), Valid: d.pair}
}

// writeDifferences writes a run's differences, numbered in their order,
// in one statement.
func writeDifferences(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run uuid.UUID, diffs []difference) error {
	n := len(diffs)
	ids, nos, elims, statuses := make([]uuid.UUID, n), make([]int32, n), make([]string, n), make([]string, n)
	companies, partners := make([]uuid.UUID, n), make([]pgtype.UUID, n)
	amounts, partnerAmounts, sums, tolerances := make([]int64, n), make([]pgtype.Int8, n), make([]pgtype.Int8, n), make([]pgtype.Int8, n)
	for i, d := range diffs {
		ids[i], nos[i], elims[i], statuses[i], companies[i], amounts[i] = uuid.Must(uuid.NewV7()), int32(i+1), d.elimination, d.status, d.company, int64(d.amount)
		if d.partner != nil {
			partners[i] = pgtype.UUID{Bytes: *d.partner, Valid: true}
		}
		partnerAmounts[i], sums[i], tolerances[i] = optMinor(d, d.partnerAmount), optMinor(d, d.diff), optMinor(d, d.tolerance)
	}
	_, err := tx.Exec(ctx, `INSERT INTO consolidation.run_difference (tenant_id, id, run_id, line_no, elimination_code, legal_entity_id, partner_id,
			status, amount, partner_amount, difference, tolerance)
		SELECT $1, x.id, $2, x.n, x.e, x.c, x.p, x.s, x.a, x.pa, x.d, x.t
		FROM unnest($3::uuid[], $4::int[], $5::text[], $6::uuid[], $7::uuid[], $8::text[], $9::bigint[], $10::bigint[], $11::bigint[], $12::bigint[])
			AS x(id, n, e, c, p, s, a, pa, d, t)`,
		p.TenantID, run, ids, nos, elims, companies, partners, statuses, amounts, partnerAmounts, sums, tolerances)
	return errs.Wrapf(err, "consolidation: intercompany differences of run %s", run)
}

// optUUID is an id or NULL.
func optUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// optText is a text or NULL for "".
func optText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// writeEntries writes a run's elimination entries, numbered in their
// order (the intercompany ones first, then the investment ones), and
// their lines, each in one statement.
func writeEntries(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run uuid.UUID, entries []entry) error {
	var nos, lineEntries, lineNos []int32
	var kinds, groups []string
	var elims, components []pgtype.Text
	var companies []uuid.UUID
	var partners, lineCompanies, accounts, tradingPartners []pgtype.UUID
	var amounts []int64
	for i, en := range entries {
		no := int32(i + 1)
		nos, kinds, elims, companies = append(nos, no), append(kinds, en.kind), append(elims, optText(en.elimination)), append(companies, en.company)
		partners = append(partners, pgtype.UUID{Bytes: en.partner, Valid: en.partner != uuid.Nil})
		for j, l := range en.lines {
			lineEntries, lineNos, groups, amounts = append(lineEntries, no), append(lineNos, int32(j+1)), append(groups, l.group), append(amounts, int64(l.amount))
			lineCompanies, accounts, tradingPartners = append(lineCompanies, optUUID(l.company)), append(accounts, optUUID(l.account)),
				append(tradingPartners, optUUID(l.partner))
			components = append(components, optText(l.component))
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO consolidation.run_entry (tenant_id, run_id, entry_no, kind, legal_entity_id, partner_id, elimination_code)
		SELECT $1, $2, x.n, x.k, x.c, x.p, x.e FROM unnest($3::int[], $4::text[], $5::uuid[], $6::uuid[], $7::text[]) AS x(n, k, c, p, e)`,
		p.TenantID, run, nos, kinds, companies, partners, elims); err != nil {
		return fmt.Errorf("consolidation: elimination entries of run %s: %w", run, err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO consolidation.run_entry_line (tenant_id, run_id, entry_no, line_no, group_account, legal_entity_id, account_id,
			trading_partner_id, component, amount_gc)
		SELECT $1, $2, x.e, x.n, x.g, x.c, x.a, x.t, x.k, x.m
		FROM unnest($3::int[], $4::int[], $5::text[], $6::uuid[], $7::uuid[], $8::uuid[], $9::text[], $10::bigint[]) AS x(e, n, g, c, a, t, k, m)`,
		p.TenantID, run, lineEntries, lineNos, groups, lineCompanies, accounts, tradingPartners, components, amounts)
	return errs.Wrapf(err, "consolidation: elimination entry lines of run %s", run)
}
