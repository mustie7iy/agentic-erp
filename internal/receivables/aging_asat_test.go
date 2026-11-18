package receivables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// asAtScene is agingScene with what happened to the items since: Northwind
// pays its invoice of 50.00 on 21 May; a debit of 25.00 posted on 1 April
// is reversed on 10 April; Contoso's invoice of 60.00 posted on 2 April is
// paid on 6 April and the payment is reset (T75i), so the invoice is open
// again.
func asAtScene(t *testing.T) (env, uuid.UUID) {
	t.Helper()
	e, other := agingScene(t)
	fifty := e.openItemOn(t, e.customer, "2026-05-01") // the scene's invoice of 50.00
	receipt := e.receipt(t, 5000, "2026-05-20")
	e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-05-21", fifty), "")
	debit := e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date(t, "2026-04-01"), PostingDate: date(t, "2026-04-01"),
			Currency: "EUR", SourceContext: "ledger", Lines: []ledger.PostingLine{
				{AccountID: e.receivable, Amount: 2500, PartnerID: &e.customer}, {AccountID: e.revenue, Amount: -2500}}})
	})
	on := date(t, "2026-04-10")
	e.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Reverse(ctx, debit.ID, &on, nil) })
	sixty := e.invoiced(t, other, "60.00", "2026-04-02")
	paid := e.receipt(t, 6000, "2026-04-05")
	got, tag := e.Must(201, "POST", "/v1/incoming-payments", e.payment(other, paid.ID, "2026-04-06", sixty), "")
	e.Must(200, "POST", "/v1/incoming-payments/"+got["id"].(string)+":reset", because, tag)
	return e, other
}

// openItemOn is the customer's open item posted on the day, as a payment
// names it.
func (e env) openItemOn(t *testing.T, customer uuid.UUID, day string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+customer.String(), "", "")
	for _, it := range items(t, page) {
		if it["posting_date"] == day {
			return itemRef(it["journal_entry_id"], 1)
		}
	}
	t.Fatalf("no open item of %s posted on %s", customer, day)
	return ""
}

// TestAgingAsAt (T75l acceptance): with open_on key_date the report ages
// the items open on the key date: posted on or before it and neither
// cleared nor reversed by then. An item posted after the key date is left
// out; one cleared or paid after it is aged as open, one cleared on or
// before it is not; one of an entry reversed after it is open; a payment
// reset is no clearing on any day; on a key date after the last event
// the items are those open now; with open_on now the report is T75e's.
func TestAgingAsAt(t *testing.T) {
	e, _ := asAtScene(t)
	for _, c := range []struct {
		name, key string
		count     int
		amount    string
	}{
		{"before anything is posted", "2026-03-01", 0, "0.00"},
		{"Contoso's invoice of 40.00 open before its memo is posted", "2026-03-13", 2, "140.00"},
		{"its memo posted, not yet cleared against it", "2026-03-14", 3, "100.00"},
		{"both cleared on 15 March, Northwind's credit of 5.00 posted", "2026-03-15", 2, "95.00"},
		{"the debit of 25.00 open before its reversal, Contoso's 60.00 before its payment", "2026-04-05", 5, "150.00"},
		{"the debit reversed on 10 April; the payment of the 60.00 reset is no clearing", "2026-04-10", 4, "125.00"},
		{"Northwind's invoice of 50.00 open the day before it is paid", "2026-05-20", 5, "175.00"},
		{"paid on 21 May", "2026-05-21", 4, "125.00"},
		{"after the last posting, the items open now", "2026-12-31", 5, "325.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := e.aging(t, "open_on=key_date&key_date="+c.key)
			fields(t, "the report", got, map[string]any{"key_date": c.key, "open_on": "key_date", "aging_basis": "net_due_date"})
			totals, _ := got["totals"].(map[string]any)
			fields(t, "the totals", totals, map[string]any{"item_count": c.count, "amount": eur(c.amount)})
		})
	}
	t.Run("open now, whatever the key date", func(t *testing.T) {
		for _, key := range []string{"2026-03-01", "2026-12-31"} {
			got := e.aging(t, "key_date="+key)
			fields(t, "the report on "+key, got, map[string]any{"open_on": "now", "aging_basis": "net_due_date"})
			totals, _ := got["totals"].(map[string]any)
			fields(t, "the totals on "+key, totals, map[string]any{"item_count": 5, "amount": eur("325.00")})
		}
	})
	t.Run("the rows add up to the totals", func(t *testing.T) {
		got := e.aging(t, "open_on=key_date&key_date=2026-04-05")
		var sum money.Minor
		var count int64
		for _, row := range items(t, got) {
			s := sumOf(t, row, "amount")
			sum, count = sum+s.cents, count+int64(row["item_count"].(float64))
		}
		totals, _ := got["totals"].(map[string]any)
		if total := sumOf(t, totals, "amount"); sum != total.cents || count != int64(totals["item_count"].(float64)) {
			t.Errorf("the customers' rows: got %d over %d items, want the totals' %d over %v", sum, count, total.cents, totals["item_count"])
		}
	})
}

// TestAgingByDocumentDate (T75l acceptance): with aging_basis
// document_date each item falls in the bucket of its days past its
// document date, its terms aside; one dated on the key date or later is
// not due.
func TestAgingByDocumentDate(t *testing.T) {
	e, _ := asAtScene(t)
	for _, c := range []struct{ name, query, want string }{
		// Now: the invoice of 100.00 dated 10 March is 66 days old on 15 May,
		// the credit of 5.00 61, its memo of 30.00 56, Contoso's 60.00 43;
		// Contoso's 200.00 dated 1 June is not due.
		{"the items open now", "key_date=2026-05-15&aging_basis=document_date",
			"not_due 1 200.00 EUR; overdue[1-30] 0 0.00 EUR; overdue[31-60] 2 30.00 EUR; overdue[61-90] 2 95.00 EUR; overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
		// On 20 May, as at that day: the invoice of 50.00 dated 1 May is
		// open and 19 days old, the memo 61.
		{"the items open on the key date", "key_date=2026-05-20&aging_basis=document_date&open_on=key_date",
			"not_due 0 0.00 EUR; overdue[1-30] 1 50.00 EUR; overdue[31-60] 1 60.00 EUR; overdue[61-90] 3 65.00 EUR; overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := e.aging(t, c.query)
			if got["aging_basis"] != "document_date" {
				t.Errorf("aging_basis: got %v, want document_date", got["aging_basis"])
			}
			totals, _ := got["totals"].(map[string]any)
			if b := buckets(t, totals); b != c.want {
				t.Errorf("the totals' buckets: got %s, want %s", b, c.want)
			}
		})
	}
	t.Run("an item dated on the key date is not due", func(t *testing.T) {
		got := e.aging(t, "key_date=2026-06-01&aging_basis=document_date&aging_days=1")
		totals, _ := got["totals"].(map[string]any)
		if b, want := buckets(t, totals), "not_due 1 200.00 EUR; overdue[1-1] 0 0.00 EUR; overdue[2-<nil>] 4 125.00 EUR; no_due_date 0 0.00 EUR; "; b != want {
			t.Errorf("the totals' buckets: got %s, want %s", b, want)
		}
	})
}

// TestAgingVariantRefusals: an open_on or aging_basis the contract does
// not list is refused naming the field, and a page token is good only
// with the open_on and the basis it was given for.
func TestAgingVariantRefusals(t *testing.T) {
	e, _ := agingScene(t)
	path := "/v1/receivables-aging?legal_entity_id=" + e.DE.String() + "&key_date=2026-05-15"
	for param, value := range map[string]string{"open_on": "yesterday", "aging_basis": "posting_date"} {
		t.Run(param, func(t *testing.T) {
			if p := e.Problem(400, "request.invalid_parameter", "GET", path+"&"+param+"="+value, "", ""); field(p) != param {
				t.Errorf("the field refused: got %v, want %s", field(p), param)
			}
		})
	}
	for name, query := range map[string][2]string{
		"open_on":     {"&open_on=key_date", ""},
		"aging_basis": {"&aging_basis=document_date", "&aging_basis=net_due_date"},
	} {
		t.Run("a token given for another "+name, func(t *testing.T) {
			// On 30 June both customers have items open, whichever open_on.
			path := strings.Replace(path, "2026-05-15", "2026-06-30", 1)
			first, _ := e.Must(200, "GET", path+"&page_size=1"+query[0], "", "")
			token, _ := first["next_page_token"].(string)
			if token == "" {
				t.Fatalf("the first page: got no token, want one")
			}
			e.Problem(400, "request.invalid_page_token", "GET", fmt.Sprintf("%s&page_size=1%s&page_token=%s", path, query[1], token), "", "")
		})
	}
}
