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

// TestApprovedExceptionCountsForItsVersion: an approved exception lets the
// invoice post as it was approved, not as it was edited afterwards (F16).
// Before this release an edit kept the link to the approved request, and
// the previous release still does during the rollout: an invoice approved
// at 167.80 EUR, edited to 90,167.80 EUR on that path, posted without a new
// match or approval.
func TestApprovedExceptionCountsForItsVersion(t *testing.T) {
	e := setup(t)
	path := func(inv map[string]any) string { return "/v1/supplier-invoices/" + id(inv) }
	// approved is an invoice whose price variance on item 1 (130.00 for
	// 120.00) was approved as an exception.
	held := func(t *testing.T, e env, number string) map[string]any {
		t.Helper()
		po := e.order(t, e.Supplier, number)
		e.receive(t, po, 1, "10")
		e.receive(t, po, 2, "5")
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-"+number, "167.80", "22.80",
			poItem(po, 1, "10", "EA", "130.00"), poItem(po, 2, "5", "KG", "15.00")), "")
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != false {
			t.Fatalf("match is_clean: got %v, want false", m["is_clean"])
		}
		return inv
	}
	approved := func(t *testing.T, e env, number string) map[string]any {
		t.Helper()
		inv := held(t, e, number)
		e.Approve(t, id(inv))
		return inv
	}
	// oldEdit changes item 1 and the gross amount by delta minor units as
	// the previous release's edit did: the version moves on, the approved
	// request stays linked.
	oldEdit := func(t *testing.T, inv map[string]any, delta int64) {
		t.Helper()
		invoice := uuid.MustParse(id(inv))
		pgtest.Exec(t, `UPDATE payables.supplier_invoice_item SET amount = amount + $2 WHERE supplier_invoice_id = $1 AND item_no = 1`, invoice, delta)
		pgtest.Exec(t, `UPDATE payables.supplier_invoice SET gross_amount = gross_amount + $2, version = version + 1 WHERE id = $1`, invoice, delta)
	}
	posted := func(t *testing.T, e env, inv map[string]any) {
		t.Helper()
		if p := e.act(t, 200, id(inv), "post", ""); p["status"] != "posted" {
			t.Errorf("status: got %v, want posted", p["status"])
		}
	}

	t.Run("approved again", func(t *testing.T) {
		e := e.in(t)
		inv := approved(t, e, "PO-1")
		oldEdit(t, inv, 9000000)
		if got, _ := e.Must(200, "GET", path(inv), "", ""); got["workflow_request_id"] == nil {
			t.Fatalf("workflow request after the old edit: got none, want the approved one")
		}
		e.Problem(409, "payables.match_required", "POST", path(inv)+":post", "", e.tag(t, id(inv)))
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != false {
			t.Fatalf("match of the edited invoice: got is_clean %v, want false", m["is_clean"])
		}
		e.Approve(t, id(inv))
		posted(t, e, inv)
	})

	t.Run("matched again", func(t *testing.T) {
		e := e.in(t)
		inv := approved(t, e, "PO-2")
		oldEdit(t, inv, -1000) // item 1 at the order price: the match is clean
		e.Problem(409, "payables.match_required", "POST", path(inv)+":post", "", e.tag(t, id(inv)))
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != true {
			t.Fatalf("match of the corrected invoice: got is_clean %v, want true", m["is_clean"])
		}
		posted(t, e, inv)
	})

	t.Run("not edited", func(t *testing.T) {
		e := e.in(t)
		posted(t, e, approved(t, e, "PO-3"))
	})

	// Matching an approved, unchanged invoice again finds the issues the
	// approval covers: it leaves the invoice parked on its approval rather
	// than holding it and asking the approvers again, which every run of
	// `erp invoices` did (F18).
	t.Run("matched again unchanged", func(t *testing.T) {
		e := e.in(t)
		inv := approved(t, e, "PO-5")
		before, tag := e.Must(200, "GET", path(inv), "", "")
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != false {
			t.Fatalf("match of the approved invoice: got is_clean %v, want false", m["is_clean"])
		}
		after, afterTag := e.Must(200, "GET", path(inv), "", "")
		if after["status"] != "parked" {
			t.Errorf("status after the match: got %v, want parked", after["status"])
		}
		if after["workflow_request_id"] != before["workflow_request_id"] {
			t.Errorf("workflow request after the match: got %v, want %v", after["workflow_request_id"], before["workflow_request_id"])
		}
		if afterTag != tag {
			t.Errorf("etag after the match: got %s, want %s (unchanged)", afterTag, tag)
		}
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM workflow.approval_request WHERE object_id = $1`, []any{uuid.MustParse(id(inv))}, &n)
		if n != 1 {
			t.Errorf("approval requests: got %d, want 1", n)
		}
		posted(t, e, inv)
	})

	// An exception approved before 000041, or by the previous release during
	// the rollout, has no version and counts for none: the invoice is
	// matched and approved again before it posts.
	t.Run("approval without a version", func(t *testing.T) {
		e := e.in(t)
		inv := held(t, e, "PO-4")
		invoice := uuid.MustParse(id(inv))
		// The previous release's approval: the request completes and the
		// invoice is released with it linked, no version recorded.
		pgtest.Exec(t, `UPDATE workflow.approval_request SET status = 'approved', completed_at = now(), version = version + 1
			WHERE object_id = $1 AND status = 'pending'`, invoice)
		pgtest.Exec(t, `UPDATE payables.supplier_invoice SET status = 'parked', hold_reason = NULL, version = version + 1 WHERE id = $1`, invoice)
		if got, _ := e.Must(200, "GET", path(inv), "", ""); got["workflow_request_id"] == nil || got["status"] != "parked" {
			t.Fatalf("invoice approved by the previous release: got %v, want parked with its request", got)
		}
		e.Problem(409, "payables.match_required", "POST", path(inv)+":post", "", e.tag(t, id(inv)))
		e.act(t, 200, id(inv), "match", "")
		e.Approve(t, id(inv))
		posted(t, e, inv)
	})
}

// TestMatchNamesItsInvoiceVersion: a match result and its
// SupplierInvoiceMatched event name the invoice version the match left the
// invoice at, the one :post accepts it for (T30); a match recorded without
// a version (before 000040) names none.
func TestMatchNamesItsInvoiceVersion(t *testing.T) {
	e := setup(t)
	version := func(t *testing.T, e env, inv string) any {
		t.Helper()
		got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+inv, "", "")
		return got["version"]
	}
	matched := func(t *testing.T, e env, inv string) map[string]any {
		t.Helper()
		for _, ev := range events(t, e.Tenant, "supplier_invoice_matched") {
			if ev["supplier_invoice_id"] == inv {
				return ev
			}
		}
		t.Fatalf("no supplier_invoice_matched event for invoice %s", inv)
		return nil
	}
	t.Run("clean", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-1")
		e.receive(t, po, 1, "10")
		e.receive(t, po, 2, "5")
		inv := id(e.clean(t, po, "INV-1"))
		m := e.act(t, 200, inv, "match", "")
		want := version(t, e, inv)
		if m["invoice_version"] != want {
			t.Errorf("match response invoice_version: got %v, want %v", m["invoice_version"], want)
		}
		if got := matched(t, e, inv)["invoice_version"]; got != want {
			t.Errorf("event invoice_version: got %v, want %v", got, want)
		}
		list, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+inv+"/match-results", "", "")
		items := list["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("match history: got %d results, want 1", len(items))
		}
		if got := items[0].(map[string]any)["invoice_version"]; got != want {
			t.Errorf("history invoice_version: got %v, want %v", got, want)
		}
	})
	// A match with issues holds the invoice and names the held version.
	t.Run("held by the match", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-2")
		e.receive(t, po, 1, "10")
		inv := id(e.clean(t, po, "INV-2"))
		before := version(t, e, inv)
		m := e.act(t, 200, inv, "match", "")
		want := version(t, e, inv)
		if want == before {
			t.Fatalf("invoice version: got %v after the hold, want a version after %v", want, before)
		}
		if m["invoice_version"] != want {
			t.Errorf("match response invoice_version: got %v, want %v", m["invoice_version"], want)
		}
		if got := matched(t, e, inv)["invoice_version"]; got != want {
			t.Errorf("event invoice_version: got %v, want %v", got, want)
		}
	})
	// A clean match of an invoice its earlier match held releases it and
	// names the released version.
	t.Run("released by the match", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-4")
		e.receive(t, po, 1, "10")
		inv := id(e.clean(t, po, "INV-4"))
		e.act(t, 200, inv, "match", "")
		held := version(t, e, inv)
		e.receive(t, po, 2, "5")
		m := e.act(t, 200, inv, "match", "")
		want := version(t, e, inv)
		if want == held {
			t.Fatalf("invoice version: got %v after the release, want a version after %v", want, held)
		}
		if m["invoice_version"] != want {
			t.Errorf("match response invoice_version: got %v, want %v", m["invoice_version"], want)
		}
	})
	t.Run("recorded without a version", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-3")
		e.receive(t, po, 1, "10")
		e.receive(t, po, 2, "5")
		inv := id(e.clean(t, po, "INV-3"))
		e.act(t, 200, inv, "match", "")
		old := uuid.Must(uuid.NewV7())
		pgtest.Exec(t, `INSERT INTO payables.match_result (tenant_id, id, supplier_invoice_id, evaluated_at, is_clean, issues, config_version, evaluated_by)
			SELECT tenant_id, $2, supplier_invoice_id, clock_timestamp(), true, issues, config_version, evaluated_by
			FROM payables.match_result WHERE supplier_invoice_id = $1`, uuid.MustParse(inv), old)
		read, _ := e.Must(200, "GET", "/v1/match-results/"+old.String(), "", "")
		if got, present := read["invoice_version"]; present {
			t.Errorf("invoice_version of a match without one: got %v, want absent", got)
		}
	})
}
