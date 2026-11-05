package cash

import (
	"context"
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
// statement). They are what the forecast agent's comment proposals may
// cite. Each document is shown only to a caller who may use its context's
// read permission, as the snapshot's lines and the actuals' lines show
// them; no supplier is named.

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

// DocRef is a document behind a contribution; Occurrence is a memo
// series' occurrence's day, as the snapshot forecast it.
type DocRef struct {
	Kind       string
	ID         uuid.UUID
	Occurrence *time.Time
}

// VarianceEntry is one contribution to an attributed variance: its side,
// attribution, currency (and its exponent), liquidity item ("" for what
// no rule assigns), value date, the start of its bucket, its signed
// amount, the documents shown of the DocCount it is known by, and a
// statement line's number when its statement is shown.
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
}

// VarianceDocs are the documents behind a variance: the variance's
// request (its figures not computed), the row asked for, and the entries.
type VarianceDocs struct {
	Variance
	Item         string
	Unclassified bool
	Entries      []VarianceEntry
}

// DocsQuery is a variance's request and the row whose documents are
// listed: a liquidity item and its descendants, what no rule assigns
// (Unclassified), or every row.
type DocsQuery struct {
	VarianceQuery
	Item         string
	Unclassified bool
}

// VarianceDocuments lists the documents behind the legal entity's
// attributed variance, refused as Variance refuses its request; both an
// item and the unclassified row are refused.
func (e *Endpoints) VarianceDocuments(ctx context.Context, q DocsQuery) (VarianceDocs, error) {
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
	may := mayUse(ctx)
	exponents := map[string]int{}
	for i := range entries {
		en := &entries[i]
		if _, ok := exponents[en.Currency]; !ok {
			if exponents[en.Currency], err = currency.Exponent(ctx, vr.ar.tx, en.Currency, errVarianceCurrency(en.Currency)); err != nil {
				return VarianceDocs{}, err
			}
		}
		en.Exponent = exponents[en.Currency]
		if err := en.show(may); err != nil {
			return VarianceDocs{}, err
		}
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

// forecastEntries are the snapshot's document lines in the period's
// buckets whose item is in the row, by value date.
func (m *matching) forecastEntries(in func(string) bool) []VarianceEntry {
	var out []VarianceEntry
	for i, l := range m.kept {
		b := m.bucket(l.date)
		if b < 0 || !in(l.item) {
			continue
		}
		en := VarianceEntry{Side: SideForecast, Attribution: attributions[m.forecastAttribution(m.keys[i], b)], Currency: l.currency, Item: l.item,
			ValueDate: l.date, Bucket: m.bs[b].Start, Amount: l.amount}
		if ref, ok := refOf(m.keys[i]); ok {
			en.Docs = []DocRef{ref}
		}
		out = append(out, en)
	}
	slices.SortStableFunc(out, func(a, b VarianceEntry) int { return a.ValueDate.Compare(b.ValueDate) })
	return out
}

// actualEntries are the statement lines' parts in the period's buckets,
// one per share whose item is in the row, by value date.
func (m *matching) actualEntries(in func(string) bool) []VarianceEntry {
	var out []VarianceEntry
	for _, p := range m.pieces {
		b := m.bucket(p.a.ValueDate)
		if b < 0 {
			continue
		}
		docs := make([]DocRef, 0, len(p.keys)+1)
		for _, k := range p.keys {
			if ref, ok := refOf(k); ok && !slices.ContainsFunc(docs, func(d DocRef) bool { return sameRef(d, ref) }) {
				docs = append(docs, ref)
			}
		}
		docs = append(docs, DocRef{Kind: DocBankStatement, ID: p.a.StatementID})
		for _, sh := range p.shares {
			if !in(sh.Item) {
				continue
			}
			no := p.a.LineNo
			out = append(out, VarianceEntry{Side: SideActual, Attribution: attributions[m.actualAttribution(p, b)], Currency: p.a.Currency, Item: sh.Item,
				ValueDate: p.a.ValueDate, Bucket: m.bs[b].Start, Amount: sh.Amount, Docs: slices.Clone(docs), LineNo: &no})
		}
	}
	slices.SortStableFunc(out, func(a, b VarianceEntry) int { return a.ValueDate.Compare(b.ValueDate) })
	return out
}

// sameRef reports whether two references name the same document (and
// occurrence).
func sameRef(a, b DocRef) bool {
	return a.Kind == b.Kind && a.ID == b.ID && (a.Occurrence == nil) == (b.Occurrence == nil) &&
		(a.Occurrence == nil || a.Occurrence.Equal(*b.Occurrence))
}

// refOf is the document a matching key names (paymentKey, docKey, a
// series' occurrence as keptKey writes it); false for none.
func refOf(key string) (DocRef, bool) {
	kind, rest, ok := strings.Cut(key, ":")
	if !ok {
		return DocRef{}, false
	}
	idText, day, series := strings.Cut(rest, "@")
	id, err := uuid.Parse(idText)
	if err != nil {
		return DocRef{}, false
	}
	ref := DocRef{Kind: kind, ID: id}
	if series {
		on, err := time.Parse(time.DateOnly, day)
		if err != nil {
			return DocRef{}, false
		}
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

// show keeps the entry's documents the caller may see, counting them all;
// a line's number goes with its statement.
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
		en.LineNo = nil
	}
	return nil
}

// cites is the documents of the entries, as "kind:id", that a citation
// may name.
func (d VarianceDocs) cites() map[string]bool {
	out := map[string]bool{}
	for _, en := range d.Entries {
		for _, r := range en.Docs {
			out[r.Kind+":"+r.ID.String()] = true
		}
	}
	return out
}
