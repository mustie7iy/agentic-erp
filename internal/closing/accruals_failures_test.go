package closing_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// input is a proposal of the amount on the account resting on the
// documents, with the supplier and centers given (uuid.Nil: none).
func (a *accruing) input(account uuid.UUID, amount string, supplier, cost, profit uuid.UUID, evidence ...gen.AccrualEvidenceInput) *gen.AccrualProposalInput {
	in := &gen.AccrualProposalInput{SessionID: uuid.MustParse(a.session), CloseID: uuid.MustParse(a.close), AccountID: account,
		Amount: gen.Money{Amount: amount, Currency: "EUR"}, Description: "Courier of March", Reason: "The courier invoices monthly.", Evidence: evidence}
	for _, o := range []struct {
		id  uuid.UUID
		opt *gen.OptUUID
	}{{supplier, &in.SupplierID}, {cost, &in.CostCenterID}, {profit, &in.ProfitCenterID}} {
		if o.id != uuid.Nil {
			o.opt.SetTo(o.id)
		}
	}
	return in
}

// TestAccrualDatabaseFailures calls each handler of the proposals with
// each of their database statements failing in turn, and outside a unit
// of work.
func TestAccrualDatabaseFailures(t *testing.T) {
	a := newAccruing(t)
	invoice := gen.AccrualEvidenceInput{Kind: "supplier_invoice", DocumentID: a.invoice}
	retried := a.input(a.expense, "100.00", a.Supplier, a.centers.Cost, uuid.Nil, invoice)
	h := endpoints(t)
	var first gen.ProposeAccrualRes
	if err := apitest.Committed(t, a.Tenant, a.Tenant.Agent, func(ctx context.Context) error {
		var err error
		first, err = h.ProposeAccrual(ctx, retried, gen.ProposeAccrualParams{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	proposed := first.(*gen.ProposeAccrualCreatedHeaders).Response.ID
	every := a.input(a.Account(t, "2000", "530000"), "20.00", a.Supplier, a.centers.Cost, a.centers.Profit, invoice,
		gen.AccrualEvidenceInput{Kind: "purchase_order", DocumentID: a.order}, gen.AccrualEvidenceInput{Kind: "journal_entry", DocumentID: a.entry})
	calls := map[string]struct {
		as pgtest.Actor
		fn func(ctx context.Context) error
	}{
		"propose": {a.Tenant.Agent, func(ctx context.Context) error {
			_, err := h.ProposeAccrual(ctx, every, gen.ProposeAccrualParams{})
			return err
		}},
		"propose again": {a.Tenant.Agent, func(ctx context.Context) error {
			_, err := h.ProposeAccrual(ctx, retried, gen.ProposeAccrualParams{})
			return err
		}},
		"get": {a.Tenant.Human, func(ctx context.Context) error {
			_, err := h.GetAccrualProposal(ctx, gen.GetAccrualProposalParams{ID: proposed})
			return err
		}},
		"list": {a.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListAccrualProposals(ctx, gen.ListAccrualProposalsParams{PageSize: gen.NewOptInt32(1)})
			return err
		}},
	}
	for name, k := range calls {
		t.Run(name, func(t *testing.T) {
			apitest.EveryFailure(t, a.Tenant, k.as, name, k.fn)
			if err := k.fn(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
	err := apitest.InRequest(t, a.Tenant, a.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListAccrualProposals(ctx, gen.ListAccrualProposalsParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "page") {
		t.Errorf("page size 0: got %v, want the page size refused", err)
	}
}

// TestAccrualBrokenData: a proposal whose agent the identity context does
// not hold, or in a currency the reference data lacks, fails the response
// rather than showing it without a name or in other digits; a close whose
// company the tenancy context does not hold fails a proposal.
func TestAccrualBrokenData(t *testing.T) {
	a := newAccruing(t)
	p, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.proposal("100.00"), "")
	id := uuid.MustParse(apitest.String(t, p, "id"))
	h := endpoints(t)
	get := func() error {
		return apitest.InRequest(t, a.Tenant, a.Tenant.Human, func(ctx context.Context) error {
			_, err := h.GetAccrualProposal(ctx, gen.GetAccrualProposalParams{ID: id})
			return err
		})
	}
	pgtest.Unguarded(t, `UPDATE closing.accrual_proposal SET agent_id = $2 WHERE id = $1`, id, uuid.New())
	if err := get(); err == nil || !strings.Contains(err.Error(), "not held by the identity context") {
		t.Errorf("an unknown agent: got %v, want the identity context's failure", err)
	}
	pgtest.Unguarded(t, `UPDATE closing.accrual_proposal SET agent_id = $2, currency = 'XXZ' WHERE id = $1`, id, a.Tenant.Agent.ID)
	if err := get(); err == nil || !strings.Contains(err.Error(), "currency of accrual proposal") {
		t.Errorf("an unknown currency: got %v, want the currency's failure", err)
	}
	pgtest.Unguarded(t, `UPDATE closing.period_close SET legal_entity_id = $2 WHERE id = $1`, uuid.MustParse(a.close), uuid.New())
	err := apitest.InRequest(t, a.Tenant, a.Tenant.Agent, func(ctx context.Context) error {
		_, err := h.ProposeAccrual(ctx, a.input(a.Account(t, "2000", "530000"), "1.00", uuid.Nil, uuid.Nil, uuid.Nil,
			gen.AccrualEvidenceInput{Kind: "journal_entry", DocumentID: a.entry}), gen.ProposeAccrualParams{})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not held by the tenancy context") {
		t.Errorf("a close of an unknown company: got %v, want the tenancy context's failure", err)
	}
}

// TestAccrualOnSignedOffClose: a close signed off with its periods locked
// refuses a proposal; once a covered period is unlocked, the first
// proposal sets it open again and is parked.
func TestAccrualOnSignedOffClose(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withSignOff(doc)
		withAccruals(doc)
	}, append(append([]string{}, signPermissions...), "closing.accrual.propose", "agents.session.record")...)
	entry := e.PostedBy(t, "", "2026-01-15", 10000)
	s := signable(t, e, 1)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	agent := e.As(e.Tenant.Agent)
	session, _ := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Accrue", "model": "erp-default"}`, "")
	a := &accruing{Env: e, close: strings.TrimPrefix(s.path, "/v1/period-closes/"), session: apitest.String(t, session, "id"), agent: agent,
		expense: e.Account(t, "2000", "520000")}
	body := a.body(a.expense, "10.00", a.session, [][2]string{{"journal_entry", entry.String()}}, "")
	s.periods(t, "close")
	s.read(t)
	s.signOff(t, "Signed")
	agent.Problem(409, "closing.close_signed_off", "POST", "/v1/accrual-proposals", body, "")
	for _, x := range apitest.List(t, s.read(t), "ledgers") {
		if l := x.(map[string]any); l["ledger_code"] == "2L" {
			period := "/v1/fiscal-periods/" + l["fiscal_period_id"].(string)
			_, ptag := e.Must(200, "GET", period, "", "")
			unlock, _ := e.Must(200, "POST", period+":unlock", `{"reason": "Audit adjustment"}`, ptag)
			e.Approve(t, unlock["id"].(string))
		}
	}
	h := endpoints(t)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Agent, "propose, reopening", func(ctx context.Context) error {
		_, err := h.ProposeAccrual(ctx, a.input(a.expense, "10.00", uuid.Nil, uuid.Nil, uuid.Nil,
			gen.AccrualEvidenceInput{Kind: "journal_entry", DocumentID: entry}), gen.ProposeAccrualParams{})
		return err
	})
	agent.Must(201, "POST", "/v1/accrual-proposals", body, "")
	wantFields(t, "the close after the proposal", s.read(t), map[string]any{"status": "open"})
}
