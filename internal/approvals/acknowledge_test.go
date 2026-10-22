package approvals_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// acknowledger is an owning context that asks approvers to acknowledge
// what, or fails with err when asked.
type acknowledger struct {
	recorder
	what string
	err  error
}

func (a acknowledger) Acknowledgement(context.Context, uuid.UUID) (string, error) {
	return a.what, a.err
}

// TestAcknowledgement: when the owning context asks approvers to
// acknowledge something, an approval without a comment is refused and
// the request stays pending; a comment, or nothing to acknowledge, lets
// it through (T72l).
func TestAcknowledgement(t *testing.T) {
	f := setup(t)
	var calls []string
	decider := func(what string, err error) approvals.Decider {
		return approvals.Decider{Roles: identity.Directory{}, Handlers: map[string]approvals.Handler{
			approvals.Requisition: acknowledger{recorder: recorder{&calls}, what: what, err: err}}}
	}
	open := func(t *testing.T) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		amount, eur := money.Minor(100), "EUR"
		if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
			id, _, err = fixture.Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: uuid.New(), Title: "PR",
				Amount: &amount, Currency: &eur, LegalEntityID: f.DE})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	approve := func(t *testing.T, d approvals.Decider, id uuid.UUID, comment string) (approvals.Request, error) {
		t.Helper()
		var r approvals.Request
		err := apitest.Committed(t, f.Tenant, f.Approver, func(ctx context.Context) (err error) {
			r, err = d.Approve(ctx, id, func(int32) error { return nil }, comment)
			return err
		})
		return r, err
	}
	errDown := errors.New("down")
	cases := map[string]struct {
		what    string
		err     error
		comment string
		code    string // the refusal's problem code; "" approves
		fails   error
	}{
		"nothing to acknowledge":    {},
		"acknowledged":              {what: "Say so.", comment: "I accept it."},
		"no comment":                {what: "Say so.", code: "workflow.acknowledgement_required"},
		"a blank comment":           {what: "Say so.", comment: "  ", code: "workflow.acknowledgement_required"},
		"the owning context failed": {err: errDown, fails: errDown},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			id := open(t)
			r, err := approve(t, decider(c.what, c.err), id, c.comment)
			switch {
			case c.fails != nil:
				if !errors.Is(err, c.fails) {
					t.Fatalf("approval: got error %v, want %v", err, c.fails)
				}
			case c.code != "":
				if got := problem.From(err).Code; got != c.code {
					t.Fatalf("approval: got %v, want %s", err, c.code)
				}
				var n int
				pgtest.Scan(t, `SELECT count(*) FROM workflow.decision WHERE request_id = $1`, []any{id}, &n)
				if n != 0 {
					t.Errorf("decisions after the refusal: got %d, want 0", n)
				}
			case err != nil:
				t.Fatalf("approval: %v", err)
			case r.Status != approvals.Approved:
				t.Errorf("status: got %s, want %s", r.Status, approvals.Approved)
			}
		})
	}
}
