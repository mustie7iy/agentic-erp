package receivables

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// calculated is the output tax of an invoice's items on its invoice date:
// the calculator's result over the items that carry a code (coded, in
// item order: res.Lines[j] is item coded[j]'s) and the codes as the ledger
// read them on the date, whose components name the accounts their tax
// posts to.
type calculated struct {
	coded []int
	res   tax.Result
	codes map[string]ledger.TaxCode
}

// itemTax is the tax of the j-th coded item: its components' together.
func (c calculated) itemTax(j int) (money.Minor, error) {
	parts := make([]money.Minor, len(c.res.Lines[j].Components))
	for k, p := range c.res.Lines[j].Components {
		parts[k] = p.Tax
	}
	// Each component's tax is below the item's amount; several may not add up in range.
	sum, err := money.Sum(parts...)
	if err != nil {
		return 0, errTooLarge()
	}
	return sum, nil
}

// calculate is what the items' output tax codes give at the invoice
// date, the invoice's tax date (ADR-0049 §4; ADR-0063 §4), from the
// calculator (internal/tax). Tax is rounded as the company's tax settings
// say, once per code and component over the invoice's items by default.
// An item without a code bears none. Each item names the jurisdiction its
// code levies exactly where the code levies its lines' subdivision
// (jurisdiction): the tax is the code's over the invoice whatever the
// jurisdictions, each item keeping its share, which the posting adds up
// per jurisdiction (taxLines).
func (e *Endpoints) calculate(ctx context.Context, le tenancy.Entity, currency string, date time.Time, items []Item) (calculated, error) {
	doc := tax.Document{Company: le.Code, TaxDate: date, Currency: currency, Codes: map[string]tax.Code{}}
	c := calculated{codes: map[string]ledger.TaxCode{}}
	for i, it := range items {
		var code ledger.TaxCode // the zero code: an item without one
		if it.TaxCode != nil {
			if err := e.outputCode(ctx, le, date, i, *it.TaxCode, c.codes); err != nil {
				return calculated{}, err
			}
			code = c.codes[*it.TaxCode]
		}
		if err := jurisdiction(i, it, code); err != nil {
			return calculated{}, err
		}
		if it.TaxCode == nil {
			continue
		}
		doc.Codes[*it.TaxCode] = code.Calculable()
		c.coded = append(c.coded, i)
		doc.Lines = append(doc.Lines, tax.Line{Amount: it.Net, Code: *it.TaxCode})
	}
	if len(c.coded) == 0 {
		return c, nil
	}
	settings, _, err := e.Settings.Document(ctx)
	if err != nil {
		return calculated{}, fmt.Errorf("receivables: tax settings of company %s: %w", le.Code, err)
	}
	// A company the configuration lacks rounds by the default, per document.
	if co := settings.Company(le.Code); co != nil {
		doc.Rounding = tax.Rounding(co.TaxSettings.Rounding)
	}
	c.res, err = tax.Calculate(doc)
	if errors.Is(err, money.ErrOverflow) {
		return calculated{}, errTooLarge()
	}
	if err != nil { // the codes were read on the date with their rates: not expected
		return calculated{}, fmt.Errorf("receivables: output tax on %s: %w", date.Format(time.DateOnly), err)
	}
	return c, nil
}

// outputTax sets each item's tax (calculate): each item keeps its share
// (money.Allocate in the calculator), so the items' tax adds up to the
// invoice's.
func (e *Endpoints) outputTax(ctx context.Context, le tenancy.Entity, currency string, date time.Time, items []Item) error {
	c, err := e.calculate(ctx, le, currency, date, items)
	if err != nil {
		return err
	}
	for j, i := range c.coded {
		if items[i].Tax, err = c.itemTax(j); err != nil {
			return err
		}
	}
	return nil
}

// outputCode reads item i's tax code on the date into codes, once per
// code: an output tax code of the legal entity with a rate in force then
// (422 receivables.unknown_tax_code naming the item's field otherwise).
func (e *Endpoints) outputCode(ctx context.Context, le tenancy.Entity, date time.Time, i int, code string, codes map[string]ledger.TaxCode) error {
	if _, ok := codes[code]; ok {
		return nil
	}
	field := fmt.Sprintf("items[%d].tax_code", i)
	t, ok, err := e.Accounts.TaxCode(ctx, le.ID, code, date)
	if err != nil {
		return fmt.Errorf("receivables: tax code %s of item %d: %w", code, i+1, err)
	}
	if !ok || t.Direction != config.TaxOutput {
		return problem.Invalid("receivables.unknown_tax_code", field,
			"Tax code "+code+" is not an output tax code of the legal entity on the invoice date "+date.Format(time.DateOnly)+".")
	}
	codes[code] = t
	return nil
}

// jurisdiction holds the tax jurisdiction item i names to its tax code,
// the zero code for an item without one, as the ledger holds a line's
// (ledger.TaxCode.CheckJurisdiction; ADR-0049 §7, ADR-0063 §4 As built):
// a code whose component levies the subdivision its lines name (one code
// for every emirate) needs the item's (422
// receivables.tax_jurisdiction_required), a subdivision of the
// component's country and, where the component lists those it accepts,
// one of them; an item of any other code, or of none, names no
// jurisdiction (422 receivables.tax_jurisdiction_not_levied). Each names
// items[i].tax_jurisdiction.
func jurisdiction(i int, it Item, code ledger.TaxCode) error {
	field := fmt.Sprintf("items[%d].tax_jurisdiction", i)
	switch fault, c := code.CheckJurisdiction(it.TaxJurisdiction); fault {
	case ledger.JurisdictionOK: // named, or not, as the item's code asks
	case ledger.JurisdictionRequired:
		return problem.Invalid("receivables.tax_jurisdiction_required", field, "Tax code "+code.Code+
			" levies the tax of the subdivision each item names: name the province or emirate ("+c.Jurisdiction+"-…).")
	case ledger.JurisdictionNotLevied:
		return problem.Invalid("receivables.tax_jurisdiction_not_levied", field, "Jurisdiction "+*it.TaxJurisdiction+
			" is not one the item's tax code levies: an item names one only with a code that levies its lines' subdivision, and one of that code's country.")
	case ledger.JurisdictionNotAccepted:
		return problem.Invalid("receivables.tax_jurisdiction_not_levied", field, "Tax code "+code.Code+" does not levy jurisdiction "+
			*it.TaxJurisdiction+": it levies "+strings.Join(c.Subdivisions, ", ")+".")
	}
	return nil
}

// given says whether the invoice's items bear the tax they were entered
// with rather than what their codes give at posting: a credit memo
// entered from a source document (givenTax; sales' credit memo request,
// T85d), whose taxes are shares of what the credited invoice bore.
func given(v Invoice) bool { return v.Kind == KindCreditMemo && v.SourceDocumentID != nil }

// shareGiven makes the document's tax per code and component the items'
// taxes as entered (given): each coded item's tax is shared out among
// its code's components as the calculator splits that item's tax
// (money.Allocate), and taxGroups adds the shares up per component. A code
// that gives no tax at all on the invoice date for an item entered with
// tax is refused, 409 receivables.tax_changed, as a changed rate is.
func (c *calculated) shareGiven(v Invoice) error {
	for j, i := range c.coded {
		parts := c.res.Lines[j].Components
		var total int64
		for _, p := range parts {
			total += int64(p.Tax) // each part is below the item's amount, and an item's tax adds up in range (itemTax)
		}
		it := v.Items[i]
		if total == 0 {
			if it.Tax != 0 {
				return problem.Conflict("receivables.tax_changed", fmt.Sprintf("Item %d's tax code %s gives no tax on the invoice date; "+
					"the credit memo was entered with %s %s.", it.ItemNo, *it.TaxCode, it.Tax.Plain(v.Exponent), v.Currency))
			}
			continue
		}
		exact := make([]*big.Rat, len(parts))
		for k, p := range parts {
			exact[k] = new(big.Rat).Mul(big.NewRat(int64(p.Tax), total), new(big.Rat).SetInt64(int64(it.Tax)))
		}
		shares, _ := money.Allocate(exact) // the shares add up to the item's tax, which is in range
		for k := range parts {
			parts[k].Tax = shares[k] // taxGroups adds the lines' components up per code, jurisdiction and component
		}
	}
	return nil
}
