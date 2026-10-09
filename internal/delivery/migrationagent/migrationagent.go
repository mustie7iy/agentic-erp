// Package migrationagent is the migration agent: it does the work of an SAP
// data migration consultant. It profiles extracts, proposes account
// classifications with confidence, rehearses migrations in a sandbox,
// explains every reconciliation difference down to the document, and
// drafts cleansing actions for data owners. It never loads into the live
// store; cutover stays a human decision.
package migrationagent

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/simulate"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

type Agent struct {
	LLM     *llm.Runner
	Source  sap.Source
	Store   *core.Store // live store; rehearsals run on a copy
	Mapping migration.Mapping
}

const prompt = `You are the migration agent of an ERP platform that replaces SAP scope by scope. You do the work of an SAP data migration consultant.

How to work:
1. Profile the scope's extract (profile_extract) and review data-quality findings.
2. Review account classification proposals (propose_account_classification); call out every low-confidence account.
3. Rehearse the migration in a sandbox (run_rehearsal). If reconciliation fails, use explain_difference on each failing account or vendor to find the exact documents.
4. Draft cleansing actions for the data owners (propose_cleansing).
5. Finish with a cutover readiness summary: blocking issues, decisions needed from the business, and whether the scope reconciles.

Rules: you only rehearse in a sandbox; you never load into production and never declare a scope ready for cutover on your own. Quote document numbers and amounts from the tools.`

type scopeArg struct {
	Company string `json:"company" jsonschema:"required,description=Company code"`
	Year    string `json:"year" jsonschema:"required,description=Fiscal year"`
}

type rehearsalArg struct {
	Company string `json:"company" jsonschema:"required"`
	Year    string `json:"year" jsonschema:"required"`
	Mode    string `json:"mode,omitempty" jsonschema:"enum=documents,enum=balances"`
}

type explainArg struct {
	Company string `json:"company" jsonschema:"required"`
	Year    string `json:"year" jsonschema:"required"`
	Account string `json:"account,omitempty" jsonschema:"description=G/L account to explain"`
	Vendor  string `json:"vendor,omitempty" jsonschema:"description=Vendor whose open items to explain"`
}

func (a *Agent) extract(ctx context.Context, company, year string) (*sap.Extract, sap.Scope, error) {
	scope := sap.Scope{CompanyCodes: []string{company}, FiscalYear: year}
	e, err := a.Source.Extract(ctx, scope)
	return e, scope, err
}

func (a *Agent) mapping() migration.Mapping {
	if a.Mapping.AccountTypes == nil {
		return migration.DefaultMapping()
	}
	return a.Mapping
}

// Profile returns the extract counts and data-quality findings.
func (a *Agent) Profile(ctx context.Context, company, year string) (map[string]any, error) {
	e, _, err := a.extract(ctx, company, year)
	if err != nil {
		return nil, err
	}
	issues := migration.CheckQuality(e, a.mapping())
	return map[string]any{"rows": e.Counts(), "findings": issues, "blocking": migration.HasErrors(issues)}, nil
}

// Rehearse migrates the scope into a copy of the live store and reconciles.
func (a *Agent) Rehearse(ctx context.Context, company, year, mode string) (*migration.Result, error) {
	box := simulate.Clone(a.Store)
	_, scope, err := a.extract(ctx, company, year)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = string(migration.ModeDocuments)
	}
	return migration.Run(ctx, a.Source, scope, migration.Options{Mode: migration.Mode(mode), Mapping: a.mapping(), Force: true}, box)
}

// Explain lists the documents behind a difference, rehearsing first.
func (a *Agent) Explain(ctx context.Context, company, year, account, vendor string) ([]migration.DocDifference, error) {
	box := simulate.Clone(a.Store)
	e, scope, err := a.extract(ctx, company, year)
	if err != nil {
		return nil, err
	}
	if _, err := migration.Run(ctx, a.Source, scope, migration.Options{Mapping: a.mapping(), Force: true}, box); err != nil {
		return nil, err
	}
	switch {
	case account != "":
		return migration.ExplainAccount(e, box, a.mapping(), company, account)
	case vendor != "":
		return migration.ExplainVendor(e, box, a.mapping(), company, vendor)
	}
	return nil, fmt.Errorf("give an account or a vendor to explain")
}

func (a *Agent) tools() []anthropic.BetaTool {
	return []anthropic.BetaTool{
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("profile_extract", "Row counts and data-quality findings for a scope.",
			func(ctx context.Context, in scopeArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				p, err := a.Profile(ctx, in.Company, in.Year)
				if err != nil {
					return llm.Error(err)
				}
				return llm.Result(p)
			})),
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("propose_account_classification",
			"Proposed account type for every G/L account with confidence and reasons.",
			func(ctx context.Context, in scopeArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				e, _, err := a.extract(ctx, in.Company, in.Year)
				if err != nil {
					return llm.Error(err)
				}
				return llm.Result(migration.ProposeAccountTypes(e, a.mapping()))
			})),
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("run_rehearsal",
			"Migrate the scope into a sandbox copy and reconcile. Returns findings, load rejections and the reconciliation report.",
			func(ctx context.Context, in rehearsalArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				res, err := a.Rehearse(ctx, in.Company, in.Year, in.Mode)
				if err != nil {
					return llm.Error(err)
				}
				return llm.Result(res)
			})),
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("explain_difference",
			"Document-level comparison between SAP and the migrated data for one account or vendor.",
			func(ctx context.Context, in explainArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				d, err := a.Explain(ctx, in.Company, in.Year, in.Account, in.Vendor)
				if err != nil {
					return llm.Error(err)
				}
				if len(d) == 0 {
					return llm.Result("No document-level differences.")
				}
				return llm.Result(d)
			})),
		llm.Must(toolrunner.NewBetaToolFromJSONSchema("propose_cleansing",
			"Concrete cleansing actions for each data-quality finding, with the owner.",
			func(ctx context.Context, in scopeArg) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				e, _, err := a.extract(ctx, in.Company, in.Year)
				if err != nil {
					return llm.Error(err)
				}
				return llm.Result(migration.ProposeCleansing(e, migration.CheckQuality(e, a.mapping())))
			})),
	}
}

// Assess runs the agent over a scope and returns its readiness summary.
func (a *Agent) Assess(ctx context.Context, company, year string) (string, error) {
	tools := a.tools()
	q := fmt.Sprintf("Assess migration readiness for company %s, fiscal year %s.", company, year)
	msg, err := a.LLM.RunTools(ctx, tools, a.LLM.Params(prompt, anthropic.NewBetaTextBlock(q)), 15)
	if err != nil {
		return "", err
	}
	return llm.Text(msg), nil
}
