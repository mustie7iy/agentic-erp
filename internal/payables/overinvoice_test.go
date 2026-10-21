package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// postIn posts a matched document in the request's transaction, on
// 2026-03-21, as :post does.
func postIn(v map[string]any) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		body := gen.NewOptSupplierInvoicePost(gen.SupplierInvoicePost{PostingDate: gen.NewOptDate(time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC))})
		_, err := endpoints(nil).PostSupplierInvoice(ctx, body, gen.PostSupplierInvoiceParams{ID: uuid.MustParse(id(v)), IfMatch: "*"})
		return err
	}
}

// matched enters a document and matches it clean.
func (e env) matched(t *testing.T, body string) map[string]any {
	t.Helper()
	v, _ := e.Must(201, "POST", "/v1/supplier-invoices", body, "")
	if m := e.act(t, 200, id(v), "match", ""); m["is_clean"] != true {
		t.Fatalf("match of %v: got issues %v, want clean", v["supplier_reference"], m["issues"])
	}
	return v
}

// issueCodes are the codes of a match's issues, in order.
func issueCodes(m map[string]any) string {
	list, _ := m["issues"].([]any)
	codes := make([]string, len(list))
	for i, is := range list {
		codes[i], _ = is.(map[string]any)["code"].(string)
	}
	return strings.Join(codes, " ")
}

// wantParked checks that a refused document is left as it was: parked,
// still on its clean match.
func (e env) wantParked(t *testing.T, v map[string]any) {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(v), "", "")
	if got["status"] != "parked" {
		t.Errorf("%v after the refused post: got %v, want parked", v["supplier_reference"], got["status"])
	}
}

// TestPostRechecksTheMatch: two documents on one order item, each matched
// clean before either posts, together go past what the item allows; the
// second to post is refused (409 payables.match_outdated) and left parked,
// so the three-way match cannot be bypassed by posting matches that were
// clean only apart. An invoice is held to the received quantity, a credit
// memo to the invoiced one, a subsequent debit to something invoiced, and
// every document to the price tolerance as the order now prices it.
func TestPostRechecksTheMatch(t *testing.T) {
	e := setup(t)
	inv := func(po uuid.UUID, ref, qty, net, tax, gross string) string {
		return e.invoice(e.Supplier, ref, gross, tax, poItem(po, 1, qty, "EA", net))
	}
	t.Run("invoices past the received quantity", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q1")
		e.receive(t, po, 1, "10")
		a, b := e.matched(t, inv(po, "Q1-A", "6", "72.00", "13.68", "85.68")), e.matched(t, inv(po, "Q1-B", "6", "72.00", "13.68", "85.68"))
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(a)); err != nil {
			t.Fatalf("first invoice: got %v, want it posted", err)
		}
		pgtest.WantErr(t, apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(b)), "payables.match_outdated")
		e.wantParked(t, b)
		m := e.act(t, 200, id(b), "match", "")
		if got := issueCodes(m); got != "quantity_exceeds_received" {
			t.Errorf("match again: got %v, want quantity_exceeds_received", got)
		}
	})
	t.Run("approved exception past the received quantity", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q7")
		e.receive(t, po, 1, "10")
		// A's price is outside the tolerance, its quantity within: the
		// approvers excuse the price only.
		a, _ := e.Must(201, "POST", "/v1/supplier-invoices", inv(po, "Q7-A", "6", "80.00", "15.20", "95.20"), "")
		if got := issueCodes(e.act(t, 200, id(a), "match", "")); got != "price_variance" {
			t.Fatalf("match of A: got %v, want price_variance", got)
		}
		e.Approve(t, id(a))
		e.posted(t, e.matched(t, inv(po, "Q7-B", "6", "72.00", "13.68", "85.68")))
		pgtest.WantErr(t, apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(a)), "payables.match_outdated")
		e.wantParked(t, a)
	})
	t.Run("approved exception with the issues it was approved on", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q8")
		e.receive(t, po, 1, "4")
		a, _ := e.Must(201, "POST", "/v1/supplier-invoices", inv(po, "Q8-A", "6", "80.00", "15.20", "95.20"), "")
		if got := issueCodes(e.act(t, 200, id(a), "match", "")); got != "price_variance quantity_exceeds_received" {
			t.Fatalf("match of A: got %v, want price_variance quantity_exceeds_received", got)
		}
		e.Approve(t, id(a))
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(a)); err != nil {
			t.Errorf("A on its approval: got %v, want it posted", err)
		}
	})
	t.Run("credit memos past the invoiced quantity", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q2")
		e.receive(t, po, 1, "10")
		e.posted(t, e.matched(t, inv(po, "Q2-I", "10", "120.00", "22.80", "142.80")))
		memo := func(ref string) map[string]any {
			return e.matched(t, e.document("credit_memo", ref, "85.68", "13.68", "", poItem(po, 1, "6", "EA", "72.00")))
		}
		a, b := memo("Q2-A"), memo("Q2-B")
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(a)); err != nil {
			t.Fatalf("first credit memo: got %v, want it posted", err)
		}
		pgtest.WantErr(t, apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(b)), "payables.match_outdated")
		e.wantParked(t, b)
	})
	t.Run("subsequent debit once nothing is invoiced", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q3")
		e.receive(t, po, 1, "10")
		invoice := e.matched(t, inv(po, "Q3-I", "10", "120.00", "22.80", "142.80"))
		e.posted(t, invoice)
		debit := e.matched(t, e.document("subsequent_debit", "Q3-D", "5.00", "0.00", fmt.Sprintf(`, "purchase_order_id": %q`, po), poItem(po, 1, "0", "EA", "5.00")))
		e.act(t, 200, id(invoice), "reverse", `{"reason": "wrong"}`)
		pgtest.WantErr(t, apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(debit)), "payables.match_outdated")
		e.wantParked(t, debit)
	})
	t.Run("price changed on the order", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q4")
		e.receive(t, po, 1, "10")
		a := e.matched(t, inv(po, "Q4-A", "10", "120.00", "22.80", "142.80"))
		// An order change lowering the price, as an applied change writes it.
		pgtest.Unguarded(t, `UPDATE procurement.purchase_order_item SET unit_price = 1000 WHERE purchase_order_id = $1 AND item_no = 1`, po)
		pgtest.WantErr(t, apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(a)), "payables.match_outdated")
		e.wantParked(t, a)
	})
	t.Run("database failures", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q6")
		e.receive(t, po, 1, "10")
		// Every statement of the lock and the match again reports its failure.
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post on a clean match", postIn(e.matched(t, inv(po, "Q6-A", "6", "72.00", "13.68", "85.68"))))
		over, _ := e.Must(201, "POST", "/v1/supplier-invoices", inv(po, "Q6-B", "6", "80.00", "15.20", "95.20"), "")
		e.act(t, 200, id(over), "match", "")
		e.Approve(t, id(over))
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post on an approved exception", postIn(over))
	})
	t.Run("within what the item allows", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-Q5")
		e.receive(t, po, 1, "10")
		a, b := e.matched(t, inv(po, "Q5-A", "4", "48.00", "9.12", "57.12")), e.matched(t, inv(po, "Q5-B", "6", "72.00", "13.68", "85.68"))
		for _, v := range []map[string]any{a, b} {
			if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, postIn(v)); err != nil {
				t.Errorf("%v: got %v, want it posted", v["supplier_reference"], err)
			}
		}
	})
}

// TestConcurrentPostsSerialize posts two documents matched clean on one
// order item in two transactions at once: the second waits for the
// first's lock on the order item, then sees it posted and is refused, so
// only one posts.
func TestConcurrentPostsSerialize(t *testing.T) {
	e := setup(t)
	t.Run("invoices", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-R1")
		e.receive(t, po, 1, "10")
		body := func(ref string) string {
			return e.invoice(e.Supplier, ref, "85.68", "13.68", poItem(po, 1, "6", "EA", "72.00"))
		}
		a, b := e.matched(t, body("R1-A")), e.matched(t, body("R1-B"))
		first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, postIn(a), postIn(b))
		if first != nil {
			t.Fatalf("first invoice: got %v, want it posted", first)
		}
		pgtest.WantErr(t, second, "payables.match_outdated")
		e.wantParked(t, b)
	})
	t.Run("credit memos", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-R2")
		e.receive(t, po, 1, "10")
		e.posted(t, e.matched(t, e.invoice(e.Supplier, "R2-I", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00"))))
		body := func(ref string) string {
			return e.document("credit_memo", ref, "85.68", "13.68", "", poItem(po, 1, "6", "EA", "72.00"))
		}
		a, b := e.matched(t, body("R2-A")), e.matched(t, body("R2-B"))
		first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, postIn(a), postIn(b))
		if first != nil {
			t.Fatalf("first credit memo: got %v, want it posted", first)
		}
		pgtest.WantErr(t, second, "payables.match_outdated")
	})
}
