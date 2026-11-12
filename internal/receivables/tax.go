package receivables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// outputTax sets each item's tax: what its output tax code gives at the
// invoice date, the invoice's tax date (ADR-0049 §4; ADR-0063 §4), from
// the calculator (internal/tax). Tax is rounded as the company's tax
// settings say, once per code and component over the invoice's items by
// default, and each item keeps its share (money.Allocate), so the items'
// tax adds up to the invoice's. An item without a code bears none.
func (e *Endpoints) outputTax(ctx context.Context, le tenancy.Entity, currency string, date time.Time, items []Item) error {
	doc := tax.Document{Company: le.Code, TaxDate: date, Currency: currency, Codes: map[string]tax.Code{}}
	var coded []int
	for i, it := range items {
		if it.TaxCode == nil {
			continue
		}
		if err := e.outputCode(ctx, le, date, i, *it.TaxCode, doc.Codes); err != nil {
			return err
		}
		coded = append(coded, i)
		doc.Lines = append(doc.Lines, tax.Line{Amount: it.Net, Code: *it.TaxCode})
	}
	if len(coded) == 0 {
		return nil
	}
	settings, _, err := e.Settings.Document(ctx)
	if err != nil {
		return fmt.Errorf("receivables: tax settings of company %s: %w", le.Code, err)
	}
	// A company the configuration lacks rounds by the default, per document.
	if co := settings.Company(le.Code); co != nil {
		doc.Rounding = tax.Rounding(co.TaxSettings.Rounding)
	}
	res, err := tax.Calculate(doc)
	if errors.Is(err, money.ErrOverflow) {
		return errTooLarge()
	}
	if err != nil { // the codes were read on the date with their rates: not expected
		return fmt.Errorf("receivables: output tax on %s: %w", date.Format(time.DateOnly), err)
	}
	for j, i := range coded {
		parts := make([]money.Minor, len(res.Lines[j].Components))
		for k, c := range res.Lines[j].Components {
			parts[k] = c.Tax
		}
		// Each component's tax is below the item's amount; several may not add up in range.
		if items[i].Tax, err = money.Sum(parts...); err != nil {
			return errTooLarge()
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
func (e *Endpoints) outputCode(ctx context.Context, le tenancy.Entity, date time.Time, i int, code string, codes map[string]tax.Code) error {
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
	codes[code] = t.Calculable()
	return nil
}
