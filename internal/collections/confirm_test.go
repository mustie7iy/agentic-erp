package collections_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// start starts a run of company 2000 at the key date and returns its id
// and entity tag.
func (e env) start(t *testing.T, key string) (id, tag string) {
	t.Helper()
	got, tag := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, key), "")
	return apitest.String(t, got, "id"), tag
}

// confirm confirms the run and returns it.
func (e env) confirm(t *testing.T, id, tag string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "POST", "/v1/dunning-runs/"+id+":confirm", "", tag)
	return got
}

// proposed are a run's items as "reference previous->level", by customer
// number and item.
func (e env) proposed(t *testing.T, id string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	var out []string
	for _, n := range objects(t, page, "items") {
		for _, it := range objects(t, n, "items") {
			out = append(out, fmt.Sprintf("%v %v->%v", it["reference"], it["previous_level"], it["level"]))
		}
	}
	return strings.Join(out, ", ")
}

// dunned are the tenant's recorded levels as "reference level date", by
// reference and date: what collections.dunned_item holds.
func (e env) dunned(t *testing.T) string {
	t.Helper()
	var out *string
	pgtest.Scan(t, `SELECT string_agg(i.reference || ' ' || d.level || ' ' || d.dunning_date, ', ' ORDER BY i.reference, d.dunning_date)
		FROM collections.dunned_item d JOIN collections.dunning_notice_item i ON i.tenant_id = d.tenant_id AND i.dunning_run_id = d.dunning_run_id
			AND i.fiscal_year = d.fiscal_year AND i.journal_entry_id = d.journal_entry_id AND i.line_no = d.line_no
		WHERE d.tenant_id = $1`, []any{e.Tenant.ID}, &out)
	if out == nil {
		return ""
	}
	return *out
}

// issued are the tenant's DunningNoticeIssued events in the order they
// were written.
func (e env) issued(t *testing.T) []map[string]any {
	t.Helper()
	return apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "collections", Name: "dunning_notice_issued"}))
}

// TestConfirm: confirming a run records each item's level and dunning
// date, the run's key date, moves the run to confirmed and publishes
// DunningRunChanged and one DunningNoticeIssued per customer, by id and
// number and never by name, with one audit record; nothing changes on the
// open items. Confirming twice, as a retry and as a new request, records
// and publishes once (T76b acceptance).
func TestConfirm(t *testing.T) {
	e := setup(t)
	e.story(t)
	before := e.openItems(t)
	id, tag := e.start(t, keyDate)
	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	notices := objects(t, page, "items") // Southwind's, then Northwind's
	req := apitest.Req{Method: "POST", Path: "/v1/dunning-runs/" + id + ":confirm", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: tag}
	first, retry := e.API.Do(req), e.API.Do(req)
	if first.Code != 200 || retry.Code != 200 || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("confirmed and retried: got %d and %d (replayed %q), want 200 twice, the second replayed: %s", first.Code, retry.Code,
			retry.Header().Get("Idempotent-Replayed"), first.Body)
	}
	got := apitest.JSON(t, first)
	for f, want := range map[string]any{"id": id, "status": "confirmed", "version": float64(2), "key_date": keyDate, "company_code": "2000",
		"notice_count": float64(2), "item_count": float64(3)} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	if tag := first.Header().Get("ETag"); tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	read, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	t.Run("confirmed again", func(t *testing.T) {
		e.In(t).Problem(409, "collections.invalid_status", "POST", "/v1/dunning-runs/"+id+":confirm", "", `"2"`)
	})
	t.Run("confirmed again at the version it was proposed with", func(t *testing.T) {
		e.In(t).Problem(412, "request.version_mismatch", "POST", "/v1/dunning-runs/"+id+":confirm", "", tag)
	})
	if got, want := e.dunned(t), "CI-A 1 2026-06-30, CI-B 1 2026-06-30, CI-USD 1 2026-06-30"; got != want {
		t.Errorf("recorded levels: got %q, want %q", got, want)
	}
	if after := e.openItems(t); after != before {
		t.Errorf("the open items after the confirmation: got %s, want them unchanged: %s", after, before)
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'collections.dunning_run.confirm' AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &audits)
	if audits != 1 {
		t.Errorf("audit records of the confirmation: got %d, want 1", audits)
	}
	changes := apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "collections", Name: "dunning_run_changed"}))
	if len(changes) != 2 {
		t.Fatalf("DunningRunChanged events: got %d, want 2: proposed and confirmed", len(changes))
	}
	want := map[string]any{"dunning_run_id": id, "legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "key_date": keyDate,
		"notice_count": float64(2), "item_count": float64(3), "overdue_amount": map[string]any{"amount": "310.00", "currency": "EUR"},
		"status": "confirmed", "status_before": "proposed", "version": float64(2)}
	if a, b := mustJSON(t, changes[1]), mustJSON(t, want); a != b {
		t.Errorf("the confirmation's event: got %s, want %s", a, b)
	}
	issued := e.issued(t)
	if len(issued) != 2 {
		t.Fatalf("DunningNoticeIssued events: got %d, want 2, one per customer", len(issued))
	}
	for i, w := range []struct {
		customer uuid.UUID
		number   string
		items    float64
		overdue  string
	}{{e.south, "BP70000001", 1, "160.00"}, {e.north, "BP70000002", 2, "150.00"}} {
		t.Run("the event of "+w.number, func(t *testing.T) {
			want := map[string]any{"dunning_notice_id": notices[i]["id"], "dunning_run_id": id, "legal_entity_id": e.DE.String(),
				"legal_entity_code": "2000", "customer_id": w.customer.String(), "customer_number": w.number, "dunning_date": keyDate,
				"level": float64(1), "item_count": w.items, "overdue_amount": map[string]any{"amount": w.overdue, "currency": "EUR"}}
			if a, b := mustJSON(t, issued[i]), mustJSON(t, want); a != b {
				t.Errorf("got %s, want %s", a, b)
			}
		})
	}
	for _, name := range []string{"Northwind", "Southwind"} {
		apitest.NeverPublished(t, e.Tenant, "collections", name)
	}
}

// TestNextLevel: a run reads each item's previous level from what the
// confirmed runs recorded and proposes the next: an item rises one level
// per confirmed run at most, the highest level stays and is recorded
// again with the later date, and a level never falls, at an earlier key
// date either, as long as the item is overdue then (T76b acceptance).
func TestNextLevel(t *testing.T) {
	e := setup(t)
	e.story(t)
	for _, c := range []struct {
		name, key, want string
		confirm         bool
	}{
		{"the first run", keyDate, "CI-USD 0->1, CI-A 0->1, CI-B 0->1", true},
		{"proposed again before the next level's days", "2026-07-01", "CI-USD 1->2, CI-A 1->2, CI-B 1->1, CI-C 0->1", false},
		{"the second, a level up where the days reach it", "2026-07-15", "CI-USD 1->2, CI-A 1->2, CI-B 1->2, CI-C 0->1, CI-D 0->1, MANUAL 0->1", true},
		{"the third", "2026-12-31", "CI-USD 2->3, CI-A 2->3, CI-B 2->3, CI-C 1->2, CI-D 1->2, MANUAL 1->2, CI-E 0->1", true},
		{"the fourth: the highest level stays", "2027-01-31", "CI-USD 3->3, CI-A 3->3, CI-B 3->3, CI-C 2->3, CI-D 2->3, MANUAL 2->3, CI-E 1->2", true},
		{"at an earlier key date a level does not fall", "2026-04-02", "CI-A 3->3", false},
		{"on its due date an item dunned before is not overdue", "2026-03-31", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			id, tag := e.In(t).start(t, c.key)
			if got := e.In(t).proposed(t, id); got != c.want {
				t.Errorf("proposed at %s: got %q, want %q", c.key, got, c.want)
			}
			if c.confirm {
				e.In(t).confirm(t, id, tag)
			}
		})
	}
	want := "CI-A 1 2026-06-30, CI-A 2 2026-07-15, CI-A 3 2026-12-31, CI-A 3 2027-01-31, " +
		"CI-B 1 2026-06-30, CI-B 2 2026-07-15, CI-B 3 2026-12-31, CI-B 3 2027-01-31, " +
		"CI-C 1 2026-07-15, CI-C 2 2026-12-31, CI-C 3 2027-01-31, CI-D 1 2026-07-15, CI-D 2 2026-12-31, CI-D 3 2027-01-31, " +
		"CI-E 1 2026-12-31, CI-E 2 2027-01-31, CI-USD 1 2026-06-30, CI-USD 2 2026-07-15, CI-USD 3 2026-12-31, CI-USD 3 2027-01-31, " +
		"MANUAL 1 2026-07-15, MANUAL 2 2026-12-31, MANUAL 3 2027-01-31" // the debit without terms, 15 days past its document date at the second run
	if got := e.dunned(t); got != want {
		t.Errorf("recorded levels: got %q, want %q", got, want)
	}
}

// In returns e calling the API for tb.
func (e env) In(tb testing.TB) env {
	tb.Helper()
	e.Client = e.Client.In(tb)
	return e
}

// TestStaleProposal: a proposal with an item cleared since, or with an
// item another confirmed run has dunned since, is refused as stale and
// records and publishes nothing; a new run is proposed and confirmed; a
// run without notices is confirmed and records nothing (T76b acceptance,
// ADR-0065 §8).
func TestStaleProposal(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	payment := e.post(t, item{customer: e.north, amount: -5000, currency: "EUR", day: "2026-06-28", noTerms: true, ref: "PAYMENT"})[0]
	a, aTag := e.start(t, keyDate)
	b, bTag := e.start(t, keyDate)
	none, noneTag := e.start(t, "2026-03-31")
	// Northwind pays CI-B: its item is cleared with the payment.
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-06-30", "items": [
		{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`,
		e.DE, entries[1].ID, payment.ID), "")
	stale := func(t *testing.T, id, tag, detail string) {
		t.Helper()
		got := e.In(t).Problem(409, "collections.proposal_stale", "POST", "/v1/dunning-runs/"+id+":confirm", "", tag)
		if d, _ := got["detail"].(string); !strings.Contains(d, detail) {
			t.Errorf("detail: got %q, want it to say %q", d, detail)
		}
		read, readTag := e.In(t).Must(200, "GET", "/v1/dunning-runs/"+id, "", "")
		if read["status"] != "proposed" || readTag != tag {
			t.Errorf("the stale run: got %v at %s, want it proposed at %s still", read["status"], readTag, tag)
		}
	}
	t.Run("an item cleared since", func(t *testing.T) {
		stale(t, a, aTag, "of its 3 items, 1 cleared and 0 dunned by another run")
		if got := e.dunned(t); got != "" {
			t.Errorf("recorded levels: got %q, want none", got)
		}
		if n := len(e.issued(t)); n != 0 {
			t.Errorf("DunningNoticeIssued events: got %d, want none", n)
		}
	})
	t.Run("a new run is proposed and confirmed", func(t *testing.T) {
		id, tag := e.In(t).start(t, keyDate)
		if got, want := e.In(t).proposed(t, id), "CI-USD 0->1, CI-A 0->1"; got != want {
			t.Errorf("the new run: got %q, want %q", got, want)
		}
		e.In(t).confirm(t, id, tag)
	})
	const recorded = "CI-A 1 2026-06-30, CI-USD 1 2026-06-30"
	t.Run("items dunned by another run since", func(t *testing.T) {
		stale(t, b, bTag, "of its 3 items, 1 cleared and 2 dunned by another run")
		if got := e.dunned(t); got != recorded {
			t.Errorf("recorded levels: got %q, want %q", got, recorded)
		}
	})
	t.Run("a run without notices", func(t *testing.T) {
		if got := e.In(t).confirm(t, none, noneTag); got["status"] != "confirmed" || got["notice_count"] != float64(0) {
			t.Errorf("got %v with %v notices, want it confirmed with none", got["status"], got["notice_count"])
		}
		if got := e.dunned(t); got != recorded {
			t.Errorf("recorded levels: got %q, want %q", got, recorded)
		}
		if n := len(e.issued(t)); n != 2 {
			t.Errorf("DunningNoticeIssued events: got %d, want the 2 of the run confirmed before", n)
		}
	})
}

// TestConfirmationsSerialize: the confirmations of one company wait for
// each other: of two proposals with the same items confirmed at once,
// the second waits for the first, reads what it recorded and is stale,
// also where the first left the items' levels as they were (a key date
// short of the next level's days; the highest level); the same run
// confirmed twice at once is confirmed once (ADR-0065 §8, ADR-0008).
func TestConfirmationsSerialize(t *testing.T) {
	e := setup(t)
	e.story(t)
	h := endpoints(t)
	confirm := func(id string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.ConfirmRun(ctx, uuid.MustParse(id), func(int32) error { return nil })
			return err
		}
	}
	a, _ := e.start(t, keyDate)
	b, _ := e.start(t, keyDate)
	t.Run("two proposals with the same items", func(t *testing.T) {
		first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, confirm(a), confirm(b))
		if p := problem.From(second); first != nil || p.Code != "collections.proposal_stale" {
			t.Errorf("got %v and %v, want the first confirmed and the second refused as collections.proposal_stale", first, second)
		}
	})
	// A day later CI-B, 6 days overdue, stays at level 1, so its level tells the second proposal nothing once the
	// first is confirmed; the times it was dunned do, and the refusal counts it among the items dunned since: all 4.
	t.Run("two proposals whose items stay at their level", func(t *testing.T) {
		stay, _ := e.In(t).start(t, "2026-07-01")
		twin, _ := e.In(t).start(t, "2026-07-01")
		if got, want := e.In(t).proposed(t, twin), "CI-USD 1->2, CI-A 1->2, CI-B 1->1, CI-C 0->1"; got != want {
			t.Fatalf("proposed: got %q, want %q", got, want)
		}
		first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, confirm(stay), confirm(twin))
		if p := problem.From(second); first != nil || p.Code != "collections.proposal_stale" || !strings.Contains(p.Detail, "of its 4 items, 0 cleared and 4 dunned") {
			t.Errorf("got %v and %v, want the first confirmed and the second refused as collections.proposal_stale for its 4 items", first, second)
		}
	})
	c, _ := e.start(t, "2026-07-15")
	t.Run("the same run twice", func(t *testing.T) {
		first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, confirm(c), confirm(c))
		if p := problem.From(second); first != nil || p.Code != "collections.invalid_status" {
			t.Errorf("got %v and %v, want the first confirmed and the second refused as collections.invalid_status", first, second)
		}
	})
	want := "CI-A 1 2026-06-30, CI-A 2 2026-07-01, CI-A 3 2026-07-15, CI-B 1 2026-06-30, CI-B 1 2026-07-01, CI-B 2 2026-07-15, " +
		"CI-C 1 2026-07-01, CI-C 1 2026-07-15, CI-D 1 2026-07-15, CI-USD 1 2026-06-30, CI-USD 2 2026-07-01, CI-USD 3 2026-07-15, MANUAL 1 2026-07-15"
	if got := e.dunned(t); got != want {
		t.Errorf("recorded levels: got %q, want each run's once: %q", got, want)
	}
	if n := len(e.issued(t)); n != 6 {
		t.Errorf("DunningNoticeIssued events: got %d, want 6, two customers' of three confirmed runs", n)
	}
}

// TestConfirmRefusals: a run of a company the caller may not act in, and
// one that does not exist, is not found; the entity tag is required; the
// permission is the one that starts runs.
func TestConfirmRefusals(t *testing.T) {
	e := setup(t)
	e.story(t)
	id, tag := e.start(t, keyDate)
	path := "/v1/dunning-runs/" + id + ":confirm"
	other := e.As(e.Reader(t, "A person of company 1000", e.US))
	t.Run("a company the caller may not act in", func(t *testing.T) {
		other.In(t).Problem(404, "request.not_found", "POST", path, "", tag)
	})
	t.Run("a run that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "POST", "/v1/dunning-runs/"+uuid.NewString()+":confirm", "", tag)
	})
	t.Run("without the entity tag", func(t *testing.T) {
		e.In(t).Problem(428, "request.precondition_required", "POST", path, "", "")
	})
	t.Run("at another version", func(t *testing.T) {
		e.In(t).Problem(412, "request.version_mismatch", "POST", path, "", `"7"`)
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'collections.dunning.run'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", path, "", tag)
	})
	if got := e.dunned(t); got != "" {
		t.Errorf("recorded levels after the refusals: got %q, want none", got)
	}
}

// TestKeyDateLimit: a key date after the last one a request may name is
// refused, naming the field: the days overdue would no longer be counted
// right (src review:T76a); the last one itself is taken.
func TestKeyDateLimit(t *testing.T) {
	e := setup(t)
	e.story(t)
	t.Run("the day after the last key date", func(t *testing.T) {
		got := e.In(t).Problem(422, "collections.invalid_key_date", "POST", "/v1/dunning-runs", run(e.DE, "2101-01-01"), "")
		if f := field(got); f != "key_date" {
			t.Errorf("field: got %v, want key_date", f)
		}
	})
	t.Run("the last key date", func(t *testing.T) {
		got, _ := e.In(t).Must(201, "POST", "/v1/dunning-runs", run(e.DE, "2100-12-31"), "")
		page, _ := e.In(t).Must(200, "GET", "/v1/dunning-runs/"+apitest.String(t, got, "id")+"/notices?customer_id="+e.south.String(), "", "")
		// Southwind's item, due 30 April 2026: 27,273 days to the last key date.
		its := objects(t, objects(t, page, "items")[0], "items")
		if d := its[0]["days_overdue"]; d != float64(27273) {
			t.Errorf("days overdue at the last key date: got %v, want 27273", d)
		}
	})
}
