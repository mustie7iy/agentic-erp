package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

// The production extractor is built lazily; building it must not need the network.
var defaultExtractor = newExtractor

func TestDefaultExtractor(t *testing.T) {
	if _, ok := defaultExtractor().(*capture.ClaudeExtractor); !ok {
		t.Fatal("the default extractor reads invoices with Claude")
	}
}

func TestConfigCommandErrors(t *testing.T) {
	cfg := setup(t)
	os.WriteFile("afile", nil, 0o644)
	os.Mkdir("outdir", 0o755)
	os.Mkdir("empty", 0o755)

	// generate
	fails(t, "invalid -source", "config", "generate", "-source", "nope")
	fails(t, "no such file", "config", "generate", "-source", "file:missing.json")
	fails(t, "not a directory", "config", "generate", "-out", "afile/x.yaml")
	fails(t, "is a directory", "config", "generate", "-out", "outdir")

	// validate: warnings alone pass; unreadable files fail
	warn := *cfg
	warn.Companies = append(warn.Companies[:0:0], cfg.Companies...)
	warn.Companies[0].TaxCodes = nil
	writeConfig(t, "warn.yaml", &warn)
	if o := must(t, "config", "validate", "-file", "warn.yaml"); !strings.Contains(o, "[warning]") {
		t.Fatal(o)
	}
	fails(t, "no such file", "config", "validate", "-file", "missing.yaml")

	// bootstrap and show
	fails(t, "no such file", "config", "bootstrap", "-file", "missing.yaml", "-as", "architect1")
	fails(t, "already has an active", "config", "bootstrap", "-file", "fixed.yaml", "-as", "architect1")
	fails(t, "no version 99", "config", "show", "-version", "99")
	fails(t, "no active configuration", "config", "show", "-config-dir", "empty")

	// propose: missing file, invalid proposal (findings shown), no suite yet
	fails(t, "no such file", "config", "propose", "-file", "missing.yaml", "-title", "t", "-as", "analyst1")
	bad := *cfg
	bad.Companies = append(bad.Companies[:0:0], cfg.Companies...)
	bad.Companies[0].Currency = "dollars"
	writeConfig(t, "bad.yaml", &bad)
	if o := must(t, "config", "propose", "-file", "bad.yaml", "-title", "Bad", "-as", "analyst1"); !strings.Contains(o, "finding: [error]") || !strings.Contains(o, "invalid") {
		t.Fatal(o)
	}
	limit := *cfg
	limit.Companies = append(limit.Companies[:0:0], cfg.Companies...)
	limit.Companies[0].AutoPostLimit = "100000.00"
	writeConfig(t, "limit.yaml", &limit)
	o := must(t, "config", "propose", "-file", "limit.yaml", "-title", "Limit", "-as", "analyst1")
	tier3 := idIn(t, `CR-\d{4}`, o)
	if !strings.Contains(o, "Note:") {
		t.Fatalf("a tier-3 proposal without a suite explains how to add evidence:\n%s", o)
	}
	terms := *cfg
	terms.PaymentTerms = append(terms.PaymentTerms[:0:0], cfg.PaymentTerms...)
	terms.PaymentTerms[0].NetDays = 21
	writeConfig(t, "terms.yaml", &terms)
	if o := must(t, "config", "propose", "-file", "terms.yaml", "-title", "Terms", "-as", "analyst1"); strings.Contains(o, "Note:") {
		t.Fatalf("a tier-2 proposal needs no evidence:\n%s", o)
	}

	// evidence: needs a suite; then succeeds
	fails(t, "no regression suite", "config", "evidence", "-cr", tier3)
	must(t, "regress", "capture")
	if o := must(t, "config", "evidence", "-cr", tier3); !strings.Contains(o, "pending_approval") {
		t.Fatal(o)
	}

	fails(t, "cannot roll back to version 99", "config", "rollback", "-to", "99", "-as", "architect1", "-reason", "r")
	fails(t, "no change request CR-9999", "config", "diff", "-cr", "CR-9999")

	// After a change that alters decisions is activated, the suite is stale
	// and the next proposal reports it.
	tol := *cfg
	tol.Companies = append(tol.Companies[:0:0], cfg.Companies...)
	tol.Companies[0].InvoiceTolerance.PricePercent = 15
	writeConfig(t, "tol.yaml", &tol)
	tolCR := idIn(t, `CR-\d{4}`, must(t, "config", "propose", "-file", "tol.yaml", "-title", "Tolerance", "-as", "analyst1"))
	must(t, "config", "approve", "-cr", tolCR, "-as", "owner1")
	must(t, "config", "activate", "-cr", tolCR, "-as", "architect1")
	fails(t, "capture a new suite", "config", "propose", "-file", "terms.yaml", "-title", "Terms again", "-as", "analyst1")
}

func TestRegressCommandErrors(t *testing.T) {
	cfg := setup(t)
	os.Mkdir("empty", 0o755)
	fails(t, "no regression suite", "regress", "run")
	os.MkdirAll("data", 0o755)
	os.WriteFile("data/suites", nil, 0o644)
	fails(t, "not a directory", "regress", "capture")
	os.Remove("data/suites")
	fails(t, "no active configuration", "regress", "capture", "-config-dir", "empty")
	fails(t, "no invoices", "regress", "capture", "-store", "empty.json")

	terms := *cfg
	terms.PaymentTerms = append(terms.PaymentTerms[:0:0], cfg.PaymentTerms...)
	terms.PaymentTerms[0].NetDays = 21
	writeConfig(t, "terms.yaml", &terms)
	cr := idIn(t, `CR-\d{4}`, must(t, "config", "propose", "-file", "terms.yaml", "-title", "Terms", "-as", "analyst1"))
	fails(t, "no change request CR-9999", "regress", "run", "-cr", "CR-9999")
	// A change request edited outside the process no longer parses.
	var state map[string]any
	b, _ := os.ReadFile("data/config/state.json")
	json.Unmarshal(b, &state)
	state["changes"].(map[string]any)[cr].(map[string]any)["proposed"] = "version: ["
	b, _ = json.Marshal(state)
	os.WriteFile("data/config/state.json", b, 0o644)
	fails(t, "configuration", "regress", "run", "-cr", cr)

	os.MkdirAll("data/suites", 0o755)
	os.WriteFile("data/suites/RS-zzzz.json", []byte(`{"id":"RS-zzzz","snapshot":{"companies":5}}`), 0o644)
	fails(t, "suite snapshot", "regress", "run")
}

func TestProcurementCommandErrors(t *testing.T) {
	setup(t)
	os.WriteFile("afile", nil, 0o644)
	fails(t, "invalid decimal", "req", "create", "-as", "buyer1", "-line", "a;x;1;520000")
	fails(t, "invalid decimal", "req", "create", "-as", "buyer1", "-line", "a;1;x;520000")
	fails(t, "not found", "req", "approve", "-id", "PR-NONE", "-as", "purchmgr1")
	fails(t, "not found", "po", "create", "-req", "PR-NONE", "-as", "buyer1")

	pr := idIn(t, `PR-\d{8}`, must(t, "req", "create", "-as", "buyer1", "-vendor", "100001", "-line", "Paper;1;10.00;520000"))
	must(t, "req", "approve", "-id", pr, "-as", "purchmgr1")
	// The outbox cannot be created, then cannot be written: the PO is not saved either time.
	os.WriteFile("data/outbox", nil, 0o644)
	fails(t, "not a directory", "po", "create", "-req", pr, "-as", "buyer1")
	os.Remove("data/outbox")
	os.Mkdir("data/outbox", 0o555)
	t.Cleanup(func() { os.Chmod("data/outbox", 0o755) })
	fails(t, "permission denied", "po", "create", "-req", pr, "-as", "buyer1")
	os.Chmod("data/outbox", 0o755)
	must(t, "po", "create", "-req", pr, "-as", "buyer1")

	fails(t, "invalid decimal", "contract", "create", "-as", "buyer1", "-vendor", "100003", "-target", "1", "-item", "M;d;x;520000")
	fails(t, "invalid decimal", "contract", "create", "-as", "buyer1", "-vendor", "100003", "-target", "x")
	fails(t, "not set up", "contract", "create", "-as", "buyer1", "-vendor", "999999", "-from", "2026-01-01", "-to", "2026-12-31",
		"-target", "1", "-item", "M;d;1.00;520000")

	fails(t, "already exists", "vendor", "create", "-as", "mdclerk1", "-id", "100001", "-name", "Dup")
	if o := must(t, "vendor", "create", "-as", "mdclerk1", "-id", "400001", "-name", "Initech Components"); !strings.Contains(o, "warning: ") {
		t.Fatalf("duplicate name and missing tax number are warned:\n%s", o)
	}
	fails(t, "not found", "vendor", "bank", "-as", "mdclerk1", "-id", "999999", "-bank", "X", "-reason", "r")
	fails(t, "needs a reason", "vendor", "block", "-as", "mdclerk1", "-id", "100002")
	fails(t, "is not blocked", "vendor", "unblock", "-as", "mdclerk1", "-id", "100002")

	// Payment release: the bank file directory cannot be created, then cannot
	// be written; nothing is saved until it can.
	run := idIn(t, `PAY-\d{8}`, must(t, "pay", "propose", "-as", "treas1", "-company", "1000"))
	os.WriteFile("data/bankfiles", nil, 0o644)
	fails(t, "not a directory", "pay", "release", "-run", run, "-as", "treasmgr1")
	os.Remove("data/bankfiles")
	os.Mkdir("data/bankfiles", 0o555)
	t.Cleanup(func() { os.Chmod("data/bankfiles", 0o755) })
	fails(t, "permission denied", "pay", "release", "-run", run, "-as", "treasmgr1")
	os.Chmod("data/bankfiles", 0o755)
	must(t, "pay", "release", "-run", run, "-as", "treasmgr1")
}

type errExtractor struct{}

func (errExtractor) Extract(context.Context, []byte) (*capture.Extraction, error) {
	return nil, errors.New("model unavailable")
}

func TestCaptureCommandBranches(t *testing.T) {
	setup(t)
	os.Mkdir("empty", 0o755)
	os.WriteFile("invoice.pdf", []byte("%PDF-1.4 branch"), 0o644)
	t.Cleanup(func() { newExtractor = defaultExtractor })
	fails(t, "no active configuration", "capture", "-file", "invoice.pdf", "-config-dir", "empty")
	fails(t, "not a configured user", "capture", "-file", "invoice.pdf", "-as", "nobody")
	fails(t, "no such file", "capture", "-file", "missing.pdf")
	newExtractor = func() capture.Extractor { return errExtractor{} }
	fails(t, "model unavailable", "capture", "-file", "invoice.pdf")
	s, _ := core.LoadStore(defaultStore)
	v := s.Vendors["100001"]
	newExtractor = func() capture.Extractor {
		return fakeExtractor{&capture.Extraction{VendorName: v.Name, VendorTaxID: v.TaxID, InvoiceNumber: "W-1", InvoiceDate: "2026-10-01",
			Currency: "USD", Lines: []capture.Line{{Description: "x", Quantity: "1", Amount: "10.00"}}, TaxAmount: "0.80", GrossAmount: "99.00", Confidence: "high"}}
	}
	if o := must(t, "capture", "-file", "invoice.pdf"); !strings.Contains(o, "warning: printed gross") {
		t.Fatal(o)
	}
}

func TestAgentCommandBranches(t *testing.T) {
	cfg := setup(t)
	must(t, "regress", "capture")

	// agent config: unreadable extract, a failing model, a filed change, and
	// an empty store falling back to company 1000 for the extract scope.
	fails(t, "no such file", "agent", "config", "-extract", "missing.json", "change")
	fakeModel(t, broken)
	failsAny(t, "agent", "config", "change")
	terms := *cfg
	terms.PaymentTerms = append(terms.PaymentTerms[:0:0], cfg.PaymentTerms...)
	terms.PaymentTerms[0].NetDays = 21
	doc, _ := terms.Marshal()
	fakeModel(t,
		llmtest.Tool("propose_change", map[string]any{"title": "Terms", "rationale": "vendor request", "yaml": string(doc)}),
		llmtest.Final("Filed."))
	if o := must(t, "agent", "config", "change", "terms"); !strings.Contains(o, "Filed CR-") {
		t.Fatal(o)
	}
	fakeModel(t, llmtest.Final("Nothing to do."))
	must(t, "agent", "config", "-store", "empty.json", "-extract", "data/sap-extract.json", "check")

	// agent migration: bad source, failing model.
	fails(t, "invalid -source", "agent", "migration", "-source", "nope")
	fakeModel(t, broken)
	failsAny(t, "agent", "migration")

	// agent test: failing model, then a review that saves a UAT script.
	fakeModel(t, broken)
	failsAny(t, "agent", "test", "-cr", "CR-0001")
	fakeModel(t,
		llmtest.Tool("save_uat_script", map[string]any{"change_request": "CR-0001", "script": "1. Pay vendor. Expected: paid."}),
		llmtest.Final("Safe."))
	if o := must(t, "agent", "test", "-cr", "CR-0001"); !strings.Contains(o, "UAT script: ") {
		t.Fatal(o)
	}
}

func TestQueryWithoutCSVAndClosedTasks(t *testing.T) {
	t.Chdir(t.TempDir())
	fixedClock(t)
	must(t, "gen-mock-sap")
	must(t, "migrate", "-company", "1000")
	if o := must(t, "query", "-measures", "revenue", "-by", "fiscal_month"); !strings.Contains(o, "rows") {
		t.Fatal(o)
	}
	must(t, "invoices")
	task := idIn(t, `TASK-\d{8}`, must(t, "tasks"))
	must(t, "reject", "-task", task, "-as", "controller", "-comment", "dup")
	if o := must(t, "tasks"); strings.Contains(o, task) {
		t.Fatal("closed tasks are not listed")
	}
}
