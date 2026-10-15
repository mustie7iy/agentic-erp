package payables_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestStaleMatchDoesNotPost: a clean match counts for the invoice as it was
// matched (F13). Before, the latest clean match let an invoice post after
// its gross amount and items were edited, bypassing the three-way match and
// the auto-post limit: 157.80 EUR matched clean, edited to 90,037.80 EUR,
// posted without a new match or an exception.
func TestStaleMatchDoesNotPost(t *testing.T) {
	e := setup(t)
	received := func(t *testing.T, number string) uuid.UUID {
		t.Helper()
		po := e.order(t, e.Supplier, number)
		e.receive(t, po, 1, "10")
		e.receive(t, po, 2, "5")
		return po
	}
	path := func(inv map[string]any) string { return "/v1/supplier-invoices/" + id(inv) }

	t.Run("edited amounts", func(t *testing.T) {
		e := e.in(t)
		po := received(t, "PO-1")
		inv := e.clean(t, po, "INV-1")
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != true {
			t.Fatalf("match is_clean: got %v, want true", m["is_clean"])
		}
		e.Must(200, "PATCH", path(inv), `{"gross_amount": {"amount": "90037.80", "currency": "EUR"},
			"items": [`+poItem(po, 1, "10", "EA", "90000.00")+`, `+poItem(po, 2, "5", "KG", "15.00")+`]}`, e.tag(t, id(inv)))
		e.Problem(409, "payables.match_required", "POST", path(inv)+":post", "", e.tag(t, id(inv)))
		// Matching the edited invoice finds the price variance and holds it.
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != false {
			t.Errorf("match of the edited invoice: got is_clean %v, want false", m["is_clean"])
		}
		if got, _ := e.Must(200, "GET", path(inv), "", ""); got["status"] != "on_hold" {
			t.Errorf("status: got %v, want on_hold", got["status"])
		}
	})

	t.Run("matched again", func(t *testing.T) {
		e := e.in(t)
		inv := e.clean(t, received(t, "PO-2"), "INV-2")
		e.act(t, 200, id(inv), "match", "")
		e.Must(200, "PATCH", path(inv), `{"supplier_reference": "INV-2a"}`, e.tag(t, id(inv)))
		e.Problem(409, "payables.match_required", "POST", path(inv)+":post", "", e.tag(t, id(inv)))
		e.act(t, 200, id(inv), "match", "")
		if posted := e.act(t, 200, id(inv), "post", ""); posted["status"] != "posted" {
			t.Errorf("status: got %v, want posted", posted["status"])
		}
	})

	// A hold and release by hand change the invoice after its match.
	t.Run("held and released by hand", func(t *testing.T) {
		e := e.in(t)
		inv := e.clean(t, received(t, "PO-3"), "INV-3")
		e.act(t, 200, id(inv), "match", "")
		e.act(t, 200, id(inv), "hold", `{"reason": "Checking the bank details"}`)
		e.act(t, 200, id(inv), "release", `{"comment": "Checked"}`)
		e.Problem(409, "payables.match_required", "POST", path(inv)+":post", "", e.tag(t, id(inv)))
	})

	// A clean match that releases its own hold records the version it
	// leaves the invoice at, which posts.
	t.Run("released by the match", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-4")
		e.receive(t, po, 1, "10")
		inv := e.clean(t, po, "INV-4")
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != false {
			t.Fatalf("match before the receipt: got is_clean %v, want false", m["is_clean"])
		}
		e.receive(t, po, 2, "5")
		e.act(t, 200, id(inv), "match", "")
		if posted := e.act(t, 200, id(inv), "post", ""); posted["status"] != "posted" {
			t.Errorf("status: got %v, want posted", posted["status"])
		}
	})

	// A clean match without a version, recorded before 000040 or by the
	// previous release during the rollout, counts for no version: the
	// invoice is matched again before it posts.
	t.Run("match without a version", func(t *testing.T) {
		e := e.in(t)
		inv := e.clean(t, received(t, "PO-5"), "INV-5")
		e.act(t, 200, id(inv), "match", "")
		pgtest.Exec(t, `INSERT INTO payables.match_result (tenant_id, id, supplier_invoice_id, evaluated_at, is_clean, issues, config_version, evaluated_by)
			SELECT tenant_id, $2, supplier_invoice_id, clock_timestamp(), true, issues, config_version, evaluated_by
			FROM payables.match_result WHERE supplier_invoice_id = $1`, uuid.MustParse(id(inv)), uuid.Must(uuid.NewV7()))
		e.Problem(409, "payables.match_required", "POST", path(inv)+":post", "", e.tag(t, id(inv)))
		e.act(t, 200, id(inv), "match", "")
		if posted := e.act(t, 200, id(inv), "post", ""); posted["status"] != "posted" {
			t.Errorf("status after matching again: got %v, want posted", posted["status"])
		}
	})
}
