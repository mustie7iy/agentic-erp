package cash

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The documents behind an attributed variance (ADR-0051 §11, §13, T53i):
// each contribution the variance attributes within the snapshot's
// document level, a forecast line or the part of a statement line one
// document behind it pays per liquidity item, with the documents it is
// known by (the keys the matching uses, and a statement line's bank
// statement) and the supplier it pays. They are what the forecast agent's
// comment proposals may cite. Each document, and the supplier, is shown
// only to a caller who may use its context's read permission, as the
// snapshot's lines and the actuals' lines show them.

// Sides of a contribution: a forecast line of the snapshot, or a
// statement line's part.
const (
	SideForecast = "forecast"
	SideActual   = "actual"
)

// DocBankStatement and DocPayment are the kinds of document a statement
// line's part is known by beside the forecast's kinds.
const (
	DocBankStatement = "bank_statement"
	DocPayment       = "payment"
)

// attributions name the attributions as the contract does.
var attributions = map[int]string{attrTiming: "timing", attrAmount: "amount", attrNew: "new", attrMissing: "missing", attrUnclassified: "unclassified"}

// DocRef is a document behind a contribution, with its number where known
// (a document's number as the snapshot kept it, a payment's run number, a
// statement's reference); Occurrence is a memo series' occurrence's day,
// as the snapshot forecast it.
type DocRef struct {
	Kind       string
	ID         uuid.UUID
	Number     string
	Occurrence *time.Time
}

// VarianceEntry is one contribution to an attributed variance: its side,
// attribution, currency (and its exponent), liquidity item ("" for what
// no rule assigns), value date, the start of its bucket, its signed
// amount, the documents shown of the DocCount it is known by, a statement
// line's number and bank reference when its statement is shown, and the
// supplier it pays when the caller may read suppliers.
type VarianceEntry struct {
	Side, Attribution string
	Currency          string
	Exponent          int
	Item              string
	ValueDate, Bucket time.Time
	Amount            money.Minor
	Docs              []DocRef
	DocCount          int
	LineNo            *int32
	BankReference     string
	Supplier          *PartnerName
	supplier          *uuid.UUID
	key               string // the side, value date and a digest of the line: the order, and the page cursor
}

// VarianceDocs are the documents behind a variance: the variance's
// request (its figures not computed), the row asked for, a page of the
// entries, how many there are over every page, and the next page's token
// ("" on the last).
type VarianceDocs struct {
	Variance
	Item         string
	Unclassified bool
	Entries      []VarianceEntry
	Count        int
	Next         string
}

// DocsQuery is a variance's request, the row whose documents are listed
// (a liquidity item and its descendants, what no rule assigns, or every
// row), the side, attribution and currency they are narrowed to ("" for
// any), and a page of PageSize after the page token's.
type DocsQuery struct {
	VarianceQuery
	Item                        string
	Unclassified                bool
	Side, Attribution, Currency string
	PageSize                    int
	PageToken                   string
}

// VarianceDocuments lists a page of the documents behind the legal
// entity's attributed variance, refused as Variance refuses its request;
// both an item and the unclassified row are refused.
func (e *Endpoints) VarianceDocuments(ctx context.Context, q DocsQuery) (VarianceDocs, error) {
	scope := fmt.Sprintf("listForecastVarianceDocuments|%s|%s|%s|%d|%s|%s|%t|%s|%s|%s", q.LegalEntity, dayText(&q.From), dayText(&q.To), q.Lag, q.Grain,
		q.Item, q.Unclassified, q.Side, q.Attribution, q.Currency)
	var c flowCursor
	if q.PageToken != "" {
		if err := e.Pages.Decode(scope, q.PageToken, &c); err != nil {
			return VarianceDocs{}, err
		}
	}
	d, err := e.varianceEntries(ctx, q)
	if err != nil {
		return VarianceDocs{}, err
	}
	d.Entries = slices.DeleteFunc(d.Entries, func(en VarianceEntry) bool {
		return q.Side != "" && en.Side != q.Side || q.Attribution != "" && en.Attribution != q.Attribution || q.Currency != "" && en.Currency != q.Currency
	})
	d.Count = len(d.Entries)
	start, found := slices.BinarySearchFunc(d.Entries, c.After, func(en VarianceEntry, after string) int { return cmp.Compare(en.key, after) })
	if found {
		start++
	}
	size := max(q.PageSize, 1)
	page := d.Entries[start:min(start+size, len(d.Entries))]
	if start+size < len(d.Entries) {
		// Safe to ignore: a cursor of one string always encodes.
		d.Next, _ = e.Pages.Encode(scope, flowCursor{After: page[len(page)-1].key})
	}
	d.Entries = page
	if err := showDocs(ctx, d.Entries); err != nil {
		return VarianceDocs{}, err
	}
	return d, e.nameSuppliers(ctx, d.Entries)
}

// varianceEntries are every entry behind the variance's row, in their
// order, none shown yet.
func (e *Endpoints) varianceEntries(ctx context.Context, q DocsQuery) (VarianceDocs, error) {
	if q.Item != "" && q.Unclassified {
		return VarianceDocs{}, problem.Unprocessable("request.validation_failed", "A liquidity item and the row of what no rule assigns are two different rows.",
			problem.FieldError{Field: "unclassified", Code: "invalid", Message: "not with liquidity_item"})
	}
	vr, err := e.readVariance(ctx, q.VarianceQuery)
	if err != nil {
		return VarianceDocs{}, err
	}
	in := inRow(vr.ar.r.doc.Cash.LiquidityItems, q.Item, q.Unclassified)
	entries := slices.Concat(vr.m.forecastEntries(in), vr.m.actualEntries(in))
	if err := inKeyOrder(entries); err != nil {
		return VarianceDocs{}, err
	}
	exponents := map[string]int{}
	for i := range entries {
		en := &entries[i]
		if _, ok := exponents[en.Currency]; !ok {
			if exponents[en.Currency], err = currency.Exponent(ctx, vr.ar.tx, en.Currency, errVarianceCurrency(en.Currency)); err != nil {
				return VarianceDocs{}, err
			}
		}
		en.Exponent = exponents[en.Currency]
	}
	return VarianceDocs{Variance: vr.v, Item: q.Item, Unclassified: q.Unclassified, Entries: entries}, nil
}

// errVarianceCurrency is a currency of the variance that the reference
// data does not know.
func errVarianceCurrency(code string) error {
	return fmt.Errorf("cash: currency %s of a variance is not known", code)
}

// inRow is whether a liquidity item is in the row asked for: the item and
// its descendants (by the configured tree), what no rule assigns, or any
// row when neither is asked for.
func inRow(items []config.LiquidityItem, item string, unclassified bool) func(string) bool {
	parents := make(map[string]string, len(items))
	for _, it := range items {
		parents[it.Code] = it.Parent
	}
	return func(code string) bool {
		switch {
		case unclassified:
			return code == ""
		case item == "":
			return true
		}
		// A tree is at most MaxLiquidityDepth deep; the bound keeps a
		// cycle, which configuration refuses, from looping.
		for i := 0; code != "" && i <= config.MaxLiquidityDepth; i++ {
			if code == item {
				return true
			}
			code = parents[code]
		}
		return false
	}
}

// entryKey orders the entries: the side, the value date, then a digest of
// what the entry is (never its position among others, so a line read
// between two pages moves no other), so a page token names no document.
func entryKey(side int, day time.Time, what string) string {
	h := sha256.Sum256([]byte(what))
	return fmt.Sprintf("%d|%s|%s", side, day.Format(time.DateOnly), hex.EncodeToString(h[:])[:32])
}

// inKeyOrder sorts the entries by key, refusing two alike. A key names
// what its entry is, so two alike are a fault in what the matching read,
// and the page cursor, which resumes after a key, would end a page on the
// first of them and never list the second, or list the page again for
// ever: refused rather than paged wrongly.
func inKeyOrder(entries []VarianceEntry) error {
	slices.SortFunc(entries, func(a, b VarianceEntry) int { return cmp.Compare(a.key, b.key) })
	for i := 1; i < len(entries); i++ {
		if entries[i].key == entries[i-1].key {
			return fmt.Errorf("cash: two documents behind a variance have one cursor key %s", entries[i].key)
		}
	}
	return nil
}

// forecastEntries are the snapshot's document lines in the period's
// buckets whose item is in the row.
func (m *matching) forecastEntries(in func(string) bool) []VarianceEntry {
	var out []VarianceEntry
	for i, l := range m.kept {
		b := m.bucket(l.date)
		if b < 0 || !in(l.item) {
			continue
		}
		en := VarianceEntry{Side: SideForecast, Attribution: attributions[m.forecastAttribution(m.keys[i], b)], Currency: l.currency, Item: l.item,
			ValueDate: l.date, Bucket: m.bs[b].Start, Amount: l.amount, supplier: l.supplier, key: entryKey(0, l.date, fmt.Sprint(l.lineNo))}
		if ref, ok := refOf(m.keys[i]); ok {
			ref.Number = m.numbers[ref.Kind+":"+ref.ID.String()]
			en.Docs = []DocRef{ref}
		}
		out = append(out, en)
	}
	return out
}

// actualEntries are the statement lines' parts in the period's buckets,
// one per share whose item is in the row, each keyed by its statement
// line, its piece within the line (pieceIDs: the payment and the settled
// item's ledger line) and the share's item, which together are unique.
func (m *matching) actualEntries(in func(string) bool) []VarianceEntry {
	var out []VarianceEntry
	for _, p := range m.pieces {
		b := m.bucket(p.a.ValueDate)
		if b < 0 {
			continue
		}
		docs, supplier := m.pieceDocs(p)
		for _, sh := range p.shares {
			if !in(sh.Item) {
				continue
			}
			no := p.a.LineNo
			out = append(out, VarianceEntry{Side: SideActual, Attribution: attributions[m.actualAttribution(p, b)], Currency: p.a.Currency, Item: sh.Item,
				ValueDate: p.a.ValueDate, Bucket: m.bs[b].Start, Amount: sh.Amount, Docs: slices.Clone(docs), LineNo: &no, BankReference: p.a.BankReference,
				supplier: supplier, key: entryKey(1, p.a.ValueDate, fmt.Sprintf("%s|%d|%s|%s", p.a.StatementID, p.a.LineNo, p.id, sh.Item))})
		}
	}
	return out
}

// pieceDocs are the documents a piece is known by, each once with its
// number where known, its line's statement last, and the supplier of its
// payment.
func (m *matching) pieceDocs(p matchedPiece) ([]DocRef, *uuid.UUID) {
	docs := make([]DocRef, 0, len(p.keys)+1)
	var supplier *uuid.UUID
	for _, k := range p.keys {
		ref, ok := refOf(k)
		if !ok || slices.ContainsFunc(docs, func(d DocRef) bool { return sameRef(d, ref) }) {
			continue
		}
		ref.Number = m.numbers[ref.Kind+":"+ref.ID.String()]
		if i := slices.IndexFunc(p.a.Payments, func(pay Payment) bool { return ref.Kind == DocPayment && pay.ID == ref.ID }); i >= 0 {
			ref.Number, supplier = p.a.Payments[i].RunNumber, &p.a.Payments[i].SupplierID
		}
		docs = append(docs, ref)
	}
	return append(docs, DocRef{Kind: DocBankStatement, ID: p.a.StatementID, Number: p.a.StatementRef}), supplier
}

// sameRef reports whether two references name the same document (and
// occurrence).
func sameRef(a, b DocRef) bool {
	return a.Kind == b.Kind && a.ID == b.ID && (a.Occurrence == nil) == (b.Occurrence == nil) &&
		(a.Occurrence == nil || a.Occurrence.Equal(*b.Occurrence))
}

// refOf is the document a matching key names (paymentKey, docKey, a
// series' occurrence as keptKey writes it); false for a key naming none.
func refOf(key string) (DocRef, bool) {
	kind, rest, _ := strings.Cut(key, ":")
	idText, day, series := strings.Cut(rest, "@")
	id, err := uuid.Parse(idText)
	if err != nil {
		return DocRef{}, false
	}
	ref := DocRef{Kind: kind, ID: id}
	if on, err := time.Parse(time.DateOnly, day); series && err == nil {
		ref.Occurrence = &on
	}
	return ref, true
}

// docPermission is the read permission that shows a document of the kind.
func docPermission(kind string) string {
	switch kind {
	case DocPayment:
		return documentReadPermissions[DocPaymentRun]
	case DocBankStatement:
		return statementReadPermission
	}
	return documentReadPermissions[kind]
}

// showDocs keeps of each entry the documents the caller may see, the
// permissions read once.
func showDocs(ctx context.Context, entries []VarianceEntry) error {
	may := mayUse(ctx)
	for i := range entries {
		if err := entries[i].show(may); err != nil {
			return err
		}
	}
	return nil
}

// nameSuppliers names the supplier of each entry paying one, to a caller
// who may read suppliers.
func (e *Endpoints) nameSuppliers(ctx context.Context, entries []VarianceEntry) error {
	may := mayUse(ctx)
	var suppliers []uuid.UUID
	for _, en := range entries {
		if en.supplier != nil && !slices.Contains(suppliers, *en.supplier) {
			suppliers = append(suppliers, *en.supplier)
		}
	}
	names, err := e.partnerNames(ctx, suppliers, may)
	if err != nil {
		return err
	}
	for i := range entries {
		en := &entries[i]
		if en.supplier != nil {
			if n, ok := names[*en.supplier]; ok {
				en.Supplier = &n
			}
		}
	}
	return nil
}

// show keeps the entry's documents the caller may see, counting them all;
// a line's number and bank reference go with its statement.
func (en *VarianceEntry) show(may func(string) (bool, error)) error {
	en.DocCount = len(en.Docs)
	shown := make([]DocRef, 0, len(en.Docs))
	statement := false
	for _, d := range en.Docs {
		ok, err := may(docPermission(d.Kind))
		if err != nil {
			return err
		}
		if ok {
			shown = append(shown, d)
			statement = statement || d.Kind == DocBankStatement
		}
	}
	en.Docs = shown
	if !statement {
		en.LineNo, en.BankReference = nil, ""
	}
	return nil
}

// cites is the documents of the entries, as "kind:id", that a citation
// may name.
func cites(entries []VarianceEntry) map[string]bool {
	out := map[string]bool{}
	for _, en := range entries {
		for _, r := range en.Docs {
			out[r.Kind+":"+r.ID.String()] = true
		}
	}
	return out
}
