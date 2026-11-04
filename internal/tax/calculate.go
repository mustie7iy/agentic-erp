package tax

import (
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

var hundred = big.NewRat(100, 1)

// Calculate is the built-in calculation of a document's tax (ADR-0049 §8,
// §9). Lines are grouped by code in order of first appearance; each
// code's components are rated on the tax date and rounded by the
// document's mode. A line's results are in its order; the items follow
// the codes' first lines, then the components' order.
func Calculate(doc Document) (Result, error) {
	conv, err := check(doc)
	if err != nil {
		return Result{}, err
	}
	res := Result{Lines: make([]LineTax, len(doc.Lines))}
	byCode := map[string][]int{}
	var codes []string
	for i, l := range doc.Lines {
		if _, ok := byCode[l.Code]; !ok {
			codes = append(codes, l.Code)
		}
		byCode[l.Code] = append(byCode[l.Code], i)
	}
	for _, name := range codes {
		lines := byCode[name]
		code, ok := doc.Codes[name]
		if !ok {
			return Result{}, fmt.Errorf("%w %q on line %d", ErrUnknownCode, name, lines[0]+1)
		}
		if code.Code != name { // the items would carry another code than the lines
			return Result{}, fmt.Errorf("%w %s: listed as %s", ErrInvalidCode, code.Code, name)
		}
		comps, err := rateOn(code, doc.TaxDate)
		if err != nil {
			return Result{}, err
		}
		g := group{doc: doc, code: code, comps: comps, lines: lines, conv: conv}
		items, err := g.calculate(res.Lines)
		if err != nil {
			return Result{}, fmt.Errorf("tax: code %s: %w", name, err)
		}
		res.Items = append(res.Items, items...)
	}
	return res, nil
}

// check checks the document's rounding mode, conversion and amounts, and
// returns the conversion's factor from transaction to local minor units,
// nil without one.
func check(doc Document) (*big.Rat, error) {
	if doc.Rounding != "" && doc.Rounding != RoundDocument && doc.Rounding != RoundLine {
		return nil, fmt.Errorf("%w: rounding %q is document or line", ErrInvalid, doc.Rounding)
	}
	for i, l := range doc.Lines {
		if l.Amount == math.MinInt64 { // not an amount (money.ErrOverflow): it has no negation
			return nil, fmt.Errorf("tax: line %d: %w", i+1, money.ErrOverflow)
		}
	}
	c := doc.Local
	if c == nil {
		return nil, nil
	}
	rate, err := money.ParsePlain(c.Rate)
	if err != nil {
		return nil, fmt.Errorf("%w: local currency rate: %w", ErrInvalid, err)
	}
	if c.FromExp < 0 || c.FromExp > maxExp || c.ToExp < 0 || c.ToExp > maxExp {
		return nil, fmt.Errorf("%w: local currency exponents %d and %d are 0 to %d", ErrInvalid, c.FromExp, c.ToExp, maxExp)
	}
	return rate.Mul(rate, new(big.Rat).Quo(money.Rat(1, -c.ToExp), money.Rat(1, -c.FromExp))), nil
}

// maxExp bounds currency exponents, as money does.
const maxExp = 18

// round rounds exact amounts in minor units by the mode: each half away
// from zero (RoundLine), or their sum once, allocated (money.Allocate).
// Each exact amount here is smaller than an amount, so a line's rounding
// cannot fail.
func round(exact []*big.Rat, mode Rounding) ([]money.Minor, error) {
	if mode == RoundLine {
		out := make([]money.Minor, len(exact))
		for i, x := range exact {
			out[i], _ = money.Round(x, 0) // cannot fail: smaller than an amount
		}
		return out, nil
	}
	return money.Allocate(exact)
}

// rated is a component with its rate on the tax date and its recoverable
// share, exactly.
type rated struct {
	Component
	pct         string
	rate        *big.Rat
	recoverable *big.Rat
}

// rateOn rates a code's components on the date: each needs one rate in
// force; a zero, exempt or out-of-scope code's are 0; recoverable shares
// are 0 to 100; and self-assessed components come as one self_output and
// one self_input at the same rate, so the pair nets to zero. Whether a
// code's kind has such a pair is the configuration's rule
// (config.CheckTaxCodes, ADR-0049 §3), not checked here.
func rateOn(code Code, day time.Time) ([]rated, error) {
	if len(code.Components) == 0 {
		return nil, fmt.Errorf("%w %s: no components", ErrInvalidCode, code.Code)
	}
	out := make([]rated, len(code.Components))
	selfRates := map[string][]*big.Rat{}
	for i, c := range code.Components {
		r, err := c.rateOn(code, day)
		if err != nil {
			return nil, err
		}
		if selfAssessed(c.AccountKey) {
			selfRates[c.AccountKey] = append(selfRates[c.AccountKey], r.rate)
		}
		out[i] = r
	}
	in, outs := selfRates[KeySelfInput], selfRates[KeySelfOutput]
	if len(in)+len(outs) > 0 && (len(in) != 1 || len(outs) != 1 || in[0].Cmp(outs[0]) != 0) {
		return nil, fmt.Errorf("%w %s: self-assessed components are one self_output and one self_input at the same rate", ErrInvalidCode, code.Code)
	}
	return out, nil
}

// rateOn is the component's rate in force on the day and its recoverable
// share, checked.
func (c Component) rateOn(code Code, day time.Time) (rated, error) {
	i := slices.IndexFunc(c.Rates, func(r Rate) bool {
		return !day.Before(r.From) && (r.Until.IsZero() || day.Before(r.Until))
	})
	if i < 0 {
		return rated{}, fmt.Errorf("%w: code %s component %s on %s", ErrNoRate, code.Code, c.Key, day.Format(time.DateOnly))
	}
	if !slices.Contains([]string{KeyInput, KeyOutput, KeyNonDeductible, KeySelfOutput, KeySelfInput}, c.AccountKey) {
		return rated{}, fmt.Errorf("%w %s: component %s: account key %q", ErrInvalidCode, code.Code, c.Key, c.AccountKey)
	}
	pct := c.Rates[i].Pct
	// Below 100%, as the ledger keeps rates: a line's tax is then smaller
	// than its amount, so whatever an amount holds its tax holds.
	rate, err := money.ParsePlain(pct)
	if err != nil || rate.Cmp(hundred) >= 0 {
		return rated{}, fmt.Errorf("%w %s: component %s: rate %q is a percentage from 0 to below 100", ErrInvalidCode, code.Code, c.Key, pct)
	}
	if rate.Sign() != 0 && slices.Contains([]string{KindZero, KindExempt, KindOutOfScope, KindAdjustment}, code.Kind) {
		return rated{}, fmt.Errorf("%w %s: a %s code has a rate of 0, component %s has %s", ErrInvalidCode, code.Code, code.Kind, c.Key, pct)
	}
	rec, err := money.ParsePlain(c.RecoverablePct)
	if err != nil || rec.Cmp(hundred) > 0 {
		return rated{}, fmt.Errorf("%w %s: component %s: recoverable share %q is 0 to 100", ErrInvalidCode, code.Code, c.Key, c.RecoverablePct)
	}
	return rated{Component: c, pct: pct, rate: rate, recoverable: rec}, nil
}

// group is one code's lines of a document.
type group struct {
	doc   Document
	code  Code
	comps []rated
	lines []int
	conv  *big.Rat // to the local currency, nil when not converted
}

// calculate works out the code's tax on its lines, fills them in lines
// and returns its items.
func (g group) calculate(lines []LineTax) ([]ComponentTax, error) {
	bases, taxes, err := g.taxes()
	if err != nil {
		return nil, err
	}
	items := make([]ComponentTax, len(g.comps))
	perLine := make([][]ComponentTax, len(g.lines))
	for k, c := range g.comps {
		amounts := g.split(c, bases, taxes[k])
		item := ComponentTax{Code: g.code.Code, Key: c.Key, Jurisdiction: c.Jurisdiction, AccountKey: c.AccountKey, RatePct: c.pct}
		if item.Amounts, err = total(amounts); err != nil {
			return nil, err
		}
		var local []Amounts
		if g.conv != nil {
			if local, item.Local, err = g.local(amounts); err != nil {
				return nil, err
			}
		}
		items[k] = item
		for j := range g.lines {
			line := item
			line.Amounts = amounts[j]
			if local != nil {
				line.Local = local[j]
			}
			perLine[j] = append(perLine[j], line)
		}
	}
	for j, i := range g.lines {
		lines[i] = LineTax{Base: bases[j], Components: perLine[j]}
	}
	return items, nil
}

// taxes are the lines' bases and each component's tax per line: the
// charged components' (charged), then the self-assessed pair's on the
// bases (selfAssess).
func (g group) taxes() ([]money.Minor, [][]money.Minor, error) {
	amounts := make([]money.Minor, len(g.lines))
	for j, i := range g.lines {
		amounts[j] = g.doc.Lines[i].Amount
	}
	taxes := make([][]money.Minor, len(g.comps))
	bases, err := g.charged(amounts, taxes)
	if err != nil {
		return nil, nil, err
	}
	if err := g.selfAssess(bases, taxes); err != nil {
		return nil, nil, err
	}
	return bases, taxes, nil
}

// charged fills in the charged components' tax and returns the bases. A
// charged component's exact tax is the base × rate / 100, or, on an
// inclusive document, the gross × rate / (100 + the charged rates), the
// base then being the gross less the tax charged.
func (g group) charged(amounts []money.Minor, taxes [][]money.Minor) ([]money.Minor, error) {
	divisor := new(big.Rat).Set(hundred)
	if g.doc.Inclusive {
		for _, c := range g.comps {
			if !selfAssessed(c.AccountKey) {
				divisor.Add(divisor, c.rate)
			}
		}
	}
	bases := slices.Clone(amounts)
	for k, c := range g.comps {
		if selfAssessed(c.AccountKey) {
			continue
		}
		var err error
		if taxes[k], err = round(exactTax(amounts, c.rate, divisor), g.doc.Rounding); err != nil {
			return nil, err
		}
		if g.doc.Inclusive {
			for j := range bases {
				bases[j] -= taxes[k][j] // the tax is of the gross's sign and within a unit per component of it: no wrap
			}
		}
	}
	return bases, nil
}

// selfAssess fills in the self-assessed pair's tax on the bases:
// self_input the tax at its rate, self_output its negation (rateOn checked
// the pair).
func (g group) selfAssess(bases []money.Minor, taxes [][]money.Minor) error {
	in := slices.IndexFunc(g.comps, func(c rated) bool { return c.AccountKey == KeySelfInput })
	if in < 0 {
		return nil
	}
	t, err := round(exactTax(bases, g.comps[in].rate, hundred), g.doc.Rounding)
	if err != nil {
		return err
	}
	out := slices.IndexFunc(g.comps, func(c rated) bool { return c.AccountKey == KeySelfOutput })
	taxes[in], taxes[out] = t, make([]money.Minor, len(t))
	for j, v := range t {
		taxes[out][j] = -v // smaller than the base in magnitude, so negatable
	}
	return nil
}

// exactTax is each amount × rate / divisor in minor units, unrounded.
func exactTax(amounts []money.Minor, rate, divisor *big.Rat) []*big.Rat {
	share := new(big.Rat).Quo(rate, divisor)
	out := make([]*big.Rat, len(amounts))
	for j, a := range amounts {
		out[j] = new(big.Rat).Mul(new(big.Rat).SetInt64(int64(a)), share)
	}
	return out
}

// split gives a component's tax per line its base and its recoverable
// part, the tax × the recoverable share rounded as the tax is; the rest is
// non-recoverable.
func (g group) split(c rated, bases, taxes []money.Minor) []Amounts {
	// Cannot fail: each part and their sum lie between 0 and the tax, which
	// was rounded in range.
	rec, _ := round(exactTax(taxes, c.recoverable, hundred), g.doc.Rounding)
	out := make([]Amounts, len(taxes))
	for j, t := range taxes {
		// The recoverable part lies between 0 and the tax: the difference
		// is in range.
		out[j] = Amounts{Base: bases[j], Tax: t, Recoverable: rec[j], NonRecoverable: t - rec[j]}
	}
	return out
}

// total adds up the lines' amounts.
func total(lines []Amounts) (Amounts, error) {
	cols := make([][]money.Minor, 4)
	for _, a := range lines {
		cols[0], cols[1], cols[2], cols[3] = append(cols[0], a.Base), append(cols[1], a.Tax), append(cols[2], a.Recoverable), append(cols[3], a.NonRecoverable)
	}
	var out Amounts
	var err error
	for i, dst := range []*money.Minor{&out.Base, &out.Tax, &out.Recoverable, &out.NonRecoverable} {
		if *dst, err = money.Sum(cols[i]...); err != nil {
			return Amounts{}, err
		}
	}
	return out, nil
}

// local converts a component's lines to the local currency (ADR-0049
// §8): the bases and the tax at the conversion factor, each rounded once
// for the item and allocated to the lines, so the lines add up to the
// item; each line's recoverable part is its share of the line's local tax,
// rounded as the tax is, so it stays of the tax's sign and no larger.
func (g group) local(lines []Amounts) ([]Amounts, Amounts, error) {
	n := len(lines)
	bases, taxes := make([]*big.Rat, n), make([]*big.Rat, n)
	for j, a := range lines {
		bases[j] = new(big.Rat).Mul(new(big.Rat).SetInt64(int64(a.Base)), g.conv)
		taxes[j] = new(big.Rat).Mul(new(big.Rat).SetInt64(int64(a.Tax)), g.conv)
	}
	localBases, err := money.Allocate(bases)
	if err != nil {
		return nil, Amounts{}, err
	}
	localTaxes, err := money.Allocate(taxes)
	if err != nil {
		return nil, Amounts{}, err
	}
	shares := make([]*big.Rat, n)
	for j, a := range lines {
		shares[j] = new(big.Rat)
		if a.Tax != 0 {
			shares[j].SetFrac(big.NewInt(int64(a.Recoverable)), big.NewInt(int64(a.Tax)))
			shares[j].Mul(shares[j], new(big.Rat).SetInt64(int64(localTaxes[j])))
		}
	}
	// Each share lies between 0 and its line's local tax, but lines of both
	// signs with different shares can add up out of range.
	recs, err := round(shares, g.doc.Rounding)
	if err != nil {
		return nil, Amounts{}, err
	}
	out := make([]Amounts, n)
	for j := range out {
		out[j] = Amounts{Base: localBases[j], Tax: localTaxes[j], Recoverable: recs[j], NonRecoverable: localTaxes[j] - recs[j]}
	}
	// Cannot fail: the bases and taxes add up to totals Allocate rounded in
	// range, the parts to no more than the tax.
	sum, _ := total(out)
	return out, sum, nil
}
