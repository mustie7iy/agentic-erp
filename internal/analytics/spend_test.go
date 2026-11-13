package analytics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// spendPages are validPages with posted supplier documents: an invoice
// with an item on an order made on a contract, one on an account and a
// cost center of its own and one on an item its order no longer lists, a
// credit memo, and a subsequent credit on an
// order made on no contract.
func spendPages() map[string][]string {
	usd := func(v string) map[string]any { return map[string]any{"amount": v, "currency": "USD"} }
	pages := validPages()
	pages["/v1/supplier-invoices"] = []string{items(
		map[string]any{"id": "i1", "supplier_id": "p1", "posting_date": "2026-03-06", "kind": "invoice", "items": []any{
			map[string]any{"purchase_order_id": "o1", "po_item_no": 10, "amount": usd("100.00")},
			map[string]any{"amount": usd("40.00"), "account_id": "a2", "cost_center_id": "c1"},
			// On an item the order no longer lists: still the order's contract.
			map[string]any{"purchase_order_id": "o1", "po_item_no": 20, "amount": usd("8.00")}}},
		map[string]any{"id": "i2", "supplier_id": "p1", "posting_date": "2026-03-07", "kind": "credit_memo", "items": []any{
			map[string]any{"amount": usd("15.00"), "account_id": "a2"}}},
		map[string]any{"id": "i3", "supplier_id": "p2", "posting_date": "2026-04-01", "kind": "subsequent_credit", "items": []any{
			map[string]any{"purchase_order_id": "o2", "po_item_no": 10, "amount": usd("5.00")}}})}
	pages["/v1/purchase-orders"] = []string{items(
		map[string]any{"id": "o1", "contract_id": "k1", "items": []any{map[string]any{"item_no": 10, "account_id": "a2", "cost_center_id": "c2"}}},
		map[string]any{"id": "o2", "items": []any{map[string]any{"item_no": 10, "account_id": "a1"}}})}
	pages["/v1/contracts"] = []string{items(map[string]any{"id": "k1", "document_number": "CT-1"})}
	pages["/v1/cost-centers"] = []string{items(map[string]any{"id": "c1", "code": "1000"}, map[string]any{"id": "c2", "code": "2000"})}
	return pages
}

// TestSourceReadsSpendLines: a posted supplier document's items are facts
// of spend_lines dated its posting date, a credit memo's and a subsequent
// credit's negative; an item on an order takes the order item's account
// and cost center and the order's contract, and is under contract when
// the order names one; the invoices are asked for posted alone (T91b).
func TestSourceReadsSpendLines(t *testing.T) {
	m, _ := DefaultModel()
	asked := map[string]string{}
	api := fakeAPI(t, spendPages())
	api.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked[r.URL.Path] = r.URL.Query().Get("status") + " of " + r.URL.Query().Get("legal_entity_id")
		return http.DefaultTransport.RoundTrip(r)
	})}
	f, err := Source{API: api, Datasets: []string{"spend_lines"}}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/v1/supplier-invoices": "posted of le", "/v1/purchase-orders": " of le", "/v1/contracts": " of le",
		"/v1/cost-centers": " of le", "/v1/accounts": " of le"} {
		if asked[path] != want {
			t.Errorf("%s: asked for %q, want %q", path, asked[path], want)
		}
	}
	for _, path := range []string{"/v1/journal-entries", "/v1/sales-orders"} {
		if _, read := asked[path]; read {
			t.Errorf("%s: read, want only the spend's reads", path)
		}
	}
	var got []string
	for _, l := range f.Tables["spend_lines"] {
		d := l.Dims
		got = append(got, strings.Join([]string{l.ID, d["company"], d["posting_date"], d["vendor"], d["vendor_name"], d["account"], d["account_name"],
			"cc=" + d["cost_center"], "contract=" + d["contract"], d["on_contract"], d["currency"], l.Amount.Plain(2)}, " "))
	}
	want := []string{
		"i1 1000 2026-03-06 BP1 Acme 520000 Office cc=2000 contract=CT-1 true USD 100.00",
		"i1 1000 2026-03-06 BP1 Acme 520000 Office cc=1000 contract= false USD 40.00",
		"i1 1000 2026-03-06 BP1 Acme   cc= contract=CT-1 true USD 8.00",
		"i2 1000 2026-03-07 BP1 Acme 520000 Office cc= contract= false USD -15.00",
		"i3 1000 2026-04-01 BP2 Beta 211000 AP cc= contract= false USD -5.00",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("spend lines:\n got %q\nwant %q", got, want)
	}
	for metric, want := range map[string]string{"spend": "128.00", "spend_under_contract": "108.00", "spend_off_contract": "20.00", "off_contract_share": "0.1563"} {
		res, err := Execute(m, f, Principal{}, Query{Measures: []string{metric}})
		if err != nil {
			t.Errorf("%s: got %v, want an answer", metric, err)
			continue
		}
		if got := fmt.Sprint(res.Rows); got != "[["+want+"]]" {
			t.Errorf("%s: got %s, want [[%s]]", metric, got, want)
		}
	}
	// A supplier with no spend under contract has a share off contract of 1.
	res, err := Execute(m, f, Principal{}, Query{Measures: []string{"off_contract_share", "spend_under_contract"}, By: []string{"vendor"}, OrderBy: "vendor"})
	if err != nil {
		t.Fatalf("share by vendor: got %v, want an answer", err)
	}
	if got := fmt.Sprint(res.Rows); got != "[[BP1 0.1880 108.00] [BP2 1.0000 <nil>]]" {
		t.Errorf("share by vendor: got %s, want BP1 0.1880 of 133.00 and BP2 1.0000", got)
	}
}

// TestSourceSpendErrors: a read the spend lines need that the API refuses,
// an amount or a posting date that does not read, refuses the facts.
func TestSourceSpendErrors(t *testing.T) {
	m, _ := DefaultModel()
	for _, path := range []string{"/v1/accounts", "/v1/supplier-invoices", "/v1/purchase-orders", "/v1/contracts", "/v1/cost-centers"} {
		t.Run("without "+path, func(t *testing.T) {
			pages := spendPages()
			delete(pages, path)
			_, err := (Source{API: fakeAPI(t, pages), Datasets: []string{"spend_lines"}}).Facts(context.Background(), m, time.Now())
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("got %v, want the read of %s refused", err, path)
			}
		})
	}
	for name, c := range map[string]struct{ old, want string }{
		"amount":       {"40.00", "invoice i1: "},
		"posting date": {"2026-03-07", "invoice i2: "},
	} {
		t.Run(name, func(t *testing.T) {
			pages := spendPages()
			pages["/v1/supplier-invoices"][0] = strings.Replace(pages["/v1/supplier-invoices"][0], c.old, `x`, 1)
			_, err := (Source{API: fakeAPI(t, pages), Datasets: []string{"spend_lines"}}).Facts(context.Background(), m, time.Now())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %s", err, c.want)
			}
		})
	}
}
