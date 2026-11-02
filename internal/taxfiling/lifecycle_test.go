package taxfiling_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
)

// The return lifecycle (ADR-0049 §11-§13, T61i): prepared from the draft,
// claiming its items once; approved by another person; recorded as filed
// with the authority's confirmation, locking its fiscal periods; withdrawn
// or rejected before filing, releasing its items; a filed return amended
// by the items of its period that arrived after it.

var lifecyclePerms = []string{"tax.return.prepare", "tax.return.file"}

// lifecycleEnv is returnsEnv whose human prepares, files and withdraws.
func lifecycleEnv(t *testing.T) fixture.Env {
	t.Helper()
	return returnsEnv(t, lifecyclePerms...)
}

// closePeriods closes company 2000's fiscal periods from to to of 2026.
func closePeriods(t *testing.T, e fixture.Env, from, to int) {
	t.Helper()
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period BETWEEN $3 AND $4 AND status = 'open'`, e.Tenant.ID, e.DE, from, to)
}

// periodStatuses are company 2000's 2026 statuses of periods from to to.
func periodStatuses(t *testing.T, e fixture.Env, from, to int) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT string_agg(DISTINCT status, ',') FROM ledger.period_status WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period BETWEEN $3 AND $4`, []any{e.Tenant.ID, e.DE, from, to}, &got)
	return got
}

// in is e reporting to a subtest's t.
func in(t *testing.T, e fixture.Env) fixture.Env {
	t.Helper()
	e.Client = e.In(t)
	return e
}

func returnsOf(registration string) string {
	return "/v1/tax-registrations/" + registration + "/returns"
}

// snapshot renders a return's boxes as code=amount/prior-period part/corrected[#items].
func snapshot(m map[string]any) string {
	var out []string
	for _, it := range m["boxes"].([]any) {
		b := it.(map[string]any)
		amount := func(k string) any { return b[k].(map[string]any)["amount"] }
		s := fmt.Sprintf("%s=%s/%s/%s", b["code"], amount("amount"), amount("prior_period_amount"), amount("corrected_amount"))
		if n, ok := b["item_count"]; ok {
			s += fmt.Sprintf("#%v", n)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// claimedBy counts the items reported in the return.
func claimedBy(t *testing.T, e fixture.Env, ret any) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.tax_item WHERE tenant_id = $1 AND reported_in = $2`, []any{e.Tenant.ID, ret}, &n)
	return n
}

// hashOf recomputes a return's items hash from the ledger's items it
// claimed, as the snapshot names them.
func hashOf(t *testing.T, e fixture.Env, ret any) string {
	t.Helper()
	var lines string
	pgtest.Scan(t, `SELECT coalesce(string_agg(format('%s|%s|%s|%s|%s|%s', i.fiscal_year, i.journal_entry_id, i.item_no, i.base_lc, i.tax_lc,
			i.recoverable_lc) || E'\n', '' ORDER BY i.fiscal_year, i.journal_entry_id, i.item_no), '')
		FROM tax.tax_return_item r JOIN ledger.tax_item i ON i.tenant_id = r.tenant_id AND i.fiscal_year = r.fiscal_year
			AND i.journal_entry_id = r.journal_entry_id AND i.item_no = r.item_no WHERE r.tenant_id = $1 AND r.tax_return_id = $2`,
		[]any{e.Tenant.ID, ret}, &lines)
	sum := sha256.Sum256([]byte(lines))
	return hex.EncodeToString(sum[:])
}

func taxEvents(t *testing.T, e fixture.Env, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, e.Tenant, outbox.Event{Context: "tax", Name: name})
}

// TestTaxReturnLifecycle: a return of an ended period, its fiscal periods
// closed, is prepared from the draft and claims its items, which no later
// draft reads; it waits for another person's approval; recorded as filed
// with the authority's confirmation it locks the fiscal periods, and
// a retry is refused without a second event; the period's items that
// arrive later (a reversal) are its amendment's.
func TestTaxReturnLifecycle(t *testing.T) {
	e := lifecycleEnv(t)
	feb := sale(t, e, "2026-02-10", "")
	purchase(t, e, "2026-03-10")
	sale(t, e, "2026-04-15", "2026-03-30") // posted late, dated in Q1, posted in an open period
	testID := id(t, e, "TEST")
	draft, _ := e.Must(200, "GET", compute(testID, "2026-01-01"), "", "")
	body := `{"period_start": "2026-01-01"}`

	e.Problem(409, "tax.fiscal_period_open", "POST", returnsOf(testID), body, "")
	closePeriods(t, e, 1, 3)
	ret, tag := e.Must(201, "POST", returnsOf(testID), body, "")
	path := "/v1/tax-returns/" + ret["id"].(string)
	t.Run("prepared", func(t *testing.T) {
		e := in(t, e)
		want(t, "return", fmt.Sprintf("%v %v %v %v %v %v %v %v", ret["status"], ret["kind"], ret["registration_code"], ret["period_end"], ret["return_type"],
			ret["currency"], ret["item_count"], ret["pack"]), "prepared original TEST 2026-03-31 TEST-1 EUR 3 map[country:DE version:2026.1]")
		want(t, "boxes", snapshot(ret), "sales=200.00/0.00/200.00#2 out=10.00/0.00/10.00#2 in=19.00/0.00/19.00#1 cost=0.00/0.00/0.00#1 net=-9.00/0.00/-9.00")
		want(t, "the draft's boxes", boxes(draft), "sales=200.00/0.00#2 out=10.00/0.00#2 in=19.00/0.00#1 cost=0.00/0.00#1 net=-9.00/0.00")
		want(t, "ETag", tag, `"1"`)
		want(t, "items hash", ret["items_hash"].(string), hashOf(t, e, ret["id"]))
		want(t, "items claimed", claimedBy(t, e, ret["id"]), 3)
		got, _ := e.Must(200, "GET", path, "", "")
		want(t, "read back", fmt.Sprint(got), fmt.Sprint(ret))
		after, _ := e.Must(200, "GET", compute(testID, "2026-01-01"), "", "")
		want(t, "the period's draft", fmt.Sprint(after["item_count"]), "0")
		q2, _ := e.Must(200, "GET", compute(testID, "2026-04-01"), "", "")
		want(t, "the next draft's items", fmt.Sprintf("%v %v", q2["item_count"], q2["prior_period"].(map[string]any)["item_count"]), "0 0")
		r := e.Pending(t, ret["id"].(string))
		want(t, "request", fmt.Sprintf("%v %v %v %v", r["object_type"], r["id"] == ret["approval_request_id"], r["requester_id"], r["object_resource"]),
			fmt.Sprintf("tax_return true %s %s", e.Tenant.Human.ID, path))
		e.Problem(409, "tax.return_in_progress", "POST", returnsOf(testID), body, "")
		e.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "Mine"}`, fmt.Sprintf(`"%v"`, r["version"]))
		e.Problem(409, "tax.invalid_return_status", "POST", path+":file", `{"confirmation_number": "CRA-1", "filed_on": "2026-04-20"}`, tag)
		events := taxEvents(t, e, "tax_return_prepared")
		want(t, "prepared events", len(events), 1)
		want(t, "event's hash", events[0]["items_hash"], ret["items_hash"])
	})

	e.Approve(t, ret["id"].(string))
	approved, tag := e.Must(200, "GET", path, "", "")
	want(t, "approved", fmt.Sprintf("%v %v %v", approved["status"], approved["approved_by"], tag), fmt.Sprintf("approved %s \"2\"", e.Approver.ID))
	want(t, "approved events", len(taxEvents(t, e, "tax_return_approved")), 1)

	t.Run("refused filings", func(t *testing.T) {
		e := in(t, e)
		for name, c := range map[string]struct{ body, ifMatch, code string }{
			"on the period's last day": {`{"confirmation_number": "CRA-1", "filed_on": "2026-03-31"}`, tag, "tax.invalid_filed_on"},
			"in the future":            {`{"confirmation_number": "CRA-1", "filed_on": "2099-01-01"}`, tag, "tax.invalid_filed_on"},
			"without a confirmation":   {`{"confirmation_number": "  ", "filed_on": "2026-04-20"}`, tag, "tax.confirmation_required"},
			"an old version":           {`{"confirmation_number": "CRA-1", "filed_on": "2026-04-20"}`, `"1"`, "request.version_mismatch"},
		} {
			t.Run(name, func(t *testing.T) {
				_, m, _ := e.In(t).Do("POST", path+":file", c.body, c.ifMatch)
				want(t, "code", m["code"], any(c.code))
			})
		}
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND period = 2`, e.Tenant.ID, e.DE)
		e.Problem(409, "tax.fiscal_period_open", "POST", path+":file", `{"confirmation_number": "CRA-1", "filed_on": "2026-04-20"}`, tag)
		closePeriods(t, e, 2, 2)
	})

	filed, tag := e.Must(200, "POST", path+":file", `{"confirmation_number": " CRA-2026-0420-1 ", "filed_on": "2026-04-20"}`, tag)
	t.Run("filed", func(t *testing.T) {
		e := in(t, e)
		f := filed["filing"].(map[string]any)
		want(t, "filed", fmt.Sprintf("%v %v %v %v %v", filed["status"], f["confirmation_number"], f["filed_on"], f["recorded_by"], tag),
			fmt.Sprintf("filed CRA-2026-0420-1 2026-04-20 %s \"3\"", e.Tenant.Human.ID))
		want(t, "Q1's fiscal periods", periodStatuses(t, e, 1, 3), "locked")
		want(t, "Q2's", periodStatuses(t, e, 4, 6), "open")
		events := taxEvents(t, e, "tax_return_filed")
		want(t, "filed events", len(events), 1)
		want(t, "locked periods", len(events[0]["locked_periods"].([]any)), 3)
		// A retry is refused, filing and publishing nothing again.
		e.Problem(409, "tax.invalid_return_status", "POST", path+":file", `{"confirmation_number": "CRA-2026-0420-1", "filed_on": "2026-04-20"}`, tag)
		want(t, "filed events after a retry", len(taxEvents(t, e, "tax_return_filed")), 1)
		e.Problem(409, "tax.invalid_return_status", "POST", path+":withdraw", `{"reason": "Too late"}`, tag)
		e.Problem(409, "tax.return_exists", "POST", returnsOf(testID), body, "")
		e.Problem(409, "tax.nothing_to_amend", "POST", returnsOf(testID), `{"period_start": "2026-01-01", "kind": "amendment"}`, "")
		var audited int
		pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action LIKE 'tax.return.%'`,
			[]any{e.Tenant.ID, ret["id"]}, &audited)
		want(t, "audit records (prepare, approve, file)", audited, 3)
	})

	t.Run("amended", func(t *testing.T) {
		e := in(t, e)
		// A reversal arrives after the filing, dated in Q1 as its original
		// is; a manual entry may not be (P6, TestTaxDateHold).
		e.Must(200, "POST", "/v1/journal-entries/"+feb+":reverse", `{"posting_date": "2026-05-05"}`, "*")
		sale(t, e, "2026-05-06", "2026-04-10") // after the period: not the amendment's
		amendment, _ := e.Must(201, "POST", returnsOf(testID), `{"period_start": "2026-01-01", "kind": "amendment"}`, "")
		want(t, "amendment", fmt.Sprintf("%v %v %v", amendment["kind"], amendment["amends_id"], amendment["item_count"]), "amendment "+ret["id"].(string)+" 1")
		want(t, "its boxes", snapshot(amendment), "sales=-100.00/0.00/100.00#1 out=-5.00/0.00/5.00#1 in=0.00/0.00/19.00#0 cost=0.00/0.00/0.00#0 net=-5.00/0.00/-14.00")
		e.Problem(409, "tax.return_in_progress", "POST", returnsOf(testID), `{"period_start": "2026-01-01", "kind": "amendment"}`, "")
		q2, _ := e.Must(200, "GET", compute(testID, "2026-04-01"), "", "")
		want(t, "the next draft's items, of earlier periods", fmt.Sprintf("%v %v", q2["item_count"], q2["prior_period"].(map[string]any)["item_count"]), "1 0")
		// Filed, its periods already locked, it locks none.
		e.Approve(t, amendment["id"].(string))
		amended, _ := e.Must(200, "POST", "/v1/tax-returns/"+amendment["id"].(string)+":file", `{"confirmation_number": "CRA-2", "filed_on": "2026-05-20"}`, `"2"`)
		want(t, "amendment filed", amended["status"], any("filed"))
		events := taxEvents(t, e, "tax_return_filed")
		want(t, "periods its filing locked", len(events[len(events)-1]["locked_periods"].([]any)), 0)
	})

	t.Run("listed", func(t *testing.T) {
		e := in(t, e)
		page, _ := e.Must(200, "GET", returnsOf(testID)+"?page_size=1", "", "")
		next, _ := e.Must(200, "GET", returnsOf(testID)+"?page_size=1&page_token="+page["next_page_token"].(string), "", "")
		var kinds []string
		for _, p := range []map[string]any{page, next} {
			for _, it := range p["items"].([]any) {
				kinds = append(kinds, it.(map[string]any)["kind"].(string))
			}
		}
		want(t, "pages", strings.Join(kinds, " "), "original amendment")
		for query, n := range map[string]int{"?status=filed": 2, "?status=prepared": 0, "?period_start=2026-04-01": 0, "?period_start=2026-01-01": 2} {
			got, _ := e.Must(200, "GET", returnsOf(testID)+query, "", "")
			want(t, query, len(got["items"].([]any)), n)
		}
		e.Problem(404, "request.not_found", "GET", returnsOf("00000000-0000-7000-8000-000000000000"), "", "")
	})
}

// TestTaxReturnEnded: a return withdrawn before or after its approval, or
// whose request is rejected or withdrawn, releases its items to the next
// return, keeps its snapshot and names who ended it; a return is then
// prepared anew.
func TestTaxReturnEnded(t *testing.T) {
	e := lifecycleEnv(t)
	sale(t, e, "2026-02-10", "")
	closePeriods(t, e, 1, 3)
	testID := id(t, e, "TEST")
	body := `{"period_start": "2026-01-01"}`
	prepare := func(t *testing.T) (map[string]any, string) {
		t.Helper()
		ret, tag := e.In(t).Must(201, "POST", returnsOf(testID), body, "")
		want(t, "claimed", claimedBy(t, e, ret["id"]), 1)
		return ret, tag
	}
	ended := func(t *testing.T, ret map[string]any, status string) {
		t.Helper()
		got, _ := e.In(t).Must(200, "GET", "/v1/tax-returns/"+ret["id"].(string), "", "")
		want(t, "status", got["status"], any(status))
		if _, ok := got["decision"].(map[string]any)["decided_by"]; !ok {
			t.Errorf("decision: got %v, want who ended it", got["decision"])
		}
		want(t, "snapshot kept", snapshot(got), snapshot(ret))
		want(t, "released", claimedBy(t, e, ret["id"]), 0)
	}
	t.Run("withdrawn while prepared", func(t *testing.T) {
		ret, tag := prepare(t)
		got, _ := e.In(t).Must(200, "POST", "/v1/tax-returns/"+ret["id"].(string)+":withdraw", `{"reason": "A late invoice first"}`, tag)
		want(t, "comment", got["decision"].(map[string]any)["comment"], any("A late invoice first"))
		ended(t, ret, "withdrawn")
		var status string
		pgtest.Scan(t, `SELECT status FROM workflow.approval_request WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, ret["approval_request_id"]}, &status)
		want(t, "its request", status, "cancelled")
	})
	t.Run("withdrawn once approved", func(t *testing.T) {
		ret, _ := prepare(t)
		e.Approve(t, ret["id"].(string))
		_, tag := e.In(t).Must(200, "GET", "/v1/tax-returns/"+ret["id"].(string), "", "")
		e.In(t).Must(200, "POST", "/v1/tax-returns/"+ret["id"].(string)+":withdraw", `{"reason": "Wrong period"}`, tag)
		ended(t, ret, "withdrawn")
	})
	t.Run("its request rejected", func(t *testing.T) {
		ret, _ := prepare(t)
		r := e.Pending(t, ret["id"].(string))
		e.As(e.Approver).In(t).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "Box out is short"}`,
			fmt.Sprintf(`"%v"`, r["version"]))
		ended(t, ret, "rejected")
		want(t, "rejected events", len(taxEvents(t, e, "tax_return_rejected")), 1)
	})
	t.Run("its request withdrawn", func(t *testing.T) {
		ret, _ := prepare(t)
		r := e.Pending(t, ret["id"].(string))
		e.In(t).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":cancel", `{"comment": "Not yet"}`, fmt.Sprintf(`"%v"`, r["version"]))
		ended(t, ret, "withdrawn")
	})
	want(t, "withdrawn events", len(taxEvents(t, e, "tax_return_withdrawn")), 3)
	prepare(t)
}

// TestPrepareConcurrently: two prepares of one period at once, each under
// the registration's lock: one claims the items, the other finds its
// return and is refused, so no item is in two returns.
func TestPrepareConcurrently(t *testing.T) {
	e := lifecycleEnv(t)
	sale(t, e, "2026-02-10", "")
	purchase(t, e, "2026-03-10")
	closePeriods(t, e, 1, 3)
	path := returnsOf(id(t, e, "TEST"))
	codes := make([]string, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			code, body, _ := e.Do("POST", path, `{"period_start": "2026-01-01"}`, "")
			codes[i] = fmt.Sprintf("%d %v", code, body["code"])
		})
	}
	wg.Wait()
	got := codes[0] + "; " + codes[1]
	if got != "201 <nil>; 409 tax.return_in_progress" && got != "409 tax.return_in_progress; 201 <nil>" {
		t.Errorf("prepares: got %s, want one 201 and one 409 tax.return_in_progress", got)
	}
	// The sale's output tax and the purchase's input tax recovered: the
	// items some box selects.
	var returns, claimed int
	pgtest.Scan(t, `SELECT count(*), (SELECT count(*) FROM ledger.tax_item WHERE tenant_id = $1 AND reported_in IS NOT NULL) FROM tax.tax_return
		WHERE tenant_id = $1`, []any{e.Tenant.ID}, &returns, &claimed)
	want(t, "returns, items claimed", fmt.Sprint(returns, claimed), "1 2")
}

// TestTaxReturnPeriodOrder: an original is prepared only once every
// earlier period of the registration is filed, the earliest unfiled one
// named, so a period's items are claimed by its own return; prepared or
// approved is not filed.
func TestTaxReturnPeriodOrder(t *testing.T) {
	e := lifecycleEnv(t)
	sale(t, e, "2026-02-10", "")
	sale(t, e, "2026-05-10", "")
	closePeriods(t, e, 1, 9)
	path := returnsOf(id(t, e, "TEST"))
	q1, q2, q3 := `{"period_start": "2026-01-01"}`, `{"period_start": "2026-04-01"}`, `{"period_start": "2026-07-01"}`
	refused := func(t *testing.T, body, unfiled string) {
		t.Helper()
		_, m, _ := e.In(t).Do("POST", path, body, "")
		want(t, "code", m["code"], any("tax.earlier_period_unfiled"))
		want(t, "names the earliest unfiled period", strings.HasPrefix(fmt.Sprint(m["detail"]), "Period "+unfiled+" of registration TEST has no filed return"), true)
		var later int
		pgtest.Scan(t, `SELECT count(*) FROM tax.tax_return WHERE tenant_id = $1 AND period_start > '2026-01-01'`, []any{e.Tenant.ID}, &later)
		want(t, "returns of later periods", later, 0)
	}
	t.Run("nothing filed", func(t *testing.T) {
		refused(t, q2, "2026-01-01 to 2026-03-31")
		refused(t, q3, "2026-01-01 to 2026-03-31")
	})
	first, _ := e.Must(201, "POST", path, q1, "")
	want(t, "Q1's items", claimedBy(t, e, first["id"]), 1)
	t.Run("Q1 prepared", func(t *testing.T) { refused(t, q2, "2026-01-01 to 2026-03-31") })
	e.Approve(t, first["id"].(string))
	t.Run("Q1 approved", func(t *testing.T) { refused(t, q2, "2026-01-01 to 2026-03-31") })
	e.Must(200, "POST", "/v1/tax-returns/"+first["id"].(string)+":file", `{"confirmation_number": "CRA-1", "filed_on": "2026-04-20"}`, `"2"`)
	t.Run("Q1 filed, Q2 not", func(t *testing.T) { refused(t, q3, "2026-04-01 to 2026-06-30") })
	second, _ := e.Must(201, "POST", path, q2, "")
	want(t, "Q2's items, none of earlier periods", fmt.Sprintf("%v %v", second["item_count"], second["prior_period"].(map[string]any)["item_count"]), "1 0")
	e.Problem(409, "tax.nothing_to_amend", "POST", path, `{"period_start": "2026-01-01", "kind": "amendment"}`, "")
}

// TestPrepareInOrderConcurrently: the returns of two periods prepared at
// once: the later one is refused whichever runs first, so the earlier
// period's item is its own return's; filing the earlier one while the
// later is prepared lets the later through only once it is filed.
func TestPrepareInOrderConcurrently(t *testing.T) {
	e := lifecycleEnv(t)
	sale(t, e, "2026-02-10", "")
	sale(t, e, "2026-05-10", "")
	closePeriods(t, e, 1, 6)
	path := returnsOf(id(t, e, "TEST"))
	at := func(calls ...func() string) []string {
		got := make([]string, len(calls))
		var wg sync.WaitGroup
		for i, call := range calls {
			wg.Go(func() { got[i] = call() })
		}
		wg.Wait()
		return got
	}
	post := func(url, body, ifMatch string) func() string {
		return func() string {
			code, m, _ := e.Do("POST", url, body, ifMatch)
			return fmt.Sprintf("%d %v", code, m["code"])
		}
	}
	got := at(post(path, `{"period_start": "2026-01-01"}`, ""), post(path, `{"period_start": "2026-04-01"}`, ""))
	want(t, "Q1, Q2 prepared at once", strings.Join(got, "; "), "201 <nil>; 409 tax.earlier_period_unfiled")
	var q1 string
	pgtest.Scan(t, `SELECT id::text FROM tax.tax_return WHERE tenant_id = $1`, []any{e.Tenant.ID}, &q1)
	want(t, "Q1's return claims Q1's item", claimedBy(t, e, q1), 1)
	e.Approve(t, q1)
	got = at(post("/v1/tax-returns/"+q1+":file", `{"confirmation_number": "CRA-1", "filed_on": "2026-04-20"}`, `"2"`),
		post(path, `{"period_start": "2026-04-01"}`, ""))
	if got[0] != "200 <nil>" || (got[1] != "201 <nil>" && got[1] != "409 tax.earlier_period_unfiled") {
		t.Errorf("Q1 filed, Q2 prepared at once: got %s, want Q1 filed and Q2 prepared or refused", strings.Join(got, "; "))
	}
	if got[1] != "201 <nil>" {
		e.Must(201, "POST", path, `{"period_start": "2026-04-01"}`, "")
	}
	var claimed string
	pgtest.Scan(t, `SELECT string_agg(r.period_start::text || '=' || r.item_count, ' ' ORDER BY r.period_start) FROM tax.tax_return r
		WHERE r.tenant_id = $1`, []any{e.Tenant.ID}, &claimed)
	want(t, "each period's return, its own item", claimed, "2026-01-01=1 2026-04-01=1")
}

// TestTaxReturnPeople: every transition is a person's: an agent may not
// prepare, file or withdraw, nor approve; a person of company 2000 alone
// moves no return of the group covering 1000 too; a return of a
// registration the caller does not see is not found; a kind that is
// neither is refused.
func TestTaxReturnPeople(t *testing.T) {
	e := lifecycleEnv(t)
	sale(t, e, "2026-02-10", "")
	closePeriods(t, e, 1, 3)
	testID := id(t, e, "TEST")
	ret, tag := e.Must(201, "POST", returnsOf(testID), `{"period_start": "2026-01-01"}`, "")
	path := "/v1/tax-returns/" + ret["id"].(string)
	agent := e.As(e.Tenant.Agent)
	for name, c := range map[string]struct{ path, body string }{
		"prepare":  {returnsOf(testID), `{"period_start": "2026-04-01"}`},
		"file":     {path + ":file", `{"confirmation_number": "X", "filed_on": "2026-04-20"}`},
		"withdraw": {path + ":withdraw", `{"reason": "X"}`},
	} {
		t.Run("an agent may not "+name, func(t *testing.T) {
			agent.In(t).Problem(403, "identity.agent_not_allowed", "POST", c.path, c.body, tag)
		})
	}
	r := e.Pending(t, ret["id"].(string))
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	e.Problem(422, "tax.invalid_kind", "POST", returnsOf(testID), `{"period_start": "2026-01-01", "kind": "voluntary_disclosure"}`, "")
	e.Problem(409, "tax.nothing_to_amend", "POST", returnsOf(testID), `{"period_start": "2026-04-01", "kind": "amendment"}`, "")
	e.Problem(412, "request.version_mismatch", "POST", path+":withdraw", `{"reason": "X"}`, `"9"`)
	// A return of the group covering 1000 and 2000, which a person of 2000
	// alone sees but may not move.
	vat := id(t, e, "VAT")
	pgtest.Exec(t, `INSERT INTO tax.tax_return (tenant_id, id, registration_id, period_start, period_end, kind, return_type, return_name, pack_country,
			pack_version, currency, item_count, items_hash, prior_item_count, prior_tax, above_threshold, approval_request_id, created_by, updated_by)
		VALUES ($1, $2, $3, '2026-01-15', '2026-02-14', 'original', 'VAT201', 'VAT', 'DE', '2026.1', 'EUR', 0, repeat('0', 64), 0, 0, false,
			gen_random_uuid(), $4, $4)`, e.Tenant.ID, ret["approval_request_id"], vat, e.Tenant.Human.ID)
	de := e.As(e.Reader(t, "Germany only", e.DE))
	de.Must(200, "GET", "/v1/tax-returns/"+ret["approval_request_id"].(string), "", "")
	de.Problem(403, "tax.registration_companies", "POST", "/v1/tax-returns/"+ret["approval_request_id"].(string)+":withdraw", `{"reason": "X"}`, `"1"`)
	nobody := e.As(e.Reader(t, "No company"))
	nobody.Problem(404, "request.not_found", "GET", path, "", "")
	nobody.Problem(404, "request.not_found", "POST", path+":withdraw", `{"reason": "X"}`, tag)
}

// noApproval is an approvals context whose policy asks for none.
type noApproval struct{ taxfiling.Opener }

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

// TestPrepareFailures: a snapshot whose sums, expressions or corrected
// figures leave the range of an amount, a policy that asks no approval,
// and an amended return whose pack no longer defines it fail the prepare
// rather than store a wrong return.
func TestPrepareFailures(t *testing.T) {
	s := newLifecycleScene(t)
	big := money.Minor(math.MaxInt64)
	out := ledger.TaxItemGroup{Direction: "output", AccountKey: "output", Items: 1, TaxLC: -big}
	in := ledger.TaxItemGroup{Direction: "input", AccountKey: "input", Items: 1, TaxLC: -big, RecoverableLC: -big}
	one := []ledger.ClaimedItem{{}}
	q4 := &gen.TaxReturnPrepare{PeriodStart: date("2026-10-01")}
	amend := &gen.TaxReturnPrepare{PeriodStart: date("2026-07-01"), Kind: gen.NewOptString(taxfiling.Amendment)}
	prepare := func(t *testing.T, h *taxfiling.Endpoints, req *gen.TaxReturnPrepare) error {
		t.Helper()
		return apitest.InRequest(t, s.e.Tenant, s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.PrepareTaxReturn(ctx, req, gen.PrepareTaxReturnParams{ID: s.testID})
			return err
		})
	}
	for name, c := range map[string]struct {
		items taxfiling.TaxItems
		req   *gen.TaxReturnPrepare
	}{
		"a box":                  {overflowing{claim: true, groups: []ledger.TaxItemGroup{out, out}}, q4},
		"an expression":          {overflowing{claim: true, groups: []ledger.TaxItemGroup{out, in}}, q4},
		"a corrected box":        {overflowing{claim: true, groups: []ledger.TaxItemGroup{out}, claimed: one}, amend},
		"a corrected expression": {overflowing{claim: true, groups: []ledger.TaxItemGroup{in}, claimed: one}, amend},
	} {
		t.Run(name, func(t *testing.T) {
			h := *s.h
			h.Items = c.items
			if err := prepare(t, &h, c.req); err == nil || problem.From(err).Status != 500 {
				t.Errorf("got %v, want an internal error", err)
			}
		})
	}
	t.Run("no approval asked", func(t *testing.T) {
		h := *s.h
		h.Approvals = noApproval{}
		if err := prepare(t, &h, q4); err == nil || problem.From(err).Status != 500 {
			t.Errorf("got %v, want an internal error", err)
		}
	})
	t.Run("the amended return's pack gone", func(t *testing.T) {
		pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
			UPDATE tax.tax_return SET pack_version = '1999.1' WHERE tenant_id = '%s' AND status = 'filed'; END $$`, s.e.Tenant.ID))
		if err := prepare(t, s.h, amend); err == nil || problem.From(err).Status != 500 {
			t.Errorf("got %v, want an internal error", err)
		}
	})
}

// TestTaxReturnConstraints: the indexes backing up prepare's own checks
// are its problems: a second return of a period in progress, a second
// original that stands.
func TestTaxReturnConstraints(t *testing.T) {
	s := newLifecycleScene(t)
	// The prepared amendment copied, kind and all; Q3's filed original, of
	// a period with nothing in progress, copied.
	for name, c := range map[string]struct{ from, kind, code string }{
		"in progress":       {`id = '` + s.prepared.String() + `'`, "kind, amends_id", "tax.return_in_progress"},
		"a second original": {`kind = 'original' AND period_start = '2026-07-01'`, "'original', NULL", "tax.return_exists"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, s.e.Tenant, s.e.Tenant.Human, func(ctx context.Context) error {
				tx, _, err := reqctx.Request(ctx)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `INSERT INTO tax.tax_return (tenant_id, id, registration_id, period_start, period_end, kind, amends_id, return_type,
						return_name, pack_country, pack_version, currency, item_count, items_hash, prior_item_count, prior_tax, above_threshold,
						approval_request_id, created_by, updated_by)
					SELECT tenant_id, gen_random_uuid(), registration_id, period_start, period_end, `+c.kind+`, return_type, return_name, pack_country,
						pack_version, currency, 0, items_hash, 0, 0, false, gen_random_uuid(), created_by, created_by
					FROM tax.tax_return WHERE `+c.from)
				return err
			})
			want(t, "problem", problem.From(err).Code, c.code)
		})
	}
}
