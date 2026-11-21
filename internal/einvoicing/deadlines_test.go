package einvoicing_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTransmissionDeadlines: the worklist lists the UAE company's posted
// invoices and credit memos whose e-invoice is not transmitted, each with
// its deadline (the 14th business day after its issue date, the company's
// weekend and holiday left out) and the business days left on the key
// date, by deadline; one completed is not listed; one whose deadline is
// before the key date is overdue, also on a key date no business day
// follows the deadline by (0 business days left); the totals are every
// row's; it pages; a period and a company it cannot list are refused.
func TestTransmissionDeadlines(t *testing.T) {
	s := senderSetup(t)
	sent := s.invoice["id"].(string)
	op := s.send(t, sent, s.connector)
	s.advance(t, sent, op)
	unsent := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))
	memo := s.posted(t, s.customer, fmt.Sprintf(`"kind": "credit_memo", "reference_invoice_id": %q, `, s.invoice["customer_invoice_id"]), "AED",
		s.item("1.00", "AED", "Z0"))
	path := "/v1/e-invoice-transmission-deadlines?legal_entity_id=" + s.le.String() + "&key_date="
	rows := func(page map[string]any) string {
		var out []string
		for _, it := range page["items"].([]any) {
			r := it.(map[string]any)
			out = append(out, words(r["customer_invoice_id"], r["kind"], r["deadline"], r["overdue"], r["business_days_left"], r["einvoice_status"],
				r["transmission_status"], r["customer_number"]))
		}
		return strings.Join(out, ", ")
	}
	got, _ := s.f.Must(200, "GET", path+"2026-03-31", "", "")
	check(t, "the period", words(got["company_code"], got["key_date"], got["issued_from"], got["count"], got["overdue_count"]), "5000 2026-03-31 2025-12-31 3 0")
	want := []string{words(s.invoice["customer_invoice_id"], "invoice 2026-03-31 false 0 valid submitted BP80000001")}
	for _, id := range []string{unsent, memo} {
		kind := "invoice"
		if id == memo {
			kind = "credit_memo"
		}
		want = append(want, words(id, kind, "2026-03-31 false 0 <nil> <nil> BP80000001"))
	}
	check(t, "the rows", len(strings.Split(rows(got), ", ")), 3)
	for _, w := range want {
		if !strings.Contains(rows(got), w) {
			t.Errorf("the rows: got %s, want among them %s", rows(got), w)
		}
	}
	s.later(30 * time.Second)
	s.advance(t, sent, op) // COMPLETED
	holiday, _ := s.f.Must(200, "GET", path+"2026-04-01", "", "")
	check(t, "the holiday after the deadline", words(holiday["count"], holiday["overdue_count"]), "2 2")
	check(t, "overdue, no business day past it", strings.Count(rows(holiday), "true 0"), 2)
	late, _ := s.f.Must(200, "GET", path+"2026-04-02", "", "")
	check(t, "two days after the deadline", words(late["count"], late["overdue_count"]), "2 2")
	check(t, "overdue by one business day", strings.Count(rows(late), "true -1"), 2)
	early, _ := s.f.Must(200, "GET", path+"2026-03-19&page_size=1", "", "")
	check(t, "a page of one", words(len(early["items"].([]any)), early["count"], early["next_page_token"] != nil), "1 2 true")
	check(t, "seven business days left (the holiday left out)", strings.Contains(rows(early), "false 7"), true)
	next, _ := s.f.Must(200, "GET", path+"2026-03-19&page_size=1&page_token="+early["next_page_token"].(string), "", "")
	check(t, "the next page", words(len(next["items"].([]any)), next["next_page_token"] == nil, rows(next) != rows(early)), "1 true true")
	for _, c := range []struct{ name, query, code string }{
		{"issued after the key date", path + "2026-03-31&issued_from=2026-04-01", "einvoicing.invalid_period"},
		{"issued more than a year before", path + "2026-03-31&issued_from=2025-03-29", "einvoicing.invalid_period"},
		{"not a UAE company", "/v1/e-invoice-transmission-deadlines?key_date=2026-03-31&legal_entity_id=" + s.f.DE.String(), "einvoicing.not_a_uae_company"},
		{"no company", "/v1/e-invoice-transmission-deadlines?key_date=2026-03-31&legal_entity_id=" + uuid.NewString(), "request.reference_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) { s.f.Problem(422, c.code, "GET", c.query, "", "") })
	}
	none, _ := s.f.Must(200, "GET", path+"2026-03-31&issued_from=2026-03-11", "", "")
	check(t, "none issued in the period", words(none["count"], len(none["items"].([]any))), "0 0")
}
