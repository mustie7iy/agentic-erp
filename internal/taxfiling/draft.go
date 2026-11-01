package taxfiling

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// GetTaxReturnDraft implements GET /v1/tax-registrations/{id}/draft-return: a draft of the
// registration's return for one of its ended periods (ADR-0049 §11),
// computed from the unreported tax items of its companies and never
// stored. It claims no item: the same request gives the same draft until
// items are posted, or reported by a prepared return (T61i), which sets
// reported_in under a lock on the registration.
func (e *Endpoints) GetTaxReturnDraft(ctx context.Context, params gen.GetTaxReturnDraftParams) (gen.GetTaxReturnDraftRes, error) {
	s, err := e.scope(ctx, params.ID, params.PeriodStart)
	if err != nil {
		return nil, err
	}
	groups, err := e.Items.Unreported(ctx, s.entities, s.period.Start, s.period.End)
	if err != nil {
		return nil, err
	}
	sums, err := s.sum(groups)
	if err != nil {
		return nil, err
	}
	return s.draft(ctx, sums)
}

// boxSum is a selection box's sum: all its items', and the prior-period
// items' part.
type boxSum struct {
	all, prior money.Minor
	items      int64
}

// returnSums are the selection boxes' sums by code, and the items some box
// selects: their count, and the prior-period ones' count and tax, in all
// and by the earlier period and direction they belong to.
type returnSums struct {
	boxes             map[string]boxSum
	items, priorItems int64
	priorTax          money.Minor
	priorBy           map[priorKey]money.Minor
}

// priorKey is an earlier period of the registration (its start; zero
// before its first) and a direction.
type priorKey struct {
	period    time.Time
	direction string
}

// sum sums each selection box's measure over the groups it selects, and
// counts the items some box selects, once.
func (s *returnScope) sum(groups []ledger.TaxItemGroup) (returnSums, error) {
	out := returnSums{boxes: map[string]boxSum{}, priorBy: map[priorKey]money.Minor{}}
	selected := make([]bool, len(groups))
	for _, b := range s.def.Boxes {
		if b.Select == nil {
			continue
		}
		sum, err := boxSumOf(b.Select, groups, selected)
		if err != nil {
			return returnSums{}, fmt.Errorf("taxfiling: return of %s box %s: %w", s.reg.Code, b.Code, err)
		}
		out.boxes[b.Code] = sum
	}
	for i, g := range groups {
		if !selected[i] {
			continue
		}
		out.items += g.Items
		if g.PriorPeriod {
			if err := s.addPrior(&out, g); err != nil {
				return returnSums{}, fmt.Errorf("taxfiling: return of %s prior-period tax: %w", s.reg.Code, err)
			}
		}
	}
	return out, nil
}

// addPrior adds a prior-period group's tax in all and to its earlier
// period and direction.
func (s *returnScope) addPrior(out *returnSums, g ledger.TaxItemGroup) error {
	var err error
	if out.priorTax, err = money.Sum(out.priorTax, g.TaxLC); err != nil {
		return err
	}
	k := priorKey{direction: g.Direction}
	if ps := config.TaxPeriods(s.reg.FirstPeriodStart, s.reg.Frequency, nil, *g.PriorTaxDate); len(ps) > 0 {
		k.period = ps[len(ps)-1].Start
	}
	if out.priorBy[k], err = money.Sum(out.priorBy[k], g.TaxLC); err != nil {
		return err
	}
	out.priorItems += g.Items
	return nil
}

// aboveThreshold reports whether the input or the output tax of the items
// of any one earlier period exceeds the threshold in magnitude: the
// conservative reading of P7 until the tax specialist settles the measure
// (net or gross, per period or pooled, per error), so that the flag errs
// towards a person's review (ADR-0049 §11 As built).
func (r returnSums) aboveThreshold(threshold money.Minor) bool {
	for _, v := range r.priorBy {
		if v.Abs() > threshold {
			return true
		}
	}
	return false
}

// boxSumOf sums a selection's measure over the groups it selects and
// marks them selected.
func boxSumOf(sel *countrypack.Selection, groups []ledger.TaxItemGroup, selected []bool) (boxSum, error) {
	var sum boxSum
	match := selection(sel)
	for i, g := range groups {
		if !match.Matches(g) {
			continue
		}
		v, err := measure(g, sel)
		if err == nil {
			sum.all, err = money.Sum(sum.all, v)
		}
		if err == nil && g.PriorPeriod {
			sum.prior, err = money.Sum(sum.prior, v)
		}
		if err != nil {
			return boxSum{}, err
		}
		sum.items += g.Items
		selected[i] = true
	}
	return sum, nil
}

// draft is the draft return: the expression boxes computed from the
// selection boxes' sums, and the prior-period items held to the pack's
// threshold.
func (s *returnScope) draft(ctx context.Context, sums returnSums) (*gen.TaxReturnDraft, error) {
	all, prior, err := s.values(ctx, sums)
	if err != nil {
		return nil, err
	}
	threshold, set := s.def.Threshold(s.exp)
	m := func(v money.Minor) gen.Money { return apitypes.Money(v, s.currency, s.exp) }
	out := &gen.TaxReturnDraft{RegistrationID: s.reg.ID, RegistrationCode: s.reg.Code, ReturnType: s.def.Type, ReturnName: s.def.Name,
		PeriodStart: s.period.Start, PeriodEnd: s.period.End, Currency: s.currency, ItemCount: sums.items,
		Pack:        gen.TaxReturnDraftPack{Country: s.pack.Manifest.Country, Version: s.pack.Manifest.Version},
		PriorPeriod: gen.TaxReturnDraftPriorPeriod{ItemCount: sums.priorItems, Tax: m(sums.priorTax), AboveThreshold: set && sums.aboveThreshold(threshold)},
		Boxes:       make([]gen.TaxReturnBox, len(s.def.Boxes))}
	if set {
		out.PriorPeriod.Threshold = gen.NewOptMoney(m(threshold))
	}
	for i, b := range s.def.Boxes {
		box := gen.TaxReturnBox{Code: b.Code, Label: b.Label, Kind: "expression", Amount: m(all[b.Code]), PriorPeriodAmount: m(prior[b.Code])}
		if b.Select != nil {
			box.Kind, box.ItemCount = "selection", gen.NewOptInt64(sums.boxes[b.Code].items)
		}
		out.Boxes[i] = box
	}
	return out, nil
}

// values are every box's value from the selection boxes' sums, in all and
// their prior-period part, by code: the expression boxes computed.
func (s *returnScope) values(ctx context.Context, sums returnSums) (all, prior map[string]money.Minor, err error) {
	all, prior = map[string]money.Minor{}, map[string]money.Minor{}
	for code, b := range sums.boxes {
		all[code], prior[code] = b.all, b.prior
	}
	all, err = s.def.Evaluate(ctx, all)
	if err == nil {
		prior, err = s.def.Evaluate(ctx, prior)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("taxfiling: return of %s: %w", s.reg.Code, err)
	}
	return all, prior, nil
}

// itemCursor is a box items page's position: the last item's place in the
// order the ledger pages by.
type itemCursor struct {
	TaxDate    string    `json:"d"`
	FiscalYear int16     `json:"y"`
	EntryID    uuid.UUID `json:"e"`
	ItemNo     int32     `json:"n"`
}

// ListTaxReturnDraftItems implements GET
// /v1/tax-registrations/{id}/draft-return/items: the items a
// selection box of the draft sums, each with what it adds to the box,
// paged in tax date and entry order.
func (e *Endpoints) ListTaxReturnDraftItems(ctx context.Context, params gen.ListTaxReturnDraftItemsParams) (gen.ListTaxReturnDraftItemsRes, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listTaxReturnDraftItems|%s|%s|%s", params.ID, params.PeriodStart.Format(time.DateOnly), params.Box)
	var after *ledger.TaxItemKey
	if params.PageToken.Set {
		var c itemCursor
		if err := e.Pages.Decode(scope, params.PageToken.Value, &c); err != nil {
			return nil, err
		}
		d, _ := time.Parse(time.DateOnly, c.TaxDate) // signed by this endpoint, from a date
		after = &ledger.TaxItemKey{TaxDate: d, FiscalYear: c.FiscalYear, EntryID: c.EntryID, ItemNo: c.ItemNo}
	}
	s, err := e.scope(ctx, params.ID, params.PeriodStart)
	if err != nil {
		return nil, err
	}
	box := s.selectionBox(params.Box)
	if box == nil {
		return nil, problem.Unprocessable("tax.not_a_selection_box", fmt.Sprintf("Return %s has no box %s that selects tax items.", s.def.Type, params.Box),
			problem.FieldError{Field: "box", Code: "not_a_selection_box", Message: "the code of a box of the return with a selection of tax items"})
	}
	items, err := e.Items.UnreportedItems(ctx, s.entities, s.period.End, selection(box.Select), after, size+1)
	if err != nil {
		return nil, err
	}
	page := &gen.TaxReturnItemPage{Box: box.Code, Currency: s.currency, Items: []gen.TaxReturnItem{}}
	if len(items) > size {
		items = items[:size]
		last := items[size-1]
		tok, _ := e.Pages.Encode(scope, itemCursor{TaxDate: last.TaxDate.Format(time.DateOnly), FiscalYear: last.FiscalYear, EntryID: last.EntryID,
			ItemNo: last.ItemNo}) // strings and numbers always encode
		page.NextPageToken = gen.NewOptString(tok)
	}
	for _, it := range items {
		out, err := s.itemToAPI(it, box.Select)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, out)
	}
	return page, nil
}

// selectionBox is the return's box of the code that selects items, or nil.
func (s *returnScope) selectionBox(code string) *countrypack.Box {
	for i, b := range s.def.Boxes {
		if b.Code == code && b.Select != nil {
			return &s.def.Boxes[i]
		}
	}
	return nil
}

// itemToAPI is a box's item as the drill-down shows it: prior-period when
// dated before the period's start, as Unreported's groups tell it in SQL
// (tax_date < the start).
func (s *returnScope) itemToAPI(it ledger.ReturnItem, sel *countrypack.Selection) (gen.TaxReturnItem, error) {
	v, err := measure(ledger.TaxItemGroup{AccountKey: it.AccountKey, BaseLC: it.BaseLC, TaxLC: it.TaxLC, RecoverableLC: it.RecoverableLC}, sel)
	if err != nil {
		return gen.TaxReturnItem{}, fmt.Errorf("taxfiling: item %d of entry %s: %w", it.ItemNo, it.EntryID, err)
	}
	m := func(v money.Minor) gen.Money { return apitypes.Money(v, s.currency, s.exp) }
	return gen.TaxReturnItem{FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID, ItemNo: it.ItemNo, LegalEntityID: it.LegalEntityID,
		CompanyCode: s.codes[it.LegalEntityID], TaxDate: it.TaxDate, TaxCode: it.Code, Component: it.Component, Jurisdiction: apitypes.OptString(it.Jurisdiction),
		Direction: it.Direction, Kind: it.Kind, AccountKey: it.AccountKey, Base: m(it.BaseLC), Tax: m(it.TaxLC), Recoverable: m(it.RecoverableLC),
		BoxAmount: m(v), PriorPeriod: it.TaxDate.Before(s.period.Start), DocumentNumber: it.DocumentNumber, PostingDate: it.PostingDate,
		Reference: apitypes.OptString(it.Reference), HeaderText: apitypes.OptString(it.Header)}, nil
}
