package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/creditagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// The recommending agents (ADR-0072): each command runs one session of
// its agent with the agents' token and prints what it recommended; each
// evaluation adapter runs the agent as its command does.

// recommended prints what a recommending agent's run did: its session,
// each recommendation the API recorded and the model's reply. They are
// printed even when the run failed afterwards: its recommendations wait
// for a person either way.
func recommended(command string, res recommend.Result, runErr error) error {
	if res.Session == "" {
		return runErr
	}
	w := printer()
	fmt.Fprintf(w, "Session %s: recommendations made %d, refused %d.\n", res.Session, len(res.Recommendations), res.Refused)
	for _, m := range res.Recommendations {
		fmt.Fprintf(w, "  recommendation %s: %s on %s %s\n", m.ID, m.Kind, m.ObjectType, m.ObjectID)
	}
	if res.Reply != "" {
		fmt.Fprintln(w, res.Reply)
	}
	return errors.Join(runErr, errs.Wrapf(w.Flush(), "%s: print", command))
}

// graded hands a recommending agent's run to its suite's checks: the
// reply ($reply.text), how many recommendations it made and how many
// were refused.
func graded(env eval.Env, res recommend.Result) {
	env.Reply(map[string]any{"text": res.Reply, "recommendations": len(res.Recommendations), "refused": res.Refused})
}

// agentCreditScore has the credit scoring agent review a customer's
// credit limit in a company (ADR-0072, T105a): a credit manager reads its
// recommendation and decides it, and changes the limit, or not.
func agentCreditScore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent credit-score", flag.ContinueOnError)
	company := fs.String("company", "", "company code")
	customer := fs.String("customer", "", "the customer's business partner id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *company == "" || *customer == "" {
		return errors.New("agent credit-score: want -company CODE -customer ID")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&creditagent.Agent{LLM: newRunner(c), API: c}).Score(ctx, *company, *customer)
	return recommended("agent credit-score", res, runErr)
}

// creditScoringEval has the credit scoring agent review the task's
// customer in its company as erp agent credit-score does; what it
// recommended is the checks' to judge, and its reply theirs too.
func creditScoringEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "company", "customer")
	if err != nil {
		return err
	}
	res, err := (&creditagent.Agent{LLM: env.LLM, API: env.Agent}).Score(ctx, t["company"], t["customer"])
	if err != nil {
		return fmt.Errorf("credit_scoring: %w", err)
	}
	graded(env, res)
	return nil
}
