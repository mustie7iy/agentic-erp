package telemetry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Agent telemetry (ADR-0043, T54c): what agents cost and how people judge
// what they propose, by agent kind (an agent type, never a principal).
// The exact cost of a call is in the call ledger (agents.model_call,
// getAiUsage); these series are for trends and alerts.

// modelBuckets bound a model call's latency: seconds to the minutes a long
// answer takes (agents.DefaultTimeout).
var modelBuckets = []float64{.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// Decisions on an agent's proposal. Anything but approved is an override:
// the override rate is the share of proposals a person rejected, or
// modified before approving.
const (
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
	DecisionModified = "modified" // approved after a person changed it
)

// TokenKinds are the kind label's values: the token kinds a call is
// billed for.
var TokenKinds = []string{"input", "output", "cache_read", "cache_write"}

// Decisions are the decision label's values.
var Decisions = []string{DecisionApproved, DecisionRejected, DecisionModified}

// agentMetrics are the agent families, registered by NewMetrics.
type agentMetrics struct {
	tokens    *prometheus.CounterVec
	cost      *prometheus.CounterVec
	calls     *prometheus.HistogramVec
	decisions *prometheus.CounterVec
}

func (m *Metrics) registerAgents() {
	m.agents = agentMetrics{
		tokens: m.counterVec("erp_model_tokens_total", "Model tokens through the gateway, by agent kind, alias and token kind.",
			"agent_kind", "alias", "kind"),
		cost: m.counterVec("erp_model_cost_micro_total", "Model cost through the gateway in millionths of the currency, by agent kind; a trend, not an amount.",
			"agent_kind", "currency"),
		calls: m.histogramVec("erp_model_call_duration_seconds", "Model call latency through the gateway, by agent kind, alias and how the call ended.",
			modelBuckets, "agent_kind", "alias", "status"),
		decisions: m.counterVec("erp_agent_proposals_decided_total", "Agent-raised approval requests a person decided, by agent kind and decision.",
			"agent_kind", "decision"),
	}
}

// ModelCall is one call the gateway forwarded and recorded.
type ModelCall struct {
	AgentKind, Alias, Status string
	Duration                 time.Duration
	Input, Output            int64
	CacheRead, CacheWrite    int64
	CostMicro                int64 // money.Micro of Currency
	Currency                 string
}

// ModelCalled counts a recorded model call.
func (m *Metrics) ModelCalled(c ModelCall) {
	for i, n := range []int64{c.Input, c.Output, c.CacheRead, c.CacheWrite} {
		m.agents.tokens.WithLabelValues(c.AgentKind, c.Alias, TokenKinds[i]).Add(float64(n))
	}
	m.agents.cost.WithLabelValues(c.AgentKind, c.Currency).Add(float64(c.CostMicro))
	m.agents.calls.WithLabelValues(c.AgentKind, c.Alias, c.Status).Observe(c.Duration.Seconds())
}

// ProposalDecided counts a committed decision on an agent's proposal.
func (m *Metrics) ProposalDecided(agentKind, decision string) {
	m.agents.decisions.WithLabelValues(agentKind, decision).Inc()
}
