package config

import (
	"fmt"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

// Settings is the runtime view of one company's configuration, in the
// units the business services use.
type Settings struct {
	CompanyCode      string
	Currency         string
	Tolerance        core.MatchTolerance
	AutoPostLimit    money.Minor
	APReconciliation string
	InputTax         string
	Bank             string
	OverDelivery     int
	HouseBank        HouseBank
}

// Settings resolves a company's runtime settings. The configuration must
// already have passed validation.
func (c *Config) Settings(company string) (Settings, error) {
	co := c.Company(company)
	if co == nil {
		return Settings{}, fmt.Errorf("company %s is not configured", company)
	}
	dec := decimalsFor(co.Currency)
	abs, err := money.ParseDecimal(co.InvoiceTolerance.PriceAbsolute, dec)
	if err != nil {
		return Settings{}, fmt.Errorf("company %s tolerance: %w", company, err)
	}
	limit, err := money.ParseDecimal(co.AutoPostLimit, dec)
	if err != nil {
		return Settings{}, fmt.Errorf("company %s auto-post limit: %w", company, err)
	}
	return Settings{
		CompanyCode: co.Code, Currency: co.Currency,
		Tolerance:     core.MatchTolerance{PercentOfLine: int64(co.InvoiceTolerance.PricePercent), Absolute: abs},
		AutoPostLimit: limit, APReconciliation: co.AccountDetermination.APReconciliation,
		InputTax: co.AccountDetermination.InputTax, Bank: co.AccountDetermination.Bank,
		OverDelivery: co.OverDeliveryPercent, HouseBank: co.HouseBank,
	}, nil
}

// Policy builds the governance policy for a company.
func (c *Config) Policy(company string) (governance.Policy, error) {
	s, err := c.Settings(company)
	if err != nil {
		return governance.Policy{}, err
	}
	return governance.Policy{
		AutoLimit:    s.AutoPostLimit,
		AlwaysHuman:  []string{governance.ActVendorBank, governance.ActPaymentRun},
		SoDConflicts: c.SoDConflicts,
	}, nil
}

// Identity resolves a configured user or agent to a governance identity.
func (c *Config) Identity(id string) (governance.Identity, error) {
	if u := c.User(id); u != nil {
		return governance.Identity{ID: "user:" + u.ID, Kind: "human", Permissions: c.UserPermissions(id), Autonomy: governance.FullyAutonomous}, nil
	}
	if a := c.Agent(id); a != nil {
		return governance.Identity{ID: a.ID, Kind: "agent", Permissions: slices.Clone(a.Permissions), Autonomy: governance.Autonomy(a.Autonomy)}, nil
	}
	return governance.Identity{}, fmt.Errorf("%q is not a configured user or agent", id)
}

// Actor resolves a user to a workflow actor (roles drive approval steps).
func (c *Config) Actor(id string) (workflow.Actor, error) {
	if u := c.User(id); u != nil {
		return workflow.Actor{ID: "user:" + u.ID, Kind: "human", Roles: slices.Clone(u.Roles)}, nil
	}
	if a := c.Agent(id); a != nil {
		return workflow.Actor{ID: a.ID, Kind: "agent"}, nil
	}
	return workflow.Actor{}, fmt.Errorf("%q is not a configured user or agent", id)
}

// CanAccess reports whether a user may act in a company (no list = all).
func (c *Config) CanAccess(id, company string) bool {
	u := c.User(id)
	if u == nil {
		return c.Agent(id) != nil
	}
	return len(u.Companies) == 0 || slices.Contains(u.Companies, company)
}

// RequisitionSteps returns the approval steps for a requisition amount.
func (c *Config) RequisitionSteps(amount money.Minor) ([]workflow.Step, error) {
	for _, t := range c.Purchasing.RequisitionApproval {
		if t.UpTo != "" {
			limit, err := money.ParseDecimal(t.UpTo, 2)
			if err != nil {
				return nil, err
			}
			if amount > limit {
				continue
			}
		}
		steps := make([]workflow.Step, len(t.Steps))
		for i, s := range t.Steps {
			steps[i] = workflow.Step{Role: s.Role, Required: s.Approvals}
		}
		return steps, nil
	}
	return nil, fmt.Errorf("no requisition approval tier covers %s", amount)
}

// ApprovalSteps for a configuration change of the given tier. Tier 1 needs
// no approval; higher tiers need more, and more senior, approvers.
func ApprovalSteps(t Tier) []workflow.Step {
	switch t {
	case TierOperational:
		return []workflow.Step{{Role: "process_owner", Required: 1}}
	case TierPostingRelated:
		return []workflow.Step{{Role: "process_owner", Required: 1}, {Role: "controller", Required: 1}}
	case TierStructural:
		return []workflow.Step{{Role: "solution_architect", Required: 1}, {Role: "controller", Required: 1}}
	}
	return nil
}
