package closeagent_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/closeagent"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"closing.close.read", "closing.close.manage", "closing.accrual.propose", "closing.variance.propose", "ledger.entry.read", "ledger.entry.park",
	"ledger.account.read", "payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "procurement.order.read",
	"partner.supplier.read", "agents.session.read", "agents.session.record"}

var (
	sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)
	closeRe   = regexp.MustCompile(`Period close ([0-9a-f-]{36})`)
)

// withAccruals gives the tenant a close template of one task and company
// 2000 an accrual account, 219500, and its variances against the prior
// period and year from 50.00 (ADR-0058 §3).
func withAccruals(doc *config.Config) {
	doc.Close.Tasks = []config.CloseTask{{Code: "ACCRUALS", Name: "Accruals decided", Kind: config.CloseManual, DueDay: 2}}
	co := doc.Company("2000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "219500", Name: "Accrued liabilities", Type: config.Liability})
	co.Close.AccrualAccount = "219500"
	co.Close.Variance = &config.CloseVariance{Bases: []string{config.BasisPriorPeriod, config.BasisPriorYear}, ThresholdAmount: "50.00"}
}

// accruing is company 2000's close of March 2026, a posted invoice of the
// supplier to cite, its journal entry, and the agent's client of the API.
type accruing struct {
	fixture.Env
	close   string
	invoice uuid.UUID
	entry   string
	api     *apitools.Client
}

func setup(t *testing.T) accruing {
	t.Helper()
	f := fixture.NewWith(t, nil, withAccruals, perms...)
	f.Tenant.AllCompanies(t, f.Tenant.Agent)
	c, _ := f.Must(201, "POST", "/v1/period-closes", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "fiscal_period": 3}`, f.DE), "")
	inv := f.SupplierInvoice(t, f.Supplier, "COURIER-02", "119.00", "19.00", f.AccountItem(t, "Courier of February", "100.00"))
	_, tag := f.Must(200, "GET", "/v1/supplier-invoices/"+inv.String(), "", "")
	posted, _ := f.Must(200, "POST", "/v1/supplier-invoices/"+inv.String()+":post", `{"posting_date": "2026-03-12"}`, tag)
	srv := httptest.NewServer(f.API.Handler)
	t.Cleanup(srv.Close)
	return accruing{Env: f, close: apitest.String(t, c, "id"), invoice: inv, entry: apitest.String(t, posted, "journal_entry_id"),
		api: &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}}
}

// proposal is the model's proposal for the close: the conversation's
// session, the close named (or another), 100.00 on office supplies citing
// the invoice, or the evidence given.
func (a accruing) proposal(t *testing.T, closeID string, evidence string) func(string) any {
	t.Helper()
	expense := a.Account(t, "2000", "520000")
	return func(conversation string) any {
		if closeID == "" {
			closeID = closeRe.FindStringSubmatch(conversation)[1]
		}
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "close_id": closeID,
			"account_id": expense.String(), "supplier_id": a.Supplier.String(), "amount": map[string]any{"amount": "100.00", "currency": "EUR"},
			"description": "Courier of March", "reason": "The courier invoices monthly; March's has not arrived.",
			"evidence": []any{map[string]any{"kind": "supplier_invoice", "document_id": evidence}}}}
	}
}

// checks are values and what each should be, checked as subtests.
type checks map[string][2]any

func (cs checks) check(t *testing.T) {
	t.Helper()
	for name, c := range cs {
		t.Run(name, func(t *testing.T) {
			if c[0] != c[1] {
				t.Errorf("got %v, want %v", c[0], c[1])
			}
		})
	}
}

// TestAccruals: one session on the close; the model reads it and proposes;
// a proposal for another close is refused before the API and one the API
// refuses, each counted refused; the one it accepts is reported with its
// parked entry, once though the model retries it; the last text is the
// reply (ADR-0058 §12, T64g).
func TestAccruals(t *testing.T) {
	a := setup(t)
	model := llmtest.New(t,
		llmtest.ToolFn("getPeriodClose", func(conversation string) any {
			return map[string]any{"id": closeRe.FindStringSubmatch(conversation)[1]}
		}),
		llmtest.ToolFn("proposeAccrual", a.proposal(t, uuid.NewString(), a.invoice.String())),
		llmtest.ToolFn("proposeAccrual", a.proposal(t, "", uuid.NewString())),
		llmtest.Tool("proposeAccrual", map[string]any{}),
		llmtest.ToolFn("proposeAccrual", a.proposal(t, "", a.invoice.String())),
		llmtest.ToolFn("proposeAccrual", a.proposal(t, "", a.invoice.String())),
		llmtest.Final("Proposed March's courier accrual."))
	res, err := (&closeagent.Agent{LLM: model.Runner(), API: a.api}).Accruals(context.Background(), a.close)
	if err != nil {
		t.Fatalf("accruals: %v", err)
	}
	list, _ := a.Must(200, "GET", "/v1/accrual-proposals?close_id="+a.close, "", "")
	items := apitest.List(t, list, "items")
	sessions, _ := a.Must(200, "GET", "/v1/agent-sessions", "", "")
	if len(res.Proposals) != 1 || len(items) != 1 {
		t.Fatalf("proposals: got %v reported and %d listed, want one of each", res.Proposals, len(items))
	}
	item := items[0].(map[string]any)
	checks{
		"the proposal reported":              {res.Proposals[0].ID, item["id"]},
		"its parked entry reported":          {res.Proposals[0].ParkedEntryID, item["parked_entry_id"]},
		"the session reported":               {res.Session, item["session_id"]},
		"proposals refused":                  {res.Refused, 3},
		"the reply":                          {res.Reply, "Proposed March's courier accrual."},
		"tools offered":                      {fmt.Sprint(slices.Sorted(slices.Values(model.ToolNames()))), fmt.Sprint(slices.Sorted(slices.Values(closeagent.AccrualTools)))},
		"its amount":                         {item["amount"].(map[string]any)["amount"], "100.00"},
		"another close refused to the model": {strings.Contains(model.Requests[2], closeagent.ErrOtherClose.Error()), true},
		"the model's model, the agent's":     {strings.Contains(model.Requests[0], `"model":"erp-default"`), true},
		"one session":                        {len(apitest.List(t, sessions, "items")), 1},
		"the session ended":                  {apitest.List(t, sessions, "items")[0].(map[string]any)["status"], "completed"},
	}.check(t)
}

// TestAccrualsStopped: a person cannot start the agent's session; a model
// that does not answer fails the run with no reply, its session reported;
// a run bounded to one request stops after it.
func TestAccrualsStopped(t *testing.T) {
	a := setup(t)
	t.Run("a person", func(t *testing.T) {
		person := &apitools.Client{BaseURL: a.api.BaseURL, Token: a.API.Issuer.Token(t, a.Tenant.ID, a.Tenant.Human.Subject, nil)}
		_, err := (&closeagent.Agent{LLM: llmtest.New(t).Runner(), API: person}).Accruals(context.Background(), a.close)
		if got := fmt.Sprint(err); !strings.Contains(got, "not_an_agent") {
			t.Errorf("got %s, want agents.not_an_agent", got)
		}
	})
	t.Run("a model that does not answer", func(t *testing.T) {
		res, err := (&closeagent.Agent{LLM: llmtest.New(t).Runner(), API: a.api}).Accruals(context.Background(), a.close)
		checks{
			"an error":        {err != nil, true},
			"no reply":        {res.Reply, ""},
			"its session":     {res.Session != "", true},
			"nothing created": {len(res.Proposals), 0},
		}.check(t)
	})
	t.Run("one request at most", func(t *testing.T) {
		model := llmtest.New(t, llmtest.Tool("listAccounts", map[string]any{}), llmtest.Final("never asked"))
		res, err := (&closeagent.Agent{LLM: model.Runner(), API: a.api, MaxIterations: 1}).Accruals(context.Background(), a.close)
		checks{
			"no error":    {err, nil},
			"model calls": {model.Calls(), 1},
			"proposals":   {len(res.Proposals), 0},
		}.check(t)
	})
}

// TestAccrualsFailures: a close that cannot be read, a contract without one
// of the tools, stop the run before the model is asked.
func TestAccrualsFailures(t *testing.T) {
	a := setup(t)
	tools := closeagent.AccrualTools
	for name, c := range map[string]struct {
		api   *apitools.Client
		close string
		tools []string
		want  string
	}{
		"no server":        {&apitools.Client{BaseURL: "http://127.0.0.1:1"}, a.close, tools, "connect"},
		"no such close":    {a.api, uuid.NewString(), tools, "404"},
		"a tool not there": {a.api, a.close, append(slices.Clone(tools), "noSuchOperation"), "noSuchOperation"},
	} {
		t.Run(name, func(t *testing.T) {
			closeagent.AccrualTools = c.tools
			t.Cleanup(func() { closeagent.AccrualTools = tools })
			model := llmtest.New(t)
			_, err := (&closeagent.Agent{LLM: model.Runner(), API: c.api}).Accruals(context.Background(), c.close)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %s", err, c.want)
			}
			if model.Calls() != 0 {
				t.Errorf("model calls: got %d, want 0", model.Calls())
			}
			if c.tools[len(c.tools)-1] == "noSuchOperation" {
				sessions, _ := a.As(a.Tenant.Agent).Must(200, "GET", "/v1/agent-sessions?status=running", "", "")
				if got := len(apitest.List(t, sessions, "items")); got != 0 {
					t.Errorf("running sessions: got %d, want 0", got)
				}
			}
		})
	}
}
