package agent_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "payables.invoice.hold", "procurement.order.read",
	"procurement.receipt.read", "partner.supplier.read", "agents.session.read", "agents.session.record"}

var (
	sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)
	invoiceRe = regexp.MustCompile(`Invoice ([0-9a-f-]{36})`)
)

// recommend is the model's createRecommendation call for the invoice and
// session named in the conversation.
func recommend(body string) any {
	return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(body)[1], "object_type": "supplier_invoice",
		"object_id": invoiceRe.FindStringSubmatch(body)[1], "kind": "request_credit_note", "content": "Price is 10% above the order."}}
}

// failAfter answers its first n requests and fails the rest.
type failAfter struct{ n, seen int }

func (f *failAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.seen++; f.seen > f.n {
		return nil, errors.New("connection reset")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func held(t *testing.T, f fixture.Env, ref string) string {
	t.Helper()
	inv, tag := f.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": "2026-03-10", "gross_amount": {"amount": "10.00", "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "10.00", "currency": "EUR"}}]}`,
		f.DE, f.Supplier, ref, f.Account(t, "2000", "520000")), "")
	f.Must(200, "POST", "/v1/supplier-invoices/"+inv["id"].(string)+":hold", `{"reason": "Price variance"}`, tag)
	return inv["id"].(string)
}

func TestReview(t *testing.T) {
	f := fixture.New(t, perms...)
	f.Tenant.AllCompanies(t, f.Tenant.Agent)
	srv := httptest.NewServer(f.API.Handler)
	defer srv.Close()
	api := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	first, second := held(t, f, "H-1"), held(t, f, "H-2")
	model := llmtest.New(t,
		llmtest.ToolFn("getSupplierInvoice", func(body string) any { return map[string]any{"id": invoiceRe.FindStringSubmatch(body)[1]} }),
		llmtest.ToolFn("createRecommendation", recommend), llmtest.Final("Recommended a credit note."),
		llmtest.Final("Nothing to say."))
	a := &agent.Agent{LLM: model.Runner(), API: api, MaxInvoices: 5}
	res, err := a.Review(context.Background())
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if res.Recommendations != 1 {
		t.Errorf("review recommendations: got %d, want 1", res.Recommendations)
	}
	if len(res.Reviewed) != 2 {
		t.Errorf("review reviewed: got %v, want 2 invoices", res.Reviewed)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("review errors: got %v, want 1", res.Errors)
	}
	if !strings.Contains(res.Errors[0], "no recommendation") {
		t.Errorf("review error: got %q, want it to mention no recommendation", res.Errors[0])
	}
	if names := model.ToolNames(); len(names) != len(agent.Tools) {
		t.Fatalf("tools offered: %v", names)
	}
	recs, _ := f.Must(200, "GET", "/v1/recommendations?object_id="+first, "", "")
	if items := recs["items"].([]any); len(items) != 1 || items[0].(map[string]any)["kind"] != "request_credit_note" {
		t.Fatalf("recommendations: %v", recs)
	}
	// Invoices with an open recommendation are not reviewed again; model failures are reported per invoice.
	res, err = (&agent.Agent{LLM: llmtest.New(t).Runner(), API: api}).Review(context.Background())
	if err != nil {
		t.Fatalf("second review: %v", err)
	}
	if len(res.Reviewed) != 1 {
		t.Fatalf("second review reviewed: got %v, want 1 invoice", res.Reviewed)
	}
	if res.Reviewed[0] == "" {
		t.Error("second review reviewed: got an empty id, want the invoice id")
	}
	if len(res.Errors) != 1 {
		t.Errorf("second review errors: got %v, want 1", res.Errors)
	}
	if second == "" {
		t.Error("second held invoice: got an empty id, want an id")
	}
	// The agent needs to read invoices, recommendations and start sessions.
	person := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil)}
	if res, _ := (&agent.Agent{LLM: model.Runner(), API: person}).Review(context.Background()); len(res.Errors) != 1 ||
		!strings.Contains(res.Errors[0], "not_an_agent") {
		t.Fatalf("person: %+v", res)
	}
	pgtest.Exec(t, `UPDATE identity.role_permission SET valid_to = current_date, ended_by = created_by WHERE tenant_id = $1 AND permission_code = 'agents.session.read'`,
		f.Tenant.ID)
	if _, err := (&agent.Agent{LLM: model.Runner(), API: api}).Review(context.Background()); err == nil {
		t.Fatal("reviewed without reading recommendations")
	}
	for name, c := range map[string]*apitools.Client{"no server": {BaseURL: "http://127.0.0.1:1"}, "no token": {BaseURL: srv.URL},
		"server gone": {BaseURL: srv.URL, Token: api.Token, HTTP: &http.Client{Transport: &failAfter{n: 1}}}} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&agent.Agent{LLM: model.Runner(), API: c}).Review(context.Background()); err == nil {
				t.Error("Review: got nil error, want a failure")
			}
		})
	}
}
