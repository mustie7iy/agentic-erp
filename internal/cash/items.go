package cash

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Liquidity items in the forecast (ADR-0051 §4, §9, T53e3): each flow is
// split over the liquidity items its documents' offsetting lines derive
// (Derive), and the forecast gives rows by item and its hierarchy beside
// its rows by source and certainty.

// DocumentLine is an offsetting line of a flow's document as the ports
// read it: an expense, asset or tax line, with its account's number, its
// amount signed as the journal signs it (a debit positive), its tax code,
// and for a tax line the account key of the component it posts (input,
// output, self_output or self_input), "" for a base line.
type DocumentLine struct {
	Account         string
	Amount          money.Minor
	TaxCode, TaxKey string
}

// part is a document a flow pays: what the flow pays of it, signed as the
// flow, and what its liquidity item is derived from.
type part struct {
	amount        money.Minor
	lines         []DocumentLine
	planningGroup string
}

// offsetting are the lines a document's liquidity items are derived from
// (ADR-0051 §4): its base lines, each carrying the tax its code's tax
// lines post, shared out over the code's base lines in proportion to their
// amounts (money.Allocate: rounded once, by largest remainder, ties to the
// earlier line), so the tax follows its base line. A self-assessed pair
// (reverse charge or import, T61e: self_output and self_input) moves no
// cash, so its lines are left out before the split, never classifying a
// share of their own. A tax line whose code has no base line to follow,
// or whose base lines cancel out, stays a line of its own account.
func offsetting(lines []DocumentLine) ([]Line, error) {
	var out []Line
	base := map[string][]int{} // the base lines of each tax code, by index in out
	for _, l := range lines {
		if l.TaxKey == "" {
			if l.TaxCode != "" {
				base[l.TaxCode] = append(base[l.TaxCode], len(out))
			}
			out = append(out, Line{Account: l.Account, Amount: l.Amount})
		}
	}
	for _, l := range lines {
		if l.TaxKey == "" || l.TaxKey == config.KeySelfOutput || l.TaxKey == config.KeySelfInput {
			continue
		}
		var err error
		if out, err = follow(out, base[l.TaxCode], l); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// follow adds the tax line to the base lines at the indexes in proportion
// to their amounts, or, when they are none or cancel out, adds it as a line
// of its own.
func follow(out []Line, at []int, l DocumentLine) ([]Line, error) {
	weights := new(big.Rat)
	for _, i := range at {
		weights.Add(weights, new(big.Rat).SetInt64(int64(out[i].Amount)))
	}
	if weights.Sign() == 0 {
		return append(out, Line{Account: l.Account, Amount: l.Amount}), nil
	}
	exact := make([]*big.Rat, len(at))
	for j, i := range at {
		exact[j] = new(big.Rat).Quo(new(big.Rat).Mul(new(big.Rat).SetInt64(int64(l.Amount)), new(big.Rat).SetInt64(int64(out[i].Amount))), weights)
	}
	shares, err := money.Allocate(exact)
	if err != nil {
		return nil, fmt.Errorf("cash: share tax %d of code %s out: %w", l.Amount, l.TaxCode, err)
	}
	for j, i := range at {
		if out[i].Amount, err = money.Sum(out[i].Amount, shares[j]); err != nil {
			return nil, fmt.Errorf("cash: tax of code %s on account %s: %w", l.TaxCode, out[i].Account, err)
		}
	}
	return out, nil
}

// classify splits a flow over its liquidity items under the derivation:
// each part derived on its own (Derive, its direction the flow's) and the
// shares of one item summed, in the order the parts first name the items,
// so the shares add up to the parts, the flow's amount.
func classify(d config.Derivation, s *sums, amount money.Minor, parts []part) ([]Share, error) {
	direction := config.DirectionOutflow
	if amount > 0 {
		direction = config.DirectionInflow
	}
	var out []Share
	for _, p := range parts {
		lines, err := offsetting(p.lines)
		if err != nil {
			return nil, err
		}
		shares, err := Derive(d, p.amount, Basis{Direction: direction, Lines: lines, PlanningGroup: p.planningGroup})
		if err != nil {
			return nil, err
		}
		for _, sh := range shares {
			i := slices.IndexFunc(out, func(x Share) bool { return x.Item == sh.Item })
			if i < 0 {
				out = append(out, sh)
				continue
			}
			out[i].Amount = s.add(out[i].Amount, sh.Amount)
		}
	}
	return out, nil
}

// ItemRow is a row of a currency's forecast by liquidity item (ADR-0051
// §9): the item, its name and parent in the hierarchy and its level (0 at
// the top), its flows and its descendants' per bucket, in total and after
// the last bucket, counted as the buckets' totals count them, and its own
// flows by source and certainty. Item "" is the row of what no rule
// assigns and no default covers.
type ItemRow struct {
	Item, Name, Parent string
	Level              int
	Buckets            []money.Minor
	Total, Beyond      money.Minor
	Flows              []ItemFlows
}

// ItemFlows is an item's own flows of one source and certainty per bucket,
// in total and after the last bucket, listed whether or not the totals
// count them (a blocked supplier's without include_blocked).
type ItemFlows struct {
	Source, Certainty string
	Buckets           []money.Minor
	Total, Beyond     money.Minor
}

// itemRows gathers a currency's rows by liquidity item.
type itemRows struct {
	tree    map[string]config.LiquidityItem
	buckets int
	rows    map[string]*ItemRow
}

func newItemRows(items []config.LiquidityItem, buckets int) *itemRows {
	tree := map[string]config.LiquidityItem{}
	for _, it := range items {
		tree[it.Code] = it
	}
	return &itemRows{tree: tree, buckets: buckets, rows: map[string]*ItemRow{}}
}

// row is the item's row, made with its ancestors' when new.
func (t *itemRows) row(code string) *ItemRow {
	if r, ok := t.rows[code]; ok {
		return r
	}
	it := t.tree[code]
	r := &ItemRow{Item: code, Name: it.Name, Parent: it.Parent, Buckets: make([]money.Minor, t.buckets), Flows: []ItemFlows{}}
	t.rows[code] = r
	if it.Parent != "" {
		r.Level = t.row(it.Parent).Level + 1
	}
	return r
}

// add adds a share of a flow in bucket b (b == buckets: after the last) to
// its item's flows of the source and certainty, and, when counted, to the
// item's row and its ancestors'.
func (t *itemRows) add(s *sums, sh Share, f datedFlow, b int, counted bool) {
	r := t.row(sh.Item)
	i := slices.IndexFunc(r.Flows, func(x ItemFlows) bool { return x.Source == f.source && x.Certainty == f.certainty })
	if i < 0 {
		i = len(r.Flows)
		r.Flows = append(r.Flows, ItemFlows{Source: f.source, Certainty: f.certainty, Buckets: make([]money.Minor, t.buckets)})
	}
	fl := &r.Flows[i]
	if b < t.buckets {
		fl.Buckets[b], fl.Total = s.add(fl.Buckets[b], sh.Amount), s.add(fl.Total, sh.Amount)
	} else {
		fl.Beyond = s.add(fl.Beyond, sh.Amount)
	}
	if !counted {
		return
	}
	for code := sh.Item; ; code = r.Parent {
		r = t.rows[code]
		if b < t.buckets {
			r.Buckets[b], r.Total = s.add(r.Buckets[b], sh.Amount), s.add(r.Total, sh.Amount)
		} else {
			r.Beyond = s.add(r.Beyond, sh.Amount)
		}
		if r.Parent == "" {
			return
		}
	}
}

// ordered are the rows depth first: each item followed by its children,
// siblings by code, its own flows by source and certainty; the row of
// what no rule assigns last.
func (t *itemRows) ordered() []ItemRow {
	children := map[string][]string{}
	for code, r := range t.rows {
		if code != "" {
			children[r.Parent] = append(children[r.Parent], code)
		}
	}
	out := []ItemRow{}
	var walk func(parent string)
	walk = func(parent string) {
		codes := children[parent]
		slices.Sort(codes)
		for _, code := range codes {
			r := t.rows[code]
			slices.SortFunc(r.Flows, func(a, b ItemFlows) int {
				return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Certainty, b.Certainty))
			})
			out = append(out, *r)
			walk(code)
		}
	}
	walk("")
	if r, ok := t.rows[""]; ok {
		slices.SortFunc(r.Flows, func(a, b ItemFlows) int {
			return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Certainty, b.Certainty))
		})
		out = append(out, *r)
	}
	return out
}
