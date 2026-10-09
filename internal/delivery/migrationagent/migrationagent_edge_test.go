package migrationagent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

// flaky answers once, then fails: the extract works but the rehearsal's own extract does not.
type flaky struct {
	sap.Source
	calls atomic.Int32
}

func (f *flaky) Extract(ctx context.Context, s sap.Scope) (*sap.Extract, error) {
	if f.calls.Add(1) > 1 {
		return nil, errors.New("connection lost")
	}
	return f.Source.Extract(ctx, s)
}

func TestExplainWhenRehearsalExtractFails(t *testing.T) {
	a := &Agent{Source: &flaky{Source: testkit.MemSource{E: testkit.Extract()}}, Store: core.NewStore()}
	if _, err := a.Explain(context.Background(), "1000", "2026", "400000", ""); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("want the rehearsal's extract error, got %v", err)
	}
}

func TestCustomMappingAndExplainTool(t *testing.T) {
	m := migration.DefaultMapping()
	m.PaymentTerms["NT99"] = 99
	a := &Agent{Source: corrupt{testkit.MemSource{E: testkit.Extract()}}, Store: core.NewStore(), Mapping: m}
	if res, err := a.Rehearse(context.Background(), "1000", "2026", ""); err != nil || res.Report.Passed {
		t.Fatalf("custom-mapping rehearsal of a corrupt extract: %v", err)
	}
	srv := llmtest.New(t,
		llmtest.Tool("explain_difference", map[string]any{"company": "1000", "year": "2026", "account": "100000"}),
		llmtest.Step{Text: "Document 0100000002 is missing.", Expect: "missing_in_core"},
	)
	a.LLM = srv.Runner()
	if out, err := a.Assess(context.Background(), "1000", "2026"); err != nil || !strings.Contains(out, "missing") {
		t.Fatalf("%v %q", err, out)
	}
}
