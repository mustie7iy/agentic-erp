package telemetry

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentMetrics(t *testing.T) {
	m := NewMetrics()
	m.ModelCalled(ModelCall{AgentKind: "ap_exceptions", Alias: "erp-default", Status: "succeeded", Duration: 3 * time.Second,
		Input: 100, Output: 20, CacheRead: 5, CacheWrite: 7, CostMicro: 1234, Currency: "USD"})
	m.ProposalDecided("ap_exceptions", DecisionModified)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		`erp_model_tokens_total{agent_kind="ap_exceptions",alias="erp-default",kind="input"} 100`,
		`erp_model_tokens_total{agent_kind="ap_exceptions",alias="erp-default",kind="output"} 20`,
		`erp_model_tokens_total{agent_kind="ap_exceptions",alias="erp-default",kind="cache_read"} 5`,
		`erp_model_tokens_total{agent_kind="ap_exceptions",alias="erp-default",kind="cache_write"} 7`,
		`erp_model_cost_micro_total{agent_kind="ap_exceptions",currency="USD"} 1234`,
		`erp_model_call_duration_seconds_bucket{agent_kind="ap_exceptions",alias="erp-default",status="succeeded",le="5"} 1`,
		`erp_agent_proposals_decided_total{agent_kind="ap_exceptions",decision="modified"} 1`,
	} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("metrics: got %s, want %s", rec.Body.String(), want)
			}
		})
	}
}
