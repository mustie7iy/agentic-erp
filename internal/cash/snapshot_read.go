package cash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Reading snapshots (ADR-0051 §6, T53f), and which are due.

const snapshotSelect = `SELECT s.id, s.legal_entity_id, s.kind, s.name, s.snapshot_date, s.time_zone, s.horizon_end, s.config_version, s.taken_at
	FROM cash.snapshot s`

func scanSnapshot(row pgx.Row) (Snapshot, error) {
	var s Snapshot
	err := row.Scan(&s.ID, &s.LegalEntity, &s.Kind, &s.Name, &s.Date, &s.TimeZone, &s.HorizonEnd, &s.ConfigVersion, &s.TakenAt)
	return s, errs.Wrapf(err, "scan cash snapshot")
}

// Snapshot is a snapshot of a legal entity the caller may act in, with its
// totals per currency; any other is not found.
func (e *Endpoints) Snapshot(ctx context.Context, id uuid.UUID) (Snapshot, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	s, err := scanSnapshot(tx.QueryRow(ctx, snapshotSelect+` WHERE s.id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(s.LegalEntity)):
		return Snapshot{}, problem.NotFoundOf("cash snapshot")
	case err != nil:
		return Snapshot{}, fmt.Errorf("cash: snapshot %s: %w", id, err)
	}
	ss := []Snapshot{s}
	if err := e.complete(ctx, tx, ss); err != nil {
		return Snapshot{}, err
	}
	return ss[0], nil
}

// complete gives each snapshot its company's code and its totals.
func (e *Endpoints) complete(ctx context.Context, tx pgx.Tx, ss []Snapshot) error {
	entities := make([]uuid.UUID, len(ss))
	for i, s := range ss {
		entities[i] = s.LegalEntity
	}
	codes, err := e.companyCodes(ctx, entities)
	if err != nil {
		return fmt.Errorf("cash: snapshots: %w", err)
	}
	for i, s := range ss {
		ss[i].CompanyCode = codes[s.LegalEntity]
	}
	return totals(ctx, tx, ss)
}

// companyCodes are the company codes of the legal entities, each read
// once: a stored row's legal entity the directory does not hold fails.
func (e *Endpoints) companyCodes(ctx context.Context, entities []uuid.UUID) (map[uuid.UUID]string, error) {
	codes := map[uuid.UUID]string{}
	for _, le := range entities {
		if _, ok := codes[le]; ok {
			continue
		}
		co, found, err := e.LegalEntities.Company(ctx, le)
		if err != nil {
			return nil, fmt.Errorf("legal entity %s: %w", le, err)
		}
		if !found {
			return nil, fmt.Errorf("legal entity %s is not held by the tenancy context", le)
		}
		codes[le] = co.Code
	}
	return codes, nil
}

// totals gives each snapshot its totals per currency, by currency code.
func totals(ctx context.Context, tx pgx.Tx, ss []Snapshot) error {
	ids, at := make([]uuid.UUID, len(ss)), map[uuid.UUID]int{}
	for i, s := range ss {
		ids[i], at[s.ID] = s.ID, i
		ss[i].Currencies = []SnapshotCurrency{}
	}
	rows, _ := tx.Query(ctx, `SELECT f.snapshot_id, f.currency, c.exponent,
			coalesce(sum(f.amount) FILTER (WHERE f.source <> 'payable_blocked'), 0)::bigint,
			coalesce(sum(f.amount) FILTER (WHERE f.source = 'payable_blocked'), 0)::bigint, count(*)
		FROM cash.snapshot_flow f JOIN ref.currency c ON c.code = f.currency
		WHERE f.snapshot_id = ANY($1) GROUP BY f.snapshot_id, f.currency, c.exponent ORDER BY f.snapshot_id, f.currency`, ids)
	var id uuid.UUID
	var c SnapshotCurrency
	_, err := pgx.ForEachRow(rows, []any{&id, &c.Currency, &c.Exponent, &c.Total, &c.Blocked, &c.Lines}, func() error {
		ss[at[id]].Currencies = append(ss[at[id]].Currencies, c)
		return nil
	})
	if err != nil {
		return fmt.Errorf("cash: totals of %d snapshots: %w", len(ss), err)
	}
	return nil
}

// SnapshotsQuery filters the snapshots listed.
type SnapshotsQuery struct {
	LegalEntity *uuid.UUID
	Kind        string
	From, To    *time.Time
	Page        pagination.Request
}

var snapshotSort = pagination.Sort{Default: "snapshot_date", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "snapshot_date", Column: "s.snapshot_date", Type: "date"},
}}

// Snapshots lists a page of the snapshots of the legal entities the caller
// may act in, newest first, and the next page's token ("" on the last).
func (e *Endpoints) Snapshots(ctx context.Context, q SnapshotsQuery) ([]Snapshot, string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, "", err
	}
	scope := fmt.Sprintf("listCashSnapshots|%s|%s|%s|%s", q.LegalEntity, q.Kind, dayText(q.From), dayText(q.To))
	ks, err := e.Pages.Keyset(snapshotSort, scope, q.Page)
	if err != nil {
		return nil, "", err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, "", err
	}
	query, args := ks.SQL(snapshotSelect+` WHERE ($1 OR s.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR s.legal_entity_id = $3)
		AND ($4 = '' OR s.kind = $4) AND ($5::date IS NULL OR s.snapshot_date >= $5) AND ($6::date IS NULL OR s.snapshot_date <= $6)`,
		[]any{access.All, access.IDs, q.LegalEntity, q.Kind, q.From, q.To})
	rows, _ := tx.Query(ctx, query, args...)
	ss, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Snapshot, error) { return scanSnapshot(r) })
	if err != nil {
		return nil, "", fmt.Errorf("cash: list snapshots: %w", err)
	}
	ss, next := pagination.Page(ks, ss, func(s Snapshot) (string, uuid.UUID) { return s.Date.Format(time.DateOnly), s.ID })
	return ss, next, e.complete(ctx, tx, ss)
}

// SnapshotLinesQuery is what a snapshot's lines are listed by: the
// currency, the liquidity item or what no rule assigned, the source, the
// value dates, and a page of PageSize after the token's.
type SnapshotLinesQuery struct {
	Snapshot     uuid.UUID
	Currency     string
	Item         string
	Unclassified bool
	Source       string
	From, To     *time.Time
	PageSize     int
	PageToken    string
}

// SnapshotLinePage is a page of a snapshot's lines, with the total and
// count of the lines the filters select over every page.
type SnapshotLinePage struct {
	Snapshot Snapshot
	Exponent int
	Total    money.Minor
	Count    int
	Lines    []SnapshotLine
	Next     string
}

// SnapshotLine is a line as listed: its supplier, payment and document
// left out (nil) for a caller who may not read them; HasDocument says
// whether it names one.
type SnapshotLine struct {
	LineNo, FlowNo     int
	Date               time.Time
	End                *time.Time
	Bank               *uuid.UUID
	BankCode           string
	Source, Certainty  string
	Late               bool
	Item               *string
	Amount             money.Minor
	Count              int
	Supplier           *PartnerName
	Payment            *uuid.UUID
	HasDocument        bool
	Document           *Document
	supplier, payments *uuid.UUID // as stored, before the caller's permissions
}

// lineCursor is the last line of a page.
type lineCursor struct {
	After int `json:"after"`
}

// SnapshotLines lists a page of a snapshot's lines in a currency.
func (e *Endpoints) SnapshotLines(ctx context.Context, q SnapshotLinesQuery) (SnapshotLinePage, error) {
	if err := checkLinesQuery(q); err != nil {
		return SnapshotLinePage{}, err
	}
	scope := fmt.Sprintf("listCashSnapshotFlows|%s|%s|%s|%v|%s|%s|%s", q.Snapshot, q.Currency, q.Item, q.Unclassified, q.Source, dayText(q.From), dayText(q.To))
	var c lineCursor
	if q.PageToken != "" {
		if err := e.Pages.Decode(scope, q.PageToken, &c); err != nil {
			return SnapshotLinePage{}, err
		}
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return SnapshotLinePage{}, err
	}
	s, err := e.Snapshot(ctx, q.Snapshot)
	if err != nil {
		return SnapshotLinePage{}, err
	}
	p := SnapshotLinePage{Snapshot: s}
	if i := slices.IndexFunc(s.Currencies, func(c SnapshotCurrency) bool { return c.Currency == q.Currency }); i >= 0 {
		p.Exponent = s.Currencies[i].Exponent
	} else if p.Exponent, err = currencyExponent(ctx, tx, q.Currency); err != nil {
		return SnapshotLinePage{}, err
	}
	const where = ` WHERE f.snapshot_id = $1 AND f.currency = $2 AND ($3 = '' OR f.liquidity_item = $3) AND (NOT $4 OR f.liquidity_item IS NULL)
		AND ($5 = '' OR f.source = $5) AND ($6::date IS NULL OR f.value_date >= $6) AND ($7::date IS NULL OR f.value_date <= $7)`
	args := []any{q.Snapshot, q.Currency, q.Item, q.Unclassified, q.Source, q.From, q.To}
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(f.amount), 0)::bigint, count(*) FROM cash.snapshot_flow f`+where, args...).Scan(&p.Total, &p.Count); err != nil {
		return SnapshotLinePage{}, fmt.Errorf("cash: total of snapshot %s: %w", q.Snapshot, err)
	}
	size := max(q.PageSize, 1)
	rows, _ := tx.Query(ctx, `SELECT f.line_no, f.flow_no, f.value_date, f.bucket_end, f.house_bank_id, f.source, f.certainty, f.late, f.liquidity_item,
			f.amount, f.flow_count, f.supplier_id, f.payment_id, f.document_kind, f.document_id, f.document_number
		FROM cash.snapshot_flow f`+where+` AND f.line_no > $8 ORDER BY f.line_no LIMIT $9`, append(args, c.After, size+1)...)
	lines, err := pgx.CollectRows(rows, scanLine)
	if err != nil {
		return SnapshotLinePage{}, fmt.Errorf("cash: lines of snapshot %s: %w", q.Snapshot, err)
	}
	if len(lines) > size {
		lines = lines[:size]
		// Safe to ignore: a cursor of one number always encodes.
		p.Next, _ = e.Pages.Encode(scope, lineCursor{After: lines[size-1].LineNo})
	}
	p.Lines, err = e.shownLines(ctx, lines)
	return p, err
}

// checkLinesQuery refuses an item asked for with what no rule assigned,
// and a range ending before it starts.
func checkLinesQuery(q SnapshotLinesQuery) error {
	switch {
	case q.Item != "" && q.Unclassified:
		return problem.Unprocessable("request.validation_failed", "A liquidity item and the unclassified lines are two different rows.",
			problem.FieldError{Field: "unclassified", Code: "invalid", Message: "not with liquidity_item"})
	case q.From != nil && q.To != nil && q.To.Before(*q.From):
		return problem.Unprocessable("request.validation_failed", "The range ends before it starts.",
			problem.FieldError{Field: "to", Code: "invalid", Message: "on or after from"})
	}
	return nil
}

// currencyExponent is a currency's minor-unit digits: the snapshot has no
// line in it, so an unknown one lists nothing at no particular precision.
func currencyExponent(ctx context.Context, tx pgx.Tx, code string) (int, error) {
	var exp int
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT exponent FROM ref.currency WHERE code = $1), 2)`, code).Scan(&exp)
	if err != nil {
		return 0, fmt.Errorf("cash: currency %s: %w", code, err)
	}
	return exp, nil
}

func scanLine(r pgx.CollectableRow) (SnapshotLine, error) {
	var l SnapshotLine
	var kind, number *string
	var doc *uuid.UUID
	err := r.Scan(&l.LineNo, &l.FlowNo, &l.Date, &l.End, &l.Bank, &l.Source, &l.Certainty, &l.Late, &l.Item, &l.Amount, &l.Count, &l.supplier, &l.payments,
		&kind, &doc, &number)
	if doc != nil {
		l.HasDocument, l.Document = true, &Document{Kind: *kind, ID: *doc, Number: deref(number)}
	}
	return l, errs.Wrapf(err, "scan cash snapshot line")
}

// shownLines are the lines as the caller may see them, as the forecast's
// drill-down shows its flows: a supplier with partner.supplier.read
// usable, a payment with payments.run.read, a document with its context's
// read permission; each bank by its code.
func (e *Endpoints) shownLines(ctx context.Context, lines []SnapshotLine) ([]SnapshotLine, error) {
	may := mayUse(ctx)
	refs, err := e.lineRefs(ctx, lines, may)
	if err != nil {
		return nil, err
	}
	for i := range lines {
		if err := refs.show(&lines[i], may); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// lineRefs is what a page of lines names in other contexts, as the caller
// may see it: its suppliers' names, its banks' codes, and whether payments
// are shown.
type lineRefs struct {
	names    map[uuid.UUID]PartnerName
	codes    map[uuid.UUID]string
	payments bool
}

func (e *Endpoints) lineRefs(ctx context.Context, lines []SnapshotLine, may func(string) (bool, error)) (lineRefs, error) {
	var suppliers, banks []uuid.UUID
	for _, l := range lines {
		if l.supplier != nil && !slices.Contains(suppliers, *l.supplier) {
			suppliers = append(suppliers, *l.supplier)
		}
		if l.Bank != nil && !slices.Contains(banks, *l.Bank) {
			banks = append(banks, *l.Bank)
		}
	}
	var r lineRefs
	var err error
	if r.names, err = e.partnerNames(ctx, suppliers, may); err != nil {
		return lineRefs{}, err
	}
	if r.codes, err = idref.Resolve(ctx, e.Payments.HouseBankCodes, banks, houseBankRef); err != nil {
		return lineRefs{}, fmt.Errorf("cash: house banks of a snapshot: %w", err)
	}
	r.payments, err = may(documentReadPermissions[DocPaymentRun])
	return r, err
}

// show fills a line's supplier, payment and bank code, and leaves out a
// document the caller may not read.
func (r lineRefs) show(l *SnapshotLine, may func(string) (bool, error)) error {
	if l.supplier != nil {
		if n, ok := r.names[*l.supplier]; ok {
			l.Supplier = &n
		}
	}
	if r.payments {
		l.Payment = l.payments
	}
	if l.Bank != nil {
		l.BankCode = r.codes[*l.Bank]
	}
	if l.Document == nil {
		return nil
	}
	v, err := may(documentReadPermissions[l.Document.Kind])
	if !v {
		l.Document = nil
	}
	return err
}

// DueSnapshots are the tenant's companies whose daily snapshot is due at
// now, keyed by legal entity, with the company's date: those whose local
// time in their zone has reached their cash.snapshot_time and whose
// snapshot of the date is not taken. A tenant without a configuration has
// none; a company without a legal entity is left out.
func (e *Endpoints) DueSnapshots(ctx context.Context, now time.Time) (map[uuid.UUID]time.Time, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		if problem.From(err).Code == "config.not_bootstrapped" {
			return nil, nil
		}
		return nil, fmt.Errorf("cash: configuration: %w", err)
	}
	due := map[uuid.UUID]time.Time{}
	for _, co := range doc.Companies {
		le, date, ok, err := e.dueOf(ctx, doc, co, now)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		taken, err := dailyTaken(ctx, tx, le, date)
		if err != nil {
			return nil, err
		}
		if !taken {
			due[le] = date
		}
	}
	return due, nil
}

// dueOf is the company's legal entity and date at now, when its local time
// has reached its snapshot time.
func (e *Endpoints) dueOf(ctx context.Context, doc *config.Config, co config.Company, now time.Time) (uuid.UUID, time.Time, bool, error) {
	d, err := doc.CompanyDay(co.Code, now)
	if err != nil {
		return uuid.Nil, time.Time{}, false, fmt.Errorf("cash: %w", err)
	}
	local := now.In(d.Zone)
	if local.Hour()*60+local.Minute() < minutesOf(co.Cash.SnapshotAt()) {
		return uuid.Nil, time.Time{}, false, nil
	}
	le, ok, err := e.LegalEntities.ByCode(ctx, co.Code)
	if err != nil {
		return uuid.Nil, time.Time{}, false, fmt.Errorf("cash: legal entity of company %s: %w", co.Code, err)
	}
	return le, d.Date, ok, nil
}

// minutesOf is an HH:MM time of day in minutes; the configuration's
// validation accepted only that form (config.CompanyCash.SnapshotTime).
func minutesOf(hhmm string) int {
	h, m, _ := strings.Cut(hhmm, ":")
	// Safe to ignore: the document's validation parsed the time before it was activated.
	hours, _ := strconv.Atoi(h)
	minutes, _ := strconv.Atoi(m)
	return hours*60 + minutes
}

// SnapshotTenants lists, as the worker role across tenants, the tenants
// that manage cash: those with an active liquidity item. The dispatcher
// asks each which of its companies' snapshots are due.
func (*Endpoints) SnapshotTenants(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM cash.liquidity_item WHERE is_active AND tenant_id IN `+database.ActiveTenants+` ORDER BY tenant_id`)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("cash: tenants with liquidity items: %w", err)
	}
	return tenants, nil
}
