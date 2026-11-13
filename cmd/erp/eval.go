package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/cashagent"
	"github.com/mustie7iy/agentic-erp/internal/closeagent"
	"github.com/mustie7iy/agentic-erp/internal/creditagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/configagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/migrationagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/testagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/reconagent"
	"github.com/mustie7iy/agentic-erp/internal/taxagent"
)

// evalAgents are the adapters of the agents a suite may evaluate, by agent
// type (ADR-0039).
var evalAgents = map[string]eval.Agent{"analytics": analyticsEval, "ap_exceptions": apExceptions, "capture": captureEval,
	"close": closeEval, "configuration": configurationEval, "migration": migrationEval, "reconciliation": reconciliationEval, "tax": taxEval, "test": testEval,
	cashagent.Type: cashForecastEval, creditagent.Type: creditScoringEval}

// analyticsEval asks the task's question as erp ask does, its session
// reading what the API gives the agent principal and no more. Its reply
// is the answer graded against the semantic layer's own results for the
// task's reference queries (analytics.Grade), the answer's narrative, and
// the companies whose data the session read.
func analyticsEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "question")
	if err != nil {
		return err
	}
	var refs []analytics.Query
	if err := retype(env.Task["reference"], &refs); err != nil {
		return fmt.Errorf("analytics: the task's reference: %w", err)
	}
	if len(refs) == 0 {
		// Nothing to grade against would pass any figure.
		return errors.New("analytics: the task names no reference queries")
	}
	m, _ := analytics.DefaultModel() // embedded, and TestModelLoadsAndValidates proves it parses
	facts, err := analytics.Source{API: env.Agent}.Facts(ctx, m, time.Now())
	if err != nil {
		return fmt.Errorf("analytics: read the facts: %w", err)
	}
	reference := make([]*analytics.Result, 0, len(refs))
	for _, q := range refs {
		r, err := analytics.Execute(m, facts, analytics.Principal{ID: "eval"}, q)
		if err != nil {
			return fmt.Errorf("analytics: the reference query %v: %w", q.Measures, err)
		}
		reference = append(reference, r)
	}
	s := analytics.NewSession(m, facts, analytics.Principal{ID: "eval"})
	ans, err := analytics.NewAgent(env.LLM.Client, env.LLM.Model).Ask(ctx, s, t["question"])
	if err != nil {
		return fmt.Errorf("analytics: %w", err)
	}
	env.Reply(map[string]any{"grade": ans.Grade(s.Results, reference), "narrative": ans.Narrative(),
		"companies": slices.Sorted(maps.Keys(facts.Companies))})
	return nil
}

// retype reads a task's YAML value as v's type, through JSON.
func retype(in, v any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("eval: encode: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("eval: decode: %w", err)
	}
	return nil
}

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

// reconciliationEval has the agent propose for the task's statement as erp
// agent reconcile does; a line it could not review fails the case, a line
// it left to people does not (the checks judge that).
func reconciliationEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "statement")
	if err != nil {
		return err
	}
	res, err := (&reconagent.Agent{LLM: env.LLM, API: env.Agent}).Review(ctx, t["statement"])
	if err != nil {
		return fmt.Errorf("reconciliation: %w", err)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("reconciliation: %s", strings.Join(res.Errors, "; "))
	}
	return nil
}

// cashForecastEval has the forecast agent explain the task's variance as
// erp agent cash-variance does; what it proposed, and what it left, is the
// checks' to judge. A proposal its own guard refused fails the case: the
// suite's replay never asks for one.
func cashForecastEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "company", "from", "to", "lag")
	if err != nil {
		return err
	}
	lag, err := strconv.Atoi(t["lag"])
	if err != nil {
		return fmt.Errorf("cash_forecast: lag: %w", err)
	}
	res, err := (&cashagent.Agent{LLM: env.LLM, API: env.Agent}).Explain(ctx, cashagent.Period{Company: t["company"], From: t["from"], To: t["to"], Lag: lag})
	if err != nil {
		return fmt.Errorf("cash_forecast: %w", err)
	}
	env.Reply(map[string]any{"comments": len(res.Comments), "memo_records": len(res.Memos), "refused": res.Refused, "reply": res.Reply,
		"says_not_explained": strings.Contains(strings.ToLower(res.Reply), "not explained")})
	return nil
}

// closeEval has the close agent run on the task's close as erp agent
// close-accruals does or, when the task's run is variances, as erp agent
// close-variances does (against the task's basis, or every one the company
// configures); what it proposed, and what it left, is the checks' to
// judge, and its reply theirs too ($reply.text).
func closeEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "close")
	if err != nil {
		return err
	}
	a := &closeagent.Agent{LLM: env.LLM, API: env.Agent}
	var res closeagent.Result
	if run, _ := env.Task["run"].(string); run == "variances" { // a task without a run is the accruals'
		basis, _ := env.Task["basis"].(string) // no basis: every one the company configures
		res, err = a.Variances(ctx, t["close"], basis)
	} else {
		res, err = a.Accruals(ctx, t["close"])
	}
	if err != nil {
		return fmt.Errorf("close: %w", err)
	}
	env.Reply(map[string]any{"text": res.Reply, "proposals": len(res.Proposals), "refused": res.Refused})
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

// taxEval has the tax agent review the task's registration's draft return
// of its period as erp agent tax-draft does; what it recommended and
// parked is the checks' to judge, and its reply theirs too ($reply.text).
func taxEval(ctx context.Context, env eval.Env) error {
	t, err := tasked(env, "registration", "period")
	if err != nil {
		return err
	}
	res, err := (&taxagent.Agent{LLM: env.LLM, API: env.Agent}).Explain(ctx, t["registration"], t["period"])
	if err != nil {
		return fmt.Errorf("tax: %w", err)
	}
	env.Reply(map[string]any{"text": res.Reply, "recommendations": len(res.Recommendations), "parked": len(res.Parked), "refused": res.Refused})
	return nil
}
