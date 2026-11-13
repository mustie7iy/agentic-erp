package analytics

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Drill-through (T94b, ADR-0044 As built T94b): the documents behind one
// measure of one row of a query's result. The row is the query's filters
// and its value of each dimension of the breakdown; the documents are the
// facts of the measure's base metric in it, grouped by the document they
// are lines of, each signed as the metric adds it, so their amounts add up
// to the row's figure for a sum.

// Drill is a drill-through request: a query, the measure drilled (the
// query's only one when empty) and the row by its value of each dimension
// of the query's breakdown.
type Drill struct {
	Query   Query
	Measure string
	Row     map[string]string
}

// The kinds of document a figure is made of, each read by its own
// operation (documentPaths).
const (
	KindJournalEntry    = "journal_entry"
	KindPurchaseOrder   = "purchase_order"
	KindSupplierInvoice = "supplier_invoice"
	KindSalesOrder      = "sales_order"
	KindBillingDocument = "billing_document"
)

// documentPaths are where each kind of document is read by its id.
var documentPaths = map[string]string{
	KindJournalEntry: "/v1/journal-entries/", KindPurchaseOrder: "/v1/purchase-orders/", KindSupplierInvoice: "/v1/supplier-invoices/",
	KindSalesOrder: "/v1/sales-orders/", KindBillingDocument: "/v1/billing-documents/",
}

// drillable are the fact tables whose documents are listed, and the kind
// of each one's documents; the sales lines' kind is the line's
// (DocumentKind). The cash datasets' statements and snapshots are not
// listed (plan-breadth-week3.md, later).
var drillable = map[string]string{
	"gl_lines": KindJournalEntry, "po_lines": KindPurchaseOrder, "ap_invoices": KindSupplierInvoice,
	spendLines: KindSupplierInvoice, salesLines: KindSalesOrder,
}

// DocumentKind is the kind of the document a fact of dataset is a line
// of, given the fact's sales_document (the sales lines alone have two).
func DocumentKind(dataset, salesDocument string) string {
	if dataset == salesLines && salesDocument == salesBillingLine {
		return KindBillingDocument
	}
	return drillable[dataset]
}

// DocumentDimensions are the dimensions, besides the company, that a
// dataset's documents are grouped by to know their kind.
func DocumentDimensions(dataset string) []string {
	if dataset == salesLines {
		return []string{"company", "sales_document"}
	}
	return []string{"company"}
}

// Document is one document behind a figure: its part of the figure, in
// minor units of Currency (Exponent digits), signed as the metric adds it.
type Document struct {
	Kind, Number, Company, Currency string
	ID                              uuid.UUID
	Amount                          money.Minor
	Exponent                        int
}

// Path is where the document is read.
func (d Document) Path() string { return documentPaths[d.Kind] + d.ID.String() }

// DocumentCursor is the last document of a page: pages run by document id,
// then currency, so a document with lines in two currencies is listed
// once in each.
type DocumentCursor struct {
	ID       string `json:"id"`
	Currency string `json:"currency"`
}

// after reports whether d comes after the cursor.
func (c *DocumentCursor) after(d Document) bool {
	return c == nil || cmp.Or(strings.Compare(d.ID.String(), c.ID), strings.Compare(d.Currency, c.Currency)) > 0
}

// compareDocuments is page order: by id as text, as the store orders
// them, then currency.
func compareDocuments(a, b Document) int {
	return cmp.Or(strings.Compare(a.ID.String(), b.ID.String()), strings.Compare(a.Currency, b.Currency))
}

// RowQuery checks a drill-through against the model and returns the query
// of its row: the measure drilled alone, without a breakdown, with the
// query's filters and one per dimension of the row, in the query's ledger.
// A derived metric is refused naming the metrics to drill into instead,
// and a metric whose documents are not listed is refused.
func RowQuery(m *Model, d Drill) (Query, error) {
	q := d.Query
	if err := validateQuery(m, q); err != nil {
		return Query{}, err
	}
	measure := d.Measure
	if measure == "" && len(q.Measures) == 1 {
		measure = q.Measures[0]
	}
	if !slices.Contains(q.Measures, measure) {
		return Query{}, fmt.Errorf("name the measure to drill into, one of %s", strings.Join(q.Measures, ", "))
	}
	met := m.Metric(measure)
	if met.Derived != nil {
		return Query{}, fmt.Errorf("%s is derived (%s of %s and %s): drill into %s instead", measure, met.Derived.Op, met.Derived.Left, met.Derived.Right,
			strings.Join(m.BaseMetrics(measure), " or "))
	}
	if drillable[met.Source] == "" {
		return Query{}, fmt.Errorf("the documents behind %s (%s) are not listed", measure, met.Source)
	}
	for _, dim := range slices.Sorted(maps.Keys(d.Row)) {
		if !slices.Contains(q.By, dim) {
			return Query{}, fmt.Errorf("the row names %s, which the query does not break down by", dim)
		}
	}
	row := Query{Measures: []string{measure}, Where: slices.Clone(q.Where), Ledger: q.Ledger}
	for _, dim := range q.By {
		v, ok := d.Row[dim]
		if !ok {
			return Query{}, fmt.Errorf("the row needs its value of %s", dim)
		}
		row.Where = append(row.Where, Filter{Dim: dim, Op: "in", Values: []string{v}})
	}
	return row, nil
}

// DocumentsOf is the documents behind the row query q's one measure in the
// facts the database path built, in page order: the facts the principal
// may see and the filters select, grouped by document and currency. A
// document whose lines add up to more than an amount can hold refuses the
// drill-through.
func DocumentsOf(m *Model, f *Facts, p Principal, q Query) ([]Document, error) {
	met := m.Metric(q.Measures[0])
	terms := map[Document][]money.Minor{}
	for _, fact := range f.Tables[met.Source] {
		if !p.canSee(fact.Dims["company"]) || !inLedger(fact, q.Ledger) || !matchAll(fact.Dims, met.Filter) || !matchQuery(fact.Dims, q.Where) {
			continue
		}
		id, err := uuid.Parse(fact.ID)
		if err != nil {
			return nil, fmt.Errorf("%s fact of document %q: %w", met.Source, fact.ID, err)
		}
		// Every fact's currency has its digits: Source recorded them (minor).
		d := Document{Kind: DocumentKind(met.Source, fact.Dims["sales_document"]), ID: id, Company: fact.Dims["company"], Currency: fact.Currency,
			Exponent: f.Exponents[fact.Currency]}
		terms[d] = append(terms[d], fact.Amount*money.Minor(met.Sign))
	}
	out := make([]Document, 0, len(terms))
	for d, t := range terms {
		sum, err := money.Sum(t...)
		if err != nil {
			return nil, fmt.Errorf("%s %s adds up to more than an amount can hold: %w", d.Kind, d.ID, err)
		}
		d.Amount = sum
		out = append(out, d)
	}
	slices.SortFunc(out, compareDocuments)
	return out, nil
}

// PageOf is the page of docs, in page order, after the cursor: at most
// limit of them, and whether more follow.
func PageOf(docs []Document, after *DocumentCursor, limit int) ([]Document, bool) {
	start := slices.IndexFunc(docs, after.after)
	if start < 0 {
		return []Document{}, false
	}
	docs = docs[start:]
	if len(docs) > limit {
		return docs[:limit], true
	}
	return docs, false
}

// Numbers reads each document's number as the API's principal, from the
// document's own read: a document that principal may not read refuses the
// drill-through with the API's refusal, so a figure the caller may see
// never lists a document they may not read.
func (s Source) Numbers(ctx context.Context, docs []Document) error {
	for i, d := range docs {
		var doc struct {
			Number string `json:"document_number"`
		}
		resp, err := s.API.Do(ctx, http.MethodGet, d.Path(), nil, nil, "")
		if err == nil {
			err = resp.Decode(&doc)
		}
		if err != nil {
			return fmt.Errorf("analytics: %s %s: %w", d.Kind, d.ID, err)
		}
		docs[i].Number = doc.Number
	}
	return nil
}
