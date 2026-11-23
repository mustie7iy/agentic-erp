package main

import (
	"context"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/countagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// planCounts runs the count planning agent on a company's plant.
func planCounts(ctx context.Context, r *llm.Runner, c *apitools.Client, company, plant string) (recommend.Result, error) {
	return (&countagent.Agent{LLM: r, API: c, Now: agentDay}).Plan(ctx, company, plant)
}

// agentCountPlan has the count planning agent propose the materials of a
// company's plant to count this period (ADR-0072, ADR-0082 As built S2e):
// an inventory controller reads its recommendation and decides it, and
// creates the count documents, or not.
func agentCountPlan(ctx context.Context, args []string) error {
	return companyAgent(ctx, args, "agent count-plan", companyArg{"plant", "CODE", "the plant's code"}, planCounts)
}

// countPlanningEval has the count planning agent propose the counts of
// the task's plant in its company as erp agent count-plan does; what it
// recommended is the checks' to judge, and its reply theirs too.
func countPlanningEval(ctx context.Context, env eval.Env) error {
	return companyEval(ctx, env, countagent.Type, "plant", planCounts)
}
