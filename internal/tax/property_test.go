package tax_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// generated reads a document from bytes: a flags byte (inclusive, line
// rounding, local conversion), two rates and a recoverable share of up to
// three decimals, then lines of a four-byte amount, a shift that makes it
// large and a code: A (one component), B (two, the second partly
// recoverable) or C (a reverse charge pair).
func generated(data []byte) tax.Document {
	pad := make([]byte, 7)
	copy(pad, data)
	flags := pad[0]
	r1, r2 := fmt.Sprintf("%d.%03d", pad[1]%100, pad[2]), fmt.Sprintf("%d.%03d", pad[3]%30, pad[4])
	rec := fmt.Sprintf("%d.%03d", pad[5]%100, pad[6]%200)
	d := tax.Document{Inclusive: flags&1 != 0, Codes: map[string]tax.Code{
		"A": {Code: "A", Components: []tax.Component{comp("a", "", tax.KeyInput, "100", undated(r1))}},
		"B": {Code: "B", Components: []tax.Component{comp("b1", "", tax.KeyInput, "100", undated(r1)), comp("b2", "", tax.KeyNonDeductible, rec, undated(r2))}},
		"C": {Code: "C", Components: []tax.Component{comp("co", "", tax.KeySelfOutput, "100", undated(r2)), comp("ci", "", tax.KeySelfInput, rec, undated(r2))}},
	}}
	if flags&2 != 0 {
		d.Rounding = tax.RoundLine
	}
	if flags&4 != 0 {
		d.Local = &tax.Conversion{Rate: r1, FromExp: 2, ToExp: int(flags>>4) % 4}
	}
	for rest := data[min(len(data), 7):]; len(rest) >= 6; rest = rest[6:] {
		amount := money.Minor(int32(binary.LittleEndian.Uint32(rest))) << (rest[4] % 32) // below 2^62: never wraps
		d.Lines = append(d.Lines, tax.Line{Amount: amount, Code: string(rune('A' + rest[5]%3))})
	}
	return d
}

// exact is a line component's tax before rounding, as ADR-0049 §8 states
// it.
func exact(d tax.Document, line int, r tax.Result, c tax.ComponentTax) *big.Rat {
	rate := money.ParseRat(c.RatePct)
	if c.AccountKey == tax.KeySelfInput || c.AccountKey == tax.KeySelfOutput {
		x := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(r.Lines[line].Base)), rate)
		x.Quo(x, big.NewRat(100, 1))
		if c.AccountKey == tax.KeySelfOutput {
			x.Neg(x)
		}
		return x
	}
	divisor := big.NewRat(100, 1)
	if d.Inclusive {
		for _, o := range d.Codes[c.Code].Components {
			if o.AccountKey != tax.KeySelfInput && o.AccountKey != tax.KeySelfOutput {
				divisor.Add(divisor, money.ParseRat(o.Rates[0].Pct))
			}
		}
	}
	x := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(d.Lines[line].Amount)), rate)
	return x.Quo(x, divisor)
}

// within reports whether |a - b| < bound.
func within(a money.Minor, b *big.Rat, bound int64) bool {
	diff := new(big.Rat).Sub(new(big.Rat).SetInt64(int64(a)), b)
	return diff.Abs(diff).Cmp(big.NewRat(bound, 1)) < 0
}

func roundHalfAway(x *big.Rat) money.Minor {
	m, err := money.Round(x, 0)
	if err != nil {
		panic(err) // a result in range has an exact sum in range
	}
	return m
}

// checkInvariants checks a result against its document: lines add up to
// the items in both currencies; recoverable + non-recoverable = tax, the
// recoverable part of the tax's sign and not larger; each item's tax is
// its exact tax rounded once (document rounding) and each line's within
// one unit of its exact tax, or each line's rounded (line rounding); a
// self-assessed pair nets to zero; an inclusive line's base and charged
// tax add up to its gross.
func checkInvariants(t *testing.T, d tax.Document, r tax.Result) {
	t.Helper()
	sums := map[[2]string][2]tax.Amounts{}
	exacts := map[[2]string]*big.Rat{}
	for i, l := range r.Lines {
		var pair money.Minor
		for _, c := range l.Components {
			k := [2]string{c.Code, c.Key}
			s := sums[k]
			s[0], s[1] = add(s[0], c.Amounts), add(s[1], c.Local)
			sums[k] = s
			checkSplit(t, fmt.Sprintf("line %d %s", i+1, c.Key), c.Amounts)
			checkSplit(t, fmt.Sprintf("line %d %s local", i+1, c.Key), c.Local)
			x := exact(d, i, r, c)
			if exacts[k] == nil {
				exacts[k] = new(big.Rat)
			}
			exacts[k].Add(exacts[k], x)
			if d.Rounding == tax.RoundLine && c.Tax != roundHalfAway(x) {
				t.Errorf("line %d %s: got %d, want %s rounded", i+1, c.Key, c.Tax, x.RatString())
			}
			if !within(c.Tax, x, 1) {
				t.Errorf("line %d %s: got %d, want within 1 of %s", i+1, c.Key, c.Tax, x.RatString())
			}
			if c.AccountKey == tax.KeySelfInput || c.AccountKey == tax.KeySelfOutput {
				pair += c.Tax
			}
		}
		if pair != 0 {
			t.Errorf("line %d: self-assessed pair nets to %d, want 0", i+1, pair)
		}
		if charged, err := l.Charged(); d.Inclusive && (err != nil || l.Base+charged != d.Lines[i].Amount) {
			t.Errorf("line %d: base %d + charged %d (%v), want the gross %d", i+1, l.Base, charged, err, d.Lines[i].Amount)
		}
	}
	for _, it := range r.Items {
		k := [2]string{it.Code, it.Key}
		if got := sums[k]; got[0] != it.Amounts || got[1] != it.Local {
			t.Errorf("item %v: lines add up to %+v, want %+v", k, got, [2]tax.Amounts{it.Amounts, it.Local})
		}
		checkSplit(t, fmt.Sprintf("item %v", k), it.Amounts)
		checkSplit(t, fmt.Sprintf("item %v local", k), it.Local)
		if d.Local != nil {
			for name, v := range map[string][2]money.Minor{"base": {it.Base, it.Local.Base}, "tax": {it.Tax, it.Local.Tax}} {
				if want, err := money.Convert(v[0], d.Local.Rate, d.Local.FromExp, d.Local.ToExp); err == nil && v[1] != want {
					t.Errorf("item %v local %s: got %d, want %d converted once", k, name, v[1], want)
				}
			}
		}
		if d.Rounding != tax.RoundLine && it.Tax != roundHalfAway(exacts[k]) {
			t.Errorf("item %v: got %d, want %s rounded once", k, it.Tax, exacts[k].RatString())
		}
	}
}

func add(a, b tax.Amounts) tax.Amounts {
	return tax.Amounts{Base: a.Base + b.Base, Tax: a.Tax + b.Tax, Recoverable: a.Recoverable + b.Recoverable, NonRecoverable: a.NonRecoverable + b.NonRecoverable}
}

func checkSplit(t *testing.T, what string, a tax.Amounts) {
	t.Helper()
	if a.Recoverable+a.NonRecoverable != a.Tax {
		t.Errorf("%s: recoverable %d + non-recoverable %d, want the tax %d", what, a.Recoverable, a.NonRecoverable, a.Tax)
	}
	if a.Recoverable.Abs() > a.Tax.Abs() || (a.Recoverable != 0 && (a.Recoverable < 0) != (a.Tax < 0)) {
		t.Errorf("%s: recoverable %d, want of the tax's sign and at most %d", what, a.Recoverable, a.Tax)
	}
}

// checkRoundTrip calculates an exclusive document, then the inclusive one
// whose grosses are its bases plus the tax charged: the bases come back
// within the documented rounding, two units a line, and each item's tax
// exactly for a code of one charged component, within one unit otherwise.
func checkRoundTrip(t *testing.T, d tax.Document, r tax.Result) {
	t.Helper()
	inc := d
	inc.Inclusive, inc.Lines = true, make([]tax.Line, len(d.Lines))
	for i, l := range r.Lines {
		charged, err := l.Charged()
		if err != nil {
			t.Fatalf("line %d: charged: %v", i+1, err) // the charged tax was calculated in range
		}
		gross, err := money.Sum(l.Base, charged)
		if errors.Is(err, money.ErrOverflow) {
			return // a gross out of range
		}
		inc.Lines[i] = tax.Line{Amount: gross, Code: d.Lines[i].Code}
	}
	back, err := tax.Calculate(inc)
	if errors.Is(err, money.ErrOverflow) {
		return // the grosses' sum out of range
	}
	if err != nil {
		t.Fatalf("inclusive: %v", err)
	}
	lines := map[string]money.Minor{}
	for _, l := range d.Lines {
		lines[l.Code]++
	}
	for i, l := range back.Lines {
		if diff := l.Base - r.Lines[i].Base; diff.Abs() > 2 {
			t.Errorf("line %d: base %d back as %d", i+1, r.Lines[i].Base, l.Base)
		}
	}
	for i, it := range back.Items {
		charged := 0
		for _, c := range d.Codes[it.Code].Components {
			if c.AccountKey == tax.KeyInput || c.AccountKey == tax.KeyNonDeductible {
				charged++
			}
		}
		if diff := (it.Tax - r.Items[i].Tax).Abs(); (charged == 1 && d.Rounding != tax.RoundLine && diff != 0) || diff > lines[it.Code] {
			t.Errorf("item %s %s: tax %d back as %d", it.Code, it.Key, r.Items[i].Tax, it.Tax)
		}
	}
}

func checkGenerated(t *testing.T, data []byte) {
	t.Helper()
	d := generated(data)
	r, err := tax.Calculate(d)
	if err != nil {
		if !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("%x: %v", data, err)
		}
		return
	}
	checkInvariants(t, d, r)
	if !d.Inclusive {
		checkRoundTrip(t, d, r)
	}
}

// TestProperties runs the invariants on documents from a fixed seed.
func TestProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(61, 2)) //nolint:gosec // test data, not security
	for range 3000 {
		data := make([]byte, 7+6*rng.IntN(8))
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		checkGenerated(t, data)
		if t.Failed() {
			t.Fatalf("failed on %x", data)
		}
	}
}

// FuzzCalculate checks the invariants on any document (checkInvariants,
// checkRoundTrip); only amounts out of range may fail.
func FuzzCalculate(f *testing.F) {
	f.Add([]byte{0, 5, 0, 7, 0, 0, 0, 0x0d, 0x0d, 0, 0, 0, 0})
	f.Add([]byte{3, 9, 0x3d, 4, 0x75, 50, 0, 1, 0, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff, 39, 2})
	f.Add([]byte{0x24, 99, 0xff, 29, 0xff, 99, 199, 0x0a, 0, 0, 0, 0, 0, 0x0a, 0, 0, 0, 0, 0, 0x0a, 0, 0, 0, 0, 0})
	f.Fuzz(checkGenerated)
}
