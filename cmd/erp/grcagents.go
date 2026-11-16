package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/controlagent"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/riskagent"
)

// The recommending agents of controls and risks (ADR-0072, ADR-0078),
// printed and graded as every recommending agent is (recommend.go).

// agentControlMonitor has the control monitoring agent monitor the
// tenant's active controls, or those of a process (ADR-0072, T105f): the
// compliance officer reads its recommendation on each control and decides
// it, and tests the control, or not.
func agentControlMonitor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent control-monitor", flag.ContinueOnError)
	process := fs.String("process", "", "only the controls of this process, by its exact text")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&controlagent.Agent{LLM: newRunner(c), API: c}).Monitor(ctx, *process)
	return recommended("agent control-monitor", res, runErr)
}

// controlMonitoringEval has the control monitoring agent monitor the
// case's controls, those of the task's process when it names one, as erp
// agent control-monitor does; what it recommended is the checks' to
// judge, and its reply theirs too.
func controlMonitoringEval(ctx context.Context, env eval.Env) error {
	process, _ := env.Task["process"].(string) // a task without one monitors every control
	res, err := (&controlagent.Agent{LLM: env.LLM, API: env.Agent}).Monitor(ctx, process)
	if err != nil {
		return fmt.Errorf("control_monitoring: %w", err)
	}
	graded(env, res)
	return nil
}

// agentRisk has the risk agent review the tenant's open risks (ADR-0072,
// T105g): the risk owner reads its recommendation on each risk, and on the
// tenant a risk not yet in the register, and decides them, and rates or
// records the risk, or not.
func agentRisk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent risk", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&riskagent.Agent{LLM: newRunner(c), API: c}).Review(ctx)
	return recommended("agent risk", res, runErr)
}

// riskEval has the risk agent review the case's register as erp agent
// risk does; what it recommended is the checks' to judge, and its reply
// theirs too.
func riskEval(ctx context.Context, env eval.Env) error {
	res, err := (&riskagent.Agent{LLM: env.LLM, API: env.Agent}).Review(ctx)
	if err != nil {
		return fmt.Errorf("risk: %w", err)
	}
	graded(env, res)
	return nil
}
