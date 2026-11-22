package main

import (
	"context"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/demandagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// forecastDemand runs the demand forecasting agent on a company's
// material.
func forecastDemand(ctx context.Context, r *llm.Runner, c *apitools.Client, company, material string) (recommend.Result, error) {
	return (&demandagent.Agent{LLM: r, API: c}).Forecast(ctx, company, material)
}

// agentDemandForecast has the demand forecasting agent forecast a
// company's material at its plants (ADR-0072, ADR-0082 As built S2m): a
// planner reads its recommendation and decides it, and sets the
// material's planning data or runs planning, or not.
func agentDemandForecast(ctx context.Context, args []string) error {
	return companyAgent(ctx, args, "agent demand-forecast", companyArg{"material", "NUMBER", "the material's number"}, forecastDemand)
}

// demandForecastEval has the demand forecasting agent forecast the
// task's material in its company as erp agent demand-forecast does; what
// it recommended is the checks' to judge, and its reply theirs too.
func demandForecastEval(ctx context.Context, env eval.Env) error {
	return companyEval(ctx, env, demandagent.Type, "material", forecastDemand)
}
