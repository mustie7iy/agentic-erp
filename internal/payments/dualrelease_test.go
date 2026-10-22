package payments_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// dualSetup is a tenant whose company 2000 (EUR) releases runs above the
// limit only with a second person's approval (ADR-0038), and whose
// supplier has a bank account.
func dualSetup(t *testing.T, limit string) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(c *config.Config) { c.Company("2000").ReleaseDualLimit = limit }, perms...)
	e := setupIn(t, f, func(*app.Config) {})
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier GmbH", "2026-01-01", "", "create")
	return e
}

// proposedBy has the person propose a run of company 2000 that pays one
// invoice of the gross amount, and returns the run.
func (e env) proposedBy(t *testing.T, a pgtest.Actor, ref, gross string) map[string]any {
	t.Helper()
	e.invoice(t, e.Supplier, ref, gross, "2026-03-01")
	op, _ := e.As(a).Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, e.DE, e.bank), "")
	work(t)
	r, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	return r
}

// runPath is the run's resource.
func runPath(run map[string]any) string { return "/v1/payment-runs/" + id(run) }

// act calls the run's custom method as the person, at its current
// version, expecting the status.
func (e env) act(t *testing.T, a pgtest.Actor, status int, run map[string]any, action, body string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", runPath(run), "", "")
	got, _ := e.As(a).Must(status, "POST", runPath(run)+":"+action, body, tag)
	return got
}

// refused calls the run's custom method as the person and expects the
// problem.
func (e env) refused(t *testing.T, a pgtest.Actor, status int, code string, run map[string]any, action, body string) {
	t.Helper()
	_, tag := e.Must(200, "GET", runPath(run), "", "")
	e.As(a).Problem(status, code, "POST", runPath(run)+":"+action, body, tag)
}

// releasedBy releases the run as the person and lets the release worker
// do it; it returns the run as released.
func (e env) releasedBy(t *testing.T, a pgtest.Actor, run map[string]any) map[string]any {
	t.Helper()
	e.act(t, a, 202, run, "release", "")
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	got, _ := e.Must(200, "GET", runPath(run), "", "")
	if got["status"] != "released" {
		t.Fatalf("run status: got %v, want released", got["status"])
	}
	return got
}

// approval is the run's release approval as the run shows it.
func (e env) approval(t *testing.T, run map[string]any) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "GET", runPath(run), "", "")
	a, _ := got["release_approval"].(map[string]any)
	return a
}

// decideRequest approves or rejects the approval request as the person,
// expecting the status.
func (e env) decideRequest(t *testing.T, a pgtest.Actor, status int, request, action, body string) map[string]any {
	t.Helper()
	_, tag := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+request, "", "")
	got, _ := e.As(a).Must(status, "POST", "/v1/approval-requests/"+request+":"+action, body, tag)
	return got
}

// requestRefused expects the decision on the approval request to be
// refused with the problem.
func (e env) requestRefused(t *testing.T, a pgtest.Actor, status int, code, request, action, body string) {
	t.Helper()
	_, tag := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+request, "", "")
	e.As(a).Problem(status, code, "POST", "/v1/approval-requests/"+request+":"+action, body, tag)
}

// obj is the object under the key.
func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

func wantField(t *testing.T, m map[string]any, key string, want any) {
	t.Helper()
	if got := fmt.Sprint(m[key]); got != fmt.Sprint(want) {
		t.Errorf("%s: got %v, want %v", key, got, want)
	}
}

// TestDualReleaseWithinTheLimit: a run at its company's release_dual_limit
// is released by one person, as before; there is no approval to ask for.
func TestDualReleaseWithinTheLimit(t *testing.T) {
	e := dualSetup(t, "500.00")
	run := e.proposedBy(t, e.Tenant.Human, "INV-AT", "500.00")
	e.refused(t, e.Approver, 409, "payments.release_approval_not_required", run, "submit", "")
	released := e.releasedBy(t, e.Approver, run)
	if a, ok := released["release_approval"]; ok {
		t.Errorf("release approval: got %v, want none", a)
	}
}

// TestDualRelease: a run above the limit needs a second person's approval
// before :release; proposer, requester, approver and releaser are kept
// apart at each step, agents neither ask nor approve, and the approval
// itself releases nothing.
func TestDualRelease(t *testing.T) {
	e := dualSetup(t, "500.00")
	proposer := e.Person(t, "proposer") // holds the release role, as the others do
	run := e.proposedBy(t, proposer, "INV-BIG", "600.00")
	var request string
	ok := t.Run("refused before it is asked for", func(t *testing.T) {
		e.refusals(t, run, "", []refusal{
			{"released", e.Approver, 409, "payments.release_approval_required", "release"},
			{"the proposer asks", proposer, 403, "identity.sod_conflict", "submit"},
			{"an agent asks", e.Tenant.Agent, 403, "identity.agent_not_allowed", "submit"},
		})
	}) && t.Run("asked at a stale version", func(t *testing.T) {
		e.Problem(412, "request.version_mismatch", "POST", runPath(run)+":submit", "", `"999"`)
	}) && t.Run("a releaser asks", func(t *testing.T) {
		got := e.act(t, e.Approver, 200, run, "submit", "")
		a := obj(got, "release_approval")
		wantField(t, a, "status", "pending")
		wantField(t, a, "requested_total", map[string]any{"amount": "600.00", "currency": "EUR"})
		wantField(t, got, "status", "proposed")
		request, _ = a["request_id"].(string)
		r, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+request, "", "")
		wantField(t, r, "object_type", "payment_run_release")
		wantField(t, r, "object_id", id(run))
		wantField(t, r, "requester_id", e.Approver.ID)
		wantField(t, r, "amount", map[string]any{"amount": "600.00", "currency": "EUR"})
	}) && t.Run("refused while it waits", func(t *testing.T) {
		e.refusals(t, run, request, []refusal{
			{"asked twice", e.Approver2, 409, "payments.release_approval_pending", "submit"},
			{"released", e.Approver2, 409, "payments.release_approval_pending", "release"},
			{"the requester approves", e.Approver, 403, "workflow.own_request", "approve"},
			{"the proposer approves", proposer, 403, "workflow.segregation_of_duties", "approve"},
			{"an agent approves", e.Tenant.Agent, 403, "identity.agent_not_allowed", "approve"},
		})
	}) && t.Run("a second person approves", func(t *testing.T) {
		got := e.decideRequest(t, e.Approver2, 200, request, "approve", `{"comment": "Within the cash plan"}`)
		wantField(t, got, "status", "approved")
		after, _ := e.Must(200, "GET", runPath(run), "", "")
		wantField(t, obj(after, "release_approval"), "status", "approved")
		wantField(t, obj(after, "release_approval"), "approved_by", e.Approver2.ID)
		wantField(t, after, "status", "proposed") // the approval releases nothing
		if op, ok := after["release_operation_id"]; ok {
			t.Errorf("release operation after the approval: got %v, want none", op)
		}
	}) && t.Run("refused once approved", func(t *testing.T) {
		e.refusals(t, run, request, []refusal{
			{"asked again", e.Approver, 409, "payments.release_already_approved", "submit"},
			{"the approver releases", e.Approver2, 403, "identity.sod_conflict", "release"},
			{"the proposer releases", proposer, 403, "identity.sod_conflict", "release"},
		})
	}) && t.Run("the requester releases", func(t *testing.T) {
		got := e.releasedBy(t, e.Approver, run)
		wantField(t, got, "decided_by", e.Approver.ID)
		events := apitest.Published(t, e.Tenant, outbox.Event{Context: "payments", Name: "payment_run_released"})
		if len(events) != 1 {
			t.Fatalf("payment_run_released events: got %d, want 1", len(events))
		}
		wantField(t, events[0], "workflow_request_id", request)
	}) && t.Run("refused once released", func(t *testing.T) {
		e.refusals(t, run, request, []refusal{{"asked for", e.Approver, 409, "payments.invalid_status", "submit"}})
	})
	if ok {
		wantAudits(t, e, run, "payments.payment_run.submit", "payments.payment_run.release_approved", "payments.payment_run.release")
	}
}

// refusal is a call the two-person release refuses: a custom method of
// the run, or approve on its release request.
type refusal struct {
	name   string
	as     pgtest.Actor
	status int
	code   string
	action string // submit, release or approve
}

// refusals checks each refusal on the run and its release request.
func (e env) refusals(t *testing.T, run map[string]any, request string, cases []refusal) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.action == "approve" {
				e.requestRefused(t, c.as, c.status, c.code, request, "approve", `{}`)
				return
			}
			e.refused(t, c.as, c.status, c.code, run, c.action, "")
		})
	}
}

// wantAudits checks that the run's audit trail has each action.
func wantAudits(t *testing.T, e env, run map[string]any, actions ...string) {
	t.Helper()
	for _, action := range actions {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = $3`,
			[]any{e.Tenant.ID, uuidOf(id(run)), action}, &n)
		if n == 0 {
			t.Errorf("audit records %s of run %s: got none, want one", action, id(run))
		}
	}
}

// TestDualReleaseRefused: a rejected or withdrawn approval leaves the run
// proposed, its release refused until it is asked for again; rejecting
// the run withdraws a pending request.
func TestDualReleaseRefused(t *testing.T) {
	e := dualSetup(t, "500.00")
	run := e.proposedBy(t, e.Tenant.Human, "INV-NO", "600.00")
	ask := func(t *testing.T) string {
		t.Helper()
		got := e.act(t, e.Approver, 200, run, "submit", "")
		return got["release_approval"].(map[string]any)["request_id"].(string)
	}
	t.Run("rejected", func(t *testing.T) {
		request := ask(t)
		e.decideRequest(t, e.Approver2, 200, request, "reject", `{"comment": "Not this week"}`)
		wantField(t, e.approval(t, run), "status", "rejected")
		e.refused(t, e.Approver, 409, "payments.release_approval_required", run, "release", "")
	})
	t.Run("withdrawn by the requester", func(t *testing.T) {
		request := ask(t)
		_, tag := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+request, "", "")
		e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+request+":cancel", `{"comment": "Wrong run"}`, tag)
		wantField(t, e.approval(t, run), "status", "withdrawn")
		e.refused(t, e.Approver, 409, "payments.release_approval_required", run, "release", "")
	})
	t.Run("the run rejected", func(t *testing.T) {
		request := ask(t)
		got := e.act(t, e.Approver2, 200, run, "reject", `{"reason": "Paid by cheque"}`)
		wantField(t, got, "status", "rejected")
		wantField(t, obj(got, "release_approval"), "status", "withdrawn")
		r, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+request, "", "")
		wantField(t, r, "status", "cancelled")
		events := apitest.Published(t, e.Tenant, outbox.Event{Context: "payments", Name: "payment_run_rejected"})
		if len(events) != 1 {
			t.Fatalf("payment_run_rejected events: got %d, want 1", len(events))
		}
		wantField(t, events[0], "workflow_request_id", request)
	})
	wantAudits(t, e, run, "payments.payment_run.release_rejected", "payments.payment_run.release_withdrawn")
}

// TestDualReleaseOutdated: an approval holds for the total it was asked
// for. A proposed run's total never grows (a release only drops payments),
// so the total is raised by hand here: the release is refused and the
// approval asked for again.
func TestDualReleaseOutdated(t *testing.T) {
	e := dualSetup(t, "500.00")
	run := e.proposedBy(t, e.Tenant.Human, "INV-UP", "600.00")
	got := e.act(t, e.Approver, 200, run, "submit", "")
	request := got["release_approval"].(map[string]any)["request_id"].(string)
	e.decideRequest(t, e.Approver2, 200, request, "approve", `{}`)
	pgtest.Exec(t, `UPDATE payments.payment_run SET total_amount = total_amount + 1, version = version + 1 WHERE id = $1`, uuidOf(id(run)))
	e.refused(t, e.Approver, 409, "payments.release_approval_outdated", run, "release", "")
	again := e.act(t, e.Approver, 200, run, "submit", "")
	a := again["release_approval"].(map[string]any)
	wantField(t, a, "status", "pending")
	wantField(t, a, "requested_total", map[string]any{"amount": "600.01", "currency": "EUR"})
	if _, ok := a["approved_by"]; ok {
		t.Errorf("approved by after asking again: got %v, want none", a["approved_by"])
	}
}

// setReleaseLimit sets the release_dual_limit of company i of the
// tenant's active configuration; "" removes it. It rewrites the activated
// version with its immutability trigger off, skipping the change request
// that would validate and approve the change: only the value the release
// reads matters here, and a change request's evidence and approvals are
// tested by the configuration context.
func setReleaseLimit(t *testing.T, tenant uuid.UUID, i int, limit string) {
	t.Helper()
	value := fmt.Sprintf(`jsonb_set(document, '{companies,%d,release_dual_limit}', '%q')`, i, limit)
	if limit == "" {
		value = fmt.Sprintf(`document #- '{companies,%d,release_dual_limit}'`, i)
	}
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE config.config_version v SET document = %s
		FROM config.active_version a WHERE a.tenant_id = v.tenant_id AND a.number = v.number AND v.tenant_id = '%s'; END $$`, value, tenant))
}

// TestDualReleaseLimitRemoved: a run whose request still waits when it no
// longer needs one (the limit was removed) is released by one person, and
// the request is withdrawn, never to be approved after the release.
func TestDualReleaseLimitRemoved(t *testing.T) {
	e := dualSetup(t, "500.00")
	run := e.proposedBy(t, e.Tenant.Human, "INV-RAISED", "600.00")
	got := e.act(t, e.Approver, 200, run, "submit", "")
	request := got["release_approval"].(map[string]any)["request_id"].(string)
	setReleaseLimit(t, e.Tenant.ID, 1, "")
	released := e.releasedBy(t, e.Approver2, run)
	wantField(t, obj(released, "release_approval"), "status", "withdrawn")
	r, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+request, "", "")
	wantField(t, r, "status", "cancelled")
}

// TestDualReleaseForeignCurrency: a run in another currency than its
// company's is measured at the run date's rate, the rate its payments post
// at: USD 1000.00 is CAD 1350.00 at the invoice's 1.35 and CAD 1370.00 at
// the run date's 1.37.
func TestDualReleaseForeignCurrency(t *testing.T) {
	sc := mapleSetup(t)
	e := sc.e
	pgtest.Exec(t, `UPDATE partner.supplier_company SET payment_methods = '{T}', version = version + 1 WHERE partner_id = $1 AND legal_entity_id = $2`,
		e.Supplier, sc.le)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "US-DUAL",
		"invoice_date": "2026-03-02", "gross_amount": {"amount": "1000.00", "currency": "USD"}, "tax_amount": {"amount": "0.00", "currency": "USD"},
		"items": [{"account_id": %q, "description": "Parts", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "1000.00", "currency": "USD"}}]}`,
		sc.le, e.Supplier, e.Account(t, "3000", "520000")), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-02"}`, `"1"`)
	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "USD", "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, sc.le), "")
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	setReleaseLimit(t, e.Tenant.ID, 2, "1369.99")
	if !t.Run("above the limit at the run date's rate", func(t *testing.T) {
		e.refused(t, e.Approver, 409, "payments.release_approval_required", run, "release", "")
		got := e.act(t, e.Approver, 200, run, "submit", "")
		wantField(t, obj(got, "release_approval"), "requested_total", map[string]any{"amount": "1000.00", "currency": "USD"})
	}) {
		t.FailNow()
	}
	setReleaseLimit(t, e.Tenant.ID, 2, "1370.00")
	t.Run("at the limit at the run date's rate", func(t *testing.T) {
		released := e.releasedBy(t, e.Approver2, run)
		wantField(t, obj(released, "release_approval"), "status", "withdrawn")
	})
}

// TestReleaseApprovalsUnwaited: the approval callbacks change only a run
// waiting for the request; another request, or a run that does not exist,
// fails the decision.
func TestReleaseApprovalsUnwaited(t *testing.T) {
	e := dualSetup(t, "500.00")
	run := e.proposedBy(t, e.Tenant.Human, "INV-CB", "600.00")
	runID, other := uuidOf(id(run)), uuid.Must(uuid.NewV7())
	h := payments.ReleaseApprovals{}
	for name, call := range map[string]func(ctx context.Context) error{
		"approved, another request": func(ctx context.Context) error { return h.Approved(ctx, runID, other) },
		"rejected, another request": func(ctx context.Context) error { return h.Rejected(ctx, runID, other, "no") },
		"approved, no run":          func(ctx context.Context) error { return h.Approved(ctx, other, other) },
		"cancelled, no run":         func(ctx context.Context) error { return h.Cancelled(ctx, other, other, "no") },
		"barred, no run":            func(ctx context.Context) error { _, err := h.Barred(ctx, other, other); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.Tenant, e.Approver, call); err == nil {
				t.Error("got nil error, want a refusal")
			}
		})
	}
}

// TestDualReleaseDatabaseFailures calls the two-person release's handlers
// and callbacks outside a unit of work and with each of their database
// statements failing in turn.
func TestDualReleaseDatabaseFailures(t *testing.T) {
	e := dualSetup(t, "500.00")
	h := endpoints(nil)
	ask := func(run map[string]any) uuid.UUID {
		got := e.act(t, e.Approver, 200, run, "submit", "")
		return uuidOf(obj(got, "release_approval")["request_id"].(string))
	}
	approved := e.proposedBy(t, e.Tenant.Human, "INV-F1", "600.00")
	e.decideRequest(t, e.Approver2, 200, ask(approved).String(), "approve", `{}`)
	pending := e.proposedBy(t, e.Tenant.Human, "INV-F2", "700.00")
	request := ask(pending)
	fresh := e.proposedBy(t, e.Tenant.Human, "INV-F3", "800.00")
	anyVersion := func(int32) error { return nil }
	cb := payments.ReleaseApprovals{}
	calls := map[string]func(ctx context.Context) error{
		"submit": func(ctx context.Context) error {
			_, err := h.SubmitRelease(ctx, uuidOf(id(fresh)), anyVersion)
			return err
		},
		"release approved": func(ctx context.Context) error {
			_, err := h.StartRelease(ctx, uuidOf(id(approved)), anyVersion, "")
			return err
		},
		"reject pending": func(ctx context.Context) error {
			_, err := h.RejectRun(ctx, uuidOf(id(pending)), anyVersion, "no")
			return err
		},
		"approved callback":  func(ctx context.Context) error { return cb.Approved(ctx, uuidOf(id(pending)), request) },
		"rejected callback":  func(ctx context.Context) error { return cb.Rejected(ctx, uuidOf(id(pending)), request, "no") },
		"cancelled callback": func(ctx context.Context) error { return cb.Cancelled(ctx, uuidOf(id(pending)), request, "no") },
		"barred": func(ctx context.Context) error {
			_, err := cb.Barred(ctx, uuidOf(id(pending)), e.Approver2.ID)
			return err
		},
	}
	failAll := func(t *testing.T, calls map[string]func(ctx context.Context) error) {
		t.Helper()
		for name, call := range calls {
			t.Run(name, func(t *testing.T) {
				if err := call(context.Background()); err == nil {
					t.Error("outside a unit of work: got nil error, want an error")
				}
				apitest.EveryFailure(t, e.Tenant, e.Approver, name, call)
			})
		}
	}
	failAll(t, calls)
	setReleaseLimit(t, e.Tenant.ID, 1, "")
	t.Run("the limit removed", func(t *testing.T) {
		failAll(t, map[string]func(ctx context.Context) error{
			"release pending": func(ctx context.Context) error {
				_, err := h.StartRelease(ctx, uuidOf(id(pending)), anyVersion, "")
				return err
			},
		})
	})
}

// failedAfterPosting starts the run's release as the person, posts its
// one payment and fails the release: the run stays releasing, to be
// resumed (as failures_test's run A).
func (e env) failedAfterPosting(t *testing.T, a pgtest.Actor, run map[string]any) {
	t.Helper()
	e.act(t, a, 202, run, "release", "")
	h := endpoints(nil)
	if err := apitest.Committed(t, e.Tenant, a, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		if _, err := h.PostPayment(ctx, r, uuidOf(id(e.payments(t, run)[0].(map[string]any)))); err != nil {
			return err
		}
		_, err = h.FailRelease(ctx, r, problem.Conflict("payments.bank_file_invalid", "test"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Must(200, "GET", runPath(run), "", ""); got["status"] != "releasing" {
		t.Fatalf("run status after the failed release: got %v, want releasing", got["status"])
	}
}

// TestDualReleaseResumed: a release resumed after its operation failed is
// not measured against the limit again (it was decided when it started),
// so a limit set since never strands a half-posted run; its approver is
// still refused.
func TestDualReleaseResumed(t *testing.T) {
	e := dualSetup(t, "500.00")
	approved := e.proposedBy(t, e.Tenant.Human, "INV-RES1", "600.00")
	got := e.act(t, e.Approver, 200, approved, "submit", "")
	e.decideRequest(t, e.Approver2, 200, obj(got, "release_approval")["request_id"].(string), "approve", `{}`)
	e.failedAfterPosting(t, e.Approver, approved)
	setReleaseLimit(t, e.Tenant.ID, 1, "")
	unapproved := e.proposedBy(t, e.Tenant.Human, "INV-RES2", "700.00")
	e.failedAfterPosting(t, e.Approver, unapproved)
	setReleaseLimit(t, e.Tenant.ID, 1, "500.00")
	t.Run("approved: its approver resumes", func(t *testing.T) {
		e.refused(t, e.Approver2, 403, "identity.sod_conflict", approved, "release", "")
	})
	t.Run("approved: resumed", func(t *testing.T) { e.act(t, e.Approver, 202, approved, "release", "") })
	t.Run("released before the limit: resumed", func(t *testing.T) { e.act(t, e.Approver2, 202, unapproved, "release", "") })
}
