package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
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

// TestClaudeOrderExtractor (T90a): a purchase order is read into the
// order's schema, a PDF as a PDF document and plain text as a text
// document, with the prompt that treats the document as data.
func TestClaudeOrderExtractor(t *testing.T) {
	want := OrderExtraction{CustomerName: "Northwind", CustomerNumber: "BP1", OrderNumber: "PO-7", Currency: "CAD",
		Items: []OrderItem{{ItemKey: "K1", Description: "Brackets", Quantity: "2", Unit: "EA", UnitPrice: "5.00"}}, Confidence: "high"}
	b, _ := json.Marshal(want)
	for contentType, frag := range map[string]string{"application/pdf": `"media_type":"application/pdf"`, "text/plain": `"media_type":"text/plain"`} {
		t.Run(contentType, func(t *testing.T) {
			srv := llmtest.New(t, llmtest.Final(string(b)))
			got, err := (&ClaudeExtractor{LLM: srv.Runner()}).ExtractOrder(context.Background(), []byte("PO-7"), contentType)
			if err != nil {
				t.Fatal(err)
			}
			if got.CustomerNumber != "BP1" || len(got.Items) != 1 || got.Items[0].UnitPrice != "5.00" {
				t.Errorf("extraction: got %+v, want %+v", got, want)
			}
			for _, f := range []string{frag, `"customer_tax_number"`, "untrusted data"} {
				if !strings.Contains(srv.Requests[0], f) {
					t.Errorf("request: got %s, want it to contain %s", srv.Requests[0], f)
				}
			}
		})
	}
	down := llmtest.New(t)
	if _, err := (&ClaudeExtractor{LLM: down.Runner()}).ExtractOrder(context.Background(), []byte("x"), "text/plain"); err == nil {
		t.Error("API error: got nil, want it to propagate")
	}
}

// TestRefusal: the model gateway's refusals of the tenant's call pass
// through as they are; its failures and other errors do not.
func TestRefusal(t *testing.T) {
	budget := problem.New(429, "agents.budget_exhausted", "Too many requests", "spent")
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"a refusal":           {fmt.Errorf("capture: %w", budget), "agents.budget_exhausted"},
		"the gateway failing": {problem.New(502, "agents.model_unavailable", "Bad gateway", "down"), ""},
		"another problem":     {problem.Unprocessable("x.y", "z"), ""},
		"not a problem":       {errors.New("eof"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := ""
			if p := Refusal(c.err); p != nil {
				got = p.Code
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
