package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// termsPosting is a supplier invoice of 100.00 EUR in company 2000 with the
// terms given for its open item.
func termsPosting(t *testing.T, e env, terms *ledger.ItemTerms, partner *uuid.UUID) ledger.Posting {
	t.Helper()
	ref := "INV-T"
	return ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"), Currency: "EUR",
		Reference: &ref, SourceContext: "payables", SourceDocumentID: uuid.New(), Terms: terms,
		Lines: []ledger.PostingLine{{AccountID: settleAccount(t, e, "2000", "520000"), Amount: 10000},
			{AccountID: settleAccount(t, e, "2000", "211000"), Amount: -10000, PartnerID: partner}}}
}

// TestTermsFrozenOnOpenItems: a posting's payment terms are kept with its
// supplier open item (T36b), which the payable items and the open-items
// listing show with the net due date and each discount's last day; an
// entry without terms has none, and terms for an entry with no supplier
// open item are refused.
func TestTermsFrozenOnOpenItems(t *testing.T) {
	e := setup(t)
	terms := &ledger.ItemTerms{PaymentTermCode: "ZB01", BaselineDate: date("2026-03-01"), NetDays: 45,
		Discounts: []ledger.Discount{{Days: 14, Percent: "3"}, {Days: 30, Percent: "2.5"}}}
	var with, without ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		if with, err = postings().Post(ctx, termsPosting(t, e, terms, &e.supplier)); err != nil {
			return err
		}
		without, err = postings().Post(ctx, termsPosting(t, e, nil, &e.vendor))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var items []ledger.PayableItem
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		items, err = ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-03-01"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]*ledger.ItemTerms{}
	for _, it := range items {
		got[it.EntryID] = it.Terms
	}
	if got[with.ID] == nil {
		t.Fatal("payable item terms: got none, want the posting's")
	}
	if got, want := fmt.Sprintf("%+v", *got[with.ID]), fmt.Sprintf("%+v", *terms); got != want {
		t.Errorf("payable item terms: got %s, want %s", got, want)
	}
	if got[without.ID] != nil {
		t.Errorf("payable item of an entry without terms: got %+v, want none", got[without.ID])
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.supplier.String(), "", "")
	listed := page["items"].([]any)
	if len(listed) != 1 {
		t.Fatalf("open items of the supplier: got %d, want 1", len(listed))
	}
	shown := fmt.Sprint(listed[0].(map[string]any)["payment_terms"])
	if want := "map[baseline_date:2026-03-01 code:ZB01 discounts:[map[percent:3 until:2026-03-15] map[percent:2.5 until:2026-03-31]] net_due_date:2026-04-15]"; shown != want {
		t.Errorf("listed payment terms: got %s, want %s", shown, want)
	}
	page, _ = e.Must(200, "GET", "/v1/open-items?partner_id="+e.vendor.String(), "", "")
	if shown := page["items"].([]any)[0].(map[string]any)["payment_terms"]; shown != nil {
		t.Errorf("listed payment terms of an item without terms: got %v, want none", shown)
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		in := termsPosting(t, e, terms, nil)
		in.Lines[1].AccountID = settleAccount(t, e, "2000", "100000") // a bank line: no supplier open item
		_, err := postings().Post(ctx, in)
		return err
	})
	pgtest.CheckErr(t, err, "no supplier open item")
	three := *terms
	three.Discounts = append(three.Discounts, ledger.Discount{Days: 40, Percent: "1"})
	err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, termsPosting(t, e, &three, &e.supplier))
		return err
	})
	pgtest.CheckErr(t, err, "3 discount tiers")
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post with terms", func(ctx context.Context) error {
		_, err := postings().Post(ctx, termsPosting(t, e, terms, &e.supplier))
		return err
	})
}

// TestOpenItemsAging: open items aged on a key date by their net due
// dates (ADR-0052 G4): one not yet due (due on the key date counts as not
// due), one 30 days past in the first bucket, one past every bucket and
// one without terms apart, each with its number and its amount in local
// currency, on the first page alone; due_date_to keeps the items due by a
// date, leaving out those without terms; buckets without a key date or
// not ascending are refused.
func TestOpenItemsAging(t *testing.T) {
	e := setup(t)
	terms := func(baseline string, days int) *ledger.ItemTerms {
		return &ledger.ItemTerms{PaymentTermCode: "NT", BaselineDate: date(baseline), NetDays: days}
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		for _, p := range []ledger.Posting{termsPosting(t, e, terms("2026-03-01", 45), &e.supplier), termsPosting(t, e, terms("2026-03-01", 0), &e.supplier),
			termsPosting(t, e, terms("2026-01-01", 10), &e.supplier), termsPosting(t, e, nil, &e.vendor)} {
			if _, err := postings().Post(ctx, p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base := "/v1/open-items?legal_entity_id=" + e.de.String()
	for name, c := range map[string]struct {
		query string
		want  string
	}{
		"the default buckets, due on the key date": {"&aging_key_date=2026-04-15", "[map[amount_lc:map[amount:-100.00 currency:EUR] bucket:not_due currency:EUR item_count:1] " +
			"map[amount_lc:map[amount:-100.00 currency:EUR] bucket:overdue currency:EUR days_from:31 days_to:60 item_count:1] " +
			"map[amount_lc:map[amount:-100.00 currency:EUR] bucket:overdue currency:EUR days_from:91 item_count:1] " +
			"map[amount_lc:map[amount:-100.00 currency:EUR] bucket:no_due_date currency:EUR item_count:1]]"},
		"one bucket of 30 days": {"&aging_key_date=2026-03-31&aging_days=30", "[map[amount_lc:map[amount:-100.00 currency:EUR] bucket:not_due currency:EUR item_count:1] " +
			"map[amount_lc:map[amount:-100.00 currency:EUR] bucket:overdue currency:EUR days_from:1 days_to:30 item_count:1] " +
			"map[amount_lc:map[amount:-100.00 currency:EUR] bucket:overdue currency:EUR days_from:31 item_count:1] " +
			"map[amount_lc:map[amount:-100.00 currency:EUR] bucket:no_due_date currency:EUR item_count:1]]"},
		"due by a date": {"&due_date_to=2026-03-01&aging_key_date=2026-03-01&aging_days=5,10", "[map[amount_lc:map[amount:-100.00 currency:EUR] bucket:not_due currency:EUR item_count:1] " +
			"map[amount_lc:map[amount:-100.00 currency:EUR] bucket:overdue currency:EUR days_from:11 item_count:1]]"},
		"no aging asked for": {"", "<nil>"},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", base+c.query, "", "")
			if got := fmt.Sprint(page["aging"]); got != c.want {
				t.Errorf("aging: got %s, want %s", got, c.want)
			}
		})
	}
	due, _ := e.Must(200, "GET", base+"&due_date_to=2026-03-01", "", "")
	if got := len(due["items"].([]any)); got != 2 {
		t.Errorf("items due by 1 March: got %d, want 2", got)
	}
	first, _ := e.Must(200, "GET", base+"&page_size=1&aging_key_date=2026-04-15", "", "")
	if first["aging"] == nil || first["next_page_token"] == nil {
		t.Fatalf("the first page: got %v, want the aging and a next page", first)
	}
	next, _ := e.Must(200, "GET", base+"&page_size=1&aging_key_date=2026-04-15&page_token="+first["next_page_token"].(string), "", "")
	if next["aging"] != nil {
		t.Errorf("the next page: got aging %v, want none", next["aging"])
	}
	for name, query := range map[string]string{
		"buckets without a key date": "&aging_days=30",
		"buckets not ascending":      "&aging_key_date=2026-04-15&aging_days=60,30",
		"a bucket of no days":        "&aging_key_date=2026-04-15&aging_days=0",
	} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(400, "request.invalid_parameter", "GET", base+query, "", "")
		})
	}
}
