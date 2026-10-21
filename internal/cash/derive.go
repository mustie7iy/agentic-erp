package cash

import (
	"fmt"
	"math/big"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Basis is what a flow's liquidity item is derived from (ADR-0051 §4).
type Basis struct {
	// Direction is the flow's: config.DirectionOutflow for a payment out,
	// config.DirectionInflow for a receipt.
	Direction string
	// Lines are the offsetting lines of the flow's document, such as a
	// supplier invoice's expense and asset lines with the tax of each
	// folded into it, or an order item's account; none for a flow of no
	// document, such as a statement line posted by a posting rule.
	Lines []Line
	// PlanningGroup is the supplier's planning group in the company, if any.
	PlanningGroup string
	// PostingRule is the transaction code of the posting rule a statement
	// line was posted by, if any (ADR-0047).
	PostingRule string
}

// Line is an offsetting line: its account number and its amount, signed
// as the document's lines are, which weighs the line in the split.
type Line struct {
	Account string
	Amount  money.Minor
}

// Share is the part of a flow's amount that has a liquidity item; an
// empty Item is a part no rule assigns and no default covers.
type Share struct {
	Item   string
	Amount money.Minor
}

// Derive splits a flow's amount over its liquidity items: each line takes
// the item of the first account rule covering its account, a line no rule
// covers (or a flow without lines) the fallback; the amount is shared out
// in proportion to the lines' signed amounts (money.Allocate), so the
// shares add up to it, one per item in the order the lines first name it:
// a credit line of an invoice, such as a discount on its own account,
// takes its item's share down, as it takes the document's total down.
// Lines that cancel out give the whole amount to the fallback. The fallback
// is the planning group's item, else the posting rule's, else the
// direction's default (SAP's query sequence and Flow Builder split).
func Derive(d config.Derivation, amount money.Minor, b Basis) ([]Share, error) {
	rest := fallback(d, b)
	var items []string
	weight := map[string]*big.Rat{}
	total := new(big.Rat)
	for _, l := range b.Lines {
		item := rest
		for _, r := range d.Accounts {
			if r.Covers(l.Account) {
				item = r.Item
				break
			}
		}
		if weight[item] == nil {
			items, weight[item] = append(items, item), new(big.Rat)
		}
		w := new(big.Rat).SetInt64(int64(l.Amount))
		weight[item].Add(weight[item], w)
		total.Add(total, w)
	}
	if total.Sign() == 0 {
		return []Share{{Item: rest, Amount: amount}}, nil
	}
	exact := make([]*big.Rat, len(items))
	for i, item := range items {
		exact[i] = new(big.Rat).Mul(new(big.Rat).SetInt64(int64(amount)), new(big.Rat).Quo(weight[item], total))
	}
	parts, err := money.Allocate(exact)
	if err != nil {
		return nil, fmt.Errorf("cash: split %d over %d liquidity items: %w", amount, len(items), err)
	}
	out := make([]Share, len(items))
	for i, item := range items {
		out[i] = Share{Item: item, Amount: parts[i]}
	}
	return out, nil
}

// fallback is the item of a flow, or of its lines, no account rule
// assigns: its supplier's planning group's, else its posting rule's, else
// its direction's default; "" when none is configured.
func fallback(d config.Derivation, b Basis) string {
	for _, r := range d.PlanningGroups {
		if b.PlanningGroup != "" && r.Group == b.PlanningGroup {
			return r.Item
		}
	}
	for _, r := range d.PostingRules {
		if b.PostingRule != "" && r.Rule == b.PostingRule {
			return r.Item
		}
	}
	if b.Direction == config.DirectionInflow {
		return d.Defaults.Inflow
	}
	return d.Defaults.Outflow
}
