package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/accessagent"
	"github.com/mustie7iy/agentic-erp/internal/bidagent"
	"github.com/mustie7iy/agentic-erp/internal/creditagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
	"github.com/mustie7iy/agentic-erp/internal/regulatoryagent"
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

// accessEval has the access agent review the task's role over its period
// as erp agent access-roles does; what it recommended is the checks' to
// judge, and its reply theirs too.
func accessEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "role", "from", "to")
	if err != nil {
		return err
	}
	res, err := (&accessagent.Agent{LLM: env.LLM, API: env.Agent}).Review(ctx, t["role"], accessagent.Period{From: t["from"], To: t["to"]})
	if err != nil {
		return fmt.Errorf("access: %w", err)
	}
	graded(env, res)
	return nil
}

// agentBidCompare has the bid comparison agent compare an RFQ's
// quotations (ADR-0072, T105c): the buyer reads its recommendation and
// decides it, and awards the RFQ, or not.
func agentBidCompare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent bid-compare", flag.ContinueOnError)
	rfq := fs.String("rfq", "", "the RFQ's id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *rfq == "" {
		return errors.New("agent bid-compare: want -rfq ID")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&bidagent.Agent{LLM: newRunner(c), API: c}).Compare(ctx, *rfq)
	return recommended("agent bid-compare", res, runErr)
}

// bidComparisonEval has the bid comparison agent compare the task's RFQ
// as erp agent bid-compare does; what it recommended is the checks' to
// judge, and its reply theirs too.
func bidComparisonEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "rfq")
	if err != nil {
		return err
	}
	res, err := (&bidagent.Agent{LLM: env.LLM, API: env.Agent}).Compare(ctx, t["rfq"])
	if err != nil {
		return fmt.Errorf("bid_comparison: %w", err)
	}
	graded(env, res)
	return nil
}

// agentRegulatoryNotice has the regulatory agent read a notice of a
// regulatory change, from a file the tax accountant names, against the
// tenant's configuration (ADR-0072, T107a): the tax accountant reads its
// recommendation and decides it. The notice is read from the file only:
// the agent fetches nothing.
func agentRegulatoryNotice(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent regulatory-notice", flag.ContinueOnError)
	file := fs.String("file", "", "the notice, a text file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("agent regulatory-notice: want -file F")
	}
	notice, err := readNotice(*file)
	if err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&regulatoryagent.Agent{LLM: newRunner(c), API: c}).Read(ctx, notice)
	return recommended("agent regulatory-notice", res, runErr)
}

// readNotice reads a notice's file, at most one byte beyond the longest
// notice, which the agent refuses: a larger file is not read whole.
func readNotice(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the file the operator names
	if err != nil {
		return "", fmt.Errorf("agent regulatory-notice: %w", err)
	}
	defer func() { _ = f.Close() }() // read only: nothing to flush
	b, err := io.ReadAll(io.LimitReader(f, regulatoryagent.MaxNotice+1))
	return string(b), errs.Wrapf(err, "agent regulatory-notice: read %s", path)
}

// regulatoryEval has the regulatory agent read the task's notice as erp
// agent regulatory-notice does; what it recommended is the checks' to
// judge, and its reply theirs too.
func regulatoryEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "notice")
	if err != nil {
		return err
	}
	res, err := (&regulatoryagent.Agent{LLM: env.LLM, API: env.Agent}).Read(ctx, t["notice"])
	if err != nil {
		return fmt.Errorf("regulatory: %w", err)
	}
	graded(env, res)
	return nil
}
