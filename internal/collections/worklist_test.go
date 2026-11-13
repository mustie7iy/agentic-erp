package collections_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// worklist reads company 2000's collection worklist at the key date.
func (e env) worklist(t *testing.T, key, query string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/collection-worklist?legal_entity_id="+e.DE.String()+"&key_date="+key+query, "", "")
	return got
}

// listed are a worklist page's customers as "number open overdue oldest
// level last", in the page's order.
func listed(t *testing.T, page map[string]any) string {
	t.Helper()
	var out []string
	for _, c := range objects(t, page, "items") {
		last := "-"
		if d, ok := c["last_dunning_date"]; ok {
			last = fmt.Sprint(d)
		}
		out = append(out, fmt.Sprintf("%v %s %s %v %v %s", c["customer_number"], amount(t, c, "open_amount"), amount(t, c, "overdue_amount"),
			c["oldest_due_date"], c["dunning_level"], last))
	}
	return strings.Join(out, "; ")
}

// totals are a worklist's totals as "customers open overdue".
func totals(t *testing.T, page map[string]any) string {
	t.Helper()
	return fmt.Sprintf("%v %s %s", page["customer_count"], amount(t, page, "open_amount"), amount(t, page, "overdue_amount"))
}

// TestWorklist: the worklist lists the customers with items overdue on
// the key date, the largest overdue amount first, each with what its open
// items and its overdue items add up to, its oldest due date and the
// dunning level and date recorded for its open items, and gives their
// totals; a customer with nothing overdue is not listed; the amounts are
// the ledger's open items'; nothing is stored (T76c acceptance).
func TestWorklist(t *testing.T) {
	e := setup(t)
	e.story(t)
	got := e.worklist(t, keyDate, "")
	for f, want := range map[string]any{"legal_entity_id": e.DE.String(), "company_code": "2000", "company_name": "Acme GmbH", "key_date": keyDate,
		"currency": "EUR"} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	// Northwind owes 100.00, 50.00 and 70.00 overdue, 30.00 due on the key date and 40.00 without terms dated it (due that day), less
	// credits of 20.00 and 10.00; Southwind 200.00 USD overdue, booked at 160.00 EUR; Eastwind's 90.00 is not yet due.
	const never = "BP70000002 260.00 EUR 220.00 EUR 2026-03-31 0 -; BP70000001 160.00 EUR 160.00 EUR 2026-04-30 0 -"
	if l := listed(t, got); l != never {
		t.Errorf("the worklist: got %q, want %q", l, never)
	}
	if tot := totals(t, got); tot != "2 420.00 EUR 380.00 EUR" {
		t.Errorf("totals: got %q, want 2 customers, 420.00 EUR open, 380.00 EUR overdue", tot)
	}
	if got["next_page_token"] != nil {
		t.Errorf("next page: got %v, want none", got["next_page_token"])
	}
	first := objects(t, got, "items")[0]
	if first["customer_id"] != e.north.String() || first["customer_name"] != "Northwind Traders" {
		t.Errorf("the first customer: got %v %v, want Northwind Traders, %s", first["customer_id"], first["customer_name"], e.north)
	}
	t.Run("the amounts are the ledger's open items'", func(t *testing.T) {
		var open []ledger.ReceivableItem
		if err := e.inRequest(t, func(ctx context.Context) (err error) {
			open, err = ledger.OpenItems{}.Receivable(ctx, e.DE, []uuid.UUID{e.north})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var sum, due money.Minor
		for _, it := range open {
			sum += it.AmountLC
			if it.DaysOverdue(date(t, keyDate)) > 0 && it.Amount > 0 {
				due += it.AmountLC
			}
		}
		if sum != 26000 || due != 22000 {
			t.Errorf("Northwind's open items in the ledger: got %d open and %d overdue, want 26000 and 22000, the worklist's", sum, due)
		}
	})
	for _, c := range []struct{ name, key, list, totals string }{
		{"before anything is overdue", "2026-03-31", "", "0 0.00 EUR 0.00 EUR"},
		{"the day after the first due date", "2026-04-01", "BP70000002 260.00 EUR 100.00 EUR 2026-03-31 0 -", "1 260.00 EUR 100.00 EUR"},
		{"the day after its document date the debit without terms is overdue", "2026-07-01",
			"BP70000002 260.00 EUR 290.00 EUR 2026-03-31 0 -; BP70000001 160.00 EUR 160.00 EUR 2026-04-30 0 -", "2 420.00 EUR 450.00 EUR"},
		{"once everything is overdue", "2026-12-31", "BP70000002 260.00 EUR 290.00 EUR 2026-03-31 0 -; BP70000001 160.00 EUR 160.00 EUR 2026-04-30 0 -; " +
			"BP70000003 90.00 EUR 90.00 EUR 2026-07-15 0 -", "3 510.00 EUR 540.00 EUR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).worklist(t, c.key, "")
			if l := listed(t, got); l != c.list {
				t.Errorf("the worklist at %s: got %q, want %q", c.key, l, c.list)
			}
			if tot := totals(t, got); tot != c.totals {
				t.Errorf("totals at %s: got %q, want %q", c.key, tot, c.totals)
			}
			if items, ok := got["items"].([]any); !ok || items == nil {
				t.Errorf("items: got %v, want a list, empty when nobody is listed", got["items"])
			}
		})
	}
	t.Run("with the dunning recorded", func(t *testing.T) {
		id, tag := e.In(t).start(t, keyDate)
		e.In(t).confirm(t, id, tag)
		want := "BP70000002 260.00 EUR 220.00 EUR 2026-03-31 1 2026-06-30; BP70000001 160.00 EUR 160.00 EUR 2026-04-30 1 2026-06-30"
		if l := listed(t, e.In(t).worklist(t, keyDate, "")); l != want {
			t.Errorf("after the first confirmed run: got %q, want %q", l, want)
		}
		id, tag = e.In(t).start(t, "2026-07-15")
		e.In(t).confirm(t, id, tag)
		// The level and date are what is recorded now, whatever the key date read.
		want = "BP70000002 260.00 EUR 220.00 EUR 2026-03-31 2 2026-07-15; BP70000001 160.00 EUR 160.00 EUR 2026-04-30 2 2026-07-15"
		if l := listed(t, e.In(t).worklist(t, keyDate, "")); l != want {
			t.Errorf("after the second confirmed run: got %q, want %q", l, want)
		}
	})
	var stored int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'collections.%worklist%'`, []any{e.Tenant.ID}, &stored)
	if stored != 0 {
		t.Errorf("audit records of reading the worklist: got %d, want none: it stores nothing", stored)
	}
}

// TestWorklistPages: the worklist is paged by cursor in its order, the
// largest overdue amount first and customers with the same amount by id:
// the pages together are the worklist, nobody twice, with the worklist's
// totals on every page; a page continues after its token's place when the
// amounts changed in between, so a customer whose amount rose is not read
// again and one whose amount fell is; a token is bound to its company and
// key date (T76c acceptance: the order is stable across pages).
func TestWorklistPages(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	// Two more customers owing what Eastwind owes: three with the same overdue amount.
	tied := []uuid.UUID{e.east, e.customer(t, "BP70000004", "Westwind SA"), e.customer(t, "BP70000005", "Tradewind BV")}
	e.post(t, item{customer: tied[1], amount: 9000, currency: "EUR", day: "2026-06-15", ref: "CI-W"},
		item{customer: tied[2], amount: 9000, currency: "EUR", day: "2026-06-15", ref: "CI-T"})
	slices.SortFunc(tied, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	want := []string{e.north.String(), e.south.String(), tied[0].String(), tied[1].String(), tied[2].String()}
	const key = "2026-12-31"
	ids := func(t *testing.T, page map[string]any) []string {
		t.Helper()
		var out []string
		for _, c := range objects(t, page, "items") {
			out = append(out, apitest.String(t, c, "customer_id"))
		}
		return out
	}
	if got := ids(t, e.worklist(t, key, "")); !slices.Equal(got, want) {
		t.Fatalf("the worklist on one page: got %v, want %v", got, want)
	}
	for _, size := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("pages of %d", size), func(t *testing.T) {
			var got []string
			query := fmt.Sprintf("&page_size=%d", size)
			for pages := 0; ; pages++ {
				if pages > len(want) {
					t.Fatalf("more than %d pages of %d", len(want), size)
				}
				page := e.In(t).worklist(t, key, query)
				if tot := totals(t, page); tot != "5 690.00 EUR 720.00 EUR" {
					t.Errorf("totals on page %d: got %q, want the worklist's: 5 customers, 690.00 EUR open, 720.00 EUR overdue", pages+1, tot)
				}
				got = append(got, ids(t, page)...)
				next, more := page["next_page_token"].(string)
				if !more {
					break
				}
				query = fmt.Sprintf("&page_size=%d&page_token=%s", size, next)
			}
			if !slices.Equal(got, want) {
				t.Errorf("the pages together: got %v, want %v", got, want)
			}
		})
	}
	first := e.worklist(t, key, "&page_size=2")
	token := apitest.String(t, first, "next_page_token")
	t.Run("a token of another key date", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_page_token", "GET",
			"/v1/collection-worklist?legal_entity_id="+e.DE.String()+"&key_date=2026-12-30&page_token="+token, "", "")
	})
	t.Run("a token that is none", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_page_token", "GET",
			"/v1/collection-worklist?legal_entity_id="+e.DE.String()+"&key_date="+key+"&page_token=!!", "", "")
	})
	t.Run("amounts changed between pages", func(t *testing.T) {
		// The last tied customer now owes the most: it moves to the top, before the token's place.
		e.In(t).post(t, item{customer: uuid.MustParse(want[4]), amount: 90000, currency: "EUR", day: "2026-06-15", ref: "CI-MORE"})
		second := e.In(t).worklist(t, key, "&page_size=2&page_token="+token)
		if got := ids(t, second); !slices.Equal(got, want[2:4]) || second["next_page_token"] != nil {
			t.Errorf("the second page: got %v and next page %v, want %v, the last: nobody twice, the order kept", got, second["next_page_token"], want[2:4])
		}
		if got := ids(t, e.In(t).worklist(t, key, "&page_size=1")); !slices.Equal(got, want[4:]) {
			t.Errorf("read again from the start: got %v, want %v leading", got, want[4:])
		}
	})
	t.Run("an amount fell between pages", func(t *testing.T) {
		// The first page ends at Northwind, 290.00 overdue; Northwind then pays CI-A and the debit without terms and owes 150.00
		// overdue, less than Southwind.
		first := e.In(t).worklist(t, key, "&page_size=2")
		if got := ids(t, first); !slices.Equal(got, []string{want[4], want[0]}) {
			t.Fatalf("the first page: got %v, want %v", got, []string{want[4], want[0]})
		}
		payment := e.In(t).post(t, item{customer: e.north, amount: -14000, currency: "EUR", day: "2026-06-28", noTerms: true, ref: "PAYMENT"})[0]
		e.In(t).Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-06-30", "items": [
			{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1},
			{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`, e.DE, entries[0].ID, entries[4].ID, payment.ID), "")
		second := e.In(t).worklist(t, key, "&page_size=2&page_token="+apitest.String(t, first, "next_page_token"))
		if got := ids(t, second); !slices.Equal(got, []string{want[1], want[0]}) {
			t.Errorf("the second page: got %v, want %v: Southwind, then Northwind again at its new place", got, []string{want[1], want[0]})
		}
		if again := objects(t, second, "items")[1]; amount(t, again, "overdue_amount") != "150.00 EUR" {
			t.Errorf("Northwind listed again: got %s overdue, want its new 150.00 EUR", amount(t, again, "overdue_amount"))
		}
	})
}

// TestWorklistRefusals: a company the caller may not act in, and one that
// does not exist, is not found; a key date after the last is refused,
// naming the field; the company and the key date are required; the
// permission is its own.
func TestWorklistRefusals(t *testing.T) {
	e := setup(t)
	e.story(t)
	path := func(le uuid.UUID, key string) string {
		return "/v1/collection-worklist?legal_entity_id=" + le.String() + "&key_date=" + key
	}
	other := e.As(e.Reader(t, "A person of company 1000", e.US))
	t.Run("a company the caller may not act in", func(t *testing.T) {
		other.In(t).Problem(404, "request.not_found", "GET", path(e.DE, keyDate), "", "")
	})
	t.Run("a company that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", path(uuid.New(), keyDate), "", "")
	})
	t.Run("a company without dunning levels", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "GET", path(e.US, keyDate), "", "")
		if tot := totals(t, got); tot != "0 0.00 USD 0.00 USD" {
			t.Errorf("company 1000's worklist: got %q, want none listed, in its currency", tot)
		}
	})
	t.Run("the day after the last key date", func(t *testing.T) {
		got := e.In(t).Problem(422, "collections.invalid_key_date", "GET", path(e.DE, "2101-01-01"), "", "")
		if f := field(got); f != "key_date" {
			t.Errorf("field: got %v, want key_date", f)
		}
	})
	t.Run("the last key date", func(t *testing.T) {
		e.In(t).Must(200, "GET", path(e.DE, "2100-12-31"), "", "")
	})
	t.Run("without a key date", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_parameter", "GET", "/v1/collection-worklist?legal_entity_id="+e.DE.String(), "", "")
	})
	t.Run("without a company", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_parameter", "GET", "/v1/collection-worklist?key_date="+keyDate, "", "")
	})
	t.Run("a page size out of range", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_parameter", "GET", path(e.DE, keyDate)+"&page_size=501", "", "")
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'collections.worklist.read'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "GET", path(e.DE, keyDate), "", "")
	})
}

// TestWorklistThroughThePort: what only a port could answer: the
// worklist is read in a unit of work; amounts that add up to more than an
// amount holds are refused, for a customer and across customers.
func TestWorklistThroughThePort(t *testing.T) {
	e := setup(t)
	e.story(t)
	read := func(t *testing.T, h *collections.Endpoints) error {
		t.Helper()
		return e.inRequest(t, func(ctx context.Context) error {
			w, err := h.WorklistOf(ctx, e.DE, date(t, keyDate))
			if err == nil {
				t.Errorf("got a worklist of %d customers, want an error", len(w.Customers))
			}
			return err
		})
	}
	t.Run("outside a unit of work", func(t *testing.T) {
		if _, err := endpoints(t).WorklistOf(context.Background(), e.DE, date(t, keyDate)); err == nil {
			t.Error("got nil error, want an error")
		}
	})
	for _, c := range []struct {
		name      string
		customers []uuid.UUID
	}{
		{"a customer's amounts out of range", []uuid.UUID{e.north, e.north}},
		{"the customers' amounts out of range", []uuid.UUID{e.north, e.south}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := endpoints(t)
			h.OpenItems = huge{customers: c.customers}
			p := problem.From(read(t, h))
			if p.Code != "collections.amount_too_large" || p.Status != 422 {
				t.Errorf("got %d %s, want 422 collections.amount_too_large", p.Status, p.Code)
			}
		})
	}
}
