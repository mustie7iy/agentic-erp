package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

// cli runs one command in-process and returns its output and exit code.
func cli(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	out, errOut = &buf, &buf
	code := run(context.Background(), args)
	return buf.String(), code
}

// must runs a command that has to succeed.
func must(t *testing.T, args ...string) string {
	t.Helper()
	o, code := cli(t, args...)
	if code != 0 {
		t.Fatalf("erp %s failed (%d):\n%s", strings.Join(args, " "), code, o)
	}
	return o
}

// fails runs a command that has to fail with the given message.
func fails(t *testing.T, want string, args ...string) {
	t.Helper()
	o, code := cli(t, args...)
	if code == 0 || !strings.Contains(o, want) {
		t.Fatalf("erp %s: want failure containing %q, got (%d):\n%s", strings.Join(args, " "), want, code, o)
	}
}

func idIn(t *testing.T, pattern, s string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindString(s)
	if m == "" {
		t.Fatalf("no %s in:\n%s", pattern, s)
	}
	return m
}

type fakeExtractor struct{ e *capture.Extraction }

func (f fakeExtractor) Extract(context.Context, []byte) (*capture.Extraction, error) { return f.e, nil }

// TestPhase1EndToEnd runs the whole Phase 1 flow through the CLI: migrate,
// generate configuration from SAP customizing, fix the SoD conflict,
// bootstrap, change configuration through the full approval process,
// procure-to-pay, payment with a bank file, invoice capture, the delivery
// agents, and audit verification.
func TestPhase1EndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	clock = func() time.Time { return time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC) }
	defer func() { clock = time.Now }()

	// Migrate SAP data.
	must(t, "gen-mock-sap", "-year", "2026")
	if o := must(t, "migrate", "-company", "1000,2000"); !strings.Contains(o, "Reconciliation PASSED") {
		t.Fatal(o)
	}

	// Generate configuration from SAP customizing: the SoD conflict blocks it.
	fails(t, "validation errors", "config", "generate", "-company", "1000,2000", "-out", "draft.yaml")
	draft, err := config.Load("draft.yaml")
	if err != nil {
		t.Fatal(err)
	}
	super := draft.User("super1")
	super.Roles = slices.DeleteFunc(super.Roles, func(r string) bool { return r == "Z_TREASURY_MGR" })
	draft.Users = append(draft.Users, config.User{ID: "analyst1", Name: "Analyst", Roles: []string{"config_analyst"}})
	doc, _ := draft.Marshal()
	os.WriteFile("fixed.yaml", doc, 0o644)
	if o := must(t, "config", "validate", "-file", "fixed.yaml"); !strings.Contains(o, "Validation") {
		t.Fatal(o)
	}
	fails(t, "validation errors", "config", "validate", "-file", "draft.yaml")
	fails(t, "is required", "config", "bootstrap", "-file", "fixed.yaml")
	must(t, "config", "bootstrap", "-file", "fixed.yaml", "-as", "architect1")
	if o := must(t, "config", "show"); !strings.Contains(o, "# version 1") {
		t.Fatal(o)
	}

	// Change configuration: tier 3, needs evidence, two approvals, then activation.
	if o := must(t, "regress", "capture"); !strings.Contains(o, "decision exception") {
		t.Fatal(o)
	}
	draft.Company("1000").AutoPostLimit = "100000.00"
	doc, _ = draft.Marshal()
	os.WriteFile("limit.yaml", doc, 0o644)
	fails(t, "lacks config.propose", "config", "propose", "-file", "limit.yaml", "-title", "x", "-as", "buyer1")
	o := must(t, "config", "propose", "-file", "limit.yaml", "-title", "Raise auto-post limit", "-rationale", "fewer approvals", "-as", "analyst1")
	cr := idIn(t, `CR-\d{4}`, o)
	if !strings.Contains(o, "pending_approval") || !strings.Contains(o, "simulation (passed=") || !strings.Contains(o, "regression (passed=false)") {
		t.Fatalf("propose output:\n%s", o)
	}
	fails(t, "not approved", "config", "activate", "-cr", cr, "-as", "architect1")
	must(t, "config", "approve", "-cr", cr, "-as", "owner1", "-comment", "ok")
	fails(t, "already approved", "config", "approve", "-cr", cr, "-as", "owner1")
	must(t, "config", "approve", "-cr", cr, "-as", "controller1")
	if o := must(t, "config", "activate", "-cr", cr, "-as", "architect1"); !strings.Contains(o, "version 2") {
		t.Fatal(o)
	}
	// The suite was captured under v1; after activation it no longer describes the active configuration.
	fails(t, "capture a new suite", "config", "evidence", "-cr", cr)
	if o := must(t, "config", "history"); !strings.Contains(o, cr) || !strings.Contains(o, "active") {
		t.Fatal(o)
	}
	if o := must(t, "config", "list"); !strings.Contains(o, "active") {
		t.Fatal(o)
	}
	if o := must(t, "config", "diff", "-cr", cr); !strings.Contains(o, "auto_post_limit") {
		t.Fatal(o)
	}
	if o := must(t, "regress", "run", "-cr", cr); !strings.Contains(o, "change") {
		t.Fatal(o)
	}
	if o := must(t, "config", "rollback", "-to", "1", "-as", "architect1", "-reason", "test rollback"); !strings.Contains(o, "restored as version 3") {
		t.Fatal(o)
	}
	reject := strings.Replace(string(doc), "net_days: 14", "net_days: 21", 1)
	os.WriteFile("terms.yaml", []byte(reject), 0o644)
	o = must(t, "config", "propose", "-file", "terms.yaml", "-title", "Terms", "-as", "analyst1")
	cr2 := idIn(t, `CR-\d{4}`, o)
	if o := must(t, "config", "reject", "-cr", cr2, "-as", "owner1", "-comment", "no"); !strings.Contains(o, "rejected") {
		t.Fatal(o)
	}

	// Invoices are processed under the active configuration.
	if o := must(t, "invoices"); !strings.Contains(o, "posted") {
		t.Fatal(o)
	}
	o = must(t, "tasks")
	task := idIn(t, `TASK-\d{8}`, o)
	fails(t, "lacks", "reject", "-task", task, "-as", "buyer1")
	must(t, "reject", "-task", task, "-as", "apclerk1", "-comment", "duplicate")

	// Procure-to-pay.
	fails(t, "lacks requisition.create", "req", "create", "-as", "apclerk1", "-line", "Paper;10;25.00;520000")
	o = must(t, "req", "create", "-as", "buyer1", "-vendor", "100001", "-line", "Paper;10;25.00;520000;MAT-P")
	pr := idIn(t, `PR-\d{8}`, o)
	must(t, "req", "approve", "-id", pr, "-as", "purchmgr1")
	o = must(t, "po", "create", "-req", pr, "-as", "buyer1")
	po := idIn(t, `PO-\d{8}`, o)
	if !strings.Contains(o, "sent by email") {
		t.Fatal(o)
	}
	must(t, "gr", "post", "-po", po, "-line", "10", "-qty", "10", "-as", "whclerk1")
	fails(t, "over-delivery", "gr", "post", "-po", po, "-line", "10", "-qty", "1", "-as", "whclerk1")
	must(t, "gr", "post", "-po", po, "-line", "10", "-qty", "2", "-reverse", "-as", "whclerk1")
	o = must(t, "req", "create", "-as", "buyer1", "-line", "Servers;1;20000.00;530000")
	pr2 := idIn(t, `PR-\d{8}`, o)
	must(t, "req", "reject", "-id", pr2, "-as", "purchmgr1", "-comment", "no budget")
	o = must(t, "contract", "create", "-as", "buyer1", "-vendor", "100003", "-from", "2026-01-01", "-to", "2026-12-31", "-target", "1000.00", "-item", "MAT-T;Toner;40.00;520000")
	if !strings.Contains(o, "target 1,000.00 USD") {
		t.Fatal(o)
	}

	// Supplier master.
	o = must(t, "vendor", "create", "-as", "mdclerk1", "-id", "300001", "-name", "New Supplier", "-tax", "77-1", "-bank", "GB82WEST12345698765432")
	mdc := idIn(t, `MDC-\d{8}`, o)
	fails(t, "lacks vendor.approve", "vendor", "approve", "-change", mdc, "-as", "mdclerk1")
	must(t, "vendor", "approve", "-change", mdc, "-as", "mdmgr1")
	o = must(t, "vendor", "bank", "-as", "mdclerk1", "-id", "100001", "-bank", "GB82WEST12345698765432", "-reason", "letter")
	bank := idIn(t, `MDC-\d{8}`, o)
	must(t, "vendor", "reject", "-change", bank, "-as", "mdmgr1", "-comment", "unverified")
	must(t, "vendor", "block", "-as", "mdclerk1", "-id", "100002", "-reason", "audit")
	o = must(t, "vendor", "unblock", "-as", "mdclerk1", "-id", "100002", "-reason", "cleared")
	must(t, "vendor", "approve", "-change", idIn(t, `MDC-\d{8}`, o), "-as", "mdmgr1")

	// Payments with an ISO 20022 file.
	o = must(t, "pay", "propose", "-as", "treas1", "-company", "1000", "-date", "2027-06-30")
	payRun := idIn(t, `PAY-\d{8}`, o)
	if !strings.Contains(o, "excluded 100012") {
		t.Fatalf("blocked vendor excluded:\n%s", o)
	}
	fails(t, "lacks payment.release", "pay", "release", "-run", payRun, "-as", "treas1")
	o = must(t, "pay", "release", "-run", payRun, "-as", "treasmgr1")
	if !strings.Contains(o, "released") || !strings.Contains(o, "pain.001") {
		t.Fatal(o)
	}
	xmlFile, _ := os.ReadFile("data/bankfiles/" + payRun + ".xml")
	if !bytes.Contains(xmlFile, []byte("CstmrCdtTrfInitn")) {
		t.Fatal("bank file not written")
	}
	fails(t, "nothing is due", "pay", "propose", "-as", "treas1", "-company", "1000", "-date", "2027-06-30")

	// Invoice capture with a stand-in extractor.
	s, _ := core.LoadStore("data/store.json")
	var target *core.PurchaseOrder
	for _, id := range core.SortedKeys(s.PurchaseOrders) {
		if p := s.PurchaseOrders[id]; p.CompanyCode == "1000" && s.Vendors[p.VendorID].TaxID != "" {
			target = p
			break
		}
	}
	v := s.Vendors[target.VendorID]
	ext := &capture.Extraction{VendorName: v.Name, VendorTaxID: v.TaxID, InvoiceNumber: "E2E-1", InvoiceDate: "2026-10-01",
		PurchaseOrder: target.ID, Currency: "USD", Lines: []capture.Line{{Description: "x", Quantity: "1", Amount: "10.00"}},
		TaxAmount: "0.80", GrossAmount: "10.80", Confidence: "high"}
	newExtractor = func() capture.Extractor { return fakeExtractor{ext} }
	defer func() { newExtractor = func() capture.Extractor { return &capture.ClaudeExtractor{LLM: llm.New()} } }()
	os.WriteFile("invoice.pdf", []byte("%PDF-1.4 e2e"), 0o644)
	if o := must(t, "capture", "-file", "invoice.pdf"); !strings.Contains(o, "captured") {
		t.Fatal(o)
	}
	fails(t, "already captured", "capture", "-file", "invoice.pdf")
	fails(t, "lacks invoice.capture", "capture", "-file", "invoice.pdf", "-as", "buyer1")

	// Delivery agents against the scripted model.
	srv := llmtest.New(t,
		llmtest.Tool("get_active_config", map[string]any{}),
		llmtest.Final("No change needed."),
		llmtest.Tool("profile_extract", map[string]any{"company": "1000", "year": "2026"}),
		llmtest.Final("Company 1000 is ready."),
		llmtest.Tool("run_suite", map[string]any{"change_request": cr}),
		llmtest.Final("Safe to approve."),
	)
	newRunner = srv.Runner
	defer func() { newRunner = llm.New }()
	if o := must(t, "agent", "config", "-extract", "data/sap-extract.json", "check", "the", "limits"); !strings.Contains(o, "No change needed") {
		t.Fatal(o)
	}
	if o := must(t, "agent", "migration", "-company", "1000"); !strings.Contains(o, "ready") {
		t.Fatal(o)
	}
	if o := must(t, "agent", "test", "-cr", cr); !strings.Contains(o, "Safe to approve") {
		t.Fatal(o)
	}
	fails(t, "describe the change", "agent", "config")

	// Every action above is in an intact audit chain.
	if o := must(t, "audit-verify"); !strings.Contains(o, "chain intact") {
		t.Fatal(o)
	}
	var events int
	b, _ := os.ReadFile("data/audit.jsonl")
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) == nil {
			events++
		}
	}
	if events < 30 {
		t.Fatalf("expected a full audit trail, got %d events", events)
	}
}

func TestCLIUsageAndErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, code := cli(t); code != 2 {
		t.Fatal("no command")
	}
	if _, code := cli(t, "nope"); code != 2 {
		t.Fatal("unknown command")
	}
	if _, code := cli(t, "migrate", "-h"); code != 0 {
		t.Fatal("-h is not an error")
	}
	fails(t, "usage: erp config", "config")
	fails(t, "usage: erp pay", "pay", "bogus")
	fails(t, "no active configuration", "req", "create", "-as", "buyer1", "-line", "a;1;1;1")
	fails(t, "want description", "req", "create", "-as", "buyer1", "-line", "a;1")
	fails(t, "is required", "po", "create")
	fails(t, "-cr is required", "config", "evidence")
	fails(t, "is required", "config", "validate")
	fails(t, "no active configuration", "regress", "run")
	fails(t, "want material", "contract", "create", "-as", "b", "-vendor", "v", "-target", "1", "-item", "x")
	fails(t, "invalid decimal", "gr", "post", "-po", "p", "-qty", "abc", "-as", "u")
}

func TestMigrateJSONWriteFailureIsReported(t *testing.T) {
	t.Chdir(t.TempDir())
	must(t, "gen-mock-sap")
	os.Mkdir("report.json", 0o755) // a directory where the file should go
	fails(t, "write report.json", "migrate", "-json", "report.json")
}
