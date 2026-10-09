package configagent

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

// Without a clock the agent uses the current time for the replay window.
func TestDefaultClock(t *testing.T) {
	a := newAgent(t, llmtest.New(t))
	a.Now = nil
	c := testkit.Config(t)
	c.Company("1000").AutoPostLimit = "100000.00"
	cr, err := a.Propose("limit", "x", testkit.YAML(t, c))
	if err != nil || len(cr.Evidence) != 2 {
		t.Fatalf("%v %+v", err, cr)
	}
}
