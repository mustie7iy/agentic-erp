package closing_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestVarianceCommentDatabaseFailures calls each handler of the comments,
// the report with comments on its rows and the check, with each of their
// database statements failing in turn, and outside a unit of work.
func TestVarianceCommentDatabaseFailures(t *testing.T) {
	c := newCommenting(t)
	jan26 := uuid.MustParse(c.entryOn(t, 2026, 1, "520000"))
	session := uuid.MustParse(c.session)
	close := uuid.MustParse(c.close)
	supplies := c.Account(t, "2000", "520000")
	retried := &gen.VarianceCommentProposal{SessionID: session, CloseID: close, Basis: "prior_period", AccountID: supplies, Text: "Retried.",
		Citations: []uuid.UUID{jan26}}
	p, tag := c.agent.Must(201, "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", "Retried.", []string{jan26.String()}, c.session), "")
	proposed := uuid.MustParse(apitest.String(t, p, "id"))
	c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_year", "520000", "Accepted before.", nil, ""), "")
	_, closeTag := c.Must(200, "GET", "/v1/period-closes/"+c.close, "", "")
	h := endpoints(t)
	type call struct {
		as pgtest.Actor
		fn func(ctx context.Context) error
	}
	calls := map[string]call{
		"propose": {c.Tenant.Agent, func(ctx context.Context) error {
			_, err := h.ProposeVarianceComment(ctx, &gen.VarianceCommentProposal{SessionID: session, CloseID: close, Basis: "prior_year",
				AccountID: c.Account(t, "2000", "400000"), Text: "New.", Citations: []uuid.UUID{jan26}}, gen.ProposeVarianceCommentParams{})
			return err
		}},
		"propose again": {c.Tenant.Agent, func(ctx context.Context) error {
			_, err := h.ProposeVarianceComment(ctx, retried, gen.ProposeVarianceCommentParams{})
			return err
		}},
		"create, superseding": {c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.CreateVarianceComment(ctx, &gen.VarianceCommentInput{CloseID: close, Basis: "prior_year", AccountID: supplies, Text: "Later.",
				Citations: []uuid.UUID{jan26}}, gen.CreateVarianceCommentParams{})
			return err
		}},
		"accept": {c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.AcceptVarianceComment(ctx, gen.NewOptVarianceCommentAccept(gen.VarianceCommentAccept{Text: gen.NewOptString("Edited.")}),
				gen.AcceptVarianceCommentParams{ID: proposed, IfMatch: tag})
			return err
		}},
		"reject": {c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.RejectVarianceComment(ctx, &gen.ReasonInput{Reason: "No."}, gen.RejectVarianceCommentParams{ID: proposed, IfMatch: tag})
			return err
		}},
		"get": {c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.GetVarianceComment(ctx, gen.GetVarianceCommentParams{ID: proposed})
			return err
		}},
		"list": {c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListVarianceComments(ctx, gen.ListVarianceCommentsParams{PageSize: gen.NewOptInt32(1)})
			return err
		}},
		"report": {c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListPeriodCloseVariances(ctx, gen.ListPeriodCloseVariancesParams{ID: close, Basis: "prior_year"})
			return err
		}},
		"check": {c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.CheckCloseTask(ctx, gen.CheckCloseTaskParams{ID: close, Code: "EXPLAINED", IfMatch: closeTag})
			return err
		}},
	}
	for name, k := range calls {
		t.Run(name, func(t *testing.T) {
			apitest.EveryFailure(t, c.Tenant, k.as, name, k.fn)
			if err := k.fn(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
	err := apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListVarianceComments(ctx, gen.ListVarianceCommentsParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	if got := problem.From(err).Code; got != "request.invalid_page_size" {
		t.Errorf("page size 0: got %s, want request.invalid_page_size", got)
	}
}

// TestVarianceCommentBrokenData: a comment whose author the identity
// context does not hold, or in a currency the reference data lacks, fails
// the response rather than showing it without a name or in other digits.
func TestVarianceCommentBrokenData(t *testing.T) {
	c := newCommenting(t)
	written, _ := c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_year", "400000", "First.", nil, ""), "")
	id := uuid.MustParse(apitest.String(t, written, "id"))
	h := endpoints(t)
	get := func() error {
		return apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.GetVarianceComment(ctx, gen.GetVarianceCommentParams{ID: id})
			return err
		})
	}
	report := func() error {
		return apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListPeriodCloseVariances(ctx, gen.ListPeriodCloseVariancesParams{ID: uuid.MustParse(c.close), Basis: "prior_year"})
			return err
		})
	}
	pgtest.Unguarded(t, `UPDATE closing.variance_comment SET author_id = $2, decided_by = $2 WHERE id = $1`, id, uuid.New())
	for name, call := range map[string]func() error{"the comment": get, "the report": report} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not held by the identity context") {
			t.Errorf("%s with an unknown author: got %v, want the identity context's failure", name, err)
		}
	}
	pgtest.Unguarded(t, `UPDATE closing.variance_comment SET author_id = $2, decided_by = $2, currency = 'XXZ' WHERE id = $1`, id, c.Tenant.Human.ID)
	if err := get(); err == nil || !strings.Contains(err.Error(), "currency of variance comment") {
		t.Errorf("a comment in an unknown currency: got %v, want the currency's failure", err)
	}
}

// TestVarianceCommentOnSignedOffClose: a close signed off with its periods
// locked refuses every comment write; once a covered period is unlocked,
// the first comment written sets it open again.
func TestVarianceCommentOnSignedOffClose(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withSignOff(doc)
		doc.Company("2000").Close.Variance = &config.CloseVariance{Bases: []string{config.BasisPriorPeriod}, ThresholdAmount: "100.00"}
	}, append(append([]string{}, signPermissions...), "closing.variance.propose", "closing.variance.comment", "agents.session.record")...)
	post(t, e, "2026-01-20", map[string]money.Minor{"520000": 150000})
	s := signable(t, e, 1)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	agent := e.As(e.Tenant.Agent)
	session, _ := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Explain", "model": "erp-default"}`, "")
	c := &commenting{Env: e, close: strings.TrimPrefix(s.path, "/v1/period-closes/"), session: apitest.String(t, session, "id"), agent: agent}
	p, tag := agent.Must(201, "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", "Supplies.", nil, c.session), "")
	s.periods(t, "close")
	s.read(t)
	s.signOff(t, "Signed")
	path := "/v1/variance-comments/" + apitest.String(t, p, "id")
	e.Problem(409, "closing.close_signed_off", "POST", "/v1/variance-comments", c.body(t, "prior_period", "520000", "Supplies.", nil, ""), "")
	agent.Problem(409, "closing.close_signed_off", "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", "Other.", nil, c.session), "")
	e.Problem(409, "closing.close_signed_off", "POST", path+":acceptProposal", "", tag)
	e.Problem(409, "closing.close_signed_off", "POST", path+":rejectProposal", `{"reason": "No"}`, tag)

	for _, x := range apitest.List(t, s.read(t), "ledgers") {
		if l := x.(map[string]any); l["ledger_code"] == "2L" {
			period := "/v1/fiscal-periods/" + l["fiscal_period_id"].(string)
			_, ptag := e.Must(200, "GET", period, "", "")
			unlock, _ := e.Must(200, "POST", period+":unlock", `{"reason": "Audit adjustment"}`, ptag)
			e.Approve(t, unlock["id"].(string))
		}
	}
	h := endpoints(t)
	id, closeID := uuid.MustParse(apitest.String(t, p, "id")), uuid.MustParse(c.close)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "accept, reopening", func(ctx context.Context) error {
		_, err := h.AcceptVarianceComment(ctx, gen.OptVarianceCommentAccept{}, gen.AcceptVarianceCommentParams{ID: id, IfMatch: tag})
		return err
	})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "write, reopening", func(ctx context.Context) error {
		_, err := h.CreateVarianceComment(ctx, &gen.VarianceCommentInput{CloseID: closeID, Basis: "prior_period", AccountID: e.Account(t, "2000", "520000"),
			Text: "Supplies."}, gen.CreateVarianceCommentParams{})
		return err
	})
	e.Must(200, "POST", path+":acceptProposal", "", tag)
	wantFields(t, "the close after the acceptance", s.read(t), map[string]any{"status": "open"})
}

// TestVarianceCommentsConcurrently: two people explaining one row at once
// wait on the close's lock; both comments are written, the second
// superseding the first, so the row keeps one accepted comment.
func TestVarianceCommentsConcurrently(t *testing.T) {
	c := newCommenting(t)
	h := endpoints(t)
	write := func(text string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CreateVarianceComment(ctx, &gen.VarianceCommentInput{CloseID: uuid.MustParse(c.close), Basis: "prior_period",
				AccountID: c.Account(t, "2000", "520000"), Text: text}, gen.CreateVarianceCommentParams{})
			return err
		}
	}
	first, second := apitest.Serialized(t, c.Tenant, c.Tenant.Human, write("First."), write("Second."))
	if first != nil || second != nil {
		t.Fatalf("got %v and %v, want both written", first, second)
	}
	page, _ := c.Must(200, "GET", "/v1/variance-comments?close_id="+c.close, "", "")
	var got []string
	for _, x := range apitest.List(t, page, "items") {
		m := x.(map[string]any)
		got = append(got, m["text"].(string)+" "+m["status"].(string))
	}
	wantLines(t, "the row's comments", got, []string{"First. superseded", "Second. accepted"})
}
