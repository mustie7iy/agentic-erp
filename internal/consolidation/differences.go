package consolidation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// lineCursor is where a page of a run's differences or entries ends: by
// line or entry number (ADR-0059 §5).
type lineCursor struct {
	No int32 `json:"n"`
}

// pageAfter reads a list's page size and where its page starts.
func (e *Endpoints) pageAfter(scope string, size gen.OptInt32, token gen.OptString) (int, int32, error) {
	n, err := pagination.Size(size.Value, size.Set)
	if err != nil {
		return 0, 0, err
	}
	var after lineCursor
	if token.Set {
		if err := e.Pages.Decode(scope, token.Value, &after); err != nil {
			return 0, 0, err
		}
	}
	return n, after.No, nil
}

// eliminationRef names an elimination of the run's version.
func (n names) eliminationRef(code string) gen.EliminationRef {
	out := gen.EliminationRef{Code: code}
	for _, el := range n.chart.Eliminations {
		if el.Code == code {
			out.Name = el.Name
		}
	}
	return out
}

// storedDifference is a difference as stored, with its company's code.
type storedDifference struct {
	id                         uuid.UUID
	line, version              int32
	elimination, code, status  string
	company                    uuid.UUID
	partner, acceptedBy        *uuid.UUID
	amount                     money.Minor
	partnerAmount, diff, toler *money.Minor
	acceptedAt                 *time.Time
	reason                     *string
}

// differenceColumns are a difference's columns, as scanDifference reads
// them, over differenceWhere's tables.
const differenceColumns = `d.id, d.line_no, d.elimination_code, d.legal_entity_id, m.company_code, d.partner_id, d.status, d.amount, d.partner_amount,
	d.difference, d.tolerance, d.accepted_by, d.accepted_at, d.reason, d.version`

func scanDifference(row pgx.CollectableRow) (storedDifference, error) {
	var s storedDifference
	return s, errs.Wrapf(row.Scan(&s.id, &s.line, &s.elimination, &s.company, &s.code, &s.partner, &s.status, &s.amount, &s.partnerAmount, &s.diff,
		&s.toler, &s.acceptedBy, &s.acceptedAt, &s.reason, &s.version), "scan intercompany difference")
}

// differenceWhere selects a run's ($1) differences by status ($2),
// elimination ($3) and company ($4: the company or the partner).
const differenceWhere = `FROM consolidation.run_difference d
	JOIN consolidation.run_member m ON m.tenant_id = d.tenant_id AND m.run_id = d.run_id AND m.legal_entity_id = d.legal_entity_id
	WHERE d.run_id = $1 AND ($2 = '' OR d.status = $2) AND ($3 = '' OR d.elimination_code = $3)
		AND ($4::uuid IS NULL OR d.legal_entity_id = $4 OR d.partner_id = $4)`

// ListIntercompanyDifferences implements GET
// /v1/consolidation-runs/{id}/differences (ADR-0059 §8): a run's
// differences by line, filtered, each named, with the totals per status
// over the filter.
func (e *Endpoints) ListIntercompanyDifferences(ctx context.Context, params gen.ListIntercompanyDifferencesParams) (gen.ListIntercompanyDifferencesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listIntercompanyDifferences|%s|%s|%s|%t %s", r.ID, params.Status.Value, params.Elimination.Value, params.LegalEntityID.Set,
		params.LegalEntityID.Value)
	size, after, err := e.pageAfter(scope, params.PageSize, params.PageToken)
	if err != nil {
		return nil, err
	}
	filter := []any{r.ID, params.Status.Value, params.Elimination.Value, apitypes.Ptr[uuid.UUID](params.LegalEntityID)}
	rows, _ := tx.Query(ctx, `SELECT `+differenceColumns+` `+differenceWhere+` AND d.line_no > $5 ORDER BY d.line_no LIMIT $6`,
		append(filter, after, size+1)...)
	page, err := pgx.CollectRows(rows, scanDifference)
	if err != nil {
		return nil, fmt.Errorf("consolidation: intercompany differences of run %s: %w", r.ID, err)
	}
	out := gen.IntercompanyDifferencePage{Items: []gen.IntercompanyDifference{}}
	if len(page) > size {
		page = page[:size]
		tok, _ := e.Pages.Encode(scope, lineCursor{No: page[size-1].line}) // a number always encodes
		out.NextPageToken.SetTo(tok)
	}
	n, err := e.namesOf(ctx, tx, r, nil)
	if err != nil {
		return nil, err
	}
	if n.people, err = e.acceptors(ctx, page); err != nil {
		return nil, err
	}
	for _, s := range page {
		out.Items = append(out.Items, n.differenceAPI(s))
	}
	out.Totals, err = n.differenceTotals(ctx, tx, r, filter)
	return &out, err
}

// optMoney is an amount in the group currency, if any.
func (n names) optMoney(m *money.Minor) gen.OptMoney {
	if m == nil {
		return gen.OptMoney{}
	}
	return gen.NewOptMoney(n.money(*m, n.group))
}

// differenceAPI shows a difference with its names.
func (n names) differenceAPI(s storedDifference) gen.IntercompanyDifference {
	out := gen.IntercompanyDifference{ID: s.id, LineNo: s.line, Elimination: n.eliminationRef(s.elimination), LegalEntityID: s.company,
		CompanyCode: s.code, CompanyName: n.companies[s.company].Name, Partner: n.tradingPartner(s.partner), Status: s.status,
		Amount: n.money(s.amount, n.group), PartnerAmount: n.optMoney(s.partnerAmount), Difference: n.optMoney(s.diff), Tolerance: n.optMoney(s.toler),
		AcceptedBy: apitypes.OptUUID(s.acceptedBy), Reason: apitypes.OptString(s.reason), Version: s.version}
	if s.acceptedBy != nil {
		out.AcceptedByName = gen.NewOptString(n.people[*s.acceptedBy])
		out.AcceptedAt = gen.NewOptDateTime(*s.acceptedAt)
	}
	return out
}

// acceptors names the people who accepted a page's differences.
func (e *Endpoints) acceptors(ctx context.Context, page []storedDifference) (map[uuid.UUID]string, error) {
	return idref.Resolve(ctx, e.People.Named, idref.Of(page, func(s storedDifference) *uuid.UUID { return s.acceptedBy }), personRef)
}

// differenceTotals are the differences the filter selects summed per
// status, in the statuses' order.
func (n names) differenceTotals(ctx context.Context, tx pgx.Tx, r Run, filter []any) ([]gen.IntercompanyDifferenceTotal, error) {
	rows, _ := tx.Query(ctx, `SELECT d.status, count(*)::int, sum(d.amount)::bigint, sum(d.partner_amount)::bigint, sum(d.difference)::bigint `+
		differenceWhere+` GROUP BY d.status ORDER BY array_position($5::text[], d.status)`, append(filter, differenceStatuses)...)
	out := []gen.IntercompanyDifferenceTotal{}
	var t gen.IntercompanyDifferenceTotal
	var amount money.Minor
	var partner, diff *money.Minor
	_, err := pgx.ForEachRow(rows, []any{&t.Status, &t.Count, &amount, &partner, &diff}, func() error {
		t.Amount, t.PartnerAmount, t.Difference = n.money(amount, n.group), n.optMoney(partner), n.optMoney(diff)
		out = append(out, t)
		return nil
	})
	return out, errs.Wrapf(err, "consolidation: totals of the differences of run %s", r.ID)
}

// storedEntry is an entry as stored, with its company's code and lines.
type storedEntry struct {
	no          int32
	kind, code  string
	elimination *string
	company     uuid.UUID
	partner     *uuid.UUID
	lines       []storedEntryLine
	debits      money.Minor
}

// storedEntryLine is an entry's line as stored, with its company's code.
type storedEntryLine struct {
	entry, line          int32
	group                string
	company, account, tp *uuid.UUID
	code, component      *string
	amount               money.Minor
}

// entryWhere selects a run's ($1) entries by kind ($2), elimination ($3)
// and company ($4: the entry's company or partner).
const entryWhere = `FROM consolidation.run_entry en
	JOIN consolidation.run_member m ON m.tenant_id = en.tenant_id AND m.run_id = en.run_id AND m.legal_entity_id = en.legal_entity_id
	WHERE en.run_id = $1 AND ($2 = '' OR en.kind = $2) AND ($3 = '' OR en.elimination_code = $3)
		AND ($4::uuid IS NULL OR en.legal_entity_id = $4 OR en.partner_id = $4)`

// ListConsolidationEntries implements GET
// /v1/consolidation-runs/{id}/entries (ADR-0059 §8): a run's elimination
// entries by number with their lines, filtered, each named, with the
// totals per kind over the filter.
func (e *Endpoints) ListConsolidationEntries(ctx context.Context, params gen.ListConsolidationEntriesParams) (gen.ListConsolidationEntriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listConsolidationEntries|%s|%s|%s|%t %s", r.ID, params.Kind.Value, params.Elimination.Value, params.LegalEntityID.Set,
		params.LegalEntityID.Value)
	size, after, err := e.pageAfter(scope, params.PageSize, params.PageToken)
	if err != nil {
		return nil, err
	}
	filter := []any{r.ID, params.Kind.Value, params.Elimination.Value, apitypes.Ptr[uuid.UUID](params.LegalEntityID)}
	rows, _ := tx.Query(ctx, `SELECT en.entry_no, en.kind, en.elimination_code, en.legal_entity_id, m.company_code, en.partner_id `+entryWhere+`
		AND en.entry_no > $5 ORDER BY en.entry_no LIMIT $6`, append(filter, after, size+1)...)
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedEntry, error) {
		var s storedEntry
		return s, row.Scan(&s.no, &s.kind, &s.elimination, &s.company, &s.code, &s.partner)
	})
	if err != nil {
		return nil, fmt.Errorf("consolidation: elimination entries of run %s: %w", r.ID, err)
	}
	out := gen.ConsolidationEntryPage{Items: []gen.ConsolidationEntry{}}
	if len(page) > size {
		page = page[:size]
		tok, _ := e.Pages.Encode(scope, lineCursor{No: page[size-1].no}) // a number always encodes
		out.NextPageToken.SetTo(tok)
	}
	companies, err := entryLines(ctx, tx, r, page)
	if err != nil {
		return nil, err
	}
	n, err := e.namesOf(ctx, tx, r, companies)
	if err != nil {
		return nil, err
	}
	for _, s := range page {
		out.Items = append(out.Items, n.entryAPI(s))
	}
	out.Totals, err = n.entryTotals(ctx, tx, r, filter)
	return &out, err
}

// entryLines reads the lines of a page of entries into them, by entry and
// line, and returns the companies they name.
func entryLines(ctx context.Context, tx pgx.Tx, r Run, page []storedEntry) ([]uuid.UUID, error) {
	at := map[int32]*storedEntry{}
	nos := make([]int32, len(page))
	for i := range page {
		at[page[i].no], nos[i] = &page[i], page[i].no
	}
	rows, _ := tx.Query(ctx, `SELECT l.entry_no, l.line_no, l.group_account, l.legal_entity_id, m.company_code, l.account_id, l.trading_partner_id, l.component, l.amount_gc
		FROM consolidation.run_entry_line l
		LEFT JOIN consolidation.run_member m ON m.tenant_id = l.tenant_id AND m.run_id = l.run_id AND m.legal_entity_id = l.legal_entity_id
		WHERE l.run_id = $1 AND l.entry_no = ANY($2) ORDER BY l.entry_no, l.line_no`, r.ID, nos)
	var l storedEntryLine
	var companies []uuid.UUID
	_, err := pgx.ForEachRow(rows, []any{&l.entry, &l.line, &l.group, &l.company, &l.code, &l.account, &l.tp, &l.component, &l.amount}, func() error {
		en := at[l.entry]
		en.lines = append(en.lines, l)
		if l.amount > 0 {
			en.debits += l.amount
		}
		if l.company != nil {
			companies = append(companies, *l.company)
		}
		return nil
	})
	return companies, errs.Wrapf(err, "consolidation: lines of the elimination entries of run %s", r.ID)
}

// entryAPI shows an entry with its lines and names.
func (n names) entryAPI(s storedEntry) gen.ConsolidationEntry {
	out := gen.ConsolidationEntry{EntryNo: s.no, Kind: s.kind, LegalEntityID: s.company, CompanyCode: s.code, CompanyName: n.companies[s.company].Name,
		Partner: n.tradingPartner(s.partner), Debits: n.money(s.debits, n.group), Lines: []gen.ConsolidationEntryLine{}}
	if s.elimination != nil {
		out.Elimination = gen.NewOptEliminationRef(n.eliminationRef(*s.elimination))
	}
	for _, l := range s.lines {
		x := gen.ConsolidationEntryLine{LineNo: l.line, GroupAccount: n.groupAccount(l.group), LegalEntityID: apitypes.OptUUID(l.company),
			CompanyCode: apitypes.OptString(l.code), AccountID: apitypes.OptUUID(l.account), TradingPartner: n.tradingPartner(l.tp),
			Component: apitypes.OptString(l.component), Amount: n.money(l.amount, n.group)}
		if l.company != nil {
			x.CompanyName = gen.NewOptString(n.companies[*l.company].Name)
		}
		if l.account != nil {
			a := n.accounts[*l.account]
			x.Account = gen.NewOptAssetAccountRef(gen.AssetAccountRef{Number: a.Number, Name: a.Name})
		}
		out.Lines = append(out.Lines, x)
	}
	return out
}

// entryTotals are the entries the filter selects counted and their debits
// summed per kind, in the kinds' order.
func (n names) entryTotals(ctx context.Context, tx pgx.Tx, r Run, filter []any) ([]gen.ConsolidationEntryTotal, error) {
	rows, _ := tx.Query(ctx, `SELECT en.kind, count(*)::int, coalesce(sum((SELECT sum(l.amount_gc) FROM consolidation.run_entry_line l
			WHERE l.tenant_id = en.tenant_id AND l.run_id = en.run_id AND l.entry_no = en.entry_no AND l.amount_gc > 0)), 0)::bigint `+entryWhere+`
		GROUP BY en.kind ORDER BY array_position($5::text[], en.kind)`, append(filter, entryKinds)...)
	out := []gen.ConsolidationEntryTotal{}
	var t gen.ConsolidationEntryTotal
	var debits money.Minor
	_, err := pgx.ForEachRow(rows, []any{&t.Kind, &t.Count, &debits}, func() error {
		t.Debits = n.money(debits, n.group)
		out = append(out, t)
		return nil
	})
	return out, errs.Wrapf(err, "consolidation: totals of the elimination entries of run %s", r.ID)
}
