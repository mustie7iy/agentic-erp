// Package testagent is the test agent: it does the work of an SAP test
// consultant. It captures regression suites from real transactions, runs
// them against proposed configuration changes, explains every changed
// result in business terms, and writes user acceptance test scripts.
package testagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/llm"
)

type Agent struct {
	LLM      *llm.Runner
	Repo     *config.Repo
	Store    *core.Store
	SuiteDir string
	UATDir   string
	Now      func() time.Time
}

const prompt = `You are the test agent of an ERP platform that replaces SAP. You do the work of an SAP test consultant.

How to work:
1. For a change request, run the regression suite against it (run_suite). If no suite exists, capture one first (capture_suite).
2. For every case whose result changes, explain in business terms what changes and whether it follows from the requested change or looks unintended.
3. Write a user acceptance test script for key users (save_uat_script): numbered steps, test data from the suite (invoice ids, amounts), and the expected result of each step.
4. Finish with a recommendation to the approvers: safe to approve, approve with the noted changes, or do not approve.

Never change data or configuration yourself.`

type crArg struct {
	ChangeRequest string `json:"change_request" jsonschema:"required,description=Change request id such as CR-0003"`
}

type captureArg struct {
	MaxCases int `json:"max_cases,omitempty" jsonschema:"description=Upper limit on cases (default 50)"`
}

type uatArg struct {
	ChangeRequest string `json:"change_request" jsonschema:"required"`
	Script        string `json:"script" jsonschema:"required,description=The UAT script in Markdown"`
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Capture builds and saves a suite from the live store under the active configuration.
func (a *Agent) Capture(max int) (*regress.Suite, string, error) {
	cfg, version, err := a.Repo.Active()
	if err != nil {
		return nil, "", err
	}
	if max <= 0 {
		max = 50
	}
	s, err := regress.Capture(a.Store, cfg, version, max, a.now())
	if err != nil {
		return nil, "", err
	}
	path, err := s.Save(a.SuiteDir)
	return s, path, err
}

// RunFor runs the latest suite against a change request's proposed configuration.
func (a *Agent) RunFor(crID string) (*regress.Result, error) {
	cr, err := a.Repo.Get(crID)
	if err != nil {
		return nil, err
	}
	proposed, err := config.Parse([]byte(cr.Proposed))
	if err != nil {
		return nil, err
	}
	suite, err := regress.Latest(a.SuiteDir)
	if err != nil {
		return nil, err
	}
	return suite.Run(proposed)
}

// SaveUAT writes a UAT script for a change request.
func (a *Agent) SaveUAT(crID, script string) (string, error) {
	if _, err := a.Repo.Get(crID); err != nil {
		return "", err
	}
	if strings.TrimSpace(script) == "" {
		return "", fmt.Errorf("the script is empty")
	}
	if err := os.MkdirAll(a.UATDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(a.UATDir, crID+"-uat.md")
	return path, os.WriteFile(path, []byte(script), 0o644)
}

func (a *Agent) tools(saved *[]string) []anthropic.BetaTool {
	capture := llm.Must(toolrunner.NewBetaToolFromJSONSchema("capture_suite",
		"Capture a regression suite from current transactions under the active configuration.",
		func(_ context.Context, in captureArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			s, path, err := a.Capture(in.MaxCases)
			if err != nil {
				return llm.Error(err)
			}
			return llm.Result(map[string]any{"suite": s.ID, "path": path, "cases": len(s.Cases), "strata": s.Strata()})
		}))
	run := llm.Must(toolrunner.NewBetaToolFromJSONSchema("run_suite",
		"Run the latest regression suite against a change request. Lists every case whose outcome changes.",
		func(_ context.Context, in crArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			r, err := a.RunFor(in.ChangeRequest)
			if err != nil {
				return llm.Error(err)
			}
			return llm.Result(r)
		}))
	uat := llm.Must(toolrunner.NewBetaToolFromJSONSchema("save_uat_script", "Save the UAT script for a change request.",
		func(_ context.Context, in uatArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			path, err := a.SaveUAT(in.ChangeRequest, in.Script)
			if err != nil {
				return llm.Error(err)
			}
			*saved = append(*saved, path)
			return llm.Result("Saved to " + path)
		}))
	return []anthropic.BetaTool{capture, run, uat}
}

// Review is the agent's assessment of a change request.
type Review struct {
	Recommendation string
	UATScripts     []string
}

func (a *Agent) Review(ctx context.Context, crID string) (*Review, error) {
	if _, err := a.Repo.Get(crID); err != nil {
		return nil, err
	}
	var saved []string
	tools := a.tools(&saved)
	q := fmt.Sprintf("Test change request %s and write the UAT script.", crID)
	msg, err := a.LLM.RunTools(ctx, tools, a.LLM.Params(prompt, anthropic.NewBetaTextBlock(q)), 12)
	if err != nil {
		return nil, err
	}
	return &Review{Recommendation: llm.Text(msg), UATScripts: saved}, nil
}
