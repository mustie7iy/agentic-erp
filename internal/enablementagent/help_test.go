package enablementagent_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/enablementagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

func reply(text string) llmtest.Step {
	return llmtest.Tool(enablementagent.Reply, map[string]any{"text": text})
}

// refusedPost is a question about the problem code postSupplierInvoice
// answers for an invoice not matched clean.
var refusedPost = enablementagent.Question{Operation: "postSupplierInvoice", Problem: "payables.match_required"}

const (
	passage     = `"a clean latest match of the invoice as it now stands for PO-based invoices"`
	explanation = "1. `payables.match_required` means the invoice is posted only after " + passage + ".\n" +
		"2. The match holds its price to " + tolerance + " percent or `companies[1000].invoice_tolerance.price_absolute` = `5.00`."
)

// TestHelp: the agent explains a problem code of an operation in one
// session from the contract, given in its prompt, and the active version,
// and replies once. Its check refuses, and counts, a reply that does not
// name the code, one quoting nothing, one quoting what the contract does
// not write, a setting cited with another value, "percent" after no
// citation, a number outside the quotations and citations, and a second
// reply; a list's numbers are
// allowed.
func TestHelp(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool("getConfigVersion", map[string]any{"id": version}),
		reply("The invoice must be matched first: "+passage+"."),
		reply("`payables.match_required` means match the invoice first."),
		reply("`payables.match_required`: \"a clean match of the invoice\"."),
		reply("`payables.match_required`: "+passage+"; `companies[1000].invoice_tolerance.price_percent` = `3`."),
		reply("`payables.match_required`: "+passage+", within 2 percent."),
		reply("`payables.match_required`: "+passage+", after 2 days."),
		reply(explanation),
		reply(explanation),
		llmtest.Final("Explained."))
	res, err := (&enablementagent.Agent{LLM: model.Runner(), API: client(t, a)}).Help(context.Background(), refusedPost)
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != session || res.Reply != explanation {
		t.Errorf("result: got session %s and reply %q, want %s and %q", res.Session, res.Reply, session, explanation)
	}
	if res.Refused != 7 {
		t.Errorf("refused: got %d, want 7", res.Refused)
	}
	if n := a.sent(":end"); n != 1 {
		t.Errorf("sessions ended: got %d, want 1", n)
	}
	told(t, model.Requests[:1], "The tenant's active configuration is version 3 (id "+version+").\nThe user asks what the problem code payables.match_required of postSupplierInvoice means.",
		"<contract>\noperation postSupplierInvoice: POST /v1/supplier-invoices/{id}:post\nsummary: ", "problem codes: ", "payables.match_required")
	told(t, model.Requests, enablementagent.ErrNotNamed.Error(), enablementagent.ErrNotQuoted.Error(), `not "a clean match of the invoice"`,
		enablementagent.ErrSetting.Error(), enablementagent.ErrNumber.Error(), `not "percent"`, enablementagent.ErrHelpNumber.Error(), "not 2", enablementagent.ErrSecond.Error())
}

// TestHelpField: a field is explained from its own description, and a
// problem code every operation may answer is named so in the prompt.
func TestHelpField(t *testing.T) {
	model := llmtest.New(t, reply("`gross_amount` is the \"Gross amount; all amounts must share one currency.\""), llmtest.Final("Explained."))
	q := enablementagent.Question{Operation: "createSupplierInvoice", Field: "gross_amount"}
	res, err := (&enablementagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{})}).Help(context.Background(), q)
	if err != nil || res.Refused != 0 {
		t.Fatalf("got %v and %d refused, want a reply and none", err, res.Refused)
	}
	told(t, model.Requests[:1], "field gross_amount: Gross amount; all amounts must share one currency.")
	model = llmtest.New(t, reply("`request.invalid_body`: \"Posts the invoice to the ledger\"."), llmtest.Final("Explained."))
	q = enablementagent.Question{Operation: "postSupplierInvoice", Problem: "request.invalid_body"}
	if _, err := (&enablementagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{})}).Help(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	told(t, model.Requests[:1], "problem code request.invalid_body: one any operation may answer")
}

// TestHelpRefusals: a question that names no operation, or neither or
// both of a field and a problem code, and one the contract does not have
// stop the run before anything is read; an active version the agent may
// not read, a session it may not start and an API that is gone stop it
// before a session; a model that fails, and one that gives no reply the
// check admits, fail the run after it.
func TestHelpRefusals(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	unread := &fakeAPI{}
	for name, c := range map[string]struct {
		api  *apitools.Client
		q    enablementagent.Question
		want error
		says string
	}{
		"no operation":         {client(t, unread), enablementagent.Question{Problem: "payables.match_required"}, enablementagent.ErrAsk, ""},
		"neither":              {client(t, unread), enablementagent.Question{Operation: "postSupplierInvoice"}, enablementagent.ErrAsk, ""},
		"both":                 {client(t, unread), enablementagent.Question{Operation: "postSupplierInvoice", Field: "id", Problem: "payables.match_required"}, enablementagent.ErrAsk, ""},
		"an unknown operation": {client(t, unread), enablementagent.Question{Operation: "postInvoice", Field: "id"}, apitools.ErrNotInContract, ""},
		"an unknown field":     {client(t, unread), enablementagent.Question{Operation: "postSupplierInvoice", Field: "colour"}, apitools.ErrNotInContract, ""},
		"an unknown problem":   {client(t, unread), enablementagent.Question{Operation: "postSupplierInvoice", Problem: "payables.too_late"}, apitools.ErrNotInContract, ""},
		"a version refused":    {client(t, &fakeAPI{status: 403}), refusedPost, nil, "identity.permission_missing"},
		"a session refused":    {client(t, &fakeAPI{sessions: 403}), refusedPost, nil, "identity.permission_missing"},
		"the API gone":         {gone, refusedPost, nil, "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&enablementagent.Agent{LLM: llmtest.New(t).Runner(), API: c.api}).Help(context.Background(), c.q)
			if c.want != nil && !errors.Is(err, c.want) || c.want == nil && (err == nil || !strings.Contains(err.Error(), c.says)) {
				t.Errorf("got %v, want %v %q", err, c.want, c.says)
			}
			if res.Session != "" {
				t.Errorf("session: got %s, want none", res.Session)
			}
		})
	}
	if n := unread.sent("/v1/"); n != 0 {
		t.Errorf("a question not in the contract: got %d calls, want none", n)
	}
	if _, err := (&enablementagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, &fakeAPI{})}).Help(context.Background(), refusedPost); err == nil {
		t.Error("a model that fails: got no error")
	}
	model := llmtest.New(t, reply("Match it first."), llmtest.Final("Done."))
	res, err := (&enablementagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{}), MaxIterations: 5}).Help(context.Background(), refusedPost)
	if !errors.Is(err, enablementagent.ErrNoReply) || res.Refused != 1 {
		t.Errorf("no reply admitted: got %v and %d refused, want ErrNoReply and 1", err, res.Refused)
	}
}

// TestHelpTools: the agent's help tools are reads of the contract open to
// agents and its reply: none writes.
func TestHelpTools(t *testing.T) {
	ops := api.MustOperations()
	for _, id := range enablementagent.HelpReads {
		if op, ok := ops[id]; !ok || op.Method != http.MethodGet || !op.AgentAllowed {
			t.Errorf("%s: got in the contract %v, method %s, open to agents %v; want a read open to agents", id, ok, op.Method, op.AgentAllowed)
		}
	}
	if got, want := enablementagent.HelpTools(), append(slices.Clone(enablementagent.HelpReads), enablementagent.Reply); !slices.Equal(got, want) {
		t.Errorf("tools: got %v, want %v", got, want)
	}
}
