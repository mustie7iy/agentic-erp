package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/apitools"

	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// evalAgents are the adapters of the agents a suite may evaluate, by agent
// type (ADR-0039).
var evalAgents = map[string]eval.Agent{"ap_exceptions": apExceptions, "capture": captureEval}

// captureEval captures the task's document for its legal entity as erp
// capture does; the server's extraction runs through the cell's gateway on
// erp-extraction. A capture the API refuses is the case's to judge.
func captureEval(ctx context.Context, env eval.Env) error {
	doc, err := os.ReadFile(filepath.Join(env.Dir, fmt.Sprint(env.Task["document"]))) // #nosec G304 -- a document the suite names
	if err != nil {
		return fmt.Errorf("capture: read the document: %w", err)
	}
	_, err = capture(ctx, env.Agent, map[string]string{"legal_entity_id": fmt.Sprint(env.Task["legal_entity_id"])}, doc)
	var refused *apitools.Error
	if errors.As(err, &refused) {
		return nil
	}
	return err
}

// apExceptions reviews the held invoices as erp agent invoices does; an
// invoice it could not review fails the case.
func apExceptions(ctx context.Context, env eval.Env) error {
	res, err := (&agent.Agent{LLM: env.LLM, API: env.Agent}).Review(ctx)
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("review: %s", strings.Join(res.Errors, "; "))
	}
	return nil
}

// evalModels gives a live run its model; tests replace it with the
// scripted model, so no test spends.
var evalModels = func(alias string) eval.Models { return eval.Gateway{Alias: alias} }

// evalMint mints the throwaway tenants' tokens: the local cell's
// development issuer; tests mint from theirs.
var evalMint = func(admin string) eval.Mint { return eval.DevIssuer(admin, nil) }

// erp eval run runs a suite live against a local cell (ADR-0039): it spends
// real model money, so it is run on demand, never in CI.
//
//	erp eval run -suite evals/<agent>/suite.yaml [-repeats 3] [-max-cost 5.00] [-out evals/results]
func evalCmd(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] != "run" {
		return errors.New("eval: want eval run -suite FILE")
	}
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	suitePath := fs.String("suite", "", "the suite file, evals/<agent>/suite.yaml")
	repeats := fs.Int("repeats", eval.Repeats, "how often each case runs")
	maxCost := fs.String("max-cost", "5.00", "stop past this much model spend, in USD")
	out := fs.String("out", "evals/results", "where the report is written, under <agent>/<model>/")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	s, err := eval.Load(*suitePath)
	if err != nil {
		return err
	}
	limit, err := money.ParseDecimal(*maxCost, 2)
	if err != nil || limit <= 0 {
		return fmt.Errorf("eval: -max-cost %q: a positive amount in USD", *maxCost)
	}
	if _, ok := evalAgents[s.Agent]; !ok {
		return fmt.Errorf("eval: no adapter for agent type %s", s.Agent)
	}
	admin := orDefault(getenv("ERP_ADMIN_URL"), "http://localhost:9090")
	r := &eval.Runner{Agents: evalAgents, Models: evalModels(s.Alias), Repeats: *repeats, MaxCost: money.MicroOf(limit),
		Tenants: &eval.ControlPlane{ControlURL: orDefault(getenv("ERP_CONTROL_URL"), "http://localhost:9090"),
			APIURL: orDefault(getenv("ERP_API_URL"), "http://localhost:8080"), Mint: evalMint(admin)}}
	rep, runErr := r.Run(ctx, s)
	path, err := rep.Write(*out)
	if err != nil {
		return errors.Join(runErr, err)
	}
	if err := printf("eval", "%s: %d of %d cases passed, %d safety failures, level %d, cost %d µ¢; report %s\n",
		s.Agent, rep.Passed, len(rep.Cases), rep.SafetyFailures, rep.Level, rep.CostMicro, path); err != nil {
		return err
	}
	return runErr
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
