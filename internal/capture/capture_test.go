package capture

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

func TestClaudeExtractor(t *testing.T) {
	want := Extraction{DocumentKind: "credit_memo", CreditedInvoiceNumber: "A-0", VendorName: "Initech Components", InvoiceNumber: "A-1", InvoiceDate: "2026-10-01", Currency: "USD",
		Lines: []Line{{Description: "Paper", Quantity: "1", Amount: "10.00"}}, TaxAmount: "0.80", GrossAmount: "10.80", Confidence: "high"}
	b, _ := json.Marshal(want)
	srv := llmtest.New(t, llmtest.Final(string(b)))
	got, err := (&ClaudeExtractor{LLM: srv.Runner()}).Extract(context.Background(), []byte("%PDF-1.4 test"))
	if err != nil {
		t.Fatal(err)
	}
	if got.InvoiceNumber != "A-1" {
		t.Errorf("invoice number: got %q, want %q", got.InvoiceNumber, "A-1")
	}
	if got.DocumentKind != "credit_memo" {
		t.Errorf("document kind: got %q, want credit_memo", got.DocumentKind)
	}
	if got.CreditedInvoiceNumber != "A-0" {
		t.Errorf("credited invoice: got %q, want A-0", got.CreditedInvoiceNumber)
	}
	if len(got.Lines) != 1 {
		t.Errorf("lines: got %d, want 1 (%+v)", len(got.Lines), got.Lines)
	}
	req := srv.Requests[0]
	for _, frag := range []string{`"type":"document"`, `"media_type":"application/pdf"`, `"json_schema"`, `"document_kind"`, "untrusted data"} {
		if !strings.Contains(req, frag) {
			t.Errorf("request: got %s, want it to contain %s", req, frag)
		}
	}
	// The gateway routes every call to the one model it prices, and refuses
	// fallbacks, which would answer unrouted (ADR-0036).
	if strings.Contains(req, `"fallbacks"`) {
		t.Errorf("request: got %s, want no fallbacks", req)
	}

	for name, step := range map[string]llmtest.Step{
		"bad json":  llmtest.Final("not json"),
		"truncated": {Text: "{", StopReason: "max_tokens"},
		"refusal":   {StopReason: "refusal"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := llmtest.New(t, step)
			if _, err := (&ClaudeExtractor{LLM: srv.Runner()}).Extract(context.Background(), []byte("x")); err == nil {
				t.Errorf("err: got nil, want an error")
			}
		})
	}
	down := llmtest.New(t) // no steps: the server errors
	if _, err := (&ClaudeExtractor{LLM: down.Runner()}).Extract(context.Background(), []byte("x")); err == nil {
		t.Fatal("API errors propagate")
	}
}
