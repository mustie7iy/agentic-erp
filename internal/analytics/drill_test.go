package analytics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

func drillModel(t *testing.T) *Model {
	t.Helper()
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRowQuery: the row's query is the measure drilled alone, the query's
// filters and one per dimension of the row; what cannot be drilled is
// refused saying why.
func TestRowQuery(t *testing.T) {
	m := drillModel(t)
	month := Filter{Dim: "fiscal_year", Op: "in", Values: []string{"2026"}}
	q := Query{Measures: []string{"revenue", "expenses"}, By: []string{"company", "fiscal_month"}, Where: []Filter{month}, Ledger: "", OrderBy: "revenue", Limit: 3}
	got, err := RowQuery(m, Drill{Query: q, Measure: "expenses", Row: map[string]string{"company": "2000", "fiscal_month": "6"}})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(Query{Measures: []string{"expenses"}, Where: []Filter{month, {Dim: "company", Op: "in", Values: []string{"2000"}},
		{Dim: "fiscal_month", Op: "in", Values: []string{"6"}}}})
	if fmt.Sprint(got) != want {
		t.Errorf("row query: got %v, want %v", got, want)
	}
	only, err := RowQuery(m, Drill{Query: Query{Measures: []string{"spend"}}})
	if err != nil || fmt.Sprint(only.Measures) != "[spend]" || len(only.Where) != 0 {
		t.Errorf("a query of one measure without a breakdown: got %v, %v; want spend alone", only, err)
	}
	for name, c := range map[string]struct {
		d    Drill
		want string
	}{
		"invalid query":       {Drill{Query: Query{}}, "query needs at least one measure"},
		"measure not named":   {Drill{Query: q}, "name the measure to drill into, one of revenue, expenses"},
		"measure not asked":   {Drill{Query: q, Measure: "spend"}, "name the measure to drill into"},
		"derived":             {Drill{Query: Query{Measures: []string{"off_contract_share"}}}, "drill into spend or spend_under_contract instead"},
		"cash":                {Drill{Query: Query{Measures: []string{"actual"}}}, "the documents behind actual (cash_actuals) are not listed"},
		"a dimension missing": {Drill{Query: q, Measure: "revenue", Row: map[string]string{"company": "2000"}}, "the row needs its value of fiscal_month"},
		"a dimension too many": {Drill{Query: q, Measure: "revenue", Row: map[string]string{"company": "2000", "fiscal_month": "6", "account": "400000"}},
			"the row names account, which the query does not break down by"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RowQuery(m, c.d); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error saying %q", err, c.want)
			}
		})
	}
}

// drillFacts are journal lines of two entries and a parallel ledger's, an
// entry of another company, sales lines of an order and a billing
// document, and a document in two currencies.
func drillFacts() (*Facts, []uuid.UUID) {
	ids := []uuid.UUID{uuid.MustParse("00000000-0000-0000-0000-00000000000a"), uuid.MustParse("00000000-0000-0000-0000-00000000000b"),
		uuid.MustParse("00000000-0000-0000-0000-00000000000c"), uuid.MustParse("00000000-0000-0000-0000-00000000000d")}
	revenue := func(id uuid.UUID, company string, amount money.Minor, leading bool) Fact {
		return Fact{ID: id.String(), Dims: map[string]string{"company": company, "account_type": "revenue", "determination": "none", "fiscal_month": "6"},
			Amount: amount, Currency: "EUR", Ledger: "L", Leading: leading}
	}
	sales := func(id uuid.UUID, kind, cur string, amount money.Minor) Fact {
		return Fact{ID: id.String(), Dims: map[string]string{"company": "2000", "sales_document": kind}, Amount: amount, Currency: cur}
	}
	return &Facts{Exponents: map[string]int{"EUR": 2, "JPY": 0}, Tables: map[string][]Fact{
		"gl_lines": {revenue(ids[1], "2000", -500, true), revenue(ids[0], "2000", -100, true), revenue(ids[0], "2000", -25, true),
			revenue(ids[2], "2000", -999, false), revenue(ids[3], "1000", -7, true)},
		salesLines: {sales(ids[1], "billing", "EUR", 300), sales(ids[0], "order", "EUR", 200), sales(ids[0], "order", "JPY", 5), sales(ids[0], "order", "EUR", 50)},
	}}, ids
}

// TestDocumentsOf: the facts the principal may see and the filters select,
// by document and currency in page order, each signed as the metric adds
// it, of the kind its dataset and line say.
func TestDocumentsOf(t *testing.T) {
	m := drillModel(t)
	f, ids := drillFacts()
	got, err := DocumentsOf(m, f, Principal{Companies: []string{"2000"}}, Query{Measures: []string{"revenue"}, Where: []Filter{{Dim: "fiscal_month", Op: "in", Values: []string{"6"}}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Document{{Kind: KindJournalEntry, ID: ids[0], Company: "2000", Currency: "EUR", Exponent: 2, Amount: 125},
		{Kind: KindJournalEntry, ID: ids[1], Company: "2000", Currency: "EUR", Exponent: 2, Amount: 500}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("revenue's documents: got %v, want %v", got, want)
	}
	got, err = DocumentsOf(m, f, Principal{}, Query{Measures: []string{"order_intake"}})
	if err != nil {
		t.Fatal(err)
	}
	want = []Document{{Kind: KindSalesOrder, ID: ids[0], Company: "2000", Currency: "EUR", Exponent: 2, Amount: 250},
		{Kind: KindSalesOrder, ID: ids[0], Company: "2000", Currency: "JPY", Exponent: 0, Amount: 5}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("order intake's documents: got %v, want %v", got, want)
	}
	if got, _ := DocumentsOf(m, f, Principal{}, Query{Measures: []string{"billed_value"}}); len(got) != 1 || got[0].Kind != KindBillingDocument {
		t.Errorf("billed value's documents: got %v, want the billing document", got)
	}
	if got[0].Path() != "/v1/sales-orders/"+ids[0].String() {
		t.Errorf("path: got %s, want the sales order's read", got[0].Path())
	}
	t.Run("an id that is no document's", func(t *testing.T) {
		bad := &Facts{Tables: map[string][]Fact{"po_lines": {{ID: "x", Dims: map[string]string{}}}}}
		if _, err := DocumentsOf(m, bad, Principal{}, Query{Measures: []string{"purchase_order_value"}}); err == nil {
			t.Error("got no error, want the id refused")
		}
	})
	t.Run("a document beyond an amount", func(t *testing.T) {
		id := uuid.NewString()
		big := &Facts{Tables: map[string][]Fact{"po_lines": {{ID: id, Amount: math.MaxInt64 / 2, Dims: map[string]string{}},
			{ID: id, Amount: math.MaxInt64 / 2, Dims: map[string]string{}}, {ID: id, Amount: math.MaxInt64 / 2, Dims: map[string]string{}}}}}
		if _, err := DocumentsOf(m, big, Principal{}, Query{Measures: []string{"purchase_order_value"}}); !errors.Is(err, money.ErrOverflow) {
			t.Errorf("got %v, want money.ErrOverflow", err)
		}
	})
}

// TestPageOf: a page is the documents after the cursor, at most the
// limit, and says whether more follow.
func TestPageOf(t *testing.T) {
	_, ids := drillFacts()
	docs := []Document{{ID: ids[0], Currency: "EUR"}, {ID: ids[0], Currency: "JPY"}, {ID: ids[1], Currency: "EUR"}}
	for name, c := range map[string]struct {
		after *DocumentCursor
		limit int
		want  string
		more  bool
	}{
		"first":              {nil, 2, fmt.Sprint(docs[:2]), true},
		"all":                {nil, 3, fmt.Sprint(docs), false},
		"after a currency":   {&DocumentCursor{ID: ids[0].String(), Currency: "EUR"}, 5, fmt.Sprint(docs[1:]), false},
		"after the last":     {&DocumentCursor{ID: ids[1].String(), Currency: "EUR"}, 5, "[]", false},
		"after one gone now": {&DocumentCursor{ID: ids[0].String(), Currency: "USD"}, 1, fmt.Sprint(docs[2:]), false},
	} {
		t.Run(name, func(t *testing.T) {
			got, more := PageOf(docs, c.after, c.limit)
			if fmt.Sprint(got) != c.want {
				t.Errorf("page: got %v, want %v", got, c.want)
			}
			if more != c.more {
				t.Errorf("more: got %v, want %v", more, c.more)
			}
		})
	}
}

// TestNumbers: each document's number is read from its own read, as the
// API's principal; a refused read refuses them all with the API's problem.
func TestNumbers(t *testing.T) {
	_, ids := drillFacts()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, ids[2].String()):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code": "identity.permission_missing", "detail": "no"}`)) // the client reports what it read
		case strings.HasSuffix(r.URL.Path, ids[3].String()):
			_, _ = w.Write([]byte(`{`)) // as above
		default:
			_, _ = w.Write([]byte(`{"document_number": "N-` + r.URL.Path[len(r.URL.Path)-1:] + `"}`)) // as above
		}
	}))
	t.Cleanup(srv.Close)
	s := Source{API: &apitools.Client{BaseURL: srv.URL, HTTP: srv.Client()}}
	docs := []Document{{Kind: KindJournalEntry, ID: ids[0]}, {Kind: KindBillingDocument, ID: ids[1]}}
	if err := s.Numbers(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(docs[0].Number, " ", docs[1].Number, " ", paths); got != "N-a N-b [/v1/journal-entries/"+ids[0].String()+" /v1/billing-documents/"+ids[1].String()+"]" {
		t.Errorf("numbers: got %s, want each document's from its own read", got)
	}
	var api *apitools.Error
	if err := s.Numbers(context.Background(), []Document{{Kind: KindSupplierInvoice, ID: ids[2]}}); !errors.As(err, &api) || api.Status != 403 {
		t.Errorf("a refused read: got %v, want the API's 403", err)
	}
	if err := s.Numbers(context.Background(), []Document{{Kind: KindPurchaseOrder, ID: ids[3]}}); err == nil {
		t.Error("an answer that does not read: got no error, want one")
	}
	if err := (Source{API: &apitools.Client{BaseURL: "http://127.0.0.1:1", HTTP: srv.Client()}}).Numbers(context.Background(), docs); err == nil {
		t.Error("an API that does not answer: got no error, want one")
	}
}

// TestRemoteDocuments: every page is read, the row's figure and each
// document's amount kept exactly with their digits.
func TestRemoteDocuments(t *testing.T) {
	_, ids := drillFacts()
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.URL.Query().Get("page_token"))
		doc := `{"kind": "journal_entry", "id": "%s", "number": "%s", "company": "2000", "amount": {"amount": "%s", "currency": "%s"}, "path": "/x"}`
		if r.URL.Query().Get("page_token") == "" {
			_, _ = fmt.Fprintf(w, `{"measure": "revenue", "total": {"value": "6.25", "currency": "EUR"}, "documents": [`+doc+`], "next_page_token": "t2",
				"source": "store", "as_of": "2026-10-20T12:00:00Z", "config_version": 1}`, ids[0], "J1", "1.25", "EUR") // the client reports what it read
			return
		}
		_, _ = fmt.Fprintf(w, `{"measure": "revenue", "total": {"value": "6.25", "currency": "EUR"}, "documents": [`+doc+`],
			"source": "store", "as_of": "2026-10-20T12:00:00Z", "config_version": 1}`, ids[1], "J2", "5", "JPY") // as above
	}))
	t.Cleanup(srv.Close)
	r := Remote{API: &apitools.Client{BaseURL: srv.URL, HTTP: srv.Client()}, Companies: []string{"2000"}}
	got, err := r.Documents(context.Background(), Drill{Query: Query{Measures: []string{"revenue"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]Document{{Kind: KindJournalEntry, ID: ids[0], Number: "J1", Company: "2000", Currency: "EUR", Amount: 125, Exponent: 2},
		{Kind: KindJournalEntry, ID: ids[1], Number: "J2", Company: "2000", Currency: "JPY", Amount: 5, Exponent: 0}})
	if fmt.Sprint(got.Documents) != want {
		t.Errorf("documents: got %v, want %v", got.Documents, want)
	}
	if fmt.Sprint(got.Total, " ", got.Measure, " ", got.Source, " ", sent) != "6.25 revenue store [ t2]" {
		t.Errorf("answer: got %v %s %s after tokens %v, want 6.25 revenue from the store over two pages", got.Total, got.Measure, got.Source, sent)
	}
	for name, body := range map[string]string{
		"refused":           `{"code": "analytics.invalid_query"}`,
		"a total of no sum": `{"measure": "m", "total": {"value": "x"}, "documents": []}`,
		"an amount of none": `{"measure": "m", "total": {"value": null}, "documents": [{"id": "` + ids[0].String() + `", "amount": {"amount": "x"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			status := 200
			if name == "refused" {
				status = 422
			}
			if _, err := remote(t, status, body, nil).Documents(context.Background(), Drill{Query: Query{Measures: []string{"m"}}}); err == nil {
				t.Error("got no error, want one")
			}
		})
	}
	none, err := remote(t, 200, `{"measure": "m", "total": {"value": null}, "documents": []}`, nil).Documents(context.Background(), Drill{})
	if err != nil || none.Total != nil || len(none.Documents) != 0 {
		t.Errorf("a row of no figure: got %+v, %v; want no total and no documents", none, err)
	}
}
