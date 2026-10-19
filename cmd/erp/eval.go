package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/delivery/configagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/migrationagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/testagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// evalAgents are the adapters of the agents a suite may evaluate, by agent
// type (ADR-0039).
var evalAgents = map[string]eval.Agent{"ap_exceptions": apExceptions, "capture": captureEval,
	"configuration": configurationEval, "migration": migrationEval, "test": testEval}

// configurationEval handles the task's request as erp agent config does,
// starting from the task's draft file when it names one; what it proposed
// is the checks' to judge.
func configurationEval(ctx context.Context, env eval.Env) error {
	var draft []byte
	if name, ok := env.Task["draft"]; ok {
		var err error
		if draft, err = os.ReadFile(filepath.Join(env.Dir, fmt.Sprint(name))); err != nil { // #nosec G304 -- a draft the suite names
			return fmt.Errorf("configuration: read the draft: %w", err)
		}
	}
	t, err := tasked(env, "request")
	if err != nil {
		return err
	}
	if _, err := (&configagent.Agent{LLM: env.LLM, API: env.Agent}).Handle(ctx, t["request"], draft); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	return nil
}

// migrationEval rehearses the task's scope as erp agent migration does.
func migrationEval(ctx context.Context, env eval.Env) error {
	var entities []string
	list, _ := env.Task["legal_entities"].([]any) // a scope without entities is the API's to refuse
	for _, e := range list {
		entities = append(entities, fmt.Sprint(e))
	}
	t, err := tasked(env, "connector", "fiscal_year", "mode")
	if err != nil {
		return err
	}
	year, err := strconv.Atoi(t["fiscal_year"])
	if err != nil {
		return fmt.Errorf("migration: fiscal_year: %w", err)
	}
	if _, err := (&migrationagent.Agent{LLM: env.LLM, API: env.Agent}).Assess(ctx, t["connector"], entities, year, t["mode"]); err != nil {
		return fmt.Errorf("migration: %w", err)
	}
	return nil
}

// testEval reviews the task's change request as erp agent test does.
func testEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "change_request")
	if err != nil {
		return err
	}
	if _, err := (&testagent.Agent{LLM: env.LLM, API: env.Agent}).Review(ctx, t["change_request"]); err != nil {
		return fmt.Errorf("test: %w", err)
	}
	return nil
}

// tasked is the task's values for keys, as text; a key the task lacks is a
// suite's mistake, which fails the case rather than run the agent on
// "<nil>".
func tasked(env eval.Env, keys ...string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		v, ok := env.Task[k]
		if !ok {
			return nil, fmt.Errorf("eval: the task has no %s", k)
		}
		out[k] = fmt.Sprint(v)
	}
	return out, nil
}

// captureEval captures the task's document for its legal entity as erp
// capture does; the server's extraction runs through the cell's gateway on
// erp-extraction. Only the refusal the task names (refused: a problem code)
// is the case's to judge: any other, an unreadable document, an exhausted
// budget or a broken cell, fails the case rather than pass a check that
// nothing was entered.
func captureEval(ctx context.Context, env eval.Env) error {
	doc, err := os.ReadFile(filepath.Join(env.Dir, fmt.Sprint(env.Task["document"]))) // #nosec G304 -- a document the suite names
	if err != nil {
		return fmt.Errorf("capture: read the document: %w", err)
	}
	_, err = capture(ctx, env.Agent, map[string]string{"legal_entity_id": fmt.Sprint(env.Task["legal_entity_id"])}, doc)
	var refused *apitools.Error
	if want, ok := env.Task["refused"]; ok && errors.As(err, &refused) && refused.Code == fmt.Sprint(want) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	return nil
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
