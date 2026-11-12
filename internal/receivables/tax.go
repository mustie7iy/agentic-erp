package receivables

import (
	"context"
	"errors"
	"fmt"
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
// An item without a code bears none.
func (e *Endpoints) calculate(ctx context.Context, le tenancy.Entity, currency string, date time.Time, items []Item) (calculated, error) {
	doc := tax.Document{Company: le.Code, TaxDate: date, Currency: currency, Codes: map[string]tax.Code{}}
	c := calculated{codes: map[string]ledger.TaxCode{}}
	for i, it := range items {
		if it.TaxCode == nil {
			continue
		}
		if err := e.outputCode(ctx, le, date, i, *it.TaxCode, c.codes); err != nil {
			return calculated{}, err
		}
		doc.Codes[*it.TaxCode] = c.codes[*it.TaxCode].Calculable()
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
// A code whose component levies the subdivision its lines name (one code
// for every emirate, ADR-0049 §7) is refused: an item names no
// jurisdiction yet (ADR-0063 §4).
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
	for _, c := range t.Components {
		if c.JurisdictionOnLine {
			return problem.Invalid("receivables.tax_jurisdiction_not_supported", field,
				"Tax code "+code+" levies the tax of the subdivision each line names; a customer invoice's items name none yet.")
		}
	}
	codes[code] = t
	return nil
}
