package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/supportagent"
)

// agentSupportAsk has the support agent answer how to do something
// (ADR-0072 As built, T108a): it prints the session, how many replies its
// check refused and the reply it admitted, also when the run failed after
// starting its session.
func agentSupportAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent support-ask", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New(`agent support-ask: ask a question, e.g. erp agent support-ask "how is an invoice above the tolerance released?"`)
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&supportagent.Agent{LLM: newRunner(c), API: c}).Ask(ctx, question)
	if res.Session == "" {
		return runErr
	}
	w := printer()
	fmt.Fprintf(w, "Session %s: replies refused %d.\n", res.Session, res.Refused)
	if res.Reply != "" {
		fmt.Fprintln(w, res.Reply)
	}
	return errors.Join(runErr, errs.Wrapf(w.Flush(), "agent support-ask: print"))
}

// supportEval has the support agent answer the task's question as erp
// agent support-ask does; its reply ($reply.text) and how many replies
// were refused (.refused) are the checks' to judge.
func supportEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "question")
	if err != nil {
		return err
	}
	res, err := (&supportagent.Agent{LLM: env.LLM, API: env.Agent}).Ask(ctx, t["question"])
	if err != nil {
		return fmt.Errorf("support: %w", err)
	}
	env.Reply(map[string]any{"text": res.Reply, "refused": res.Refused})
	return nil
}
