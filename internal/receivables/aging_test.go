package receivables_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// aging reads company 2000's AR aging with the query ("key_date=…").
func (e env) aging(t *testing.T, query string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/receivables-aging?legal_entity_id="+e.DE.String()+"&"+query, "", "")
	return got
}

// buckets shows an aging's buckets as "name[from-to] count amount; …".
func buckets(t *testing.T, of map[string]any) string {
	t.Helper()
	raw, _ := of["buckets"].([]any)
	var b strings.Builder
	for _, r := range raw {
		m, _ := r.(map[string]any)
		name := fmt.Sprint(m["bucket"])
		if m["days_from"] != nil {
			name += fmt.Sprintf("[%v-%v]", m["days_from"], m["days_to"])
		}
		fmt.Fprintf(&b, "%s %v %s; ", name, m["item_count"], amount(t, m, "amount"))
	}
	return b.String()
}

// agingScene is two customers' open items: Northwind's invoice of 100.00
// dated 10 March (NT30, due 9 April) and the credit memo of 30.00 naming
// it, due with it; its invoice of 50.00 dated 1 May on NT14, due 15 May;
// a credit of 5.00 dated 15 March and posted without terms, so due that
// day (PO 2026-11-12); and Contoso's invoice of 200.00
// dated 1 June, due 1 July. An invoice of Contoso's credited in full is
// cleared against its memo and is no open item.
func agingScene(t *testing.T) (env, uuid.UUID) {
	t.Helper()
	e := setup(t)
	other := e.partner(t, "BP70000002", "Contoso", "active", "customer")
	e.companyData(t, other, "NT30")
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", ""))), "2026-03-12")
	e.post(t, e.entered(t, e.memo(e.customer, invoice["id"].(string), "2026-03-20", e.item("30.00", ""))), "2026-03-21")
	e.post(t, e.entered(t, strings.Replace(e.invoice(e.customer, "2026-05-01", e.item("50.00", "")), `"currency"`, `"payment_term_code": "NT14", "currency"`, 1)),
		"2026-05-01")
	e.credited(t, e.customer, 500, "2026-03-15")
	e.post(t, e.entered(t, e.invoice(other, "2026-06-01", e.item("200.00", ""))), "2026-06-01")
	cleared := e.post(t, e.entered(t, e.invoice(other, "2026-03-10", e.item("40.00", ""))), "2026-03-12")
	memo := e.post(t, e.entered(t, e.memo(other, cleared["id"].(string), "2026-03-14", e.item("40.00", ""))), "2026-03-14")
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-15", "items": [%s, %s]}`, e.DE,
		itemRef(cleared["journal_entry_id"], 1), itemRef(memo["journal_entry_id"], 1)), "")
	return e, other
}

// TestReceivablesAging (T75e acceptance): each open customer item falls
// in exactly one bucket by its net due date; a credit memo reduces its
// customer's bucket and total; an item without terms is due on its
// document date, not due that day and overdue from the day after; a
// cleared item is left out; amounts are in the company's currency; every
// bucket is given; and the report names the company and the customers.
func TestReceivablesAging(t *testing.T) {
	e, other := agingScene(t)
	got := e.aging(t, "key_date=2026-05-15")
	fields(t, "the report", got, map[string]any{"legal_entity_id": e.DE, "company_code": "2000", "company_name": "Acme GmbH", "key_date": "2026-05-15",
		"currency": "EUR", "aging_days": []any{30, 60, 90}})
	if _, more := got["next_page_token"]; more {
		t.Errorf("next_page_token: got %v, want none on the only page", got["next_page_token"])
	}
	totals, _ := got["totals"].(map[string]any)
	fields(t, "the totals", totals, map[string]any{"customer_count": 2, "item_count": 5, "amount": eur("315.00")})
	// On 15 May the invoice of 100.00 and its memo are 36 days overdue; the
	// invoice due that day is not yet due; the credit without terms, dated
	// 15 March, is 61 days past its document date.
	if got, want := buckets(t, totals), "not_due 2 250.00 EUR; overdue[1-30] 0 0.00 EUR; overdue[31-60] 2 70.00 EUR; overdue[61-90] 1 -5.00 EUR; "+
		"overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "; got != want {
		t.Errorf("the totals' buckets: got %s, want %s", got, want)
	}
	rows := items(t, got)
	if len(rows) != 2 {
		t.Fatalf("customers: got %d, want 2", len(rows))
	}
	fields(t, "Northwind's row", rows[0], map[string]any{"customer_id": e.customer, "customer_number": "BP70000001", "customer_name": "Northwind Traders",
		"item_count": 4, "amount": eur("115.00")})
	if got, want := buckets(t, rows[0]), "not_due 1 50.00 EUR; overdue[1-30] 0 0.00 EUR; overdue[31-60] 2 70.00 EUR; overdue[61-90] 1 -5.00 EUR; "+
		"overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "; got != want {
		t.Errorf("Northwind's buckets: got %s, want %s", got, want)
	}
	fields(t, "Contoso's row", rows[1], map[string]any{"customer_id": other, "customer_number": "BP70000002", "customer_name": "Contoso",
		"item_count": 1, "amount": eur("200.00")})
	if got, want := buckets(t, rows[1]), "not_due 1 200.00 EUR; overdue[1-30] 0 0.00 EUR; overdue[31-60] 0 0.00 EUR; overdue[61-90] 0 0.00 EUR; "+
		"overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "; got != want {
		t.Errorf("Contoso's buckets: got %s, want %s", got, want)
	}
	cases := []struct {
		name, query, want string
	}{
		{"a day later the invoice due on the key date is a day overdue", "key_date=2026-05-16",
			"not_due 1 200.00 EUR; overdue[1-30] 1 50.00 EUR; overdue[31-60] 2 70.00 EUR; overdue[61-90] 1 -5.00 EUR; overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
		{"buckets of the caller's days", "key_date=2026-05-15&aging_days=10,36",
			"not_due 2 250.00 EUR; overdue[1-10] 0 0.00 EUR; overdue[11-36] 2 70.00 EUR; overdue[37-<nil>] 1 -5.00 EUR; no_due_date 0 0.00 EUR; "},
		{"one bucket of overdue items", "key_date=2026-12-31&aging_days=3650",
			"not_due 0 0.00 EUR; overdue[1-3650] 5 315.00 EUR; overdue[3651-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
		{"before anything is due", "key_date=2026-01-01",
			"not_due 5 315.00 EUR; overdue[1-30] 0 0.00 EUR; overdue[31-60] 0 0.00 EUR; overdue[61-90] 0 0.00 EUR; overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
		{"the credit without terms on its document date is not due", "key_date=2026-03-15&customer_id=" + e.customer.String() + "&aging_days=1",
			"not_due 4 115.00 EUR; overdue[1-1] 0 0.00 EUR; overdue[2-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
		{"the day after its document date it is a day overdue", "key_date=2026-03-16&customer_id=" + e.customer.String() + "&aging_days=1",
			"not_due 3 120.00 EUR; overdue[1-1] 1 -5.00 EUR; overdue[2-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
		{"one customer", "key_date=2026-05-15&customer_id=" + other.String(),
			"not_due 1 200.00 EUR; overdue[1-30] 0 0.00 EUR; overdue[31-60] 0 0.00 EUR; overdue[61-90] 0 0.00 EUR; overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			totals, _ := e.aging(t, c.query)["totals"].(map[string]any)
			if got := buckets(t, totals); got != c.want {
				t.Errorf("the totals' buckets: got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("a company without customer items", func(t *testing.T) {
		got, _ := e.Must(200, "GET", "/v1/receivables-aging?key_date=2026-05-15&aging_days=30&legal_entity_id="+e.US.String(), "", "")
		totals, _ := got["totals"].(map[string]any)
		fields(t, "the totals", totals, map[string]any{"customer_count": 0, "item_count": 0, "amount": map[string]any{"amount": "0.00", "currency": "USD"}})
		if b := buckets(t, totals); b != "not_due 0 0.00 USD; overdue[1-30] 0 0.00 USD; overdue[31-<nil>] 0 0.00 USD; no_due_date 0 0.00 USD; " {
			t.Errorf("the totals' buckets: got %s, want every bucket empty, in USD", b)
		}
		if rows := items(t, got); len(rows) != 0 || got["currency"] != "USD" {
			t.Errorf("got %d customers in %v, want none, in USD", len(rows), got["currency"])
		}
	})
}

// TestAgingTotalsAreTheOpenItems (T75e acceptance): the totals equal the
// open items of the company's customer reconciliation accounts, in total
// and bucket by bucket as GET /v1/open-items ages them; the customers'
// rows add up to the totals. The two part for the one item without terms,
// the credit of 5.00 dated 15 March (PO 2026-11-12): the list, which
// shows items as posted, has it without a due date, and the report in the
// bucket of its days past its document date (without).
func TestAgingTotalsAreTheOpenItems(t *testing.T) {
	e, _ := agingScene(t)
	for query, without := range map[string]string{"key_date=2026-05-15": "overdue 61 90", "key_date=2026-04-10&aging_days=1,5": "overdue 6 <nil>",
		"key_date=2026-09-30&aging_days=45": "overdue 46 <nil>"} {
		t.Run(query, func(t *testing.T) {
			got := e.aging(t, query)
			totals, _ := got["totals"].(map[string]any)
			var open int64
			var count int
			pgtest.Scan(t, `SELECT coalesce(sum(l.amount_lc), 0)::bigint, count(*) FROM ledger.journal_line l
				WHERE l.tenant_id = $1 AND l.account_id = $2 AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id
					AND c.fiscal_year = l.fiscal_year AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)`,
				[]any{e.Tenant.ID, e.receivable}, &open, &count)
			fields(t, "the totals against the receivables account's open lines", totals,
				map[string]any{"amount": eur(money.Minor(open).Plain(2)), "item_count": count})
			// The list's aging leaves empty buckets out; those it gives are the report's.
			list, _ := e.Must(200, "GET", "/v1/open-items?legal_entity_id="+e.DE.String()+"&account_id="+e.receivable.String()+"&aging_"+query, "", "")
			want := map[string]agedSum{}
			for _, r := range list["aging"].([]any) {
				m := r.(map[string]any)
				want[fmt.Sprintf("%v %v %v", m["bucket"], m["days_from"], m["days_to"])] = sumOf(t, m, "amount_lc")
			}
			if got := want["no_due_date <nil> <nil>"]; got != (agedSum{1, -500}) {
				t.Fatalf("the open items' aging without a due date: got %+v, want the credit of 5.00 posted without terms", got)
			}
			delete(want, "no_due_date <nil> <nil>")
			want[without] = agedSum{want[without].count + 1, want[without].cents - 500}
			var sum, rows int64
			for _, r := range totals["buckets"].([]any) {
				m := r.(map[string]any)
				key, is := fmt.Sprintf("%v %v %v", m["bucket"], m["days_from"], m["days_to"]), sumOf(t, m, "amount")
				if listed := want[key]; listed != is { // a bucket the list leaves out is empty
					t.Errorf("bucket %s: got %+v, want the open items' %+v", key, is, listed)
				}
				delete(want, key)
			}
			if len(want) != 0 {
				t.Errorf("buckets of the open items' aging the report lacks: %v", want)
			}
			for _, row := range items(t, got) {
				a := row["amount"].(map[string]any)["amount"].(string)
				cents, err := money.ParseDecimal(a, 2)
				if err != nil {
					t.Fatal(err)
				}
				sum, rows = sum+int64(cents), rows+int64(row["item_count"].(float64))
			}
			if sum != open || rows != int64(count) {
				t.Errorf("the customers' rows: got %d over %d items, want the totals' %d over %d", sum, rows, open, count)
			}
		})
	}
}

// agedSum is a bucket's items: how many, and their amount in cents.
type agedSum struct {
	count int64
	cents money.Minor
}

// sumOf reads a bucket's items from the list's aging or the report.
func sumOf(t *testing.T, bucket map[string]any, field string) agedSum {
	t.Helper()
	a, _ := bucket[field].(map[string]any)
	cents, err := money.ParseDecimal(fmt.Sprint(a["amount"]), 2)
	if err != nil || a["currency"] != "EUR" {
		t.Fatalf("%s of bucket %v: got %v (%v), want an amount in EUR", field, bucket["bucket"], a, err)
	}
	return agedSum{int64(bucket["item_count"].(float64)), cents}
}

// TestAgingPages: the customers come in cursor pages by customer id, the
// totals on every page those of all of them; a token is good only for
// the request it was given for.
func TestAgingPages(t *testing.T) {
	e, other := agingScene(t)
	first := e.aging(t, "key_date=2026-05-15&page_size=1")
	token, _ := first["next_page_token"].(string)
	if rows := items(t, first); len(rows) != 1 || rows[0]["customer_id"] != e.customer.String() || token == "" {
		t.Fatalf("first page: got %v with token %q, want Northwind and a token", rows, token)
	}
	second := e.aging(t, "key_date=2026-05-15&page_size=1&page_token="+token)
	if rows := items(t, second); len(rows) != 1 || rows[0]["customer_id"] != other.String() {
		t.Errorf("second page: got %v, want Contoso", rows)
	}
	if _, more := second["next_page_token"]; more {
		t.Errorf("second page: got a token %v, want none on the last page", second["next_page_token"])
	}
	for name, page := range map[string]map[string]any{"first": first, "second": second} {
		totals, _ := page["totals"].(map[string]any)
		fields(t, "the totals on the "+name+" page", totals, map[string]any{"customer_count": 2, "item_count": 5, "amount": eur("315.00")})
	}
	path := "/v1/receivables-aging?legal_entity_id=" + e.DE.String()
	for name, query := range map[string]string{
		"another key date":  "&key_date=2026-05-16&page_size=1&page_token=" + token,
		"other buckets":     "&key_date=2026-05-15&aging_days=30&page_size=1&page_token=" + token,
		"a customer filter": "&key_date=2026-05-15&customer_id=" + other.String() + "&page_token=" + token,
		"a token made up":   "&key_date=2026-05-15&page_token=nonsense",
	} {
		t.Run("a token used with "+name, func(t *testing.T) {
			e.Problem(400, "request.invalid_page_token", "GET", path+query, "", "")
		})
	}
}

// TestAgingRefusals: a key date outside the years 1900 to 2999, days that
// do not ascend, another company and a caller without the permission are
// refused, each with its code (a page size out of range in
// TestDatabaseFailures, through the port: the contract refuses it first).
func TestAgingRefusals(t *testing.T) {
	e := setup(t)
	path := "/v1/receivables-aging?legal_entity_id=" + e.DE.String()
	for _, day := range []string{"1899-12-31", "3000-01-01"} {
		t.Run("key date "+day, func(t *testing.T) {
			if p := e.Problem(422, "receivables.key_date_out_of_range", "GET", path+"&key_date="+day, "", ""); field(p) != "key_date" {
				t.Errorf("the field refused: got %v, want key_date", field(p))
			}
		})
	}
	if p := e.Problem(400, "request.invalid_parameter", "GET", path+"&key_date=2026-05-15&aging_days=60,30", "", ""); field(p) != "aging_days" {
		t.Errorf("days that do not ascend: field %v, want aging_days", field(p))
	}
	e.Problem(400, "request.invalid_parameter", "GET", path, "", "")
	elsewhere := e.As(e.Reader(t, "us-only", e.US))
	if p := elsewhere.Problem(422, "request.reference_not_found", "GET", path+"&key_date=2026-05-15", "", ""); field(p) != "legal_entity_id" {
		t.Errorf("another company: field %v, want legal_entity_id", field(p))
	}
	fixture.New(t).Problem(403, "identity.permission_missing", "GET", path+"&key_date=2026-05-15", "", "")
}

// TestAgingFarDates: an item's bucket is exact however far its due date
// lies from the key date. The days overdue are counted through a
// time.Duration, which holds about 292 years; an item due in 1900 is in
// the last bucket on a key date in 2999, where one due in 2990 is in the
// bucket of its days, and neither is due in 1900.
func TestAgingFarDates(t *testing.T) {
	e := setup(t)
	for day, baseline := range map[string]string{"2026-03-10": "1900-01-01", "2026-03-11": "2990-01-01"} {
		e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
			return postings().Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date(t, day), PostingDate: date(t, day),
				Currency: "EUR", SourceContext: "receivables", SourceDocumentID: uuid.New(),
				Terms: &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: date(t, baseline), NetDays: 30},
				Lines: []ledger.PostingLine{{AccountID: e.receivable, Amount: 1000, PartnerID: &e.customer}, {AccountID: e.revenue, Amount: -1000}}})
		})
	}
	for key, want := range map[string]string{
		"2999-12-31": "not_due 0 0.00 EUR; overdue[1-3650] 1 10.00 EUR; overdue[3651-<nil>] 1 10.00 EUR; no_due_date 0 0.00 EUR; ",
		"1900-01-01": "not_due 2 20.00 EUR; overdue[1-3650] 0 0.00 EUR; overdue[3651-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; ",
	} {
		t.Run(key, func(t *testing.T) {
			totals, _ := e.aging(t, "aging_days=3650&key_date="+key)["totals"].(map[string]any)
			if got := buckets(t, totals); got != want {
				t.Errorf("the totals' buckets: got %s, want %s", got, want)
			}
		})
	}
}

// hugeItems is a ledger whose two customer items do not add up in range.
type hugeItems struct{}

func (hugeItems) Receivable(context.Context, uuid.UUID, []uuid.UUID) ([]ledger.ReceivableItem, error) {
	return []ledger.ReceivableItem{{AmountLC: math.MaxInt64}, {AmountLC: 1}}, nil
}

// TestAgingThroughThePort: amounts that do not add up in range are
// refused rather than wrapped around.
func TestAgingThroughThePort(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	h.OpenItems = hugeItems{}
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.GetReceivablesAging(ctx, gen.GetReceivablesAgingParams{LegalEntityID: e.DE, KeyDate: date(t, "2026-05-15")})
		return err
	})
	if p := problem.From(err); p.Status != 422 || p.Code != "receivables.amount_too_large" {
		t.Errorf("got %v, want 422 receivables.amount_too_large", p)
	}
}
