package approvals_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"partner.supplier.read", "partner.supplier.create", "partner.supplier.change"}

func setup(t *testing.T) fixture.Env {
	t.Helper()
	return fixture.New(t, perms...)
}

// partnerChange requests a new partner and returns its pending approval request.
func partnerChange(t *testing.T, f fixture.Env, name string) map[string]any {
	t.Helper()
	p, _ := f.Must(201, "POST", "/v1/business-partners", fmt.Sprintf(`{"kind": "organization", "name": %q, "country_code": "DE", "roles": ["customer"], "reason": "New"}`, name), "")
	changes, _ := f.Must(200, "GET", "/v1/partner-changes?partner_id="+p["id"].(string), "", "")
	return f.Pending(t, changes["items"].([]any)[0].(map[string]any)["id"].(string))
}

func path(r map[string]any, action string) string {
	return "/v1/approval-requests/" + r["id"].(string) + ":" + action
}

func tag(r map[string]any) string { return fmt.Sprintf(`"%v"`, r["version"]) }

// recorder is an owning context that records what happened to its objects.
type recorder struct{ calls *[]string }

func (r recorder) Approved(_ context.Context, id, _ uuid.UUID) error {
	*r.calls = append(*r.calls, "approved")
	return nil
}
func (r recorder) Rejected(_ context.Context, id, _ uuid.UUID, c string) error {
	*r.calls = append(*r.calls, "rejected:"+c)
	return nil
}
func (r recorder) Cancelled(_ context.Context, id, _ uuid.UUID, c string) error {
	*r.calls = append(*r.calls, "cancelled:"+c)
	return nil
}

func TestApproveThroughTheAPI(t *testing.T) {
	f := setup(t)
	r := partnerChange(t, f, "Approved Partner")
	if r["object_type"] != "partner_change" {
		t.Errorf("object_type: got %v, want partner_change", r["object_type"])
	}
	if want := f.Tenant.Human.ID.String(); r["requester_id"] != want {
		t.Errorf("requester_id: got %v, want %s", r["requester_id"], want)
	}
	if r["current_step"] != float64(1) {
		t.Errorf("current_step: got %v, want 1", r["current_step"])
	}
	if steps := r["steps"].([]any); len(steps) != 1 {
		t.Errorf("steps: got %d (%v), want 1", len(steps), steps)
	}
	if r["amount"] != nil {
		t.Errorf("amount: got %v, want none", r["amount"])
	}
	if r["completed_at"] != nil {
		t.Errorf("completed_at: got %v, want none", r["completed_at"])
	}
	f.Problem(403, "workflow.own_request", "POST", path(r, "approve"), "", tag(r))
	f.As(f.Approver).Problem(412, "request.version_mismatch", "POST", path(r, "approve"), "", `"9"`)

	// Holding the permissions is not enough without the step's role, and the other way round.
	noRole := person(t, f, "no role", "all")
	f.As(noRole).Problem(403, "workflow.role_missing", "POST", path(r, "approve"), "", tag(r))
	noPermission := person(t, f, "no permission", "vendor_master_approver")
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT tenant_id, gen_random_uuid(), id, 'workflow.request.decide', current_date - 1, created_by FROM identity.role WHERE tenant_id = $1 AND code = 'vendor_master_approver'`,
		f.Tenant.ID)
	f.As(noPermission).Problem(403, "identity.permission_missing", "POST", path(r, "approve"), "", tag(r))

	got, etag := f.As(f.Approver).Must(200, "POST", path(r, "approve"), `{"comment": "Checked"}`, tag(r))
	d := first(t, got["decisions"], "decisions")
	if got["status"] != "approved" {
		t.Errorf("status: got %v, want approved", got["status"])
	}
	if got["completed_at"] == nil {
		t.Error("completed_at: got none, want a time")
	}
	if d["decision"] != "approve" {
		t.Errorf("decision: got %v, want approve", d["decision"])
	}
	if d["comment"] != "Checked" {
		t.Errorf("comment: got %v, want Checked", d["comment"])
	}
	if n := first(t, got["steps"], "steps")["approvals"]; n != float64(1) {
		t.Errorf("step approvals: got %v, want 1", n)
	}
	if etag != tag(got) {
		t.Errorf("ETag: got %s, want %s", etag, tag(got))
	}
	if partner, _ := f.Must(200, "GET", "/v1/business-partners?name_prefix=Approved", "", ""); partner["items"].([]any)[0].(map[string]any)["status"] != "active" {
		t.Fatalf("partner: %v", partner)
	}
	f.As(f.Approver2).Problem(409, "workflow.not_pending", "POST", path(r, "approve"), "", tag(got))
	f.Problem(409, "workflow.not_pending", "POST", path(r, "cancel"), `{"comment": "Late"}`, tag(got))
	f.Problem(404, "request.not_found", "GET", "/v1/approval-requests/"+uuid.NewString(), "", "")
	completed := apitest.Published(t, f.Tenant, outbox.Event{Context: "workflow", Name: "approval_completed"})
	if len(completed) != 1 {
		t.Fatalf("approval_completed events: got %d (%v), want 1", len(completed), completed)
	}
	if completed[0]["outcome"] != "approved" {
		t.Errorf("completed outcome: got %v, want approved", completed[0]["outcome"])
	}
	if want := f.Approver.ID.String(); completed[0]["decided_by"] != want {
		t.Errorf("completed decided_by: got %v, want %s", completed[0]["decided_by"], want)
	}
	if requested := apitest.Published(t, f.Tenant, outbox.Event{Context: "workflow", Name: "approval_requested"}); len(requested) != 1 {
		t.Fatalf("requested: %v", requested)
	}
}

// person creates a human holding the roles with the given codes.
func person(t *testing.T, f fixture.Env, name string, roles ...string) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, $5, true, 'active', $2, $2)`, f.Tenant.ID, a.ID, pgtest.Issuer(t), a.Subject, name)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = ANY($3)`, f.Tenant.ID, a.ID, roles)
	return a
}

// first is the first object of a JSON list, failing the test when there is
// none, so a missing list is a failure rather than a panic that stops the
// package's other tests.
func first(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	list, _ := v.([]any)
	if len(list) == 0 {
		t.Fatalf("%s: got %v, want at least one", what, v)
	}
	m, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("%s[0]: got %v, want an object", what, list[0])
	}
	return m
}

func TestRejectAndCancel(t *testing.T) {
	f := setup(t)
	rejected := partnerChange(t, f, "Rejected Partner")
	f.As(f.Approver).Problem(400, "request.invalid_body", "POST", path(rejected, "reject"), `{}`, tag(rejected))
	got, _ := f.As(f.Approver).Must(200, "POST", path(rejected, "reject"), `{"comment": "Duplicate"}`, tag(rejected))
	if got["status"] != "rejected" {
		t.Fatalf("rejected: %v", got)
	}
	changes, _ := f.Must(200, "GET", "/v1/partner-changes?status=rejected", "", "")
	if len(changes["items"].([]any)) != 1 {
		t.Fatalf("rejected changes: %v", changes)
	}

	cancelled := partnerChange(t, f, "Cancelled Partner")
	f.As(f.Approver).Problem(403, "workflow.not_requester", "POST", path(cancelled, "cancel"), `{"comment": "Mine"}`, tag(cancelled))
	got, _ = f.Must(200, "POST", path(cancelled, "cancel"), `{"comment": "Withdrawn"}`, tag(cancelled))
	if got["status"] != "cancelled" {
		t.Errorf("status: got %v, want cancelled", got["status"])
	}
	if d := first(t, got["decisions"], "cancel decisions")["decision"]; d != "cancel" {
		t.Errorf("decision: got %v, want cancel", d)
	}
	f.Problem(412, "request.version_mismatch", "POST", path(got, "cancel"), `{"comment": "Again"}`, `"9"`)
	f.As(f.Approver).Problem(412, "request.version_mismatch", "POST", path(got, "reject"), `{"comment": "Late"}`, `"9"`)

	// The inbox: pending requests, newest first, filtered and paged.
	partnerChange(t, f, "Third")
	partnerChange(t, f, "Fourth")
	list := func(q string) []map[string]any {
		page, _ := f.Must(200, "GET", "/v1/approval-requests?"+q, "", "")
		var out []map[string]any
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any))
		}
		if tok, ok := page["next_page_token"].(string); ok {
			out = append(out, map[string]any{"next": tok})
		}
		return out
	}
	inbox := list("status=pending&object_type=partner_change")
	if len(inbox) != 2 {
		t.Fatalf("inbox: got %d requests, want 2: %v", len(inbox), inbox)
	}
	// Newest first: the tenant's fourth and third partner changes, the first
	// two having been rejected and cancelled above.
	for i, want := range []string{"PC00000004", "PC00000003"} {
		if title, _ := inbox[i]["title"].(string); !strings.Contains(title, want) {
			t.Errorf("inbox[%d] title: got %q, want one naming %s", i, title, want)
		}
	}
	page := list("page_size=3")
	if len(page) != 4 || page[3]["next"] == nil {
		t.Fatalf("page 1: %v", page)
	}
	if rest := list("page_size=3&page_token=" + page[3]["next"].(string)); len(rest) != 1 {
		t.Fatalf("page 2: %v", rest)
	}
	if asc := list("order_by=created_at%20asc&object_id=" + rejected["object_id"].(string)); len(asc) != 1 || asc[0]["status"] != "rejected" {
		t.Fatalf("by object: %v", asc)
	}
}

// TestSteps opens requisition requests directly: the amount selects the
// tier, every step needs its approvals, and nobody approves twice.
func TestSteps(t *testing.T) {
	f := setup(t)
	var calls []string
	d := approvals.Decider{Roles: identity.Directory{}, Handlers: map[string]approvals.Handler{approvals.Requisition: recorder{&calls}}}
	open := func(amount money.Minor) uuid.UUID {
		var id uuid.UUID
		eur := "EUR"
		if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			var needed bool
			var err error
			id, needed, err = fixture.Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: uuid.New(), Title: "PR",
				Amount: &amount, Currency: &eur, LegalEntityID: f.DE})
			if err == nil && !needed {
				t.Error("no approval needed")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// as reports to the tb it is given: the subtest's, inside a subtest.
	as := func(tb testing.TB, a pgtest.Actor, fn func(ctx context.Context) (approvals.Request, error)) (approvals.Request, error) {
		tb.Helper()
		var r approvals.Request
		err := apitest.Committed(tb, f.Tenant, a, func(ctx context.Context) (err error) {
			r, err = fn(ctx)
			return err
		})
		return r, err
	}
	any := func(int32) error { return nil }
	small := open(500000) // 5,000.00 EUR: one step
	r, err := as(t, f.Approver, func(ctx context.Context) (approvals.Request, error) { return approvals.GetRequest(ctx, small) })
	if err != nil {
		t.Fatalf("small: %v", err)
	}
	if len(r.Steps) != 1 {
		t.Errorf("small steps: got %d, want 1", len(r.Steps))
	}
	if r.Amount == nil || *r.Amount != 500000 {
		t.Errorf("small amount: got %v, want 500000", r.Amount)
	}
	large := open(5000000) // 50,000.00 EUR: two steps
	pgtest.Exec(t, `UPDATE workflow.approval_step SET required = 2 WHERE request_id = $1 AND step_no = 1`, large)
	if r, err = as(t, f.Approver, func(ctx context.Context) (approvals.Request, error) { return d.Approve(ctx, large, any, "") }); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	if r.CurrentStep != 1 {
		t.Errorf("first approval current step: got %d, want 1", r.CurrentStep)
	}
	if len(r.Steps) == 0 || r.Steps[0].Approvals != 1 {
		t.Errorf("first approval step 1: got %+v, want 1 approval", r.Steps)
	}
	if _, err = as(t, f.Approver, func(ctx context.Context) (approvals.Request, error) { return d.Approve(ctx, large, any, "") }); problem.From(err).Code != "workflow.already_approved" {
		t.Fatalf("second approval by the same person: %v", err)
	}
	if r, err = as(t, f.Approver2, func(ctx context.Context) (approvals.Request, error) { return d.Approve(ctx, large, any, "") }); err != nil || r.CurrentStep != 2 {
		t.Fatalf("step 1 complete: %+v %v", r, err)
	}
	third := person(t, f, "third", "all", "process_owner")
	if r, err = as(t, third, func(ctx context.Context) (approvals.Request, error) { return d.Approve(ctx, large, any, "fine") }); err != nil || r.Status != approvals.Approved {
		t.Fatalf("step 2 complete: %+v %v", r, err)
	}
	// Rejection and cancellation reach the owning context with their comments.
	if _, err = as(t, f.Approver, func(ctx context.Context) (approvals.Request, error) { return d.Reject(ctx, small, any, "too much") }); err != nil {
		t.Fatal(err)
	}
	withdrawn := open(100)
	if _, err = as(t, f.Tenant.Human, func(ctx context.Context) (approvals.Request, error) {
		return d.Cancel(ctx, withdrawn, any, "not needed")
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, "|") != "approved|rejected:too much|cancelled:not needed" {
		t.Fatalf("calls: %v", calls)
	}
	// An agent never decides, and an object type nobody owns cannot complete.
	agentReq := open(100)
	if _, err = as(t, f.Tenant.Agent, func(ctx context.Context) (approvals.Request, error) { return d.Approve(ctx, agentReq, any, "") }); problem.From(err).Code != "identity.agent_not_allowed" {
		t.Fatalf("agent: %v", err)
	}
	orphan := approvals.Decider{Roles: identity.Directory{}, Handlers: map[string]approvals.Handler{}}
	for name, fn := range map[string]func(ctx context.Context) (approvals.Request, error){
		"approve": func(ctx context.Context) (approvals.Request, error) { return orphan.Approve(ctx, agentReq, any, "") },
		"reject":  func(ctx context.Context) (approvals.Request, error) { return orphan.Reject(ctx, agentReq, any, "r") },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := as(t, f.Approver, fn)
			pgtest.CheckErr(t, err, "no handler")
		})
	}
	if _, err = as(t, f.Tenant.Human, func(ctx context.Context) (approvals.Request, error) { return orphan.Cancel(ctx, agentReq, any, "c") }); err == nil {
		t.Error("cancel without a handler")
	}
	// Withdrawing completes without calling the owner; with nothing pending it does nothing.
	if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		if err := fixture.Approvals().Withdraw(ctx, approvals.Requisition, uuid.New(), "none"); err != nil {
			return err
		}
		var objectID uuid.UUID
		if r, err := approvals.GetRequest(ctx, agentReq); err == nil {
			objectID = r.ObjectID
		}
		return fixture.Approvals().Withdraw(ctx, approvals.Requisition, objectID, "superseded")
	}); err != nil {
		t.Fatal(err)
	}
	if r, _ = as(t, f.Approver, func(ctx context.Context) (approvals.Request, error) { return approvals.GetRequest(ctx, agentReq) }); r.Status != approvals.Cancelled {
		t.Errorf("withdrawn status: got %v, want %v", r.Status, approvals.Cancelled)
	}
	if len(calls) != 3 {
		t.Errorf("owner calls after withdrawing: got %v, want 3 calls", calls)
	}
}

func TestOpenRefusals(t *testing.T) {
	f := setup(t)
	open := func(in approvals.NewRequest) (bool, error) {
		var needed bool
		err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
			_, needed, err = fixture.Approvals().Open(ctx, in)
			return err
		})
		return needed, err
	}
	// Tier 1 configuration changes need no approval.
	if needed, err := open(approvals.NewRequest{ObjectType: approvals.ConfigChange, ObjectID: uuid.New(), Title: "T", Tier: 1}); needed || err != nil {
		t.Fatalf("tier 1: %v %v", needed, err)
	}
	if needed, err := open(approvals.NewRequest{ObjectType: approvals.ConfigChange, ObjectID: uuid.New(), Title: "T", Tier: 3}); !needed || err != nil {
		t.Fatalf("tier 3: %v %v", needed, err)
	}
	if err := fixture.Approvals().Withdraw(context.Background(), approvals.ConfigChange, uuid.New(), "c"); err == nil {
		t.Fatal("withdraw outside a unit of work")
	}
	if needed, err := open(approvals.NewRequest{ObjectType: approvals.InvoiceException, ObjectID: uuid.New(), Title: "T"}); !needed || err != nil {
		t.Fatalf("invoice exception: %v %v", needed, err)
	}
	if _, err := open(approvals.NewRequest{ObjectType: "payment_run", ObjectID: uuid.New(), Title: "T"}); err == nil {
		t.Fatal("unknown object type")
	}
	pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE tenant_id = $1 AND code = 'process_owner'`, f.Tenant.ID)
	if _, err := open(approvals.NewRequest{ObjectType: approvals.InvoiceException, ObjectID: uuid.New(), Title: "T"}); problem.From(err).Code != "workflow.unknown_role" {
		t.Fatalf("inactive role: %v", err)
	}
	// One pending request per object.
	object := uuid.New()
	if err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		in := approvals.NewRequest{ObjectType: approvals.PartnerChange, ObjectID: object, Title: "T"}
		if _, _, err := fixture.Approvals().Open(ctx, in); err != nil {
			return err
		}
		_, _, err := fixture.Approvals().Open(ctx, in)
		return err
	}); problem.From(err).Code != "workflow.request_pending" {
		t.Fatalf("second request: %v", err)
	}
	for name, code := range map[string]string{"decision_one_approval_per_actor": "workflow.already_approved"} {
		t.Run(name, func(t *testing.T) {
			if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: name}); p.Code != code {
				t.Errorf("problem code: got %s, want %s", p.Code, code)
			}
		})
	}
	// A tenant without configuration cannot open requests.
	bare := pgtest.NewTenant(t)
	if err := apitest.InRequest(t, bare, bare.Human, func(ctx context.Context) error {
		_, _, err := fixture.Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.PartnerChange, ObjectID: uuid.New(), Title: "T"})
		return err
	}); problem.From(err).Code != "config.not_bootstrapped" {
		t.Fatalf("no configuration: %v", err)
	}
}

func TestDatabaseFailures(t *testing.T) {
	f := setup(t)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	var calls []string
	handlers := map[string]approvals.Handler{approvals.Requisition: recorder{&calls}}
	e := &approvals.Endpoints{Pages: pages, Decider: approvals.Decider{Roles: identity.Directory{}, Handlers: handlers}}
	eur, amount := "EUR", money.Minor(100)
	newRequest := func(ctx context.Context) (uuid.UUID, error) {
		id, _, err := fixture.Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: uuid.New(), Title: "T",
			Amount: &amount, Currency: &eur, LegalEntityID: f.DE})
		return id, err
	}
	var ids []uuid.UUID
	for range 4 {
		if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			id, err := newRequest(ctx)
			ids = append(ids, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	calls2 := map[string]struct {
		as   pgtest.Actor
		call func(ctx context.Context) error
	}{
		"open": {f.Tenant.Human, func(ctx context.Context) error { _, err := newRequest(ctx); return err }},
		"get": {f.Approver, func(ctx context.Context) error {
			_, err := e.GetApprovalRequest(ctx, gen.GetApprovalRequestParams{ID: ids[0]})
			return err
		}},
		"list": {f.Approver, func(ctx context.Context) error {
			_, err := e.ListApprovalRequests(ctx, gen.ListApprovalRequestsParams{ObjectID: gen.NewOptUUID(uuid.New())})
			return err
		}},
		"inbox": {f.Approver, func(ctx context.Context) error {
			_, err := e.ListApprovalRequests(ctx, gen.ListApprovalRequestsParams{DecidableBy: gen.NewOptString("me")})
			return err
		}},
		"approve": {f.Approver, func(ctx context.Context) error {
			_, err := e.ApproveApprovalRequest(ctx, gen.OptDecisionInput{}, gen.ApproveApprovalRequestParams{ID: ids[0], IfMatch: "*"})
			return err
		}},
		"reject": {f.Approver, func(ctx context.Context) error {
			_, err := e.RejectApprovalRequest(ctx, &gen.CommentRequiredInput{Comment: "no"}, gen.RejectApprovalRequestParams{ID: ids[1], IfMatch: "*"})
			return err
		}},
		"cancel": {f.Tenant.Human, func(ctx context.Context) error {
			_, err := e.CancelApprovalRequest(ctx, &gen.CommentRequiredInput{Comment: "no"}, gen.CancelApprovalRequestParams{ID: ids[2], IfMatch: "*"})
			return err
		}},
		"withdraw": {f.Tenant.Human, func(ctx context.Context) error {
			r, err := approvals.GetRequest(ctx, ids[3])
			if err != nil {
				return err
			}
			return fixture.Approvals().Withdraw(ctx, approvals.Requisition, r.ObjectID, "gone")
		}},
	}
	for name, c := range calls2 {
		t.Run(name, func(t *testing.T) {
			if err := c.call(context.Background()); err == nil {
				t.Errorf("%s outside a unit of work: got nil, want an error", name)
			}
			apitest.EveryFailure(t, f.Tenant, c.as, name, c.call)
		})
	}
	// A two-step request moving to its second step fails like the others.
	two := money.Minor(5000000)
	var multi uuid.UUID
	if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
		multi, _, err = fixture.Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: uuid.New(), Title: "T",
			Amount: &two, Currency: &eur, LegalEntityID: f.DE})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	apitest.EveryFailure(t, f.Tenant, f.Approver, "next step", func(ctx context.Context) error {
		_, err := e.Approve(ctx, multi, func(int32) error { return nil }, "")
		return err
	})
	if _, err := e.Approve(context.Background(), multi, nil, ""); err == nil {
		t.Error("approve outside a unit of work")
	}
	if err := apitest.InRequest(t, f.Tenant, f.Approver, func(ctx context.Context) error {
		_, err := e.ListApprovalRequests(ctx, gen.ListApprovalRequestsParams{PageSize: gen.NewOptInt32(0)})
		return err
	}); err == nil {
		t.Error("page_size 0 accepted")
	}
}
