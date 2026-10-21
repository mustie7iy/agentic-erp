package payables

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// taxDateOf is an invoice's tax date (ADR-0049 §4, decision P6): the one
// a person set, else, for a correction naming an invoice, that invoice's
// (a credit memo credits the tax charged, at its rates), else its invoice
// date.
func taxDateOf(ctx context.Context, tx pgx.Tx, set *time.Time, invoiceDate time.Time, reference *uuid.UUID) (time.Time, error) {
	if set != nil {
		return *set, nil
	}
	if reference == nil {
		return invoiceDate, nil
	}
	var d time.Time
	// The reference is a posted invoice of the tenant (kind), never deleted.
	if err := tx.QueryRow(ctx, `SELECT coalesce(tax_date, invoice_date) FROM payables.supplier_invoice WHERE id = $1`, *reference).Scan(&d); err != nil {
		return time.Time{}, fmt.Errorf("payables: tax date of invoice %s: %w", *reference, err)
	}
	return d, nil
}

// validTaxDate is the tax date an invoice input checks its codes on: a
// tax date it sets is a person's to set (decision P6), an agent's is
// refused (403 payables.tax_date_human_only); one a change keeps is not
// the agent's.
func (e *Endpoints) validTaxDate(ctx context.Context, tx pgx.Tx, in Input) (time.Time, error) {
	if p, ok := reqctx.PrincipalFrom(ctx); taxDateSet(in) && ok && p.Kind == reqctx.KindAgent {
		return time.Time{}, problem.Forbidden("payables.tax_date_human_only", "A tax date is set by a person; an agent proposes the invoice without one.")
	}
	return taxDateOf(ctx, tx, in.TaxDate, in.InvoiceDate, in.ReferenceInvoiceID)
}

// errTaxTooLarge refuses an invoice whose tax is out of range.
var errTaxTooLarge = problem.Unprocessable("payables.amount_too_large",
	"The invoice's tax amounts, or their local amounts at the tax date's rate, are too large to post.")

// taxDateSet is whether the input sets a tax date: a new invoice's given
// one, or a change's that sets, changes or clears the invoice's.
func taxDateSet(in Input) bool {
	if !in.change {
		return in.TaxDate != nil
	}
	if in.TaxDate == nil || in.storedTaxDate == nil {
		return (in.TaxDate == nil) != (in.storedTaxDate == nil)
	}
	return !in.TaxDate.Equal(*in.storedTaxDate)
}

// invoiceTax is an invoice's tax as it posts (ADR-0049 §6, T61d), in the
// invoice's direction (positive on an invoice): what each item bears on
// its own line (a non-deductible component's tax, the part of a
// component's tax not recovered), the tax lines, one per code and
// component with an account, with their local amounts at the tax date's rate, and
// the tax items.
type invoiceTax struct {
	borne []money.Minor // per item
	lines []ledger.PostingLine
	items []ledger.PostingTaxItem
}

// codedItems are an invoice's items that carry a tax code and an amount:
// the lines the calculator taxes.
func codedItems(v Invoice) []int {
	var out []int
	for i, it := range v.Items {
		if it.TaxCode != nil && it.Amount != 0 {
			out = append(out, i)
		}
	}
	return out
}

// calculateTax calculates an invoice's tax per item code at the tax date
// (internal/tax), compares it with the supplier's stated tax within the
// company's tolerance (decision P5) and returns what posts: the supplier's
// amount, its difference shared out among the components posting to an
// account. An invoice whose items carry no code posts its stated tax as
// before, on the input tax account (invoiceLines).
func (e *Endpoints) calculateTax(ctx context.Context, v Invoice, taxDate time.Time) (invoiceTax, error) {
	coded := codedItems(v)
	out := invoiceTax{borne: make([]money.Minor, len(v.Items))}
	if len(coded) == 0 {
		return out, nil
	}
	le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored invoice's legal entity exists
	if err != nil {
		return invoiceTax{}, err
	}
	doc, conv, err := e.taxDocument(ctx, v, le.LocalCurrency, taxDate, coded)
	if err != nil {
		return invoiceTax{}, err
	}
	res, err := tax.Calculate(doc)
	if err != nil {
		return invoiceTax{}, calculationFailed(v.ID, err)
	}
	units, err := e.taxTolerance(ctx, le.Code)
	if err != nil {
		return invoiceTax{}, err
	}
	var costs []money.Minor
	if res.Items, costs, err = statedTax(v, res.Items, units, conv); err != nil {
		return invoiceTax{}, err
	}
	for j, i := range coded {
		for _, c := range res.Lines[j].Components {
			if c.AccountKey == tax.KeyNonDeductible || c.AccountKey == tax.KeyInput || c.AccountKey == tax.KeySelfInput {
				out.borne[i] += c.NonRecoverable // at most the tax on one line: in range
			}
		}
	}
	// A share of the difference that is a cost goes on its code's first
	// item.
	first := map[string]int{}
	for j := len(coded) - 1; j >= 0; j-- {
		first[*v.Items[coded[j]].TaxCode] = coded[j]
	}
	for k, c := range res.Items {
		if costs[k] != 0 {
			out.borne[first[c.Code]] += costs[k]
		}
	}
	return out, e.taxPostings(ctx, v, taxDate, res.Items, &out)
}

// calculationFailed is the calculator's error: amounts out of range are
// the invoice's (422); the rest, of codes the configuration validated, are
// not expected.
func calculationFailed(id uuid.UUID, err error) error {
	if errors.Is(err, money.ErrOverflow) {
		return errTaxTooLarge
	}
	return fmt.Errorf("payables: tax of invoice %s: %w", id, err)
}

// taxDocument is the calculator's document of an invoice's coded items:
// their codes' components and rates on the tax date, converted into local
// currency at the tax date's rate (ADR-0049 §8).
func (e *Endpoints) taxDocument(ctx context.Context, v Invoice, local string, taxDate time.Time, coded []int) (tax.Document, ledger.Conversion, error) {
	doc := tax.Document{TaxDate: taxDate, Currency: v.Currency, Codes: map[string]tax.Code{}}
	for _, i := range coded {
		code := *v.Items[i].TaxCode
		doc.Lines = append(doc.Lines, tax.Line{Amount: v.Items[i].Amount, Code: code})
		if _, ok := doc.Codes[code]; ok {
			continue
		}
		t, ok, err := e.Accounts.TaxCode(ctx, v.LegalEntityID, code, taxDate)
		if err != nil {
			return tax.Document{}, ledger.Conversion{}, err
		}
		if !ok || t.Direction != config.TaxInput {
			return tax.Document{}, ledger.Conversion{}, invalid("payables.unknown_tax_code", fmt.Sprintf("items[%d].tax_code", i),
				"Tax code "+code+" is not an input tax code of the legal entity on the tax date "+taxDate.Format(time.DateOnly)+".")
		}
		c := tax.Code{Code: code, Kind: t.Kind}
		for _, k := range t.Components {
			c.Components = append(c.Components, tax.Component{Key: k.Key, Jurisdiction: k.Jurisdiction, AccountKey: k.AccountKey,
				RecoverablePct: k.RecoverablePct, Rates: []tax.Rate{{Pct: k.RatePct}}})
		}
		doc.Codes[code] = c
	}
	conv, err := e.Rates.Conversion(ctx, v.Currency, local, taxDate)
	if err != nil {
		return tax.Document{}, ledger.Conversion{}, err
	}
	doc.Local = &tax.Conversion{Rate: conv.Rate, FromExp: conv.FromExp, ToExp: conv.ToExp}
	return doc, conv, nil
}

// taxTolerance is the company's tolerance in minor units per tax code
// (config.Tolerance.TaxUnits).
func (e *Endpoints) taxTolerance(ctx context.Context, company string) (int, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return 0, err
	}
	return toleranceOf(doc.Company(company)), nil
}

// toleranceOf is a company's tolerance; without the company in the
// configuration (one set up before its legal entity's), the default.
func toleranceOf(co *config.Company) int {
	if co == nil {
		return config.Tolerance{}.TaxUnits()
	}
	return co.InvoiceTolerance.TaxUnits()
}

// takers are the components a difference to the supplier's tax goes to,
// in their weights, and whether it goes on an account: those putting tax
// on one, an input component's recovered part (an invoice's codes are
// input codes, without output components); else, with none, those whose
// tax is a cost (a non-deductible component's, an input one's recovering
// none), the difference a cost on their items' lines. Self-assessed
// components the supplier does not charge take none.
func takers(items []tax.ComponentTax) ([]money.Minor, bool) {
	weights := make([]money.Minor, len(items))
	onAccount := false
	for i, c := range items {
		if c.AccountKey == tax.KeyInput && c.Recoverable != 0 {
			weights[i], onAccount = c.Recoverable, true
		}
	}
	if onAccount {
		return weights, true
	}
	for i, c := range items {
		if c.AccountKey == tax.KeyNonDeductible || c.AccountKey == tax.KeyInput {
			weights[i] = c.Tax
		}
	}
	return weights, false
}

// statedTax holds the supplier's stated tax to the calculated within the
// tolerance, units per code that charges tax (decision P5), and posts the
// supplier's: the difference is shared out among the components that take
// it (takers), in proportion to their weights' size (money.Allocate),
// with their local amounts converted at the tax date's rate. Beyond it,
// with no component to take it, or where it would leave a component
// recovering more than its tax or of the other sign, 422
// payables.tax_mismatch names each code's calculated tax. It returns the
// components and each one's share that is a cost on its items' lines.
func statedTax(v Invoice, items []tax.ComponentTax, units int, conv ledger.Conversion) ([]tax.ComponentTax, []money.Minor, error) {
	charged, codes := chargedTax(items)
	diff := v.Tax - charged // both in range
	if diff == 0 {
		return items, make([]money.Minor, len(items)), nil
	}
	weights, onAccount := takers(items)
	shares := shareOut(diff, weights)
	if diff.Abs() > money.Minor(units*codes) || shares == nil {
		return nil, nil, mismatch(v, items, charged)
	}
	out, ok, err := withShares(items, shares, onAccount, conv)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, mismatch(v, items, charged)
	}
	if onAccount {
		return out, make([]money.Minor, len(items)), nil
	}
	return out, shares, nil
}

// chargedTax is the tax the supplier charges by the calculation (the
// self-assessed aside) and the number of codes charging some.
func chargedTax(items []tax.ComponentTax) (money.Minor, int) {
	var charged money.Minor
	codes := map[string]bool{}
	for _, c := range items {
		if c.AccountKey == tax.KeySelfInput || c.AccountKey == tax.KeySelfOutput {
			continue
		}
		charged += c.Tax // the calculator's results are in range, and so their sum
		if c.Tax != 0 {
			codes[c.Code] = true
		}
	}
	return charged, len(codes)
}

// shareOut shares a difference out in proportion to the weights' size,
// largest remainder first (money.Allocate); nil when all weigh nothing.
func shareOut(diff money.Minor, weights []money.Minor) []money.Minor {
	total := new(big.Int)
	for _, w := range weights {
		total.Add(total, big.NewInt(int64(w.Abs())))
	}
	if total.Sign() == 0 {
		return nil
	}
	exact := make([]*big.Rat, len(weights))
	for i, w := range weights {
		exact[i] = new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(diff)), big.NewInt(int64(w.Abs()))), total)
	}
	shares, _ := money.Allocate(exact) // of a difference smaller than an amount: cannot fail
	return shares
}

// withShares adds each component's share to its tax, and to its recovered
// part when the difference goes on the account, its local amounts
// converted at the tax date's rate; not ok where that leaves a component
// out of its side (sameSide).
func withShares(items []tax.ComponentTax, shares []money.Minor, onAccount bool, conv ledger.Conversion) ([]tax.ComponentTax, bool, error) {
	out := slices.Clone(items)
	for i := range out {
		if shares[i] == 0 {
			continue
		}
		recovers := onAccount && out[i].AccountKey == tax.KeyInput
		out[i].Tax += shares[i]
		if recovers {
			out[i].Recoverable += shares[i]
		}
		if !sameSide(items[i].Tax, out[i].Tax, out[i].Recoverable) {
			return nil, false, nil
		}
		lc, err := conv.Convert(out[i].Tax)
		if err != nil {
			return nil, false, errTaxTooLarge // the conversion's only error: its rate and exponents are the calculator's
		}
		if recovers {
			out[i].Local.Recoverable += lc - out[i].Local.Tax
		}
		out[i].Local.Tax = lc
	}
	return out, true, nil
}

// sameSide is whether a component's tax and recovered part after the
// difference keep the calculated tax's sign (or are zero) and the
// recovered part is at most the tax.
func sameSide(was, tax, recoverable money.Minor) bool {
	side := func(m money.Minor) bool { return m == 0 || (m > 0) == (was > 0) }
	return side(tax) && side(recoverable) && recoverable.Abs() <= tax.Abs()
}

// mismatch refuses an invoice whose stated tax is beyond the tolerance
// (422 payables.tax_mismatch), naming each code's calculated tax.
func mismatch(v Invoice, items []tax.ComponentTax, charged money.Minor) error {
	byCode := map[string]money.Minor{}
	var codes []string
	for _, c := range items {
		if c.AccountKey == tax.KeySelfInput || c.AccountKey == tax.KeySelfOutput {
			continue
		}
		if _, ok := byCode[c.Code]; !ok {
			codes = append(codes, c.Code)
		}
		byCode[c.Code] += c.Tax
	}
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = c + " " + byCode[c].Plain(v.Exponent)
	}
	return problem.Unprocessable("payables.tax_mismatch", fmt.Sprintf("The invoice states tax of %s %s; the tax calculated at its codes' rates is %s (%s), "+
		"beyond the company's tolerance or with no component to take the difference.", v.Tax.Plain(v.Exponent), v.Currency, charged.Plain(v.Exponent), strings.Join(parts, ", ")),
		problem.FieldError{Field: "tax_amount", Code: "tax_mismatch", Message: "the tax the codes' rates give: " + charged.Plain(v.Exponent)})
}

// taxPostings adds an invoice's tax lines and items to out: per code and
// component posting to an account, its line on that account, with the
// recovered input tax, or the self-assessed output tax, and its local
// amount kept (ledger.PostingLine.LC); and every component's item.
func (e *Endpoints) taxPostings(ctx context.Context, v Invoice, taxDate time.Time, items []tax.ComponentTax, out *invoiceTax) error {
	accounts := map[[2]string]*uuid.UUID{}
	for _, c := range items {
		k := [2]string{c.Code, c.Key}
		if _, ok := accounts[k]; !ok {
			t, _, err := e.Accounts.TaxCode(ctx, v.LegalEntityID, c.Code, taxDate) // read for the calculation: found
			if err != nil {
				return err
			}
			for _, comp := range t.Components {
				accounts[[2]string{c.Code, comp.Key}] = comp.AccountID
			}
		}
		code := c.Code
		// What goes on the account: input tax recovered, output tax (owed,
		// so none recovered, ledger.tax_item_recoverable) whole.
		amount, lc := c.Recoverable, c.Local.Recoverable
		rec, recLC := c.Recoverable, c.Local.Recoverable
		if c.AccountKey == tax.KeyOutput || c.AccountKey == tax.KeySelfOutput {
			amount, lc, rec, recLC = c.Tax, c.Local.Tax, 0, 0
		}
		out.items = append(out.items, ledger.PostingTaxItem{Code: c.Code, Component: c.Key, Base: signed(v, c.Base), Tax: signed(v, c.Tax),
			Recoverable: signed(v, rec), BaseLC: signed(v, c.Local.Base), TaxLC: signed(v, c.Local.Tax), RecoverableLC: signed(v, recLC)})
		account := accounts[k]
		if account == nil || amount == 0 {
			continue
		}
		lc = signed(v, lc)
		out.lines = append(out.lines, ledger.PostingLine{AccountID: *account, Amount: signed(v, amount), TaxCode: &code, LC: &lc})
	}
	return nil
}
