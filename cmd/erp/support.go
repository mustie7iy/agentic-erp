package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
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

// agentSupportDiagnose has the support agent find the root cause of a
// stuck document or a refused request about it (ADR-0072 As built, T108b):
// it prints what it recommended on the document and the model's reply,
// also when the run failed after starting its session; the user decides
// and acts through the normal operation.
func agentSupportDiagnose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent support-diagnose", flag.ContinueOnError)
	kind := fs.String("type", "", "the document's kind: "+strings.Join(slices.Sorted(maps.Keys(supportagent.Documents)), ", "))
	id := fs.String("id", "", "the document's id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *kind == "" || *id == "" {
		return errors.New(`agent support-diagnose: want -type KIND -id ID [WHAT HAPPENED], e.g. -type supplier_invoice -id ID "posting it was refused"`)
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&supportagent.Agent{LLM: newRunner(c), API: c}).Diagnose(ctx, *kind, *id, strings.TrimSpace(strings.Join(fs.Args(), " ")))
	return recommended("agent support-diagnose", res, runErr)
}

// agentSupportFix has the support agent draft the configuration change
// request that fixes a root cause of the class configuration a user
// accepted, and have it simulated (ADR-0072 As built, T108c): it prints
// what it recommended on the draft and the model's reply, also when the
// run failed after starting its session; a person submits the draft.
func agentSupportFix(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent support-fix", flag.ContinueOnError)
	rec := fs.String("recommendation", "", "the accepted root cause's recommendation id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *rec == "" {
		return errors.New("agent support-fix: want -recommendation ID")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&supportagent.Agent{LLM: newRunner(c), API: c}).Fix(ctx, *rec)
	return recommended("agent support-fix", res, runErr)
}

// supportEval has the support agent answer the task's question as erp
// agent support-ask does, or, with the task's run diagnose, find the root
// cause of its document as erp agent support-diagnose does; its reply
// ($reply.text), how many replies or recommendations were refused
// (.refused) and, for a diagnosis, how many it made are the checks' to
// judge.
func supportEval(ctx context.Context, env eval.Env) error {
	if run, _ := env.Task["run"].(string); run == "diagnose" { // a task without a run asks a question
		return supportDiagnoseEval(ctx, env)
	}
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

// supportDiagnoseEval has the support agent diagnose the task's document
// (type, id and what happened, which may be empty).
func supportDiagnoseEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "type", "id")
	if err != nil {
		return err
	}
	said, _ := env.Task["report"].(string) // none: nothing said
	res, err := (&supportagent.Agent{LLM: env.LLM, API: env.Agent}).Diagnose(ctx, t["type"], t["id"], said)
	if err != nil {
		return fmt.Errorf("support: %w", err)
	}
	graded(env, res)
	return nil
}
