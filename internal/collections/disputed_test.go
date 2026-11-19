package collections_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// disputedOf are a run's disputed items as "reference case customer due
// days amount local", by customer and item.
func disputedOf(t *testing.T, run map[string]any) string {
	t.Helper()
	var out []string
	for _, d := range objects(t, run, "disputed_items") {
		out = append(out, fmt.Sprintf("%v %v %v %v %v %s %s", d["reference"], d["dispute_case_id"], d["customer_number"], d["net_due_date"],
			d["days_overdue"], amount(t, d, "amount"), amount(t, d, "local_amount")))
	}
	return strings.Join(out, "; ")
}

// TestDisputedInDunning: an item with an open dispute case is in no
// notice, whatever part of it is disputed; the run names it as disputed
// with its case, in none of its totals, and reads back and lists so; an
// item whose case is closed is proposed again (T86b acceptance, plan
// default 4).
func TestDisputedInDunning(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	// 10.00 of Northwind's CI-A, 100.00 EUR and 91 days overdue, is disputed.
	ca, caTag := e.open(t, dispute(e.DE, entries[0], "10.00", "EUR", ""))
	got, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	id := apitest.String(t, got, "id")
	if c := fmt.Sprintf("%v %v %s %v %s", got["notice_count"], got["item_count"], amount(t, got, "overdue_amount"), got["disputed_item_count"],
		amount(t, got, "disputed_amount")); c != "2 2 210.00 EUR 1 100.00 EUR" {
		t.Errorf("the run's totals: got %s, want 2 notices of 2 items, 210.00 EUR (CI-B and CI-USD), and CI-A's 100.00 EUR disputed", c)
	}
	want := "CI-A " + ca + " BP70000002 2026-03-31 91 100.00 EUR 100.00 EUR"
	if d := disputedOf(t, got); d != want {
		t.Errorf("disputed: got %q, want %q", d, want)
	}
	if p := e.proposed(t, id); p != "CI-USD 0->1, CI-B 0->1" {
		t.Errorf("proposed: got %q, want CI-A left out", p)
	}
	read, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id, "", "")
	page, _ := e.Must(200, "GET", "/v1/dunning-runs?key_date_from="+keyDate, "", "")
	if a, b := disputedOf(t, read), disputedOf(t, objects(t, page, "items")[0]); a != want || b != want {
		t.Errorf("read back: got %q, listed %q, want %q", a, b, want)
	}
	t.Run("a run without disputed items lists none", func(t *testing.T) {
		got, _ := e.In(t).Must(201, "POST", "/v1/dunning-runs", run(e.DE, "2026-03-31"), "")
		if items, ok := got["disputed_items"].([]any); !ok || len(items) != 0 {
			t.Errorf("disputed_items: got %v, want an empty list", got["disputed_items"])
		}
	})
	e.Must(200, "POST", "/v1/dispute-cases/"+ca+":close", `{"outcome": "rejected", "reason": "Priced as quoted"}`, caTag)
	again, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	if p, d := e.proposed(t, apitest.String(t, again, "id")), disputedOf(t, again); p != "CI-USD 0->1, CI-A 0->1, CI-B 0->1" || d != "" {
		t.Errorf("once the case is closed: got %q and disputed %q, want CI-A proposed again and nothing disputed", p, d)
	}
}

// TestDisputeAfterTheProposal: a run proposed before a case was opened on
// one of its items is confirmed as proposed, the item recorded as dunned.
func TestDisputeAfterTheProposal(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	id, tag := e.start(t, keyDate)
	e.open(t, dispute(e.DE, entries[0], "100.00", "EUR", ""))
	if got := e.confirm(t, id, tag); got["status"] != "confirmed" {
		t.Errorf("status: got %v, want confirmed", got["status"])
	}
	if got, want := e.dunned(t), "CI-A 1 2026-06-30, CI-B 1 2026-06-30, CI-USD 1 2026-06-30"; got != want {
		t.Errorf("recorded levels: got %q, want %q", got, want)
	}
}

// TestDisputedInTheWorklist: the worklist shows each customer's disputed
// amount, its open cases' shares of their items in the company's currency
// whether the items are overdue or not, and how many cases they are,
// beside its overdue amount; its order and totals are as without the
// cases; a closed case counts no more (T86b acceptance).
func TestDisputedInTheWorklist(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	before := e.worklist(t, keyDate, "")
	e.open(t, dispute(e.DE, entries[0], "10.00", "EUR", ""))
	cd, cdTag := e.open(t, dispute(e.DE, entries[3], "30.00", "EUR", "")) // CI-D, due on the key date: not overdue
	e.open(t, dispute(e.DE, entries[7], "33.33", "USD", ""))              // 26.66 EUR, as booked
	disputed := func(t *testing.T) (string, map[string]any) {
		t.Helper()
		got := e.In(t).worklist(t, keyDate, "")
		var rows []string
		for _, c := range objects(t, got, "items") {
			rows = append(rows, fmt.Sprintf("%v %s %v", c["customer_number"], amount(t, c, "disputed_amount"), c["open_dispute_cases"]))
		}
		return strings.Join(rows, "; "), got
	}
	r, got := disputed(t)
	if want := "BP70000002 40.00 EUR 2; BP70000001 26.66 EUR 1"; r != want {
		t.Errorf("disputed: got %q, want %q", r, want)
	}
	if tot := fmt.Sprintf("%s %v", amount(t, got, "disputed_amount"), got["open_dispute_cases"]); tot != "66.66 EUR 3" {
		t.Errorf("the worklist's disputed totals: got %s, want 66.66 EUR in 3 cases, every customer's", tot)
	}
	// Eastwind is not listed, nothing of it being overdue: its case counts in no total.
	e.open(t, dispute(e.DE, entries[8], "5.00", "EUR", ""))
	if _, got := disputed(t); fmt.Sprint(got["open_dispute_cases"]) != "3" {
		t.Errorf("the cases counted with a customer not listed: got %v, want 3", got["open_dispute_cases"])
	}
	if a, b := listed(t, got)+" | "+totals(t, got), listed(t, before)+" | "+totals(t, before); a != b {
		t.Errorf("order and totals: got %q, want them as without the cases: %q", a, b)
	}
	for _, c := range objects(t, before, "items") {
		if a := amount(t, c, "disputed_amount"); a != "0.00 EUR" || c["open_dispute_cases"] != float64(0) {
			t.Errorf("%v without cases: got %s in %v cases, want 0.00 EUR in 0", c["customer_number"], a, c["open_dispute_cases"])
		}
	}
	e.Must(200, "POST", "/v1/dispute-cases/"+cd+":close", `{"outcome": "accepted", "reason": "Short delivery"}`, cdTag)
	if r, _ := disputed(t); r != "BP70000002 10.00 EUR 1; BP70000001 26.66 EUR 1" {
		t.Errorf("after a case is closed: got %q, want Northwind's other case alone", r)
	}
}

// TestDisputedThroughThePort: what only a port or a direct write could
// answer: the disputed items a run names count towards the most items it
// takes; disputed amounts that add up to more than an amount holds refuse
// the worklist.
func TestDisputedThroughThePort(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	e.open(t, dispute(e.DE, entries[0], "1.00", "EUR", ""))
	in := collections.RunInput{LegalEntityID: e.DE, KeyDate: date(t, keyDate)}
	for _, c := range []struct {
		name  string
		limit int
		code  string
	}{
		{"two proposed and one disputed above a limit of 2", 2, "collections.run_too_large"},
		{"two proposed and one disputed at a limit of 3", 3, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := endpoints(t)
			h.MaxItems = c.limit
			err := e.inRequest(t, func(ctx context.Context) error {
				_, err := h.ProposeRun(ctx, in)
				return err
			})
			if got := problem.From(err).Code; (err == nil) != (c.code == "") || (err != nil && got != c.code) {
				t.Errorf("got %v, want %q", err, c.code)
			}
		})
	}
	t.Run("a run of disputed items alone above the limit", func(t *testing.T) {
		e.In(t).open(t, dispute(e.DE, entries[7], "1.00", "USD", ""))
		h := endpoints(t)
		h.MaxItems = 1
		// On 5 May only CI-A and CI-USD reach the first level, and both are disputed: no notice.
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ProposeRun(ctx, collections.RunInput{LegalEntityID: e.DE, KeyDate: date(t, "2026-05-05")})
			return err
		})
		if p := problem.From(err); p.Code != "collections.run_too_large" {
			t.Errorf("got %v, want collections.run_too_large", err)
		}
	})
	t.Run("a run's disputed amounts out of range", func(t *testing.T) {
		h := endpoints(t)
		h.OpenItems = huge{customers: []uuid.UUID{e.north, e.north}}
		var real []ledger.ReceivableItem
		if err := e.inRequest(t, func(ctx context.Context) (err error) {
			real, err = ledger.OpenItems{}.Receivable(ctx, e.DE, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// huge gives real[0]'s lines 1 and 2 to Northwind at a huge local amount; both are disputed.
		for line := 1; line <= 2; line++ {
			pgtest.Exec(t, seedCase+` VALUES ($1, gen_random_uuid(), $2, $3, 'BP70000002', $4, $5, $6, 'X', 'EUR', 1, 'EUR', 1, 'x', 'open', $7, $7)
				ON CONFLICT DO NOTHING`+seededItem, e.Tenant.ID, e.DE, e.north, real[0].FiscalYear, real[0].EntryID, line, e.Tenant.Human.ID)
		}
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ProposeRun(ctx, collections.RunInput{LegalEntityID: e.DE, KeyDate: date(t, "2026-12-31")})
			return err
		})
		if p := problem.From(err); p.Code != "collections.amount_too_large" {
			t.Errorf("got %v, want collections.amount_too_large", err)
		}
	})
	t.Run("disputed amounts out of range", func(t *testing.T) {
		// The open case of CI-A's item and this one add up to more than an amount holds.
		pgtest.Exec(t, seedCase+` VALUES ($1, gen_random_uuid(), $2, $3, 'BP70000002', 2026, $4, 1, 'X', 'EUR', 1, 'EUR', 9223372036854775807, 'x', 'open',
			$5, $5)`+seededItem, e.Tenant.ID, e.DE, e.north, entries[1].ID, e.Tenant.Human.ID)
		e.In(t).Problem(422, "collections.amount_too_large", "GET", "/v1/collection-worklist?legal_entity_id="+e.DE.String()+"&key_date="+keyDate, "", "")
	})
}

// seedCase and seededItem write a dispute case without its API, with its
// one item, its own line (dispute_case_items holds them to each other).
const (
	seedCase = `WITH c AS (INSERT INTO collections.dispute_case (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year,
		journal_entry_id, line_no, document_number, currency, disputed_amount, currency_lc, disputed_amount_lc, reason, status, created_by, updated_by)`
	seededItem = ` RETURNING *) INSERT INTO collections.dispute_case_item (tenant_id, dispute_case_id, item_no, id, fiscal_year, journal_entry_id,
		line_no, document_number, disputed_amount, disputed_amount_lc)
		SELECT tenant_id, id, 1, gen_random_uuid(), fiscal_year, journal_entry_id, line_no, document_number, disputed_amount, disputed_amount_lc FROM c`
)
