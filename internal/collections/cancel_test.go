package collections_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// cancelBody is a cancellation's request.
const cancelBody = `{"reason": "Run at the wrong key date"}`

// cancel cancels the run at the entity tag and returns it.
func (e env) cancel(t *testing.T, id, tag string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "POST", "/v1/dunning-runs/"+id+":cancel", cancelBody, tag)
	return got
}

// tagOf is the run's entity tag now.
func (e env) tagOf(t *testing.T, id string) string {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/dunning-runs/"+id, "", "")
	return tag
}

// charges are a run's notices' debit memos as "customer status", by
// customer number, with each reversal's date when reversed.
func (e env) charges(t *testing.T, id string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	var out []string
	for _, n := range objects(t, page, "items") {
		c, ok := n["charge_document"].(map[string]any)
		if !ok {
			continue
		}
		s := fmt.Sprintf("%v %v", n["customer_number"], c["status"])
		if c["status"] == "reversed" && c["reversal_date"] != c["posting_date"] {
			s += fmt.Sprintf(" on %v, not the memo's %v", c["reversal_date"], c["posting_date"])
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// TestCancelRun (T76i acceptance): a run that a later confirmed run has
// dunned the items of is refused; the latest is cancelled: its levels no
// longer count, its debit memos are reversed on their posting dates and
// no longer open, the events and the audit record are written; cancelling
// it again is refused; then the earlier one is cancelled, and the next run
// proposes the items from their first level.
func TestCancelRun(t *testing.T) {
	e := postingEnv(t)
	e.story(t)
	first, tag := e.start(t, keyDate)
	e.confirm(t, first, tag)
	second, tag := e.start(t, "2026-07-30")
	e.confirm(t, second, tag)
	open := e.openItems(t)
	p := e.Problem(409, "collections.run_superseded", "POST", "/v1/dunning-runs/"+first+":cancel", cancelBody, `"2"`)
	if d, _ := p["detail"].(string); !strings.Contains(d, second+" at key date 2026-07-30") {
		t.Errorf("detail: got %q, want it to name the later run", d)
	}
	cancelled := e.cancel(t, second, `"2"`)
	if got := fields(t, cancelled, "status", "cancellation_reason", "charge_document_count", "version"); got !=
		"status cancelled, cancellation_reason Run at the wrong key date, charge_document_count 2, version 3" {
		t.Errorf("the cancelled run: got %s", got)
	}
	if got := e.charges(t, second); got != "BP70000001 reversed, BP70000002 reversed" {
		t.Errorf("its memos: got %q, want both reversed on their posting dates", got)
	}
	if got := e.memos(t); strings.Count(got, " reversed") != 2 || strings.Count(got, " posted") != 1 {
		t.Errorf("the debit memos: got %s, want the second run's two reversed and the first run's posted", got)
	}
	if got := e.openItems(t); len(got) >= len(open) {
		t.Errorf("open items: got %d bytes after the cancellation, want fewer than the %d before: the memos cleared by their reversals", len(got), len(open))
	}
	e.events(t, second)
	if got, want := e.proposed(t, e.startID(t, "2026-07-30")), "CI-USD 1->2, CI-A 0->1, CI-B 0->1, CI-C 0->1, CI-D 0->1, MANUAL 0->1"; got != want {
		t.Errorf("the next run: got %q, want %q: the items at the first run's levels", got, want)
	}
	e.Problem(409, "collections.invalid_status", "POST", "/v1/dunning-runs/"+second+":cancel", cancelBody, `"3"`)
	e.cancel(t, first, `"2"`)
	if got := e.proposed(t, e.startID(t, keyDate)); got != "CI-USD 0->1" {
		t.Errorf("the run after both cancellations: got %q, want Southwind's item from level 0", got)
	}
}

// startID starts a run of company 2000 at the key date and returns its id.
func (e env) startID(t *testing.T, key string) string {
	t.Helper()
	id, _ := e.start(t, key)
	return id
}

// events checks what the cancellation of the run published and audited:
// DunningRunChanged cancelled from confirmed, one DunningNoticeCancelled
// per notice naming its reversed memo, one audit record without the
// reason.
func (e env) events(t *testing.T, id string) {
	t.Helper()
	changes := apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "collections", Name: "dunning_run_changed"}))
	last := changes[len(changes)-1]
	if last["dunning_run_id"] != id || last["status"] != "cancelled" || last["status_before"] != "confirmed" {
		t.Errorf("the last DunningRunChanged: got %v, want the run cancelled from confirmed", last)
	}
	withdrawn := apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "collections", Name: "dunning_notice_cancelled"}))
	if len(withdrawn) != 2 {
		t.Fatalf("DunningNoticeCancelled events: got %d, want 2", len(withdrawn))
	}
	for _, w := range withdrawn {
		if w["dunning_run_id"] != id || w["customer_invoice_id"] == nil || w["reversal_entry_id"] == nil || w["reversal_date"] == nil {
			t.Errorf("DunningNoticeCancelled: got %v, want the run's notice naming its reversed memo", w)
		}
	}
	var audits int
	var detail string
	pgtest.Scan(t, `SELECT count(*), coalesce(max(detail::text), '') FROM audit.audit_event WHERE tenant_id = $1 AND action = 'collections.dunning_run.cancel' AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &audits, &detail)
	if audits != 1 || strings.Contains(detail, "wrong key date") {
		t.Errorf("audit records of the cancellation: got %d, %s, want 1 without the reason", audits, detail)
	}
}

// TestCancelStaleProposal: a proposal started before a cancellation is
// refused as stale when it is confirmed (the times its items were dunned
// changed); one started after it is confirmed.
func TestCancelStaleProposal(t *testing.T) {
	e := postingEnv(t)
	e.story(t)
	first, tag := e.start(t, keyDate)
	e.confirm(t, first, tag)
	before, beforeTag := e.start(t, "2026-07-30")
	e.cancel(t, first, `"2"`)
	e.Problem(409, "collections.proposal_stale", "POST", "/v1/dunning-runs/"+before+":confirm", "", beforeTag)
	after, afterTag := e.start(t, "2026-07-30")
	if got := e.confirm(t, after, afterTag)["status"]; got != "confirmed" {
		t.Errorf("the run after the cancellation: got %v, want it confirmed", got)
	}
}

// TestCancelRefusals: a memo whose item is cleared, or posted in a period
// since closed, refuses the cancellation with receivables' and the
// ledger's codes, the memo and customer leading the detail, and nothing
// changes; so are a proposed run, a reason of white space, a missing or
// stale entity tag, a run of another company, one that does not exist,
// and a caller without the permission.
func TestCancelRefusals(t *testing.T) {
	e := postingEnv(t)
	e.story(t)
	run, tag := e.start(t, keyDate)
	e.confirm(t, run, tag)
	path := "/v1/dunning-runs/" + run + ":cancel"
	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+run+"/notices", "", "")
	doc := apitest.Object(t, objects(t, page, "items")[0], "charge_document")
	t.Run("a memo posted in a period since closed", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.period_status s SET status = 'closed', version = version + 1 FROM ledger.journal_entry j
			WHERE s.tenant_id = $1 AND j.tenant_id = $1 AND j.id = $2 AND s.legal_entity_id = j.legal_entity_id AND s.fiscal_year = j.fiscal_year
				AND s.period = j.fiscal_period`, e.Tenant.ID, apitest.String(t, doc, "journal_entry_id"))
		defer pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND status = 'closed'`, e.Tenant.ID)
		e.In(t).Problem(409, "ledger.period_not_open", "POST", path, cancelBody, `"2"`)
	})
	credit := e.post(t, item{customer: e.south, amount: -500, currency: "EUR", day: "2026-07-01", noTerms: true, ref: "CREDIT-S"})[0]
	t.Run("a memo cleared", func(t *testing.T) {
		e.In(t).Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": %q, "items": [
			{"fiscal_year": %v, "journal_entry_id": %q, "line_no": 1}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`,
			e.DE, doc["posting_date"], doc["journal_entry_year"], doc["journal_entry_id"], credit.ID), "")
		p := e.In(t).Problem(409, "receivables.document_cleared", "POST", path, cancelBody, `"2"`)
		if d, _ := p["detail"].(string); !strings.HasPrefix(d, "Debit memo "+apitest.String(t, doc, "customer_invoice_number")+" of customer BP70000001 cannot be reversed: ") {
			t.Errorf("detail: got %q, want it led by the memo and the customer", d)
		}
	})
	proposed, proposedTag := e.start(t, "2026-07-30")
	for _, c := range []struct {
		name, path, body, tag, code string
		status                      int
	}{
		{"a proposed run", "/v1/dunning-runs/" + proposed + ":cancel", cancelBody, proposedTag, "collections.invalid_status", 409},
		{"a reason of white space", path, `{"reason": "  "}`, `"2"`, "request.validation_failed", 422},
		{"without the entity tag", path, cancelBody, "", "request.precondition_required", 428},
		{"at another version", path, cancelBody, `"7"`, "request.version_mismatch", 412},
		{"a run that does not exist", "/v1/dunning-runs/" + uuid.NewString() + ":cancel", cancelBody, `"2"`, "request.not_found", 404},
	} {
		t.Run(c.name, func(t *testing.T) { e.In(t).Problem(c.status, c.code, "POST", c.path, c.body, c.tag) })
	}
	t.Run("a company the caller may not act in", func(t *testing.T) {
		e.As(e.Reader(t, "A person of company 1000", e.US)).In(t).Problem(404, "request.not_found", "POST", path, cancelBody, `"2"`)
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'collections.dunning.run'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", path, cancelBody, `"2"`)
	})
	if got := e.charges(t, run); got != "BP70000001 posted" {
		t.Errorf("the memo after the refusals: got %q, want it posted still", got)
	}
}

// TestCancellationsSerialize: the cancellations and confirmations of one
// company wait for each other, whichever comes first: a run cancelled
// twice at once is cancelled once; a cancellation racing the confirmation
// of a later proposal over its items either refuses that proposal as
// stale or is refused as superseded, never both done and never a
// deadlock (ADR-0008).
func TestCancellationsSerialize(t *testing.T) {
	e := postingEnv(t)
	e.story(t)
	h := endpoints(t)
	cancel := func(id string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CancelRun(ctx, uuid.MustParse(id), func(int32) error { return nil }, "Twice")
			return err
		}
	}
	confirm := func(id string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.ConfirmRun(ctx, uuid.MustParse(id), func(int32) error { return nil })
			return err
		}
	}
	run, tag := e.start(t, keyDate)
	e.confirm(t, run, tag)
	t.Run("the same run twice", func(t *testing.T) {
		first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, cancel(run), cancel(run))
		if p := problem.From(second); first != nil || p.Code != "collections.invalid_status" {
			t.Errorf("got %v and %v, want the first cancelled and the second refused as collections.invalid_status", first, second)
		}
	})
	for _, c := range []struct {
		name  string
		first bool // the cancellation first
		want  string
	}{
		{"a cancellation, then a later proposal's confirmation", true, "collections.proposal_stale"},
		{"a later proposal's confirmation, then a cancellation", false, "collections.run_superseded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			earlier, earlierTag := e.In(t).start(t, keyDate)
			e.In(t).confirm(t, earlier, earlierTag)
			later, _ := e.In(t).start(t, "2026-07-30")
			a, b := cancel(earlier), confirm(later)
			if !c.first {
				a, b = b, a
			}
			first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, a, b)
			if p := problem.From(second); first != nil || p.Code != c.want {
				t.Errorf("got %v and %v, want the first done and the second refused as %s", first, second, c.want)
			}
			if c.first {
				return
			}
			if _, err := h.CancelDunningRun(context.Background(), &gen.DunningRunCancel{Reason: "x"}, gen.CancelDunningRunParams{ID: uuid.MustParse(later)}); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			e.In(t).cancel(t, later, e.In(t).tagOf(t, later))
			e.In(t).cancel(t, earlier, e.In(t).tagOf(t, earlier))
		})
	}
}

// TestCancelFailures: cancelling a run that posted, with each database
// statement failing in turn.
func TestCancelFailures(t *testing.T) {
	e := postingEnv(t)
	e.story(t)
	run, tag := e.start(t, keyDate)
	e.confirm(t, run, tag)
	h := endpoints(t)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "cancel", func(ctx context.Context) error {
		_, err := h.CancelDunningRun(ctx, &gen.DunningRunCancel{Reason: "x"}, gen.CancelDunningRunParams{ID: uuid.MustParse(run), IfMatch: `"2"`})
		return err
	})
	if got := e.charges(t, run); got != "BP70000001 posted" {
		t.Errorf("the memo after the failures: got %q, want it posted still", got)
	}
}

// TestCancelWithoutCharges: a run that posted no debit memo (a company
// naming no dunning account) is cancelled: its levels no longer count,
// nothing is reversed, and each DunningNoticeCancelled names no memo.
func TestCancelWithoutCharges(t *testing.T) {
	e := setup(t)
	e.story(t)
	run, tag := e.start(t, keyDate)
	e.confirm(t, run, tag)
	if got := fields(t, e.cancel(t, run, `"2"`), "status", "charge_document_count"); got != "status cancelled, charge_document_count 0" {
		t.Errorf("the cancelled run: got %s", got)
	}
	if got, want := e.proposed(t, e.startID(t, keyDate)), "CI-USD 0->1, CI-A 0->1, CI-B 0->1"; got != want {
		t.Errorf("the next run: got %q, want %q: the items from level 0", got, want)
	}
	withdrawn := apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "collections", Name: "dunning_notice_cancelled"}))
	if len(withdrawn) != 2 {
		t.Fatalf("DunningNoticeCancelled events: got %d, want 2", len(withdrawn))
	}
	for _, w := range withdrawn {
		if w["dunning_run_id"] != run || w["customer_invoice_id"] != nil || w["customer_invoice_number"] != nil || w["reversal_entry_id"] != nil ||
			w["reversal_date"] != nil {
			t.Errorf("DunningNoticeCancelled: got %v, want the run's notice naming no memo", w)
		}
	}
}

// TestCancelSupersededByAMemoDunned: a run whose debit memo a later
// confirmed run dunned, sharing no item with it, is refused as
// superseded, naming that run; once that run is cancelled, it is
// cancelled.
func TestCancelSupersededByAMemoDunned(t *testing.T) {
	// Without a minimum amount, so the memo of 5.00 is dunned on its own.
	e := setupWith(t, func(doc *config.Config) {
		posting(doc)
		doc.Company("2000").Dunning.MinimumAmount = ""
	})
	for _, c := range []uuid.UUID{e.north, e.south, e.east} {
		e.companyData(t, c)
	}
	entries := e.story(t)
	first, tag := e.start(t, keyDate)
	// Confirmed on 1 July: Southwind's CI-USD charged 5.00, a memo dated that day, the company's today, due 31 July.
	h := endpoints(t)
	h.Now = func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ConfirmDunningRun(ctx, gen.ConfirmDunningRunParams{ID: uuid.MustParse(first), IfMatch: tag})
		return err
	}); err != nil {
		t.Fatalf("the first run: got %v, want it confirmed", err)
	}
	// Southwind pays CI-USD, so the later run shares no item with the first.
	paid := e.post(t, item{customer: e.south, amount: -20000, currency: "USD", day: "2026-07-01", noTerms: true, ref: "PAID-USD"})[0]
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-07-01", "items": [
		{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`,
		e.DE, entries[7].ID, paid.ID), "")
	// And Northwind, whose CI-A the first run dunned too, is blocked from dunning.
	e.Must(201, "POST", "/v1/dunning-blocks", blockOf(e.DE, e.north, nil, lastHeld), "")
	later, laterTag := e.start(t, "2026-08-10")
	firstPage, _ := e.Must(200, "GET", "/v1/dunning-runs/"+first+"/notices", "", "")
	doc := apitest.Object(t, objects(t, firstPage, "items")[0], "charge_document")
	memo := apitest.String(t, doc, "customer_invoice_number")
	var dated string
	pgtest.Scan(t, `SELECT invoice_date::text || ' ' || posting_date::text FROM receivables.customer_invoice WHERE tenant_id = $1 AND id = $2`,
		[]any{e.Tenant.ID, doc["customer_invoice_id"]}, &dated)
	if dated != "2026-07-01 2026-07-01" {
		t.Errorf("the memo's date and posting date: got %s, want the company's day of the confirmation", dated)
	}
	// Its items: the memo, and Eastwind's CI-E, which the first run did not dun.
	if got, want := e.proposed(t, later), memo+" 0->1, CI-E 0->1"; got != want {
		t.Fatalf("the later run: got %q, want %q", got, want)
	}
	e.confirm(t, later, laterTag)
	p := e.Problem(409, "collections.run_superseded", "POST", "/v1/dunning-runs/"+first+":cancel", cancelBody, `"2"`)
	if d, _ := p["detail"].(string); !strings.Contains(d, later+" at key date 2026-08-10") {
		t.Errorf("detail: got %q, want it to name the run that dunned the memo", d)
	}
	e.cancel(t, later, `"2"`)
	if got := e.cancel(t, first, `"2"`)["status"]; got != "cancelled" {
		t.Errorf("the first run after the later one's cancellation: got %v, want it cancelled", got)
	}
}
