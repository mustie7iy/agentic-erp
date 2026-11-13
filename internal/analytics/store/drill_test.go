package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

var (
	docA = uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	docB = uuid.MustParse("00000000-0000-0000-0000-00000000000b")
)

// docReader answers a documents plan with rows, and the figure's plan
// with one group of 6.25 EUR, or with err.
type docReader struct {
	calls   []wire.Request
	rows    []wire.Row
	err     error
	docsErr error
}

func (r *docReader) Query(_ context.Context, req wire.Request) (*wire.Result, error) {
	r.calls = append(r.calls, req)
	res := &wire.Result{ManifestKey: req.ManifestKey, AsOf: storeAsOf, Rows: r.rows}
	if req.Plan.Aggregate != "documents" {
		res.Rows = []wire.Row{{By: make([]string, len(req.Plan.By)), Currency: "EUR", Exponent: 2, Amount: -625}}
		return res, r.err
	}
	return res, r.docsErr
}

// factsDatabase is a database path answering facts.
type factsDatabase struct {
	f   *analytics.Facts
	err error
}

func (d factsDatabase) Facts(context.Context, *analytics.Model, time.Time) (*analytics.Facts, error) {
	return d.f, d.err
}

func revenueLine(id uuid.UUID, amount money.Minor) analytics.Fact {
	return analytics.Fact{ID: id.String(), Dims: map[string]string{"company": "2000", "account_type": "revenue", "determination": "none"},
		Amount: amount, Currency: "EUR"}
}

// TestDocumentsFromTheStore: a fresh snapshot lists the documents from the
// store, one more asked for than the page holds, each signed as the metric
// adds it, with the figure's facts from the store too; otherwise, or when
// the store fails, both come from the database.
func TestDocumentsFromTheStore(t *testing.T) {
	e := fixture.New(t)
	now := time.Now()
	pointer(t, e.Tenant.ID, &now)
	m := model(t)
	q := analytics.Query{Measures: []string{"revenue"}, Where: []analytics.Filter{{Dim: "company", Op: "in", Values: []string{"2000"}}}}
	rows := []wire.Row{{By: []string{"2000"}, Document: docA.String(), Currency: "EUR", Exponent: 2, Amount: -125},
		{By: []string{"2000"}, Document: docB.String(), Currency: "EUR", Exponent: 2, Amount: -500}}
	r := &docReader{rows: rows}
	db := factsDatabase{f: &analytics.Facts{Source: analytics.SourceDatabase, Exponents: map[string]int{"EUR": 2},
		Tables: map[string][]analytics.Fact{"gl_lines": {revenueLine(docB, -500), revenueLine(docA, -125)}}}}
	after := &analytics.DocumentCursor{ID: "0", Currency: "EUR"}
	var got *store.Drilled
	inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
		var err error
		if got, err = source(r, db).Documents(ctx, m, analytics.Principal{}, q, after, 1, time.Time{}); err != nil {
			t.Fatal(err)
		}
	})
	want := []analytics.Document{{Kind: analytics.KindJournalEntry, ID: docA, Company: "2000", Currency: "EUR", Exponent: 2, Amount: 125}}
	if !reflect.DeepEqual(got.Documents, want) || !got.More || got.Facts.Source != analytics.SourceStore {
		t.Errorf("from the store: got %v (more %v) from %s, want %v and more from the store", got.Documents, got.More, got.Facts.Source, want)
	}
	plan := r.calls[len(r.calls)-1].Plan
	if got := fmt.Sprintf("%s %s %v %v %d %d", plan.Dataset, plan.Aggregate, plan.By, plan.After, plan.Limit, len(plan.Where)); got != "gl_lines documents [company] [0 EUR] 2 3" {
		t.Errorf("plan: got %+v, want the documents of gl_lines by company after the cursor, two at most, with the metric's and the row's filters", plan)
	}
	inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
		all, err := source(&docReader{rows: rows}, db).Documents(ctx, m, analytics.Principal{}, q, nil, 5, time.Time{})
		if err != nil || len(all.Documents) != 2 || all.More {
			t.Errorf("one page of all: got %+v, %v; want both documents and no more", all, err)
		}
	})
	for name, c := range map[string]struct {
		r      store.Querier
		notNew time.Time
	}{
		"the store failing":       {&docReader{docsErr: errors.New("down")}, time.Time{}},
		"the figure failing":      {&docReader{err: errors.New("down")}, time.Time{}},
		"a row of no document":    {&docReader{rows: []wire.Row{{By: []string{"2000"}, Currency: "EUR", Exponent: 2}}}, time.Time{}},
		"a row of no company":     {&docReader{rows: []wire.Row{{Document: docA.String(), Currency: "EUR", Exponent: 2}}}, time.Time{}},
		"a row of no currency":    {&docReader{rows: []wire.Row{{By: []string{"2000"}, Document: docA.String()}}}, time.Time{}},
		"a currency's two digits": {&docReader{rows: []wire.Row{{By: []string{"2000"}, Document: docA.String(), Currency: "EUR", Exponent: 3}}}, time.Time{}},
		"newer data asked for":    {&docReader{rows: rows}, now.Add(time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
				got, err := source(c.r, db).Documents(ctx, m, analytics.Principal{}, q, nil, 5, c.notNew)
				if err != nil || got.Facts.Source != analytics.SourceDatabase || len(got.Documents) != 2 || got.Documents[0].ID != docA {
					t.Errorf("got %+v, %v; want both documents from the database", got, err)
				}
			})
		})
	}
}

// TestDocumentsFailures: a refused caller, a database that fails and a
// fact that names no document refuse the drill-through.
func TestDocumentsFailures(t *testing.T) {
	e := fixture.New(t)
	m := model(t)
	q := analytics.Query{Measures: []string{"revenue"}}
	for name, c := range map[string]struct {
		src  *store.Source
		want string
	}{
		"a permission missing": {&store.Source{Reader: &docReader{}, Requires: []string{"ledger.entry.read"}}, "ledger.entry.read"},
		"the database failing": {source(nil, factsDatabase{err: errors.New("down")}), "analytics: database facts: down"},
		"a fact of no document": {source(nil, factsDatabase{f: &analytics.Facts{Tables: map[string][]analytics.Fact{"gl_lines": {{ID: "x",
			Dims: map[string]string{"company": "2000", "account_type": "revenue", "determination": "none"}}}}}}), "gl_lines fact of document"},
	} {
		t.Run(name, func(t *testing.T) {
			inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
				if _, err := c.src.Documents(ctx, m, analytics.Principal{}, q, nil, 5, time.Time{}); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("got %v, want an error naming %q", err, c.want)
				}
			})
		})
	}
}

// documents: for every metric whose documents are listed, by company and
// by month and company, each row's documents are the same from the store
// as from the database, a page at a time as all at once, and a sum's add
// up to the row's figure.
func (p parity) documents(t *testing.T) {
	m := model(t)
	dbFacts := p.dbFacts(t, m)
	for _, met := range m.Metrics {
		if met.Derived != nil || analytics.DocumentKind(met.Source, "") == "" {
			continue
		}
		for _, by := range [][]string{{"company"}, {"fiscal_month", "company"}} {
			q := analytics.Query{Measures: []string{met.Name}, By: by}
			res, err := analytics.Execute(m, dbFacts, analytics.Principal{}, q)
			if err != nil {
				continue // a sum over two currencies: no row to drill
			}
			for _, row := range res.Rows {
				d := analytics.Drill{Query: q, Row: map[string]string{}}
				for i, dim := range by {
					d.Row[dim] = row[i].(string)
				}
				t.Run(fmt.Sprint(met.Name, d.Row), func(t *testing.T) { p.sameDocuments(t, m, dbFacts, d, row[len(by)]) })
			}
		}
	}
}

// sameDocuments drills one row both ways.
func (p parity) sameDocuments(t *testing.T, m *analytics.Model, dbFacts *analytics.Facts, d analytics.Drill, figure any) {
	t.Helper()
	rq, err := analytics.RowQuery(m, d)
	if err != nil {
		t.Fatal(err)
	}
	want, err := analytics.DocumentsOf(m, dbFacts, analytics.Principal{}, rq)
	if err != nil || len(want) == 0 {
		t.Fatalf("database: got %v, %v; want documents", want, err)
	}
	var got []analytics.Document
	inRequest(t, p.Tenant.ID, nil, func(ctx context.Context) {
		var after *analytics.DocumentCursor
		for {
			page, err := p.src.Documents(ctx, m, analytics.Principal{}, rq, after, 1, time.Time{})
			if err != nil || page.Facts.Source != analytics.SourceStore {
				t.Fatalf("store: got %+v, %v; want a page from the store", page, err)
			}
			got = append(got, page.Documents...)
			if !page.More {
				return
			}
			last := page.Documents[0]
			after = &analytics.DocumentCursor{ID: last.ID.String(), Currency: last.Currency}
		}
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("documents: got %v from the store, want %v from the database", got, want)
	}
	if v, ok := figure.(analytics.Value); ok && m.Metric(rq.Measures[0]).Aggregate == "sum" {
		var sum money.Minor
		for _, doc := range got {
			sum += doc.Amount
		}
		if total := (analytics.Value{Units: sum, Scale: v.Scale, Currency: v.Currency}); total != v {
			t.Errorf("the documents add up to %v, want the row's %v", total, v)
		}
	}
}
