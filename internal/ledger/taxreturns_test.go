package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// renderGroups renders tax item groups as
// direction/kind/component/jurisdiction/account key [prior] items base tax
// recoverable non-recoverable.
func renderGroups(gs []ledger.TaxItemGroup) string {
	out := make([]string, len(gs))
	for i, g := range gs {
		j := "-"
		if g.Jurisdiction != nil {
			j = *g.Jurisdiction
		}
		prior := ""
		if g.PriorPeriod {
			prior = " prior"
		}
		out[i] = fmt.Sprintf("%s/%s/%s/%s/%s%s %d: %d %d %d %d", g.Direction, g.Kind, g.Component, j, g.AccountKey, prior, g.Items,
			g.BaseLC, g.TaxLC, g.RecoverableLC, g.NonRecoverableLC())
	}
	return strings.Join(out, "; ")
}

// TestUnreportedTaxItems: what a return of a period reads (ADR-0049 §11,
// T61h): the items not yet reported dated on or before the period's end,
// grouped, those before its start apart as prior-period items; the items
// of an entry migrated from SAP left out (ADR-0037, T72c); a reported item
// left out; a box's items paged in tax date and entry order, chosen as
// the groups are; only of legal entities the caller may act in, as a
// return's claim and its fiscal periods are (taxfiling's tests prepare
// returns).
func TestUnreportedTaxItems(t *testing.T) {
	e := setup(t)
	outputCodes(t, e)
	e.Must(201, "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-03-10", `"tax_date": "2026-02-27", `), "") // a prior period's, posted late
	e.Must(201, "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-03-10", ""), "")
	e.Must(201, "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-04-02", ""), "") // the next period's
	e.Must(201, "POST", "/v1/journal-entries", saleEntry(t, e, saleLine{"100000", "105.00", "", ""}, saleLine{"400000", "-100.00", "S5", "AE-DU"},
		saleLine{"240000", "-5.00", "S5", "AE-DU"}), "")
	reported, _ := e.Must(201, "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-03-11", ""), "")
	// Reported in a return: the tax filing context's check that the return
	// stands and claims it is off (taxfiling's tests prepare them).
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE ledger.tax_item SET reported_in = gen_random_uuid() WHERE journal_entry_id = '%s'; END $$`, apitest.String(t, reported, "id")))
	code := "V1"
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-02-01"),
			PostingDate: date("2026-02-01"), Currency: "EUR", SourceContext: "integration", SourceDocumentID: uuid.New(),
			Lines: []ledger.PostingLine{{AccountID: uuid.MustParse(e.account(t, "2000", "520000")), Amount: 1000, TaxCode: &code},
				{AccountID: uuid.MustParse(e.account(t, "2000", "140000")), Amount: 190, TaxCode: &code},
				{AccountID: uuid.MustParse(e.account(t, "2000", "100000")), Amount: -1190}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	read := func(t *testing.T, f func(ctx context.Context) error) {
		t.Helper()
		if err := apitest.InRequest(t, e.ten, e.ten.Human, f); err != nil {
			t.Fatal(err)
		}
	}
	var groups []ledger.TaxItemGroup
	read(t, func(ctx context.Context) (err error) {
		groups, err = ledger.TaxItems{}.Unreported(ctx, []uuid.UUID{e.de}, date("2026-03-01"), date("2026-03-31"))
		return err
	})
	// V1 and V0 (at 0%) of each input entry, the sale's S5.
	want := "input/standard/tax/-/input 2: 15000 1900 1900 0; input/standard/tax/-/input prior 2: 15000 1900 1900 0; " +
		"output/standard/vat/AE-DU/output 1: -10000 -500 0 0"
	if got := renderGroups(groups); got != want {
		t.Errorf("groups:\ngot  %s\nwant %s", got, want)
	}

	t.Run("a box's items, paged", func(t *testing.T) {
		sel := ledger.TaxItemSelection{Direction: "input", Kinds: []string{"standard"}, AccountKeys: []string{"input"}, Components: []string{"tax"}}
		var first, next []ledger.ReturnItem
		read(t, func(ctx context.Context) (err error) {
			first, err = ledger.TaxItems{}.UnreportedItems(ctx, []uuid.UUID{e.de}, date("2026-03-31"), sel, nil, 1)
			if err != nil {
				return err
			}
			last := first[0]
			next, err = ledger.TaxItems{}.UnreportedItems(ctx, []uuid.UUID{e.de}, date("2026-03-31"), sel,
				&ledger.TaxItemKey{TaxDate: last.TaxDate, FiscalYear: last.FiscalYear, EntryID: last.EntryID, ItemNo: last.ItemNo}, 10)
			return err
		})
		var items []ledger.TaxItem
		for _, it := range append(first, next...) {
			if it.DocumentNumber == "" || it.PostingDate.IsZero() {
				t.Errorf("item %d of %s: got no document number or posting date", it.ItemNo, it.EntryID)
			}
			items = append(items, it.TaxItem)
		}
		if got, want := renderTaxItems(items), "V0/tax 2026-02-27 5000 0 0 (5000 0 0) at 0; V1/tax 2026-02-27 10000 1900 1900 (10000 1900 1900) at 19; "+
			"V0/tax 2026-03-10 5000 0 0 (5000 0 0) at 0; V1/tax 2026-03-10 10000 1900 1900 (10000 1900 1900) at 19"; got != want {
			t.Errorf("items:\ngot  %s\nwant %s", got, want)
		}
	})
	t.Run("chosen as the groups are", func(t *testing.T) {
		for name, sel := range map[string]ledger.TaxItemSelection{
			"all output":        {Direction: "output"},
			"by jurisdiction":   {Direction: "output", Jurisdictions: []string{"AE-DU"}},
			"another emirate":   {Direction: "output", Jurisdictions: []string{"AE-AZ"}},
			"none of input":     {Direction: "input", Jurisdictions: []string{"AE-DU"}},
			"zero-rated":        {Direction: "input", Kinds: []string{"zero"}},
			"another component": {Direction: "input", Components: []string{"gst"}},
			"self-assessed":     {Direction: "input", AccountKeys: []string{"self_input"}},
		} {
			t.Run(name, func(t *testing.T) {
				var items []ledger.ReturnItem
				read(t, func(ctx context.Context) (err error) {
					items, err = ledger.TaxItems{}.UnreportedItems(ctx, []uuid.UUID{e.de}, date("2026-03-31"), sel, nil, 100)
					return err
				})
				var n int64
				for _, g := range groups {
					if sel.Matches(g) {
						n += g.Items
					}
				}
				if int64(len(items)) != n {
					t.Errorf("items: got %d, want the %d of the groups it matches", len(items), n)
				}
			})
		}
	})
	t.Run("released in a request", func(t *testing.T) {
		if _, err := (ledger.TaxItems{}).Release(context.Background(), uuid.New()); err == nil {
			t.Error("outside a unit of work: got no error, want one")
		}
	})
	t.Run("output tax is not recovered", func(t *testing.T) {
		g := ledger.TaxItemGroup{AccountKey: "output", TaxLC: -500}
		if got := g.NonRecoverableLC(); got != money.Minor(0) {
			t.Errorf("got %v, want 0", got)
		}
	})
	t.Run("of what the caller may see", func(t *testing.T) {
		// The agent acts in no legal entity.
		for name, f := range map[string]func(ctx context.Context) error{
			"groups": func(ctx context.Context) error {
				_, err := ledger.TaxItems{}.Unreported(ctx, []uuid.UUID{e.de}, date("2026-03-01"), date("2026-03-31"))
				return err
			},
			"items": func(ctx context.Context) error {
				_, err := ledger.TaxItems{}.UnreportedItems(ctx, []uuid.UUID{e.us}, date("2026-03-31"), ledger.TaxItemSelection{Direction: "input"}, nil, 1)
				return err
			},
			"a return's claim": func(ctx context.Context) error {
				_, _, err := ledger.TaxItems{}.Claim(ctx, []uuid.UUID{e.de}, nil, date("2026-03-01"), date("2026-03-31"),
					[]ledger.TaxItemSelection{{Direction: "input"}}, uuid.New())
				return err
			},
			"a return's fiscal periods": func(ctx context.Context) error {
				_, err := ledger.ReturnPeriods{}.Overlapping(ctx, []uuid.UUID{e.de}, date("2026-03-01"), date("2026-03-31"))
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				pgtest.WantErr(t, apitest.InRequest(t, e.ten, e.ten.Agent, f), "request.reference_not_found")
			})
		}
	})
}
