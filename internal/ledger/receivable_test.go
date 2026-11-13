package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// customerPosting is a customer document of company 2000 in EUR as
// receivables posts it: the amount (debit positive) on the receivables
// account 120000 with the customer against revenue, with the terms given.
func customerPosting(t *testing.T, e env, customer uuid.UUID, amount int64, day string, terms *ledger.ItemTerms) ledger.Posting {
	t.Helper()
	ref := "CI-" + day
	return ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day), Currency: "EUR",
		Reference: &ref, SourceContext: "receivables", SourceDocumentID: uuid.New(), Terms: terms,
		Lines: []ledger.PostingLine{{AccountID: settleAccount(t, e, "2000", "120000"), Amount: money.Minor(amount), PartnerID: &customer},
			{AccountID: settleAccount(t, e, "2000", "400000"), Amount: money.Minor(-amount)}}}
}

// receivable reads the open customer items of company 2000.
func receivable(t *testing.T, e env, customers ...uuid.UUID) []ledger.ReceivableItem {
	t.Helper()
	var items []ledger.ReceivableItem
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		items, err = ledger.OpenItems{}.Receivable(ctx, e.de, customers)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

// shown is the items as the tests compare them: customer, amount, due
// date, in the reader's order.
func shown(items []ledger.ReceivableItem, names map[uuid.UUID]string) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprintf("%s %d %s", names[it.PartnerID], it.Amount, it.NetDueDate().Format("2006-01-02"))
	}
	return strings.Join(out, "; ")
}

// TestReceivableItems (T75b): the terms of a posting are kept with its
// customer open item as with a supplier's, and the reader of customer open
// items returns each open line on a customer reconciliation account with
// its customer, amounts, terms and due date (without terms its document
// date, PO 2026-11-12), by customer and item: of
// every customer or of those named; cleared items, a reversed entry's and
// its reversal's, and supplier items are left out.
func TestReceivableItems(t *testing.T) {
	e := setup(t)
	if err := apply(t, e, withReceivables(t)); err != nil {
		t.Fatal(err)
	}
	north, south := e.newCustomer(t, "BP00000021", true), e.newCustomer(t, "BP00000022", true)
	names := map[uuid.UUID]string{north: "north", south: "south"}
	nt30 := &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: date("2026-03-01"), NetDays: 30}
	var invoice, reversed, paid, credit ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		post := func(to *ledger.Entry, in ledger.Posting) {
			if err == nil {
				*to, err = postings().Post(ctx, in)
			}
		}
		post(&invoice, customerPosting(t, e, north, 10000, "2026-03-01", nt30))
		post(new(ledger.Entry), customerPosting(t, e, north, -2000, "2026-03-05", nil)) // a credit without terms
		post(new(ledger.Entry), customerPosting(t, e, south, 7000, "2026-03-02", nt30))
		post(&reversed, customerPosting(t, e, south, 900, "2026-03-03", nt30))
		post(&paid, customerPosting(t, e, south, 500, "2026-03-04", nt30))
		post(&credit, customerPosting(t, e, south, -500, "2026-03-04", nil))
		post(new(ledger.Entry), termsPosting(t, e, nt30, &e.supplier)) // a supplier's item
		if err == nil {
			_, err = postings().Reverse(ctx, reversed.ID, nil, nil)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{map[string]any{"fiscal_year": 2026, "id": paid.ID.String()}, 1},
		ref{map[string]any{"fiscal_year": 2026, "id": credit.ID.String()}, 1}), "")

	all := receivable(t, e)
	if got, want := shown(all, names), "north 10000 2026-03-31; north -2000 2026-03-05; south 7000 2026-03-31"; got != want {
		t.Fatalf("every customer's open items: got %s, want %s", got, want)
	}
	first := all[0]
	if first.EntryID != invoice.ID || first.LineNo != 1 || first.FiscalYear != 2026 || first.Number != invoice.DocumentNumber ||
		first.Reference == nil || *first.Reference != "CI-2026-03-01" {
		t.Errorf("the invoice's item: got entry %s line %d of %d, number %s, reference %v; want %s line 1 of 2026, number %s, reference CI-2026-03-01",
			first.EntryID, first.LineNo, first.FiscalYear, first.Number, first.Reference, invoice.ID, invoice.DocumentNumber)
	}
	if first.AccountID.String() != e.account(t, "2000", "120000") || first.Currency != "EUR" || first.Exponent != 2 || first.LocalCurrency != "EUR" ||
		first.LocalExponent != 2 || first.AmountLC != 10000 || first.SpecialGL != nil {
		t.Errorf("the invoice's item: got account %s, %d %s (%d digits), %d %s (%d digits) local, special G/L %v; want account 120000, 10000 EUR in both, no indicator",
			first.AccountID, first.Amount, first.Currency, first.Exponent, first.AmountLC, first.LocalCurrency, first.LocalExponent, first.SpecialGL)
	}
	if first.SourceContext != "receivables" || first.SourceDocumentID == nil || first.DocumentDate != date("2026-03-01") || first.PostingDate != date("2026-03-01") {
		t.Errorf("the invoice's item: got source %s %v, dated %s, posted %s; want receivables' document, 2026-03-01 twice",
			first.SourceContext, first.SourceDocumentID, first.DocumentDate, first.PostingDate)
	}
	if first.Terms == nil || first.Terms.PaymentTermCode != "NT30" {
		t.Errorf("the invoice's item's terms: got %+v, want NT30, kept with a customer item as with a supplier's", first.Terms)
	}
	if got, want := shown(receivable(t, e, south), names), "south 7000 2026-03-31"; got != want {
		t.Errorf("one customer's open items: got %s, want %s", got, want)
	}
	if got, want := shown(receivable(t, e, south, north), names), shown(all, names); got != want {
		t.Errorf("both customers named: got %s, want %s", got, want)
	}
	if got := receivable(t, e, e.supplier); len(got) != 0 {
		t.Errorf("a supplier's customer items: got %d, want none", len(got))
	}

	for _, c := range []struct {
		name string
		item ledger.ReceivableItem
		on   string
		days int
	}{
		{"before the due date", first, "2026-03-30", 0},
		{"on the due date", first, "2026-03-31", 0},
		{"a day after", first, "2026-04-01", 1},
		{"45 days after", first, "2026-05-15", 45},
		{"a day after, late in the evening east of UTC", first, "2026-04-01T23:30:00+04:00", 1},
		{"an item without terms, before its document date", all[1], "2026-03-04", 0},
		{"an item without terms, on its document date", all[1], "2026-03-05", 0},
		{"an item without terms, the day after its document date", all[1], "2026-03-06", 1},
		{"an item without terms, 71 days after its document date", all[1], "2026-05-15", 71},
	} {
		t.Run(c.name, func(t *testing.T) {
			on, err := time.Parse(time.RFC3339, c.on)
			if err != nil {
				on = date(c.on)
			}
			if days := c.item.DaysOverdue(on); days != c.days {
				t.Errorf("days overdue on %s: got %d, want %d", c.on, days, c.days)
			}
		})
	}

	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+north.String()+"&due_date_to=2026-12-31", "", "")
	listed, _ := page["items"].([]any)
	if len(listed) != 1 {
		t.Fatalf("the customer's open items due by a date: got %d, want the invoice", len(listed))
	}
	if terms := fmt.Sprint(listed[0].(map[string]any)["payment_terms"]); terms != "map[baseline_date:2026-03-01 code:NT30 discounts:[] net_due_date:2026-03-31]" {
		t.Errorf("the listed customer item's terms: got %s, want NT30 due 2026-03-31", terms)
	}

	if _, err := (ledger.OpenItems{}).Receivable(context.Background(), e.de, nil); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "receivable items", func(ctx context.Context) error {
		_, err := ledger.OpenItems{}.Receivable(ctx, e.de, nil)
		return err
	})
}
