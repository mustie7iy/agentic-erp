package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/countagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
)

// agentCountPlan has the count planning agent propose the materials of a
// company's plant to count this period (ADR-0072, ADR-0082 As built S2e):
// an inventory controller reads its recommendation and decides it, and
// creates the count documents, or not.
func agentCountPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent count-plan", flag.ContinueOnError)
	company := fs.String("company", "", "company code")
	plant := fs.String("plant", "", "the plant's code")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *company == "" || *plant == "" {
		return errors.New("agent count-plan: want -company CODE -plant CODE")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&countagent.Agent{LLM: newRunner(c), API: c}).Plan(ctx, *company, *plant)
	return recommended("agent count-plan", res, runErr)
}

// countPlanningEval has the count planning agent propose the counts of
// the task's plant in its company as erp agent count-plan does; what it
// recommended is the checks' to judge, and its reply theirs too.
func countPlanningEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "company", "plant")
	if err != nil {
		return err
	}
	res, err := (&countagent.Agent{LLM: env.LLM, API: env.Agent}).Plan(ctx, t["company"], t["plant"])
	if err != nil {
		return fmt.Errorf("count_planning: %w", err)
	}
	graded(env, res)
	return nil
}
