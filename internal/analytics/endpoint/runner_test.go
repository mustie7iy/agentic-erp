package endpoint

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestRunnerModel: a saved report's runner is refused as a query is when
// the tenant has no model to answer under.
func TestRunnerModel(t *testing.T) {
	e := (&Endpoints{Configs: docs{err: problem.Conflict("config.not_bootstrapped", "none")}}).Init()
	if _, _, err := e.Runner(context.Background()); problem.From(err).Code != "config.not_bootstrapped" {
		t.Errorf("runner: got %v, want config.not_bootstrapped", err)
	}
}
