package approvals_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// barring is an owning context that bars one actor from approving each of
// some objects (approvals.Barrer), or fails to say.
type barring struct {
	recorder
	bars map[uuid.UUID]uuid.UUID // object -> the actor barred
	fail error
}

func (b barring) Barred(_ context.Context, object, actor uuid.UUID) (bool, error) {
	return b.bars[object] == actor, b.fail
}

// inboxEnv is TestInbox's tenant: requests opened in it and the endpoints
// that list them.
type inboxEnv struct {
	f    fixture.Env
	e    *approvals.Endpoints
	bars map[uuid.UUID]uuid.UUID
}

// open opens a requisition's request as requester, for amount (EUR minor
// units; 50,000.00 and above has two steps), and returns its id and object.
func (s inboxEnv) open(t *testing.T, requester pgtest.Actor, amount money.Minor) (id, object uuid.UUID) {
	t.Helper()
	eur, object := "EUR", uuid.New()
	if err := apitest.Committed(t, s.f.Tenant, requester, func(ctx context.Context) (err error) {
		id, _, err = fixture.Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: object, Title: "PR",
			Amount: &amount, Currency: &eur, LegalEntityID: s.f.DE})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id, object
}

// inbox lists a's inbox in pages of size and returns the requests' ids.
func (s inboxEnv) inbox(t *testing.T, a pgtest.Actor, size int32) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	params := gen.ListApprovalRequestsParams{DecidableBy: gen.NewOptString("me"), PageSize: gen.NewOptInt32(size)}
	for range 20 {
		var page *gen.ApprovalRequestPage
		if err := apitest.InRequest(t, s.f.Tenant, a, func(ctx context.Context) error {
			res, err := s.e.ListApprovalRequests(ctx, params)
			page, _ = res.(*gen.ApprovalRequestPage) // nil with err
			return err
		}); err != nil {
			t.Fatalf("inbox: %v", err)
		}
		for _, r := range page.Items {
			ids = append(ids, r.ID)
		}
		if !page.NextPageToken.Set {
			return ids
		}
		params.PageToken = page.NextPageToken
	}
	t.Fatal("inbox: more than 20 pages")
	return nil
}

// approves reports whether a may approve the request now, as the API
// decides it (the operation's authorization, then Approve), rolled back.
func (s inboxEnv) approves(t *testing.T, a pgtest.Actor, id uuid.UUID) error {
	t.Helper()
	az := authz.New(api.MustOperations())
	return apitest.InRequest(t, s.f.Tenant, a, func(ctx context.Context) error {
		if err := az.Authorize(ctx, "approveApprovalRequest"); err != nil {
			return err
		}
		_, err := s.e.Approve(ctx, id, func(int32) error { return nil }, "")
		return err
	})
}

// TestInbox: decidable_by=me lists the pending requests the caller may
// approve now, and only those: for every person and request, a request is
// in the inbox exactly when approving it would succeed (ADR-0052 G1). Pages
// keep their place when a request is barred.
func TestInbox(t *testing.T) {
	f := setup(t)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	s := inboxEnv{f: f, bars: map[uuid.UUID]uuid.UUID{}}
	var calls []string
	s.e = &approvals.Endpoints{Pages: pages, Decider: approvals.Decider{Roles: identity.Directory{},
		Handlers: map[string]approvals.Handler{approvals.Requisition: barring{recorder: recorder{&calls}, bars: s.bars}}}}
	plain, _ := s.open(t, f.Tenant.Human, 500000)
	twoSteps, _ := s.open(t, f.Tenant.Human, 5000000)
	pgtest.Exec(t, `UPDATE workflow.approval_step SET required = 2 WHERE request_id = $1 AND step_no = 1`, twoSteps)
	if err := apitest.Committed(t, f.Tenant, f.Approver, func(ctx context.Context) error {
		_, err := s.e.Approve(ctx, twoSteps, func(int32) error { return nil }, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	barred, object := s.open(t, f.Tenant.Human, 700)
	s.bars[object] = f.Approver.ID
	rejected, _ := s.open(t, f.Tenant.Human, 800)
	if err := apitest.Committed(t, f.Tenant, f.Approver, func(ctx context.Context) error {
		_, err := s.e.Reject(ctx, rejected, func(int32) error { return nil }, "no")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	own, _ := s.open(t, f.Approver, 900) // the approver's own request
	noDecide := s.rolesOnly(t, "roles without permissions")
	noObject := s.rolesOnly(t, "deciding nothing", "workflow.request.read", "workflow.request.decide")
	people := map[string]pgtest.Actor{"approver": f.Approver, "approver 2": f.Approver2, "requester": f.Tenant.Human, "agent": f.Tenant.Agent,
		"without the decide permission": noDecide, "without an approval permission": noObject}
	want := map[string][]uuid.UUID{"approver": {plain}, "approver 2": {plain, twoSteps, barred, own}}
	s.agrees(t, people, want, []uuid.UUID{plain, twoSteps, barred, rejected, own})
	// Holding both permissions of a segregation-of-duties rule bars the
	// decision itself: nothing is decidable.
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'workflow.request.cancel', 'workflow.request.decide', 'd', current_date, $2)`, f.Tenant.ID, f.Tenant.Human.ID)
	s.agrees(t, map[string]pgtest.Actor{"approver under a rule": f.Approver}, nil, []uuid.UUID{plain, twoSteps})
}

// rolesOnly is a person holding every role the configuration names, and a
// role of their own with the permissions.
func (s inboxEnv) rolesOnly(t *testing.T, name string, permissions ...string) pgtest.Actor {
	t.Helper()
	var roles []string
	pgtest.Scan(t, `SELECT array_agg(code) FROM identity.role WHERE tenant_id = $1 AND code NOT IN ('all', 'machine')`, []any{s.f.Tenant.ID}, &roles)
	code := "own_" + uuid.NewString()[:8]
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $2, 'active', $3, $3)`,
		s.f.Tenant.ID, code, s.f.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT r.tenant_id, gen_random_uuid(), r.id, p, current_date - 1, r.created_by FROM identity.role r, unnest($3::text[]) p WHERE r.tenant_id = $1 AND r.code = $2`,
		s.f.Tenant.ID, code, permissions)
	return person(t, s.f, name, append(roles, code)...)
}

// agrees checks each person's inbox, in pages of one and of fifty, against
// want, and each request against whether the person may approve it.
func (s inboxEnv) agrees(t *testing.T, people map[string]pgtest.Actor, want map[string][]uuid.UUID, requests []uuid.UUID) {
	t.Helper()
	for name, a := range people {
		t.Run(name, func(t *testing.T) {
			newest := slices.Clone(want[name])
			slices.Reverse(newest) // listed newest first
			for _, size := range []int32{1, 50} {
				if got := s.inbox(t, a, size); !slices.Equal(got, newest) {
					t.Errorf("inbox in pages of %d: got %v, want %v", size, got, newest)
				}
			}
			for _, id := range requests {
				if in, err := slices.Contains(want[name], id), s.approves(t, a, id); in != (err == nil) {
					t.Errorf("request %s: in the inbox %v, but approving it: %v", id, in, err)
				}
			}
		})
	}
}

// TestInboxRefusals: decidable_by takes me alone; with another status the
// inbox is empty; an owning context failing to say whether it bars the
// caller fails the list.
func TestInboxRefusals(t *testing.T) {
	f := setup(t)
	s := inboxEnv{f: f}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	fail := errors.New("bar unknown")
	s.e = &approvals.Endpoints{Pages: pages, Decider: approvals.Decider{Roles: identity.Directory{},
		Handlers: map[string]approvals.Handler{approvals.Requisition: barring{fail: fail}}}}
	s.open(t, f.Tenant.Human, 500000)
	as := f.As(f.Approver)
	as.Problem(400, "request.invalid_parameter", "GET", "/v1/approval-requests?decidable_by=you", "", "")
	if page, _ := as.Must(200, "GET", "/v1/approval-requests?decidable_by=me", "", ""); len(apitest.List(t, page, "items")) != 1 {
		t.Errorf("inbox through the API: got %v, want the request", page)
	}
	if page, _ := as.Must(200, "GET", "/v1/approval-requests?decidable_by=me&status=approved", "", ""); len(apitest.List(t, page, "items")) != 0 {
		t.Errorf("inbox of approved requests: got %v, want none", page)
	}
	err := apitest.InRequest(t, f.Tenant, f.Approver, func(ctx context.Context) error {
		_, err := s.e.ListApprovalRequests(ctx, gen.ListApprovalRequestsParams{DecidableBy: gen.NewOptString("me")})
		return err
	})
	if !errors.Is(err, fail) {
		t.Errorf("barrer failing: got %v, want %v", err, fail)
	}
	for _, op := range []string{"approveApprovalRequest", "rejectApprovalRequest"} {
		if got := api.MustOperations()[op].Permission; got != approvals.DecidePermission {
			t.Errorf("%s's x-permission: got %s, want approvals.DecidePermission %s", op, got, approvals.DecidePermission)
		}
	}
}

// TestInboxReadsPendingRequestsOnly: the inbox reads the pending requests,
// not the tenant's history of decided ones (04 §4.5: an index serves the
// filter and the sort, approval_request_pending). The list is planned per
// call, but runs six times before it is measured, as a cached statement
// would get its generic plan by then.
func TestInboxReadsPendingRequestsOnly(t *testing.T) {
	f := setup(t)
	s := inboxEnv{f: f}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	s.e = &approvals.Endpoints{Pages: pages, Decider: approvals.Decider{Roles: identity.Directory{}, Handlers: map[string]approvals.Handler{}}}
	for range 3 {
		s.open(t, f.Tenant.Human, 500000)
	}
	pgtest.Exec(t, `INSERT INTO workflow.approval_request (tenant_id, id, object_type, object_id, title, requester_id, status, completed_at, created_by)
		SELECT $1, gen_random_uuid(), 'requisition', gen_random_uuid(), 'old', $2, 'approved', now(), $2 FROM generate_series(1, 2000)`,
		f.Tenant.ID, f.Tenant.Human.ID)
	pgtest.Exec(t, `ANALYZE workflow.approval_request`)
	params := gen.ListApprovalRequestsParams{DecidableBy: gen.NewOptString("me")}
	if err := apitest.InRequest(t, f.Tenant, f.Approver, func(ctx context.Context) error {
		for range 6 {
			if _, err := s.e.ListApprovalRequests(ctx, params); err != nil {
				return err
			}
		}
		tx, _ := reqctx.TxFrom(ctx) // InRequest always puts its transaction in the context
		before := pgtest.RowsRead(t, ctx, tx, "workflow", `approval\_request`)
		res, err := s.e.ListApprovalRequests(ctx, params)
		if err != nil {
			return err
		}
		if got := pgtest.RowsRead(t, ctx, tx, "workflow", `approval\_request`) - before; got > 3 {
			t.Errorf("requests read for an inbox of 3 among 2003: got %d, want at most 3", got)
		}
		if page, _ := res.(*gen.ApprovalRequestPage); page == nil || len(page.Items) != 3 {
			t.Errorf("inbox: got %v, want 3 requests", res)
		}
		return nil
	}); err != nil {
		t.Fatalf("inbox: %v", err)
	}
}

// TestRequestedBy: requested_by=me lists the requests the caller asked
// for, in any status unless status says one, newest first and in pages;
// it takes me alone; and it reads the caller's requests through its index
// (approval_request_by_requester), not the tenant's (04 §4.5).
func TestRequestedBy(t *testing.T) {
	f := setup(t)
	s := inboxEnv{f: f}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	var calls []string
	s.e = &approvals.Endpoints{Pages: pages, Decider: approvals.Decider{Roles: identity.Directory{},
		Handlers: map[string]approvals.Handler{approvals.Requisition: recorder{&calls}}}}
	first, _ := s.open(t, f.Tenant.Human, 500000)
	rejected, _ := s.open(t, f.Tenant.Human, 800)
	if err := apitest.Committed(t, f.Tenant, f.Approver, func(ctx context.Context) error {
		_, err := s.e.Reject(ctx, rejected, func(int32) error { return nil }, "no")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	theirs, _ := s.open(t, f.Approver, 900)
	list := func(a pgtest.Actor, query string) []uuid.UUID {
		t.Helper()
		page, _ := f.As(a).Must(200, "GET", "/v1/approval-requests?"+query, "", "")
		var ids []uuid.UUID
		for _, it := range apitest.List(t, page, "items") {
			ids = append(ids, uuid.MustParse(it.(map[string]any)["id"].(string)))
		}
		return ids
	}
	for name, tc := range map[string]struct {
		who   pgtest.Actor
		query string
		want  []uuid.UUID
	}{
		"the requester's":             {f.Tenant.Human, "requested_by=me", []uuid.UUID{rejected, first}},
		"the requester's pending":     {f.Tenant.Human, "requested_by=me&status=pending", []uuid.UUID{first}},
		"the requester's, page of 1":  {f.Tenant.Human, "requested_by=me&page_size=1", []uuid.UUID{rejected}},
		"the approver's":              {f.Approver, "requested_by=me", []uuid.UUID{theirs}},
		"the approver's, to decide":   {f.Approver, "requested_by=me&decidable_by=me", nil},
		"another approver's":          {f.Approver2, "requested_by=me", nil},
		"everyone's, as the approver": {f.Approver, "", []uuid.UUID{theirs, rejected, first}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := list(tc.who, tc.query); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	f.As(f.Tenant.Human).Problem(400, "request.invalid_parameter", "GET", "/v1/approval-requests?requested_by=you", "", "")
	pgtest.Exec(t, `INSERT INTO workflow.approval_request (tenant_id, id, object_type, object_id, title, requester_id, status, completed_at, created_by)
		SELECT $1, gen_random_uuid(), 'requisition', gen_random_uuid(), 'old', $2, 'approved', now(), $2 FROM generate_series(1, 2000)`,
		f.Tenant.ID, f.Approver2.ID)
	pgtest.Exec(t, `ANALYZE workflow.approval_request`)
	params := gen.ListApprovalRequestsParams{RequestedBy: gen.NewOptString("me")}
	if err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		for range 6 {
			if _, err := s.e.ListApprovalRequests(ctx, params); err != nil {
				return err
			}
		}
		tx, _ := reqctx.TxFrom(ctx) // InRequest always puts its transaction in the context
		before := pgtest.RowsRead(t, ctx, tx, "workflow", `approval\_request`)
		if _, err := s.e.ListApprovalRequests(ctx, params); err != nil {
			return err
		}
		if got := pgtest.RowsRead(t, ctx, tx, "workflow", `approval\_request`) - before; got > 2 {
			t.Errorf("requests read for 2 of the requester's among 2003: got %d, want at most 2", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("requested_by=me: %v", err)
	}
}
