package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/controllingagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// agentRepost has the controlling agent propose the repostings of a
// company's fiscal period (ADR-0050 §12, T56j1): each a parked entry
// moving a line from the center it was posted on to the one it belongs
// on, which a person posts or cancels.
func agentRepost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent repost", flag.ContinueOnError)
	company := fs.String("company", "", "company code")
	year := fs.Int("year", 0, "fiscal year")
	period := fs.Int("period", 0, "fiscal period (1-16)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *company == "" {
		return errors.New("agent repost: want -company CODE -year YYYY -period P")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&controllingagent.Agent{LLM: newRunner(c), API: c}).Repostings(ctx, controllingagent.Period{Company: *company, Year: *year, Period: *period})
	if res.Session == "" {
		return runErr
	}
	// What the run parked is printed even when it failed afterwards: its
	// entries wait for a person either way.
	w := printer()
	fmt.Fprintf(w, "Session %s: repostings parked %d, refused %d.\n", res.Session, len(res.Proposals), res.Refused)
	for _, p := range res.Proposals {
		fmt.Fprintf(w, "  parked entry %s: %s, journal entry %s line %d\n", p.ParkedEntryID, p.Reference, p.JournalEntryID, p.LineNo)
	}
	if res.Reply != "" {
		fmt.Fprintln(w, res.Reply)
	}
	return errors.Join(runErr, errs.Wrapf(w.Flush(), "agent repost: print"))
}

// controllingEval has the controlling agent propose the repostings of the
// task's company and period as erp agent repost does; what it parked is
// the checks' to judge, and its reply theirs too ($reply.text).
func controllingEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "company", "year", "period")
	if err != nil {
		return err
	}
	year, yerr := strconv.Atoi(t["year"])
	period, perr := strconv.Atoi(t["period"])
	if err := errors.Join(yerr, perr); err != nil {
		return fmt.Errorf("controlling: the task's year and period: %w", err)
	}
	res, err := (&controllingagent.Agent{LLM: env.LLM, API: env.Agent}).Repostings(ctx, controllingagent.Period{Company: t["company"], Year: year, Period: period})
	if err != nil {
		return fmt.Errorf("controlling: %w", err)
	}
	env.Reply(map[string]any{"text": res.Reply, "parked": len(res.Proposals), "refused": res.Refused})
	return nil
}
