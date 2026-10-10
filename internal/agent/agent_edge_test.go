package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

func TestNewClaudeResolverModel(t *testing.T) {
	t.Setenv("ERP_AGENT_MODEL", "claude-sonnet-5-5")
	if r := NewClaudeResolver(); r.Model != "claude-sonnet-5-5" {
		t.Fatalf("model %s", r.Model)
	}
	t.Setenv("ERP_AGENT_MODEL", "")
	if r := NewClaudeResolver(); r.Model != defaultModel {
		t.Fatalf("default model %s", r.Model)
	}
}

func parkedWithIssue(t *testing.T, svc *Service, kind core.IssueKind) string {
	t.Helper()
	for _, id := range core.SortedKeys(svc.Store.Invoices) {
		if m, _ := svc.Match(id); svc.Store.Invoices[id].Status == core.InvoiceParked && hasIssue(m, kind) {
			return id
		}
	}
	t.Fatalf("no parked invoice with %s", kind)
	return ""
}

// Every tool the exception agent has, including their error paths.
func TestExceptionAgentTools(t *testing.T) {
	svc := migrated(t)
	inv := parkedWithIssue(t, svc, core.IssuePriceVariance)
	vendor := svc.Store.Invoices[inv].VendorID
	srv := llmtest.New(t,
		llmtest.Tool("get_invoice", map[string]any{"invoice_id": "NOPE"}),
		llmtest.Step{Tool: "get_invoice", Input: func(string) any { return map[string]any{"invoice_id": inv} }, Expect: "error: invoice NOPE not found"},
		llmtest.Step{Tool: "get_match_result", Input: func(string) any { return map[string]any{"invoice_id": "NOPE"} }, Expect: "bankAccountMasked"},
		llmtest.Step{Tool: "list_vendor_invoices", Input: func(string) any { return map[string]any{"vendor_id": vendor} }, Expect: "error: invoice NOPE not found"},
		llmtest.Step{Tool: "escalate_to_human", Input: func(string) any {
			return map[string]any{"invoice_id": "NOPE", "reason": "r", "recommendation": "x", "proposed_action": "other"}
		}, Expect: inv},
		llmtest.Step{Tool: "escalate_to_human", Input: func(string) any {
			return map[string]any{"invoice_id": inv, "reason": "price", "recommendation": "credit note", "proposed_action": "request_credit_note"}
		}, Expect: "error: invoice NOPE not found"},
		llmtest.Step{Text: "Escalated.", Expect: "Review task"},
	)
	r := &ClaudeResolver{Client: srv.Runner().Client, Model: defaultModel}
	m, _ := svc.Match(inv)
	if err := r.Resolve(context.Background(), svc, inv, m); err != nil {
		t.Fatal(err)
	}
	if !hasOpenTask(svc, inv) {
		t.Fatal("escalation should open a task")
	}
}

func TestResolveFailures(t *testing.T) {
	svc := migrated(t)
	inv := parkedWithIssue(t, svc, core.IssuePriceVariance)
	m, _ := svc.Match(inv)
	refuse := llmtest.New(t, llmtest.Step{StopReason: "refusal"})
	if err := (&ClaudeResolver{Client: refuse.Runner().Client, Model: defaultModel}).Resolve(context.Background(), svc, inv, m); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("refusal: %v", err)
	}
	down := llmtest.New(t)
	if err := (&ClaudeResolver{Client: down.Runner().Client, Model: defaultModel}).Resolve(context.Background(), svc, inv, m); err == nil {
		t.Fatal("API errors propagate")
	}
	if mask("ab") != "**" || mask("12345678") != "****5678" {
		t.Fatal("mask")
	}
}

type fakeResolver func(ctx context.Context, svc *Service, id string) error

func (f fakeResolver) Resolve(ctx context.Context, svc *Service, id string, _ core.MatchResult) error {
	return f(ctx, svc, id)
}

func TestPipelineBranches(t *testing.T) {
	// A resolver that escalates is credited with an agent note.
	svc := migrated(t)
	escalating := fakeResolver(func(_ context.Context, svc *Service, id string) error {
		_, err := svc.Escalate(ExceptionAgent, id, "checked", "recommend credit note")
		return err
	})
	sum := ProcessInvoices(context.Background(), svc, escalating, io.Discard)
	if len(sum.AgentNotes) == 0 || len(sum.Errors) != 0 {
		t.Fatalf("agent notes: %+v", sum)
	}

	// A failing resolver is reported, and the rule still opens the task.
	svc = migrated(t)
	failing := fakeResolver(func(context.Context, *Service, string) error { return errors.New("model down") })
	sum = ProcessInvoices(context.Background(), svc, failing, io.Discard)
	if len(sum.Errors) == 0 || !strings.Contains(sum.Errors[0], "agent: model down") || len(sum.Escalated) != 6 {
		t.Fatalf("failing resolver: %+v", sum)
	}

	// A resolver that makes escalation impossible (the invoice disappears) is reported.
	svc = migrated(t)
	vanishing := fakeResolver(func(_ context.Context, svc *Service, id string) error {
		svc.Store.Mu.Lock()
		delete(svc.Store.Invoices, id)
		svc.Store.Mu.Unlock()
		return nil
	})
	sum = ProcessInvoices(context.Background(), svc, vanishing, io.Discard)
	if len(sum.Errors) == 0 || !strings.Contains(sum.Errors[0], "not found") {
		t.Fatalf("vanishing invoice: %+v", sum)
	}

	// A cancelled context stops the run.
	svc = migrated(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sum = ProcessInvoices(ctx, svc, nil, io.Discard)
	if sum.Processed != 0 || len(sum.Errors) != 1 {
		t.Fatalf("cancelled: %+v", sum)
	}

	// Unusable configuration makes matching fail for that company.
	svc = migrated(t)
	cfg := testkit.Config(t)
	cfg.Company("1000").AutoPostLimit = "x"
	svc.Config = cfg
	sum = ProcessInvoices(context.Background(), svc, nil, io.Discard)
	if len(sum.Errors) != 8 {
		t.Fatalf("every company-1000 invoice should error: %+v", sum.Errors)
	}

	// A posting failure on a clean invoice is reported.
	svc = migrated(t)
	cfg = testkit.Config(t)
	cfg.Company("1000").AccountDetermination.InputTax = "999999"
	svc.Config = cfg
	sum = ProcessInvoices(context.Background(), svc, nil, io.Discard)
	if len(sum.Errors) == 0 || !strings.Contains(strings.Join(sum.Errors, " "), "999999") {
		t.Fatalf("posting failure: %+v", sum.Errors)
	}
}

func TestServiceErrorPaths(t *testing.T) {
	svc := migrated(t)
	if _, err := svc.Match("NOPE"); err == nil {
		t.Fatal("match missing invoice")
	}
	if _, err := svc.PostInvoice(MatchRule, "NOPE", ""); err == nil {
		t.Fatal("post missing invoice")
	}
	inv := parkedWithIssue(t, svc, core.IssuePriceVariance)
	if _, err := svc.Escalate(governance.Identity{ID: "agent:x", Kind: "agent"}, inv, "r", ""); err == nil {
		t.Fatal("escalation without invoice.review")
	}
	if _, err := svc.Escalate(MatchRule, "NOPE", "r", ""); err == nil {
		t.Fatal("escalate missing invoice")
	}
	// Escalating twice updates the open task with the newer recommendation.
	first, _ := svc.Escalate(MatchRule, inv, "first", "")
	second, _ := svc.Escalate(ExceptionAgent, inv, "second", "credit note")
	if first != second || svc.Store.Tasks[first].Recommendation != "credit note" || svc.Store.Tasks[first].CreatedBy != ExceptionAgent.ID {
		t.Fatalf("task not updated: %+v", svc.Store.Tasks[first])
	}
	if _, err := svc.Decide(humanApprover, "TASK-NONE", true, ""); err == nil {
		t.Fatal("missing task")
	}
	// Approving a task whose invoice has gone reports the posting error.
	svc.Store.Mu.Lock()
	delete(svc.Store.Invoices, inv)
	svc.Store.Mu.Unlock()
	if _, err := svc.Decide(humanApprover, first, true, ""); err == nil {
		t.Fatal("approval of a vanished invoice")
	}
	// Configuration errors surface from Match and PostInvoice.
	svc2 := migrated(t)
	cfg := testkit.Config(t)
	cfg.Companies = cfg.Companies[1:]
	svc2.Config = cfg
	other := core.SortedKeys(svc2.Store.Invoices)[0]
	if svc2.Store.Invoices[other].CompanyCode != "1000" {
		t.Fatal("fixture: first invoice should be company 1000")
	}
	if _, err := svc2.Match(other); err == nil {
		t.Fatal("unconfigured company in Match")
	}
	var parked string // an open invoice, so PostInvoice reaches the configuration
	for _, id := range core.SortedKeys(svc2.Store.Invoices) {
		if inv := svc2.Store.Invoices[id]; inv.CompanyCode == "1000" && inv.Status == core.InvoiceParked {
			parked = id
			break
		}
	}
	if _, err := svc2.PostInvoice(MatchRule, parked, ""); err == nil || !strings.Contains(err.Error(), "1000") {
		t.Fatalf("unconfigured company in PostInvoice: %v", err)
	}
	var _ *config.Config
}

func TestAuditNilAndFailClosed(t *testing.T) {
	svc := migrated(t)
	svc.Audit = nil
	inv := parkedWithIssue(t, svc, core.IssuePriceVariance)
	if _, err := svc.Escalate(MatchRule, inv, "r", ""); err != nil {
		t.Fatal("works without an audit log")
	}
	dir := t.TempDir()
	audit, _ := governance.NewAuditLog(filepath.Join(dir, "logs", "audit.jsonl"))
	os.WriteFile(filepath.Join(dir, "logs"), []byte("file"), 0o644)
	svc.Audit = audit
	defer func() {
		if recover() == nil {
			t.Fatal("an action that cannot be audited must panic")
		}
	}()
	svc.Escalate(MatchRule, inv, "r2", "")
}
