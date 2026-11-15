package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/savingsagent"
)

// agentSavings has the savings opportunity agent recommend on the
// opportunities in a company's purchasing over a period (ADR-0072 As
// built, T105d): it prints what it recommended, each object named with
// the first line of its recommendation, and the model's reply, or why
// there was nothing to recommend on; a buyer decides.
func agentSavings(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent savings", flag.ContinueOnError)
	company := fs.String("company", "", "company code")
	period := fs.String("period", "", "FROM..TO, YYYY-MM-DD..YYYY-MM-DD (default: the year to today, UTC)")
	limit := fs.Int("max", savingsagent.MaxOpportunities, "the most recommendations the run makes")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *company == "" {
		return errors.New("agent savings: want -company CODE [-period FROM..TO] [-max N]")
	}
	if *limit < 1 {
		return fmt.Errorf("agent savings: %w", savingsagent.ErrMax)
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&savingsagent.Agent{LLM: newRunner(c), API: c, Max: *limit}).Advise(ctx, *company, *period)
	return recommended("agent savings", res, runErr)
}

// savingsEval has the savings opportunity agent recommend on the task's
// company over its period as erp agent savings does; what it recommended
// is the checks' to judge, and its reply theirs too.
func savingsEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "company", "period")
	if err != nil {
		return err
	}
	res, err := (&savingsagent.Agent{LLM: env.LLM, API: env.Agent}).Advise(ctx, t["company"], t["period"])
	if err != nil {
		return fmt.Errorf("savings: %w", err)
	}
	graded(env, res)
	return nil
}
