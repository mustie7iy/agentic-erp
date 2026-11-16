package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/planningagent"
)

// agentPlan has the planning agent propose a plan category's plan of a
// year for a company (ADR-0072, T105e): a controller reads its
// recommendation and decides it, and applies the lines as a plan batch,
// or not.
func agentPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent plan", flag.ContinueOnError)
	company := fs.String("company", "", "company code")
	category := fs.String("category", "", "the plan category's code")
	year := fs.Int("year", 0, "the fiscal year to plan, from the plan and actual figures of the year before")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *company == "" || *category == "" || *year < 1901 || *year > 2999 {
		return errors.New("agent plan: want -company CODE -category CODE -year YYYY (1901-2999)")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&planningagent.Agent{LLM: newRunner(c), API: c}).Propose(ctx, *company, *category, *year)
	return recommended("agent plan", res, runErr)
}

// planningEval has the planning agent propose the task's category's plan
// of its year in its company as erp agent plan does; what it recommended
// is the checks' to judge, and its reply theirs too.
func planningEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "company", "category", "year")
	if err != nil {
		return err
	}
	year, err := strconv.Atoi(t["year"])
	if err != nil {
		return fmt.Errorf("planning: the task's year %q: %w", t["year"], err)
	}
	res, err := (&planningagent.Agent{LLM: env.LLM, API: env.Agent}).Propose(ctx, t["company"], t["category"], year)
	if err != nil {
		return fmt.Errorf("planning: %w", err)
	}
	graded(env, res)
	return nil
}
