package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/regulatoryagent"
	"github.com/mustie7iy/agentic-erp/internal/supportagent"
)

// agentSession is the session the fake API starts for an agent's run.
const agentSession = "01a1aa00-0000-7000-8000-000000000a01"

// sessionRoutes are the routes of an agent's session on the fake API:
// started, its tool calls recorded, ended.
func sessionRoutes(more routes) routes {
	r := routes{"POST /v1/agent-sessions": {reply{201, `{"id": "` + agentSession + `", "autonomy_level": 1}`}},
		"POST /v1/agent-sessions/" + agentSession + ":end":        {ok(`{}`)},
		"POST /v1/agent-sessions/" + agentSession + "/tool-calls": {reply{201, `{}`}}}
	for k, v := range more {
		r[k] = v
	}
	return r
}

// TestAgentCreditScore: erp agent credit-score needs -company and
// -customer, reads the customer's exposure, runs the credit scoring
// agent's session and prints what it recommended and the model's reply,
// also when the run failed after starting its session (ADR-0072, T105a).
func TestAgentCreditScore(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "credit-score", "-x")
	fails(t, "want -company CODE -customer ID", "agent", "credit-score", "-company", "3000")
	fails(t, "want -company CODE -customer ID", "agent", "credit-score", "-customer", "c1")
	fails(t, "404", "agent", "credit-score", "-company", "3000", "-customer", "c1")
	scored := sessionRoutes(routes{"GET /v1/legal-entities": {ok(`{"items": [{"id": "le", "code": "3000"}]}`)},
		"GET /v1/credit-exposure": {ok(`{"company_code": "3000", "customer_id": "c1", "customer_number": "100042", "currency": "CAD", "has_limit": true,
			"credit_limit_id": "l1", "limit": {"amount": "2470.00", "currency": "CAD"}}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "credit_limit", "object_id": "l1", "kind": "keep_limit"}`}}})
	fakeAPI(t, scored)
	model(t, llmtest.Tool("getCreditExposure", map[string]any{"legal_entity_id": "le", "customer_id": "c1"}),
		llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "credit_limit",
			"object_id": "l1", "kind": "keep_limit", "content": "Limit 2470.00 CAD: nothing is overdue."}}),
		llmtest.Final("The limit is kept; a credit manager decides."))
	got := must(t, "agent", "credit-score", "-company", "3000", "-customer", "c1")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: keep_limit on credit_limit l1\n" +
		"The limit is kept; a credit manager decides.\n"; got != want {
		t.Errorf("agent credit-score: got %q, want %q", got, want)
	}
	model(t, llmtest.Final("Nothing."))
	printFails(t, "agent", "credit-score", "-company", "3000", "-customer", "c1")
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "credit-score", "-company", "3000", "-customer", "c1")
	fails(t, "500", "agent", "credit-score", "-company", "3000", "-customer", "c1")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "credit-score", "-company", "3000", "-customer", "c1")
}

// TestAgentAccessRoles: erp agent access-roles needs -role and -period,
// reads the role, its permissions, its holders and their audit events,
// runs the access agent's session and prints what it recommended and the
// model's reply, also when the run failed after starting its session
// (ADR-0072, T106a).
func TestAgentAccessRoles(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	const period = "2026-04-01..2026-06-30"
	fails(t, "flag provided", "agent", "access-roles", "-x")
	fails(t, "want -role CODE -period FROM..TO", "agent", "access-roles", "-role", "clerk")
	fails(t, "want -role CODE -period FROM..TO", "agent", "access-roles", "-period", period)
	fails(t, "want -role CODE -period FROM..TO", "agent", "access-roles", "-role", "clerk", "-period", "2026-04-01")
	fails(t, "a period is two dates", "agent", "access-roles", "-role", "clerk", "-period", "April..June")
	fails(t, "404", "agent", "access-roles", "-role", "clerk", "-period", period)
	reviewed := sessionRoutes(routes{"GET /v1/roles": {ok(`{"items": [{"id": "r1", "code": "clerk", "name": "Clerks"}]}`)},
		"GET /v1/roles/r1/permissions": {ok(`{"items": [{"permission_code": "credit.limit.write"}]}`)},
		"GET /v1/role-assignments":     {ok(`{"items": [{"principal_id": "p1", "valid_from": "2026-01-01"}]}`)},
		"GET /v1/audit-events":         {ok(`{"items": [{"actor_id": "p1", "action": "credit.credit_limit.create"}]}`)},
		"POST /v1/recommendations":     {reply{201, `{"id": "rec1", "object_type": "role", "object_id": "r1", "kind": "keep"}`}}})
	fakeAPI(t, reviewed)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "role",
		"object_id": "r1", "kind": "keep", "content": "Its holder set a credit limit in the period."}}),
		llmtest.Final("The role is kept; an identity administrator decides."))
	got := must(t, "agent", "access-roles", "-role", "clerk", "-period", period)
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation rec1: keep on role r1\n" +
		"The role is kept; an identity administrator decides.\n"; got != want {
		t.Errorf("agent access-roles: got %q, want %q", got, want)
	}
	model(t, llmtest.Final("Nothing."))
	printFails(t, "agent", "access-roles", "-role", "clerk", "-period", period)
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "access-roles", "-role", "clerk", "-period", period)
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "access-roles", "-role", "clerk", "-period", period)
}

// TestAgentAccessSod: erp agent access-sod needs -role, reads the
// roles, the assignments, the rules, the principals and the holders'
// grants, runs the access agent's analysis with the permissions -add
// names and prints what it recommended and the model's reply, also when
// the run failed after starting its session (ADR-0072, T106b).
func TestAgentAccessSod(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "access-sod", "-x")
	fails(t, "want -role CODE [-add PERMISSION,...]", "agent", "access-sod", "-add", "payables.invoice.post")
	fails(t, "404", "agent", "access-sod", "-role", "clerk")
	analysed := sessionRoutes(routes{"GET /v1/roles": {ok(`{"items": [{"id": "r1", "code": "clerk", "name": "Clerks", "status": "active"}]}`)},
		"GET /v1/role-assignments":     {ok(`{"items": [{"principal_id": "p1", "role_id": "r1", "valid_from": "2026-01-01"}]}`)},
		"GET /v1/sod-rules":            {ok(`{"items": []}`)},
		"GET /v1/principals":           {ok(`{"items": [{"id": "p1", "kind": "human", "display_name": "Ruth"}]}`)},
		"GET /v1/roles/r1/permissions": {ok(`{"items": [{"permission_code": "payables.invoice.capture", "valid_from": "2026-01-01"}]}`)},
		"POST /v1/recommendations":     {reply{201, `{"id": "rec1", "object_type": "role", "object_id": "r1", "kind": "no_conflict"}`}}})
	fakeAPI(t, analysed)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "role",
		"object_id": "r1", "kind": "no_conflict", "content": "The tenant has no rule to break."}}),
		llmtest.Final("No conflict; an identity administrator decides."))
	got := must(t, "agent", "access-sod", "-role", "clerk", "-add", "payables.invoice.post")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation rec1: no_conflict on role r1\n" +
		"No conflict; an identity administrator decides.\n"; got != want {
		t.Errorf("agent access-sod: got %q, want %q", got, want)
	}
	model(t, llmtest.Final("Nothing."))
	printFails(t, "agent", "access-sod", "-role", "clerk")
	model(t)
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "access-sod", "-role", "clerk")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "access-sod", "-role", "clerk")
}

// TestAgentAccessReview: erp agent access-review needs -period, reads the
// tenant's access and the period's audit events, runs the access
// agent's review and prints what it recommended and the model's reply;
// a period whose principals were all reviewed starts no session and says
// so (ADR-0072, T106c).
func TestAgentAccessReview(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	const period = "2026-04-01..2026-06-30"
	fails(t, "flag provided", "agent", "access-review", "-x")
	fails(t, "want -period FROM..TO", "agent", "access-review")
	fails(t, "a period is two dates", "agent", "access-review", "-period", "April..June")
	fails(t, "404", "agent", "access-review", "-period", period)
	base := routes{"GET /v1/roles": {ok(`{"items": []}`)}, "GET /v1/role-assignments": {ok(`{"items": []}`)}, "GET /v1/sod-rules": {ok(`{"items": []}`)},
		"GET /v1/principals": {ok(`{"items": [{"id": "p1", "kind": "human", "display_name": "Ruth", "status": "active"}]}`)},
		"GET /v1/me":         {ok(`{"id": "agent1"}`)}, "GET /v1/audit-events": {ok(`{"items": []}`)}}
	done := sessionRoutes(base)
	done["GET /v1/recommendations"] = []reply{ok(`{"items": [{"agent_id": "agent1", "object_id": "p1", "content": "Access review 2026-04-01..2026-06-30: keep: no role"}]}`)}
	fakeAPI(t, done)
	if got, want := must(t, "agent", "access-review", "-period", period), "Every active principal has its recommendation of the access review 2026-04-01..2026-06-30 already: nothing to add.\n"; got != want {
		t.Errorf("agent access-review of a period reviewed: got %q, want %q", got, want)
	}
	printFails(t, "agent", "access-review", "-period", period)
	fresh := sessionRoutes(base)
	fresh["GET /v1/recommendations"] = []reply{ok(`{"items": []}`)}
	fresh["POST /v1/recommendations"] = []reply{{201, `{"id": "rec1", "object_type": "principal", "object_id": "p1", "kind": "keep_roles",
		"content": "Access review 2026-04-01..2026-06-30: keep: no role\nRuth holds no role."}`}}
	fakeAPI(t, fresh)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "principal",
		"object_id": "p1", "kind": "keep_roles", "content": "Access review 2026-04-01..2026-06-30: keep: no role\nRuth holds no role."}}),
		llmtest.Final("Prepared; the reviewer decides."))
	got := must(t, "agent", "access-review", "-period", period)
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation rec1: keep_roles on principal p1 (Ruth): Access review 2026-04-01..2026-06-30: keep: no role\n" +
		"Prepared; the reviewer decides.\n"; got != want {
		t.Errorf("agent access-review: got %q, want %q", got, want)
	}
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "access-review", "-period", period)
}

// TestAgentBidCompare: erp agent bid-compare needs -rfq, reads the RFQ's
// comparison, runs the bid comparison agent's session and prints what it
// recommended and the model's reply, also when the run failed after
// starting its session (ADR-0072, T105c).
func TestAgentBidCompare(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "bid-compare", "-x")
	fails(t, "want -rfq ID", "agent", "bid-compare")
	fails(t, "404", "agent", "bid-compare", "-rfq", "q1")
	compared := sessionRoutes(routes{"GET /v1/rfqs/q1/comparison": {ok(`{"rfq_id": "q1", "rfq_document_number": "RQ100004", "company_code": "3000",
			"currency": "CAD", "status": "published", "suppliers": [{"supplier_quotation_id": "01a1aa00-0000-7000-8000-000000000c01", "rank": 1,
			"total": {"amount": "28400.00", "currency": "CAD"}}]}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "rfq", "object_id": "q1", "kind": "award"}`}}})
	fakeAPI(t, compared)
	model(t, llmtest.Tool("getRfqComparison", map[string]any{"id": "q1"}),
		llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "rfq",
			"object_id": "q1", "kind": "award", "content": "Award: 01a1aa00-0000-7000-8000-000000000c01\nThe one quotation ranked, 28400.00 CAD."}}),
		llmtest.Final("Recommended the award; the buyer decides."))
	got := must(t, "agent", "bid-compare", "-rfq", "q1")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: award on rfq q1\n" +
		"Recommended the award; the buyer decides.\n"; got != want {
		t.Errorf("agent bid-compare: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "bid-compare", "-rfq", "q1")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "bid-compare", "-rfq", "q1")
}

// TestAgentRegulatoryNotice: erp agent regulatory-notice needs -file, a
// readable notice of at most regulatoryagent.MaxNotice bytes, reads the
// active configuration version, runs the regulatory agent's session and
// prints what it recommended and the model's reply, also when the run
// failed after starting its session (ADR-0072, T107a).
func TestAgentRegulatoryNotice(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	notice := "INVENTED: no authority issued this notice. From 1 July 2099 tax code V0 rises from 0 percent to 2.5 percent."
	if err := os.WriteFile("notice.txt", []byte(notice), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("long.txt", []byte(strings.Repeat("x", regulatoryagent.MaxNotice+10)), 0o600); err != nil {
		t.Fatal(err)
	}
	fails(t, "flag provided", "agent", "regulatory-notice", "-x")
	fails(t, "want -file F", "agent", "regulatory-notice")
	fails(t, "no such file", "agent", "regulatory-notice", "-file", "missing.txt")
	fails(t, "is a directory", "agent", "regulatory-notice", "-file", ".")
	fails(t, "a notice is UTF-8 text", "agent", "regulatory-notice", "-file", "long.txt")
	fails(t, "404", "agent", "regulatory-notice", "-file", "notice.txt")
	read := sessionRoutes(routes{"GET /v1/config-versions": {ok(`{"items": [{"id": "v1", "number": 3, "document": {"companies": [{"code": "1000",
			"tax_codes": [{"code": "V0", "rate_percent": "0"}]}]}}]}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "config_version", "object_id": "v1", "kind": "no_change"}`}}})
	fakeAPI(t, read)
	model(t, llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "config_version",
		"object_id": "v1", "kind": "no_change", "content": "Nothing to change. " + regulatoryagent.Caveat}}),
		llmtest.Final("No change needed; a person checks the notice."))
	got := must(t, "agent", "regulatory-notice", "-file", "notice.txt")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: no_change on config_version v1\n" +
		"No change needed; a person checks the notice.\n"; got != want {
		t.Errorf("agent regulatory-notice: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": recommendations made 0, refused 0.", "agent", "regulatory-notice", "-file", "notice.txt")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "regulatory-notice", "-file", "notice.txt")
}

// TestAgentRegulatoryDraft: erp agent regulatory-draft needs
// -recommendation, refuses a recommendation that is not accepted before a
// session, runs the drafting session on an accepted one and prints what
// it recommended and the model's reply (ADR-0072, T107b).
func TestAgentRegulatoryDraft(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "regulatory-draft", "-x")
	fails(t, "want -recommendation ID", "agent", "regulatory-draft")
	fails(t, "404", "agent", "regulatory-draft", "-recommendation", "r0")
	rec := func(outcome string) reply {
		return ok(`{"id": "r0", "object_type": "config_version", "object_id": "v1", "kind": "change_needed", "outcome": "` + outcome +
			`", "content": "Paths: companies[1000].tax_codes[V0]\nTakes effect: not stated\n"}`)
	}
	versions := ok(`{"items": [{"id": "v1", "number": 3, "document": {"companies": [{"code": "1000", "tax_codes": [{"code": "V0", "rate_percent": "0"}]}]}}]}`)
	fakeAPI(t, routes{"GET /v1/recommendations/r0": {rec("overridden")}})
	fails(t, "is change_needed on config_version, overridden", "agent", "regulatory-draft", "-recommendation", "r0")
	fakeAPI(t, sessionRoutes(routes{"GET /v1/recommendations/r0": {rec("accepted")}, "GET /v1/config-versions": {versions}}))
	model(t, llmtest.Final("Nothing drafted: the configuration cannot hold the rate."))
	got := must(t, "agent", "regulatory-draft", "-recommendation", "r0")
	if want := "Session " + agentSession + ": recommendations made 0, refused 0.\nNothing drafted: the configuration cannot hold the rate.\n"; got != want {
		t.Errorf("agent regulatory-draft: got %q, want %q", got, want)
	}
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "regulatory-draft", "-recommendation", "r0")
}

// TestAgentSupportAsk: erp agent support-ask needs a question, reads the
// active configuration version, runs the support agent's session and
// prints how many replies were refused and the reply admitted, also when
// the run failed after starting its session (ADR-0072 As built, T108a).
func TestAgentSupportAsk(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "support-ask", "-x")
	fails(t, "ask a question", "agent", "support-ask")
	fails(t, "404", "agent", "support-ask", "how", "is", "an", "invoice", "released?")
	fakeAPI(t, sessionRoutes(routes{"GET /v1/config-versions": {ok(`{"items": [{"id": "v1", "number": 3, "document": {"companies": []}}]}`)}}))
	model(t, llmtest.Tool(supportagent.Reply, map[string]any{"text": "Call releaseSupplierInvoice."}), llmtest.Final("Answered."))
	got := must(t, "agent", "support-ask", "how", "is", "an", "invoice", "released?")
	if want := "Session " + agentSession + ": replies refused 0.\nCall releaseSupplierInvoice.\n"; got != want {
		t.Errorf("agent support-ask: got %q, want %q", got, want)
	}
	model(t)
	// A failed run still names its session, then fails.
	fails(t, "Session "+agentSession+": replies refused 0.", "agent", "support-ask", "how?")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "support-ask", "how?")
}

// TestAgentCollections: erp agent collections needs -company, reads the
// worklist on the key date, runs the collections agent's session and
// prints what it recommended, each customer named with the first line of
// its recommendation, and the model's reply; a worklist without customers
// starts no session and prints why (ADR-0072 As built, T105b).
func TestAgentCollections(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "flag provided", "agent", "collections", "-x")
	fails(t, "want -company CODE", "agent", "collections", "-key-date", "2026-11-15")
	fails(t, "404", "agent", "collections", "-company", "3000")
	entities := ok(`{"items": [{"id": "le", "code": "3000"}]}`)
	fakeAPI(t, routes{"GET /v1/legal-entities": {entities}, "GET /v1/collection-worklist": {ok(`{"company_code": "3000", "currency": "CAD", "items": []}`)}})
	if got, want := must(t, "agent", "collections", "-company", "3000", "-key-date", "2026-11-15"),
		"Nothing of company 3000 is overdue on 2026-11-15: no customer to recommend on.\n"; got != want {
		t.Errorf("agent collections, nothing overdue: got %q, want %q", got, want)
	}
	fakeAPI(t, sessionRoutes(routes{"GET /v1/legal-entities": {entities},
		"GET /v1/collection-worklist": {ok(`{"company_code": "3000", "currency": "CAD", "customer_count": 1, "items": [{"customer_id": "c1",
			"customer_number": "100042", "customer_name": "Northwind", "overdue_amount": {"amount": "400.00", "currency": "CAD"}, "open_dispute_cases": 0}]}`)},
		"POST /v1/recommendations": {reply{201, `{"id": "r1", "object_type": "business_partner", "object_id": "c1", "kind": "no_action",
			"content": "Overdue 400.00 CAD, paid since.\nMore."}`}}}))
	model(t, llmtest.Tool("getCollectionWorklist", map[string]any{"legal_entity_id": "le", "key_date": "2026-11-15"}),
		llmtest.Tool("createRecommendation", map[string]any{"body": map[string]any{"session_id": agentSession, "object_type": "business_partner",
			"object_id": "c1", "kind": "no_action", "content": "Overdue 400.00 CAD, paid since.\nMore."}}),
		llmtest.Final("One customer: no action."))
	got := must(t, "agent", "collections", "-company", "3000", "-key-date", "2026-11-15")
	if want := "Session " + agentSession + ": recommendations made 1, refused 0.\n  recommendation r1: no_action on business_partner c1 (100042 Northwind): " +
		"Overdue 400.00 CAD, paid since.\nOne customer: no action.\n"; got != want {
		t.Errorf("agent collections: got %q, want %q", got, want)
	}
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "agent", "collections", "-company", "3000")
}

// TestRecommendingAdapterFailures: a recommending agent's evaluation
// adapter fails its case on a task without its values and on an agent
// that could not run.
func TestRecommendingAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, err := range map[string]error{
		"credit_scoring without a customer": creditScoringEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"company": "3000"}}),
		"credit_scoring without its API": creditScoringEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"company": "3000", "customer": "c1"}}),
		"access without a period":        accessEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"role": "clerk", "from": "2026-04-01"}}),
		"access review without a period": accessEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"run": "review", "from": "2026-04-01"}}),
		"access review without its API": accessEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"run": "review", "from": "2026-04-01", "to": "2026-06-30"}}),
		"access sod without a role": accessEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"run": "sod"}}),
		"access sod without its API": accessEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"run": "sod", "role": "clerk", "add": "payables.invoice.post"}}),
		"access without its API": accessEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"role": "clerk", "from": "2026-04-01", "to": "2026-06-30"}}),
		"bid_comparison without an RFQ":  bidComparisonEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{}}),
		"bid_comparison without its API": bidComparisonEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"rfq": "q1"}}),
		"regulatory without a notice":    regulatoryEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{}}),
		"regulatory without its API":     regulatoryEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"notice": "A notice."}}),
		"support without a question":     supportEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{}}),
		"support without its API":        supportEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"question": "How?"}}),
		"collections without a key date": collectionsEval(context.Background(), eval.Env{Agent: gone, Task: map[string]any{"company": "3000"}}),
		"collections without its API": collectionsEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{},
			Task: map[string]any{"company": "3000", "key_date": "2026-11-15"}}),
	} {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Error("got no error")
			}
		})
	}
	if err := creditScoringEval(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}, Task: map[string]any{"company": "3000", "customer": "c1"}}); err == nil ||
		!strings.Contains(err.Error(), "credit_scoring: ") {
		t.Errorf("an agent that could not run: got %v, want the agent type named", err)
	}
}
