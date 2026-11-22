package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/replenishmentagent"
)

// agentReplenish has the replenishment agent review a planning run's
// proposals (ADR-0072, plan-supply-chain.md §S2l): a buyer reads its
// recommendation on each proposed requisition and decides it, and orders,
// or not.
func agentReplenish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent replenish", flag.ContinueOnError)
	run := fs.String("run", "", "the planning run's id (GET /v1/mrp-runs)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if uuid.Validate(*run) != nil {
		return errors.New("agent replenish: want -run ID, a planning run's id")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&replenishmentagent.Agent{LLM: newRunner(c), API: c}).Review(ctx, *run)
	return recommended("agent replenish", res, runErr)
}

// replenishmentEval has the replenishment agent review the task's
// planning run as erp agent replenish does, once the cell has carried the
// run out, as a buyer waits for it; what it recommended is the checks' to
// judge, and its reply theirs too.
func replenishmentEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "run")
	if err != nil {
		return err
	}
	if err := carriedOut(ctx, env.Agent, t["run"]); err != nil {
		return fmt.Errorf("replenishment: %w", err)
	}
	res, err := (&replenishmentagent.Agent{LLM: env.LLM, API: env.Agent}).Review(ctx, t["run"])
	if err != nil {
		return fmt.Errorf("replenishment: %w", err)
	}
	graded(env, res)
	return nil
}

// runWait bounds how long an evaluation waits for its case's planning run
// to be carried out, and runPoll how often it reads it meanwhile.
var runWait, runPoll = time.Minute, 50 * time.Millisecond

// carriedOut waits until the planning run is no longer queued: a case's
// setup starts it, and the cell carries it out in the background
// (ADR-0084 §3).
func carriedOut(ctx context.Context, c *apitools.Client, run string) error {
	deadline := time.NewTimer(runWait)
	defer deadline.Stop()
	tick := time.NewTicker(runPoll)
	defer tick.Stop()
	for {
		resp, err := c.Do(ctx, http.MethodGet, "/v1/mrp-runs/"+url.PathEscape(run), nil, nil, "")
		var r struct{ Status string }
		if err == nil {
			err = resp.Decode(&r)
		}
		switch {
		case err != nil:
			return fmt.Errorf("planning run %s: %w", run, err)
		case r.Status != "queued":
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("planning run %s still queued: %w", run, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("planning run %s still queued after %s", run, runWait)
		case <-tick.C:
		}
	}
}
