// Package configagent is the configuration agent: it does the customizing
// work of an SAP functional consultant. It reads the active configuration
// and the client's SAP customizing, drafts changes, validates them, and
// files change requests with simulation and regression evidence. It has no
// tool that approves or activates anything.
package configagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/changes"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/delivery/sapconfig"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Identity is the agent's configured identity; it needs config.propose.
const Identity = "agent:configuration"

type Agent struct {
	LLM     *llm.Runner
	Repo    *config.Repo
	Store   *core.Store
	Suite   func() (*regress.Suite, error)
	Extract *sap.Extract // the client's SAP customizing, if available
	Now     func() time.Time
	// ReplayDays is the window of recent invoices replayed in simulation.
	ReplayDays int
}

const prompt = `You are the configuration agent of an ERP platform that replaces SAP. You do the customizing work an SAP functional consultant does, as configuration as code.

How to work:
1. Read the active configuration (get_active_config). If the request concerns SAP setup, read get_sap_customizing.
2. Draft the complete new configuration YAML with the smallest change that satisfies the request. Keep everything else exactly as it is.
3. Check it with validate_config. Fix every error before proposing.
4. File it with propose_change: a clear title, and a rationale that explains each changed setting in business terms and why it is needed.
5. Report the change request id, its tier, who must approve it, and what the simulation and regression evidence show.

Rules:
- You cannot approve or activate changes; people do. Never claim a change is live.
- Never weaken segregation of duties, approval routes or controls unless the request explicitly asks for it, and then say plainly what risk it adds.
- If the request is ambiguous or unsafe, propose nothing and explain what you need.`

type yamlArg struct {
	YAML string `json:"yaml" jsonschema:"required,description=The complete configuration document in YAML"`
}

type proposeArg struct {
	Title     string `json:"title" jsonschema:"required,description=Short title of the change"`
	Rationale string `json:"rationale" jsonschema:"required,description=Why each changed setting changes"`
	YAML      string `json:"yaml" jsonschema:"required,description=The complete proposed configuration in YAML"`
}

type noArgs struct{}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) tools(filed *[]string) []anthropic.BetaTool {
	return []anthropic.BetaTool{
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("get_active_config", "Return the active configuration YAML and its version.",
			func(_ context.Context, _ noArgs) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				doc, err := activeYAML(a.Repo)
				if err != nil {
					return llm.Error(err)
				}
				return llm.Result(doc)
			})),
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("get_sap_customizing",
			"Return the configuration generated from the client's SAP customizing tables, with notes on defaults and gaps.",
			func(_ context.Context, _ noArgs) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				if a.Extract == nil {
					return llm.Result("No SAP extract is available.")
				}
				cfg, notes := sapconfig.Generate(a.Extract, sapconfig.DefaultOptions())
				doc := llm.Must(cfg.Marshal()) // YAML encoding of a plain struct cannot fail
				return llm.Result(map[string]any{"generated_yaml": string(doc), "notes": notes, "findings": config.Validate(cfg)})
			})),
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("validate_config",
			"Validate a full configuration and list what differs from the active one, with each change's tier.",
			func(_ context.Context, in yamlArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				cfg, err := config.Parse([]byte(in.YAML))
				if err != nil {
					return llm.Error(err)
				}
				active, _, err := a.Repo.Active()
				if err != nil {
					return llm.Error(err)
				}
				diff := llm.Must(config.Diff(active, cfg)) // fails only if a parsed config cannot be JSON-encoded
				return llm.Result(map[string]any{"findings": config.Validate(cfg), "changes": diff, "tier": config.MaxTier(diff).String()})
			})),
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("propose_change",
			"File a change request. It is validated, simulated against recent transactions and run through the regression suite. Returns status, tier, findings and evidence.",
			func(_ context.Context, in proposeArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				cr, err := a.Propose(in.Title, in.Rationale, []byte(in.YAML))
				if err != nil {
					return llm.Error(err)
				}
				*filed = append(*filed, cr.ID)
				return llm.Result(summary(cr))
			})),
	}
}

// activeYAML returns the stored YAML of the active version, labeled with its number.
func activeYAML(r *config.Repo) (string, error) {
	_, v, err := r.Active()
	if err != nil {
		return "", err
	}
	doc, err := r.VersionYAML(v)
	return fmt.Sprintf("version %d\n%s", v, doc), err
}

// Propose files a change request as the agent and, unless it is invalid,
// attaches simulation and regression evidence.
func (a *Agent) Propose(title, rationale string, doc []byte) (*config.ChangeRequest, error) {
	cr, err := a.Repo.Propose(Identity, title, rationale, doc)
	if err != nil {
		return nil, err
	}
	if cr.Status == config.CRInvalid {
		return cr, nil
	}
	suite, err := a.Suite()
	if err != nil {
		return cr, fmt.Errorf("change request %s filed, but evidence could not be produced: %w", cr.ID, err)
	}
	days := a.ReplayDays
	if days == 0 {
		days = 90
	}
	from := a.now().AddDate(0, 0, -days).Format(time.DateOnly)
	return changes.AttachEvidence(a.Repo, cr.ID, a.Store, suite, from, "", a.now())
}

func summary(cr *config.ChangeRequest) map[string]any {
	ev := map[string]string{}
	for _, e := range cr.Evidence {
		ev[e.Kind] = e.Summary
	}
	steps := []string{}
	for _, s := range config.ApprovalSteps(cr.Tier) {
		steps = append(steps, fmt.Sprintf("%d x %s", s.Required, s.Role))
	}
	changes := make([]string, len(cr.Changes))
	for i, c := range cr.Changes {
		changes[i] = c.String() + " [" + c.Tier.String() + "]"
	}
	return map[string]any{
		"id": cr.ID, "status": cr.Status, "tier": cr.Tier.String(), "approvals_needed": strings.Join(steps, ", then "),
		"changes": changes, "findings": cr.Findings, "evidence": ev,
	}
}

// Result of a request to the agent.
type Result struct {
	Reply   string
	Changes []string // change request ids filed
}

// Handle carries out a plain-language configuration request.
func (a *Agent) Handle(ctx context.Context, request string) (*Result, error) {
	var filed []string
	msg, err := a.LLM.RunTools(ctx, a.tools(&filed), a.LLM.Params(prompt, anthropic.NewBetaTextBlock(request)), 15)
	if err != nil {
		return nil, err
	}
	return &Result{Reply: llm.Text(msg), Changes: filed}, nil
}
