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

// enablementEval has the enablement agent describe the task's process as
// erp agent enablement-docs does; what it recommended is the checks' to
// judge, and its reply theirs too.
func enablementEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "process")
	if err != nil {
		return err
	}
	res, err := (&enablementagent.Agent{LLM: env.LLM, API: env.Agent}).Document(ctx, t["process"])
	if err != nil {
		return fmt.Errorf("enablement: %w", err)
	}
	graded(env, res)
	return nil
}
