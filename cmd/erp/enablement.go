package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/mustie7iy/agentic-erp/internal/enablementagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// agentEnablementDocs has the enablement agent describe a process from the
// active configuration and roles (ADR-0072 As built, T109a): it prints
// what the agent recommended, as every recommending agent's command does,
// and writes the description to the file -out (default PROCESS.md), so a
// process owner reads it as a document; the recommendation waits for the
// owner's decision either way.
func agentEnablementDocs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent enablement-docs", flag.ContinueOnError)
	process := fs.String("process", "", "the process: purchase_to_pay or order_to_cash")
	file := fs.String("out", "", "the file the description is written to (default PROCESS.md)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *process == "" {
		return errors.New("agent enablement-docs: want -process purchase_to_pay|order_to_cash [-out F]")
	}
	if *file == "" {
		*file = *process + ".md"
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&enablementagent.Agent{LLM: newRunner(c), API: c}).Document(ctx, *process)
	err = recommended("agent enablement-docs", res, runErr)
	if len(res.Recommendations) == 0 {
		return err
	}
	// The description is the recommendation's content as the API recorded it.
	if werr := os.WriteFile(*file, []byte(res.Recommendations[0].Content+"\n"), 0o600); werr != nil {
		return errors.Join(err, fmt.Errorf("agent enablement-docs: write %s: %w", *file, werr))
	}
	w := printer()
	fmt.Fprintf(w, "Description written to %s.\n", *file)
	return errors.Join(err, errs.Wrapf(w.Flush(), "agent enablement-docs: print"))
}

// agentEnablementHelp has the enablement agent explain a field or a
// problem code of an operation (ADR-0072 As built, T109b): one not in the
// contract is refused before anything is read; it prints the session, how
// many replies its check refused and the reply it admitted, also when the
// run failed after starting its session.
func agentEnablementHelp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent enablement-help", flag.ContinueOnError)
	var q enablementagent.Question
	fs.StringVar(&q.Operation, "operation", "", "the operation's id")
	fs.StringVar(&q.Field, "field", "", "a field of the operation (a parameter, or a body's property, dotted)")
	fs.StringVar(&q.Problem, "problem", "", "a problem code of the operation")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if q.Operation == "" || (q.Field == "") == (q.Problem == "") {
		return errors.New("agent enablement-help: want -operation ID and -field NAME or -problem CODE")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&enablementagent.Agent{LLM: newRunner(c), API: c}).Help(ctx, q)
	if res.Session == "" {
		return runErr
	}
	w := printer()
	fmt.Fprintf(w, "Session %s: replies refused %d.\n", res.Session, res.Refused)
	if res.Reply != "" {
		fmt.Fprintln(w, res.Reply)
	}
	return errors.Join(runErr, errs.Wrapf(w.Flush(), "agent enablement-help: print"))
}

// enablementEval runs the enablement agent on the task as its commands
// do: a process described (task process) is graded as a recommending
// agent's run; help (task operation, with field or problem) as a reply
// ($reply.text) and how many replies were refused (.refused).
func enablementEval(ctx context.Context, env eval.Env) error {
	a := &enablementagent.Agent{LLM: env.LLM, API: env.Agent}
	if p, ok := env.Task["process"]; ok {
		res, err := a.Document(ctx, fmt.Sprint(p))
		if err != nil {
			return fmt.Errorf("enablement: %w", err)
		}
		graded(env, res)
		return nil
	}
	t, err := tasked(env, "operation")
	if err != nil {
		return err
	}
	q := enablementagent.Question{Operation: t["operation"]}
	if f, ok := env.Task["field"]; ok {
		q.Field = fmt.Sprint(f)
	}
	if p, ok := env.Task["problem"]; ok {
		q.Problem = fmt.Sprint(p)
	}
	res, err := a.Help(ctx, q)
	if err != nil {
		return fmt.Errorf("enablement: %w", err)
	}
	env.Reply(map[string]any{"text": res.Reply, "refused": res.Refused})
	return nil
}
