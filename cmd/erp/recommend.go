package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/accessagent"
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

// agentAccessRoles has the access agent review a role's permissions
// against what its holders did in a period (ADR-0072, T106a): an identity
// administrator reads its recommendation and decides it, and ends the
// grants, or not.
func agentAccessRoles(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent access-roles", flag.ContinueOnError)
	role := fs.String("role", "", "role code or id")
	period := fs.String("period", "", "the period, FROM..TO (YYYY-MM-DD..YYYY-MM-DD, UTC days)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	from, to, ok := strings.Cut(*period, "..")
	if *role == "" || !ok {
		return errors.New("agent access-roles: want -role CODE -period FROM..TO")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&accessagent.Agent{LLM: newRunner(c), API: c}).Review(ctx, *role, accessagent.Period{From: from, To: to})
	return recommended("agent access-roles", res, runErr)
}

// agentAccessSod has the access agent analyse a role against the
// tenant's segregation-of-duties rules, as it stands or with the
// permissions -add names (ADR-0072, T106b): an identity administrator
// reads its recommendation and decides it, and grants the permissions,
// splits the role, or not.
func agentAccessSod(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent access-sod", flag.ContinueOnError)
	role := fs.String("role", "", "role code or id")
	add := fs.String("add", "", "permissions to be added, comma-separated (none: the role as it stands)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *role == "" {
		return errors.New("agent access-sod: want -role CODE [-add PERMISSION,...]")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&accessagent.Agent{LLM: newRunner(c), API: c}).Analyse(ctx, *role, strings.Split(*add, ","))
	return recommended("agent access-sod", res, runErr)
}

// accessEval has the access agent run on the task as its command does:
// with the task's run sod, the role's segregation-of-duties analysis
// (erp agent access-sod, the task's add a comma-separated list), and
// otherwise the review of its role over its period (erp agent
// access-roles); what it recommended is the checks' to judge, and its
// reply theirs too.
func accessEval(ctx context.Context, env eval.Env) error {
	a := &accessagent.Agent{LLM: env.LLM, API: env.Agent}
	var (
		res recommend.Result
		err error
	)
	if run, _ := env.Task["run"].(string); run == "sod" { // a task without a run is the review of a role's use
		t, terr := tasked(env, "role")
		if terr != nil {
			return terr
		}
		add, _ := env.Task["add"].(string) // no add: the role as it stands
		res, err = a.Analyse(ctx, t["role"], strings.Split(add, ","))
	} else {
		t, terr := tasked(env, "role", "from", "to")
		if terr != nil {
			return terr
		}
		res, err = a.Review(ctx, t["role"], accessagent.Period{From: t["from"], To: t["to"]})
	}
	if err != nil {
		return fmt.Errorf("access: %w", err)
	}
	graded(env, res)
	return nil
}
