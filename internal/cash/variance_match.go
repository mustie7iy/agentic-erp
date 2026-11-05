package cash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// Matching a snapshot's documents to the statement lines that paid them
// (ADR-0051 §11, T53f3). A snapshot names each document a flow pays
// within its 31 days (T53f): a run's payment or a payment in transit by
// the payment, a supplier item by its journal entry, an invoice not yet
// posted by the invoice, a down payment request, a memo record (a series'
// occurrence by the series and its day), a purchase order's open value by
// the order (T53g). A statement line matched to payments is split by the
// items they settled (T53f2), each piece known by its payment, then its
// item's journal entry, then the invoice that entry was posted for, then
// the order that invoice was entered on, or, paying no item, by the down
// payment request; a line
// that realised a memo record by the record, and an override of a series'
// occurrence by the series and that occurrence's day as the snapshot
// dated it. Each piece is matched to the first of its keys the snapshot
// names.

// paymentKey is a payment as a snapshot names it.
func paymentKey(id uuid.UUID) string { return "payment:" + id.String() }

// docKey is a document as a snapshot names it.
func docKey(kind string, id uuid.UUID) string { return kind + ":" + id.String() }

// paymentKeys are the keys of the pieces paymentParts splits a payment
// into, in the same order: per item it settles, the payment, the item's
// journal entry, the invoice it was posted for and the purchase order that
// invoice was entered on (T53g); a payment of no item, the payment and the
// down payment request it pays.
func paymentKeys(id uuid.UUID, p Paying) [][]string {
	if len(p.Items) == 0 {
		k := []string{paymentKey(id)}
		if p.Request != nil {
			k = append(k, docKey(DocDownPaymentRequest, *p.Request))
		}
		return [][]string{k}
	}
	out := make([][]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = []string{paymentKey(id), docKey(DocJournalEntry, it.Entry)}
		if it.Invoice != nil {
			out[i] = append(out[i], docKey(DocSupplierInvoice, *it.Invoice))
		}
		if it.Order != nil {
			out[i] = append(out[i], docKey(DocPurchaseOrder, *it.Order))
		}
	}
	return out
}

// keptKey is the key a snapshot's line at document level names its
// document by: the payment of a run's or an in-transit flow, a series'
// occurrence by the series and its day, else the document; "" for a
// line naming none, which nothing can match.
func keptKey(l keptLine, series map[uuid.UUID]bool) string {
	switch {
	case l.payment != nil:
		return paymentKey(*l.payment)
	case l.doc == nil:
		return ""
	case l.doc.Kind == DocMemoRecord && series[l.doc.ID]:
		return docKey(DocMemoRecord, l.doc.ID) + "@" + l.date.Format(time.DateOnly)
	}
	return docKey(l.doc.Kind, l.doc.ID)
}

// lineRef is a statement line by its statement and number.
type lineRef struct {
	statement uuid.UUID
	lineNo    int32
}

// memoMatches are which of the snapshot's memo records are series, and
// the keys of the memo records each statement line realised: a one-off
// record by itself, an override of a series' occurrence also by the
// series and the occurrence's day at the bank as the snapshot's read
// dated it (at, the reader of the snapshot's date).
func memoMatches(ctx context.Context, tx pgx.Tx, at reader, lines []keptLine, actuals []actual) (map[uuid.UUID]bool, map[lineRef][]string, error) {
	var ids, statements []uuid.UUID
	for _, l := range lines {
		if l.doc != nil && l.doc.Kind == DocMemoRecord {
			ids = append(ids, l.doc.ID)
		}
	}
	for _, a := range actuals {
		if !slices.Contains(statements, a.StatementID) {
			statements = append(statements, a.StatementID)
		}
	}
	rows, _ := tx.Query(ctx, `SELECT id, recurrence IS NOT NULL, recurrence_of, occurrence_date, realised_statement_id, realised_line_no
		FROM cash.memo_record WHERE id = ANY($1) OR (status = 'realised' AND realised_statement_id = ANY($2)) ORDER BY id`, ids, statements)
	series, realised := map[uuid.UUID]bool{}, map[lineRef][]string{}
	var id uuid.UUID
	var isSeries bool
	var of, statement *uuid.UUID
	var on *time.Time
	var lineNo *int32
	_, err := pgx.ForEachRow(rows, []any{&id, &isSeries, &of, &on, &statement, &lineNo}, func() error {
		series[id] = isSeries
		if statement == nil {
			return nil
		}
		ref := lineRef{*statement, *lineNo}
		realised[ref] = append(realised[ref], docKey(DocMemoRecord, id))
		if of != nil {
			day, _ := at.atBank(*on)
			realised[ref] = append(realised[ref], docKey(DocMemoRecord, *of)+"@"+day.Format(time.DateOnly))
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("cash: memo records of a snapshot and the statement lines that realised them: %w", err)
	}
	return series, realised, nil
}

// Attribution of a document's part of the variance (ADR-0051 §11).
const (
	attrTiming = iota
	attrAmount
	attrNew
	attrMissing
	attrUnclassified
)

// matching is a snapshot's documents within its 31 days and the statement
// lines' pieces, matched: which documents were paid, and in which of the
// period's buckets each was forecast and paid.
type matching struct {
	bs             []Bucket
	forecast, paid map[string]map[int]bool // by key, the buckets it is in (-1: none of the period's)
	kept           []keptLine
	keys           []string // kept's keys
	pieces         []matchedPiece
}

// matchedPiece is a piece of a statement line with the key it was matched
// to ("" for none) and every key it is known by.
type matchedPiece struct {
	a      actual
	key    string
	keys   []string // every key it is known by, the memo records its line realised last
	shares []Share
}

// bucket is the period's bucket holding the day, -1 when none does.
func (m *matching) bucket(d time.Time) int {
	if len(m.bs) == 0 || d.Before(m.bs[0].Start) || d.After(m.bs[len(m.bs)-1].End) {
		return -1
	}
	b, _ := slices.BinarySearchFunc(m.bs, d, func(b Bucket, d time.Time) int { return cmp.Compare(b.End.Unix(), d.Unix()) })
	return b
}

// mark records that the key is in the day's bucket.
func mark(in map[string]map[int]bool, key string, b int) {
	if in[key] == nil {
		in[key] = map[int]bool{}
	}
	in[key][b] = true
}

// newMatching matches the snapshot's document lines (its lines naming no
// bucket) to the actuals' pieces; a line naming no document is kept, and
// counts as missing.
func newMatching(bs []Bucket, lines []keptLine, actuals []actual, series map[uuid.UUID]bool, realised map[lineRef][]string) *matching {
	m := &matching{bs: bs, forecast: map[string]map[int]bool{}, paid: map[string]map[int]bool{}}
	for _, l := range lines {
		if l.end != nil {
			continue
		}
		k := keptKey(l, series)
		m.kept, m.keys = append(m.kept, l), append(m.keys, k)
		if k != "" {
			mark(m.forecast, k, m.bucket(l.date))
		}
	}
	for _, a := range actuals {
		memo := realised[lineRef{a.StatementID, a.LineNo}]
		for _, p := range a.pieces {
			keys := append(slices.Clone(p.keys), memo...)
			var key string
			if i := slices.IndexFunc(keys, func(k string) bool { return m.forecast[k] != nil }); i >= 0 {
				key = keys[i]
				mark(m.paid, key, m.bucket(a.ValueDate))
			}
			m.pieces = append(m.pieces, matchedPiece{a: a, key: key, keys: keys, shares: p.shares})
		}
	}
	return m
}

// forecastAttribution is how a document's forecast in bucket b is
// attributed: missing when nothing paid it, amount when it was paid in the
// same bucket, else timing.
func (m *matching) forecastAttribution(key string, b int) int {
	switch {
	case m.paid[key] == nil:
		return attrMissing
	case m.paid[key][b]:
		return attrAmount
	}
	return attrTiming
}

// actualAttribution is how a piece paid in bucket b is attributed: new,
// or unclassified for a line with no decided match, when it pays no
// document the snapshot forecast; amount when that document was forecast
// in the same bucket, else timing.
func (m *matching) actualAttribution(p matchedPiece, b int) int {
	switch {
	case p.key == "" && p.a.unclassified():
		return attrUnclassified
	case p.key == "":
		return attrNew
	case m.forecast[p.key][b]:
		return attrAmount
	}
	return attrTiming
}

// beyondOf is the days after the document level compared per item only:
// up to the last day, or the day before a snapshot bucket that holds it and
// ends after it, whose sum cannot be told apart by day; nil when there are
// none.
func beyondOf(lines []keptLine, docEnd, last time.Time) *Period {
	if !last.After(docEnd) {
		return nil
	}
	end := last
	for _, l := range lines {
		if l.end != nil && l.end.After(last) {
			end = minDay(end, l.date.AddDate(0, 0, -1))
		}
	}
	if !end.After(docEnd) {
		return nil
	}
	return &Period{From: docEnd.AddDate(0, 0, 1), To: end}
}

// matched is the snapshot's lines and their matching to the actuals'
// pieces over the period's buckets, up to the last day.
func matched(ctx context.Context, ar actualsRead, s Snapshot, bs []Bucket, last time.Time) ([]keptLine, *matching, error) {
	lines, err := keptLines(ctx, ar.tx, s.ID, last)
	if err != nil {
		return nil, nil, err
	}
	at := ar.r
	at.read = s.Date
	series, realised, err := memoMatches(ctx, ar.tx, at, lines, ar.lines)
	if err != nil {
		return nil, nil, err
	}
	return lines, newMatching(bs, lines, ar.lines, series, realised), nil
}

// compare is the variance per currency of the snapshot's lines and the
// actuals over the period's buckets and the days beyond them.
func (vr varianceRead) compare(ctx context.Context) ([]VarianceCurrency, *Period, error) {
	beyond := beyondOf(vr.lines, vr.v.Snapshot.DocumentLevelUntil(), vr.v.To)
	t := newVarianceTable(vr.ar.r, len(vr.v.Buckets))
	vr.m.attribute(t)
	if beyond != nil {
		vr.m.beyond(t, vr.lines, *beyond)
	}
	out, err := t.currencies(ctx, vr.ar.tx)
	return out, beyond, err
}

// attribute adds the period's forecast documents and paid pieces to the
// table, each attributed.
func (m *matching) attribute(t *varianceTable) {
	for i, l := range m.kept {
		if b := m.bucket(l.date); b >= 0 {
			t.add(l.currency, l.item, b, cellOf(l.amount, 0), m.forecastAttribution(m.keys[i], b))
		}
	}
	for _, p := range m.pieces {
		if b := m.bucket(p.a.ValueDate); b >= 0 {
			for _, sh := range p.shares {
				t.add(p.a.Currency, sh.Item, b, cellOf(0, sh.Amount), m.actualAttribution(p, b))
			}
		}
	}
}

// beyond adds the snapshot's bucket sums and the pieces paid in the days
// beyond the document level, per item only.
func (m *matching) beyond(t *varianceTable, lines []keptLine, p Period) {
	for _, l := range lines {
		if l.end != nil && !l.date.After(p.To) {
			t.add(l.currency, l.item, -1, cellOf(l.amount, 0), -1)
		}
	}
	for _, x := range m.pieces {
		if d := x.a.ValueDate; !d.Before(p.From) && !d.After(p.To) {
			for _, sh := range x.shares {
				t.add(x.a.Currency, sh.Item, -1, cellOf(0, sh.Amount), -1)
			}
		}
	}
}

// cellOf is a contribution of a forecast and an actual amount.
func cellOf(forecast, actual money.Minor) Figures { return Figures{Forecast: forecast, Actual: actual} }

// varianceTable gathers a variance's cells per currency and item.
type varianceTable struct {
	r       reader
	buckets int
	cur     map[string]*varianceSums
}

// varianceSums are a currency's cells: per bucket, in total, beyond, and
// per item (with its descendants'), the items' tree beside them.
type varianceSums struct {
	VarianceCurrency
	tree *itemRows
	rows map[string]*VarianceRow
}

func newVarianceTable(r reader, buckets int) *varianceTable {
	return &varianceTable{r: r, buckets: buckets, cur: map[string]*varianceSums{}}
}

// add adds a contribution to its currency and to its item's row and its
// ancestors': in bucket b with its attribution, or beyond (b -1), where
// nothing is attributed.
func (t *varianceTable) add(code, item string, b int, c Figures, attribution int) {
	v := t.sums(code)
	s := t.r.sums
	if b < 0 {
		v.Beyond = s.addFigures(v.Beyond, c)
	} else {
		v.Buckets[b], v.Total = s.addCell(v.Buckets[b], c, attribution), s.addCell(v.Total, c, attribution)
	}
	v.tree.row(item)
	for code := item; ; {
		r := v.row(code, t.buckets)
		if b < 0 {
			r.Beyond = s.addFigures(r.Beyond, c)
		} else {
			r.Buckets[b], r.Total = s.addCell(r.Buckets[b], c, attribution), s.addCell(r.Total, c, attribution)
		}
		if code = v.tree.rows[code].Parent; code == "" {
			return
		}
	}
}

// sums is the currency's cells, made when new.
func (t *varianceTable) sums(code string) *varianceSums {
	v, ok := t.cur[code]
	if !ok {
		v = &varianceSums{VarianceCurrency: VarianceCurrency{Currency: code, Buckets: make([]VarianceCell, t.buckets)},
			tree: newItemRows(t.r.doc.Cash.LiquidityItems, t.buckets), rows: map[string]*VarianceRow{}}
		t.cur[code] = v
	}
	return v
}

// row is the item's row, made when new.
func (v *varianceSums) row(code string, buckets int) *VarianceRow {
	r, ok := v.rows[code]
	if !ok {
		r = &VarianceRow{Item: code, Buckets: make([]VarianceCell, buckets)}
		v.rows[code] = r
	}
	return r
}

// addFigures adds a contribution's forecast and actual.
func (s *sums) addFigures(f, c Figures) Figures {
	return Figures{Forecast: s.add(f.Forecast, c.Forecast), Actual: s.add(f.Actual, c.Actual)}
}

// addCell adds a contribution to a cell, its variance (actual less
// forecast) to the attribution's part.
func (s *sums) addCell(cell VarianceCell, c Figures, attribution int) VarianceCell {
	cell.Figures = s.addFigures(cell.Figures, c)
	at := cell.part(attribution)
	*at = s.add(*at, s.diff(c.Actual, c.Forecast))
	return cell
}

// part is the cell's part of an attribution.
func (cell *VarianceCell) part(attribution int) *money.Minor {
	switch attribution {
	case attrTiming:
		return &cell.Timing
	case attrAmount:
		return &cell.Amount
	case attrNew:
		return &cell.New
	case attrMissing:
		return &cell.Missing
	}
	return &cell.Unclassified
}

// diff is a less b, the overflow noted.
func (s *sums) diff(a, b money.Minor) money.Minor {
	d, err := money.Diff(a, b)
	s.overflow = s.overflow || err != nil
	return d
}

// currencies are the table's currencies by code, each with its variances
// computed and its rows by item in the forecast's order (ItemRow).
func (t *varianceTable) currencies(ctx context.Context, tx pgx.Tx) ([]VarianceCurrency, error) {
	codes := make([]string, 0, len(t.cur))
	for code := range t.cur {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	out := make([]VarianceCurrency, len(codes))
	for i, code := range codes {
		v := t.cur[code]
		exp, err := currency.Exponent(ctx, tx, code, fmt.Errorf("cash: currency %s of a variance is not known", code))
		if err != nil {
			return nil, err
		}
		c := v.VarianceCurrency
		c.Exponent, c.Buckets, c.Total, c.Beyond = exp, t.finished(c.Buckets), t.finish(c.Total), t.figures(c.Beyond)
		for _, r := range v.tree.ordered() {
			row := *v.rows[r.Item]
			row.Name, row.Parent, row.Level = r.Name, r.Parent, r.Level
			row.Buckets, row.Total, row.Beyond = t.finished(row.Buckets), t.finish(row.Total), t.figures(row.Beyond)
			c.Items = append(c.Items, row)
		}
		out[i] = c
	}
	return out, nil
}

// figures are the figures with their variance, actual less forecast.
func (t *varianceTable) figures(f Figures) Figures {
	f.Variance = t.r.sums.diff(f.Actual, f.Forecast)
	return f
}

// finish is the cell with its variance.
func (t *varianceTable) finish(c VarianceCell) VarianceCell {
	c.Figures = t.figures(c.Figures)
	return c
}

// finished are the cells with their variances.
func (t *varianceTable) finished(cs []VarianceCell) []VarianceCell {
	out := make([]VarianceCell, len(cs))
	for i, c := range cs {
		out[i] = t.finish(c)
	}
	return out
}
