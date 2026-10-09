package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

type memSource struct{ e *sap.Extract }

func (m memSource) Name() string { return "memory" }
func (m memSource) Extract(_ context.Context, s sap.Scope) (*sap.Extract, error) {
	return sap.FilterScope(m.e, s), nil
}

// migrated returns a service over the mock SAP data for company 1000.
func migrated(t *testing.T) *Service {
	t.Helper()
	s := core.NewStore()
	scope := sap.Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"}
	res, err := migration.Run(context.Background(), memSource{sap.GenerateMock(42, "2026")}, scope, migration.Options{}, s)
	if err != nil || !res.Report.Passed {
		t.Fatalf("migration failed: %v", err)
	}
	audit, _ := governance.NewAuditLog("")
	svc := NewService(s, audit)
	svc.Today = func() string { return "2026-07-01" }
	return svc
}

var humanApprover = governance.Identity{
	ID: "user:controller", Kind: "human", Autonomy: governance.FullyAutonomous,
	Permissions: []string{governance.ActInvoicePost, governance.ActInvoiceReview},
}

func TestPipelinePostsCleanAndEscalatesExceptions(t *testing.T) {
	svc := migrated(t)
	sum := ProcessInvoices(context.Background(), svc, nil, io.Discard)
	if sum.Processed != 8 || len(sum.Posted) != 2 || len(sum.Escalated) != 6 || len(sum.Errors) > 0 {
		t.Fatalf("unexpected summary %+v", sum)
	}
	for _, id := range sum.Escalated {
		if svc.Store.Invoices[id].Status != core.InvoiceOnHold || !hasOpenTask(svc, id) {
			t.Errorf("%s should be on hold with an open task", id)
		}
	}
	// Ledger still balances and every action is in an intact audit chain.
	var total int64
	for _, v := range svc.Store.TrialBalance("1000", "") {
		total += int64(v)
	}
	if total != 0 {
		t.Fatalf("trial balance off by %d", total)
	}
	if err := svc.Audit.Verify(); err != nil || len(svc.Audit.Events()) == 0 {
		t.Fatalf("audit: %v (%d events)", err, len(svc.Audit.Events()))
	}
}

func TestAgentsCannotPostExceptions(t *testing.T) {
	svc := migrated(t)
	ProcessInvoices(context.Background(), svc, nil, io.Discard)
	held := firstHeld(svc)

	// Even a fully autonomous non-human identity cannot post an unclean match.
	bot := governance.Identity{ID: "agent:bot", Kind: "agent", Autonomy: governance.FullyAutonomous,
		Permissions: []string{governance.ActInvoicePost}}
	res, err := svc.PostInvoice(bot, held, "")
	if err != nil || res.Outcome != governance.Escalate {
		t.Fatalf("want escalate, got %+v %v", res, err)
	}
	// The exception agent has no posting permission at all.
	if svc.Policy.Decide(ExceptionAgent, governance.ActInvoicePost, 1).Outcome != governance.Deny {
		t.Fatal("exception agent must not be able to post")
	}
}

func TestHumanApprovalPostsAndClosesTask(t *testing.T) {
	svc := migrated(t)
	ProcessInvoices(context.Background(), svc, nil, io.Discard)
	var task *core.ReviewTask
	for _, id := range core.SortedKeys(svc.Store.Tasks) {
		tk := svc.Store.Tasks[id]
		if m, _ := svc.Match(tk.InvoiceID); hasIssue(m, core.IssuePriceVariance) {
			task = tk
		}
	}
	if task == nil {
		t.Fatal("no price-variance task found")
	}
	if _, err := svc.Decide(ExceptionAgent, task.ID, true, ""); err == nil {
		t.Fatal("agents must not close review tasks")
	}
	res, err := svc.Decide(humanApprover, task.ID, true, "accepted surcharge")
	if err != nil || res.Outcome != governance.Allow || res.JournalEntryID == "" {
		t.Fatalf("approval failed: %+v %v", res, err)
	}
	if task.Status != core.TaskApproved || svc.Store.Invoices[task.InvoiceID].Status != core.InvoicePosted {
		t.Fatal("task/invoice state not updated")
	}
}

func TestHumanRejectDuplicate(t *testing.T) {
	svc := migrated(t)
	ProcessInvoices(context.Background(), svc, nil, io.Discard)
	for _, id := range core.SortedKeys(svc.Store.Tasks) {
		tk := svc.Store.Tasks[id]
		if m, _ := svc.Match(tk.InvoiceID); hasIssue(m, core.IssueDuplicate) {
			if _, err := svc.Decide(humanApprover, tk.ID, false, "duplicate"); err != nil {
				t.Fatal(err)
			}
			if svc.Store.Invoices[tk.InvoiceID].Status != core.InvoiceRejected {
				t.Fatal("invoice not rejected")
			}
			return
		}
	}
	t.Fatal("no duplicate task found")
}

// fakeClaude scripts a Messages API conversation: the agent reads the match
// result, then escalates with a recommendation, then finishes.
type fakeClaude struct {
	mu        sync.Mutex
	calls     int
	firstBody map[string]any
	betaHdr   string
	invoiceID string
}

func (f *fakeClaude) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	body, _ := io.ReadAll(r.Body)
	if f.calls == 1 {
		json.Unmarshal(body, &f.firstBody)
		f.betaHdr = r.Header.Get("anthropic-beta")
	}
	var content []map[string]any
	stop := "tool_use"
	switch f.calls {
	case 1:
		content = []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "get_match_result",
			"input": map[string]any{"invoice_id": f.invoiceID}}}
	case 2:
		if !strings.Contains(string(body), "price") {
			http.Error(w, "tool result did not reach the model", http.StatusBadRequest)
			return
		}
		content = []map[string]any{{"type": "tool_use", "id": "toolu_2", "name": "escalate_to_human",
			"input": map[string]any{
				"invoice_id": f.invoiceID, "reason": "Line 10 is 10% above the PO price.",
				"recommendation":  "Ask the vendor for a credit note for the difference.",
				"proposed_action": "request_credit_note", "vendor_message": "Please issue a credit note.",
			}}}
	default:
		content = []map[string]any{{"type": "text", "text": "Escalated."}}
		stop = "end_turn"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
}

func TestClaudeResolverEscalatesWithRecommendation(t *testing.T) {
	svc := migrated(t)
	var invoiceID string
	for _, id := range core.SortedKeys(svc.Store.Invoices) {
		if m, _ := svc.Match(id); svc.Store.Invoices[id].Status == core.InvoiceParked && hasIssue(m, core.IssuePriceVariance) {
			invoiceID = id
		}
	}
	fake := &fakeClaude{invoiceID: invoiceID}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	resolver := &ClaudeResolver{
		Client: anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		Model:  defaultModel,
	}
	m, _ := svc.Match(invoiceID)
	if err := resolver.Resolve(context.Background(), svc, invoiceID, m); err != nil {
		t.Fatal(err)
	}

	if fake.calls != 3 {
		t.Fatalf("want 3 model calls, got %d", fake.calls)
	}
	if fake.firstBody["model"] != defaultModel || fake.firstBody["fallbacks"] != "default" {
		t.Errorf("request config: model=%v fallbacks=%v", fake.firstBody["model"], fake.firstBody["fallbacks"])
	}
	if !strings.Contains(fake.betaHdr, "server-side-fallback-2026-07-01") {
		t.Errorf("beta header %q", fake.betaHdr)
	}
	var names []string
	for _, tl := range fake.firstBody["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "get_invoice,get_match_result,list_vendor_invoices,escalate_to_human" {
		t.Errorf("agent tool surface changed: %v", names)
	}

	var task *core.ReviewTask
	for _, tk := range svc.Store.Tasks {
		if tk.InvoiceID == invoiceID {
			task = tk
		}
	}
	if task == nil || task.CreatedBy != ExceptionAgent.ID || !strings.Contains(task.Recommendation, "request_credit_note") ||
		!strings.Contains(task.Recommendation, "Please issue a credit note.") {
		t.Fatalf("task not created as expected: %+v", task)
	}
	if svc.Store.Invoices[invoiceID].Status != core.InvoiceOnHold {
		t.Fatal("invoice should be on hold")
	}
}

func TestInvoiceContextMasksBankDetails(t *testing.T) {
	svc := migrated(t)
	id := core.SortedKeys(svc.Store.Invoices)[0]
	view, err := invoiceContext(svc, id)
	if err != nil {
		t.Fatal(err)
	}
	full := svc.Store.Vendors[view.Invoice.VendorID].BankAccount
	if view.Vendor.BankAccount == full || !strings.HasSuffix(view.Vendor.BankAccount, full[len(full)-4:]) {
		t.Fatalf("bank account not masked: %q", view.Vendor.BankAccount)
	}
}

func firstHeld(svc *Service) string {
	for _, id := range core.SortedKeys(svc.Store.Invoices) {
		if m, _ := svc.Match(id); svc.Store.Invoices[id].Status == core.InvoiceOnHold && !m.Clean {
			return id
		}
	}
	return ""
}

func hasIssue(m core.MatchResult, kind core.IssueKind) bool {
	for _, is := range m.Issues {
		if is.Kind == kind {
			return true
		}
	}
	return false
}

func TestOnlyReviewersCloseTasks(t *testing.T) {
	svc := migrated(t)
	ProcessInvoices(context.Background(), svc, nil, io.Discard)
	tid := core.SortedKeys(svc.Store.Tasks)[0]
	outsider := governance.Identity{ID: "user:buyer", Kind: "human", Autonomy: governance.FullyAutonomous, Permissions: []string{governance.ActPOCreate}}
	if _, err := svc.Decide(outsider, tid, false, "x"); err == nil {
		t.Fatal("a person without invoice.review must not reject")
	}
	if _, err := svc.Decide(outsider, tid, true, "x"); err == nil {
		t.Fatal("a person without invoice.review must not approve")
	}
}

// Invoices that already have a decision cannot be escalated or posted again.
func TestDecidedInvoicesAreClosed(t *testing.T) {
	svc := migrated(t)
	for _, id := range core.SortedKeys(svc.Store.Invoices) {
		inv := svc.Store.Invoices[id]
		if inv.Status != core.InvoicePosted {
			continue
		}
		tasks := len(svc.Store.Tasks)
		if _, err := svc.Escalate(MatchRule, id, "x", ""); err == nil {
			t.Fatal("a posted invoice must not be escalated")
		}
		if _, err := svc.PostInvoice(humanApprover, id, ""); err == nil {
			t.Fatal("a posted invoice must not be posted again")
		}
		if len(svc.Store.Tasks) != tasks {
			t.Fatal("no review task for a decided invoice")
		}
		return
	}
	t.Fatal("no posted invoice in the fixture")
}
