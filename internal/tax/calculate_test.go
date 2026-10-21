package tax_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

func day(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err) // test literals only
	}
	return d
}

// undated is one rate in force on every date.
func undated(pct string) []tax.Rate { return []tax.Rate{{Pct: pct}} }

func comp(key, jurisdiction, accountKey, recoverable string, rates []tax.Rate) tax.Component {
	return tax.Component{Key: key, Jurisdiction: jurisdiction, AccountKey: accountKey, RecoverablePct: recoverable, Rates: rates}
}

// The worked codes: Canada (GST+QST, NS HST 15% then 14% from 2025-04-01,
// BC GST + PST non-deductible, ON HST, meals at 50%) and the UAE (5%
// standard, zero-rated, exempt, reverse charge fully and not recoverable).
var codes = map[string]tax.Code{
	"IQ": {Code: "IQ", Kind: "standard", Components: []tax.Component{
		comp("gst", "CA", tax.KeyInput, "100", undated("5")), comp("qst", "CA-QC", tax.KeyInput, "100", undated("9.975"))}},
	"HN": {Code: "HN", Kind: "standard", Components: []tax.Component{comp("hst", "CA-NS", tax.KeyInput, "100", []tax.Rate{
		{From: day("2010-07-01"), Until: day("2025-04-01"), Pct: "15"}, {From: day("2025-04-01"), Pct: "14"}})}},
	"I5": {Code: "I5", Kind: "standard", Components: []tax.Component{
		comp("gst", "CA", tax.KeyInput, "100", undated("5")), comp("pst_bc", "CA-BC", tax.KeyNonDeductible, "0", undated("7"))}},
	"HO": {Code: "HO", Kind: "standard", Components: []tax.Component{comp("hst", "CA-ON", tax.KeyInput, "100", undated("13"))}},
	"IM": {Code: "IM", Kind: "standard", Components: []tax.Component{comp("gst", "CA", tax.KeyInput, "50", undated("5"))}},
	"S5": {Code: "S5", Kind: "standard", Components: []tax.Component{comp("vat", "AE", tax.KeyOutput, "100", undated("5"))}},
	"V5": {Code: "V5", Kind: "standard", Components: []tax.Component{comp("vat", "AE", tax.KeyInput, "100", undated("5"))}},
	"Z0": {Code: "Z0", Kind: "zero", Components: []tax.Component{comp("vat", "AE", tax.KeyOutput, "100", undated("0"))}},
	"E0": {Code: "E0", Kind: "exempt", Components: []tax.Component{comp("vat", "AE", tax.KeyOutput, "100", undated("0"))}},
	"RC": {Code: "RC", Kind: "reverse_charge", Components: []tax.Component{
		comp("vat_out", "AE", tax.KeySelfOutput, "100", undated("5")), comp("vat_in", "AE", tax.KeySelfInput, "100", undated("5"))}},
	"RB": {Code: "RB", Kind: "reverse_charge", Components: []tax.Component{
		comp("vat_out", "AE", tax.KeySelfOutput, "100", undated("5")), comp("vat_in", "AE", tax.KeySelfInput, "0", undated("5"))}},
}

func doc(date string, amounts ...any) tax.Document {
	d := tax.Document{TaxDate: day(date), Currency: "CAD", Codes: codes}
	for i := 0; i < len(amounts); i += 2 {
		d.Lines = append(d.Lines, tax.Line{Amount: money.Minor(amounts[i].(int)), Code: amounts[i+1].(string)})
	}
	return d
}

// items renders a result's items as "code key rate: base tax rec/nonrec".
func items(r tax.Result) string {
	var out []string
	for _, it := range r.Items {
		out = append(out, fmt.Sprintf("%s %s %s: %d %d %d/%d", it.Code, it.Key, it.RatePct, it.Base, it.Tax, it.Recoverable, it.NonRecoverable))
	}
	return strings.Join(out, "; ")
}

// lineTax renders each line's base and its components' tax.
func lineTax(r tax.Result) string {
	var out []string
	for _, l := range r.Lines {
		var parts []string
		for _, c := range l.Components {
			parts = append(parts, fmt.Sprint(int64(c.Tax)))
		}
		out = append(out, fmt.Sprintf("%d[%s]", l.Base, strings.Join(parts, " ")))
	}
	return strings.Join(out, " ")
}

func TestWorkedExamples(t *testing.T) {
	inclusive := func(d tax.Document) tax.Document { d.Inclusive = true; return d }
	perLine := func(d tax.Document) tax.Document { d.Rounding = tax.RoundLine; return d }
	for _, c := range []struct {
		name        string
		doc         tax.Document
		items, line string
	}{
		{"CA-QC GST 5% + QST 9.975% on 100.00", doc("2026-03-01", 10000, "IQ"),
			"IQ gst 5: 10000 500 500/0; IQ qst 9.975: 10000 998 998/0", "10000[500 998]"},
		{"CA-QC three lines allocated by largest remainder", doc("2026-03-01", 3333, "IQ", 3333, "IQ", 3334, "IQ"),
			"IQ gst 5: 10000 500 500/0; IQ qst 9.975: 10000 998 998/0", "3333[167 333] 3333[166 332] 3334[167 333]"},
		{"NS HST 15% before 2025-04-01", doc("2025-03-31", 100000, "HN"), "HN hst 15: 100000 15000 15000/0", "100000[15000]"},
		{"NS HST 14% from 2025-04-01", doc("2025-04-01", 100000, "HN"), "HN hst 14: 100000 14000 14000/0", "100000[14000]"},
		{"BC GST 5% + PST 7% non-deductible", doc("2026-03-01", 20000, "I5"),
			"I5 gst 5: 20000 1000 1000/0; I5 pst_bc 7: 20000 1400 0/1400", "20000[1000 1400]"},
		{"ON HST 13% inclusive down payment of 1130.00", inclusive(doc("2026-03-01", 113000, "HO")),
			"HO hst 13: 100000 13000 13000/0", "100000[13000]"},
		{"BC inclusive gross of 112.00", inclusive(doc("2026-03-01", 11200, "I5")),
			"I5 gst 5: 10000 500 500/0; I5 pst_bc 7: 10000 700 0/700", "10000[500 700]"},
		{"meals: GST 5% half recoverable", doc("2026-03-01", 10001, "IM"), "IM gst 5: 10001 500 250/250", "10001[500]"},
		{"document rounding: three lines of 0.10 at 13%", doc("2026-03-01", 10, "HO", 10, "HO", 10, "HO"), // 1.3 each: 3.9 -> 4
			"HO hst 13: 30 4 4/0", "10[2] 10[1] 10[1]"},
		{"line rounding: three lines of 0.10 at 13%", perLine(doc("2026-03-01", 10, "HO", 10, "HO", 10, "HO")),
			"HO hst 13: 30 3 3/0", "10[1] 10[1] 10[1]"},
		{"a credit memo is the negated invoice", doc("2026-03-01", -3333, "IQ", -3333, "IQ", -3334, "IQ"),
			"IQ gst 5: -10000 -500 -500/0; IQ qst 9.975: -10000 -998 -998/0", "-3333[-167 -333] -3333[-166 -332] -3334[-167 -333]"},
		{"a line and its cancellation: the first takes the remainder", doc("2026-03-01", 15, "HO", -15, "HO"),
			"HO hst 13: 0 0 0/0", "15[2] -15[-2]"},
		{"the same negated", doc("2026-03-01", -15, "HO", 15, "HO"), "HO hst 13: 0 0 0/0", "-15[-2] 15[2]"},
		{"AE 5% standard output on 1234.57", doc("2026-03-01", 123457, "S5"), "S5 vat 5: 123457 6173 6173/0", "123457[6173]"},
		{"AE zero-rated export and exempt supply", doc("2026-03-01", 500000, "Z0", 70000, "E0"),
			"Z0 vat 0: 500000 0 0/0; E0 vat 0: 70000 0 0/0", "500000[0] 70000[0]"},
		{"AE reverse charge on imported services, recoverable", doc("2026-03-01", 1000000, "RC"),
			"RC vat_out 5: 1000000 -50000 -50000/0; RC vat_in 5: 1000000 50000 50000/0", "1000000[-50000 50000]"},
		{"AE reverse charge, blocked input", doc("2026-03-01", 1000000, "RB"),
			"RB vat_out 5: 1000000 -50000 -50000/0; RB vat_in 5: 1000000 50000 0/50000", "1000000[-50000 50000]"},
		{"AE reverse charge on an inclusive document charges nothing", inclusive(doc("2026-03-01", 1000000, "RC")),
			"RC vat_out 5: 1000000 -50000 -50000/0; RC vat_in 5: 1000000 50000 50000/0", "1000000[-50000 50000]"},
		{"mixed codes, items in order of first line", doc("2026-03-01", 1000, "V5", 2000, "Z0", 3000, "V5"),
			"V5 vat 5: 4000 200 200/0; Z0 vat 0: 2000 0 0/0", "1000[50] 2000[0] 3000[150]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := tax.BuiltIn{}.Calculate(context.Background(), c.doc)
			if err != nil {
				t.Fatal(err)
			}
			if got := items(r); got != c.items {
				t.Errorf("items: got %s, want %s", got, c.items)
			}
			if got := lineTax(r); got != c.line {
				t.Errorf("lines: got %s, want %s", got, c.line)
			}
			checkInvariants(t, c.doc, r)
		})
	}
}

// TestLocalCurrency: a USD invoice in the UAE, converted at 3.6725: each
// item converted once (50.00 USD of VAT is 183.63 AED, 183.625 rounded
// away from zero), the lines adding up to it.
func TestLocalCurrency(t *testing.T) {
	d := doc("2026-03-01", 33333, "V5", 33333, "V5", 33334, "V5")
	d.Currency, d.Local = "USD", &tax.Conversion{Rate: "3.6725", FromExp: 2, ToExp: 2}
	r, err := tax.Calculate(d)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Items[0].Local, (tax.Amounts{Base: 367250, Tax: 18363, Recoverable: 18363}); got != want {
		t.Errorf("item: got %+v, want %+v", got, want)
	}
	var tx, base money.Minor
	for _, l := range r.Lines {
		tx += l.Components[0].Local.Tax
		base += l.Components[0].Local.Base
	}
	if tx != 18363 {
		t.Errorf("lines' tax: got %d, want 18363", tx)
	}
	if base != 367250 {
		t.Errorf("lines' base: got %d, want 367250", base)
	}
	half := doc("2026-03-01", 10001, "IM")
	half.Local = &tax.Conversion{Rate: "1.35", FromExp: 2, ToExp: 2}
	r, err = tax.Calculate(half)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Items[0].Local, (tax.Amounts{Base: 13501, Tax: 675, Recoverable: 338, NonRecoverable: 337}); got != want {
		t.Errorf("half recoverable: got %+v, want %+v", got, want)
	}
	checkInvariants(t, half, r)
	// Two lines whose local tax rounds to 1 on the first and 0 on the
	// second: the second's recoverable part follows its local tax, 0.
	small := doc("2026-03-01", 20, "IM", 20, "IM")
	small.Local = &tax.Conversion{Rate: "0.5", FromExp: 2, ToExp: 2}
	if r, err = tax.Calculate(small); err != nil {
		t.Fatal(err)
	}
	for i, want := range []tax.Amounts{{Base: 10, Tax: 1, Recoverable: 1}, {Base: 10}} {
		if got := r.Lines[i].Components[0].Local; got != want {
			t.Errorf("small line %d: got %+v, want %+v", i+1, got, want)
		}
	}
	checkInvariants(t, small, r)
}

func TestCharged(t *testing.T) {
	r, err := tax.Calculate(doc("2026-03-01", 20000, "I5", 1000000, "RC"))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []money.Minor{2400, 0} {
		got, err := r.Lines[i].Charged()
		if err != nil || got != want {
			t.Errorf("line %d: got %d (%v), want %d", i+1, got, err, want)
		}
	}
	big := tax.LineTax{Components: []tax.ComponentTax{{Amounts: tax.Amounts{Tax: 1 << 62}}, {Amounts: tax.Amounts{Tax: 1 << 62}}}}
	if _, err := big.Charged(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("overflow: got %v, want money.ErrOverflow", err)
	}
}

func TestRefusals(t *testing.T) {
	bad := func(c tax.Code) map[string]tax.Code { return map[string]tax.Code{c.Code: c} }
	one := func(cs ...tax.Component) tax.Code { return tax.Code{Code: "X", Kind: "standard", Components: cs} }
	vat := func(rates []tax.Rate) tax.Component { return comp("vat", "", tax.KeyInput, "100", rates) }
	big := money.Minor(1 << 62)
	for _, c := range []struct {
		name  string
		doc   tax.Document
		want  error
		codes map[string]tax.Code
	}{
		{"unknown code", tax.Document{Lines: []tax.Line{{Amount: 1, Code: "NO"}}}, tax.ErrUnknownCode, nil},
		{"rounding", tax.Document{Rounding: "bank"}, tax.ErrInvalid, nil},
		{"local rate", tax.Document{Local: &tax.Conversion{Rate: "-1"}}, tax.ErrInvalid, nil},
		{"no components", tax.Document{}, tax.ErrInvalidCode, bad(one())},
		{"no rate on the date", tax.Document{TaxDate: day("2024-01-01")}, tax.ErrNoRate,
			bad(one(vat([]tax.Rate{{From: day("2025-01-01"), Pct: "5"}})))},
		{"rate ended", tax.Document{TaxDate: day("2025-01-01")}, tax.ErrNoRate,
			bad(one(vat([]tax.Rate{{Until: day("2025-01-01"), Pct: "5"}})))},
		{"rate not a decimal", tax.Document{}, tax.ErrInvalidCode, bad(one(vat(undated("5%"))))},
		{"zero code with a rate", tax.Document{}, tax.ErrInvalidCode,
			bad(tax.Code{Code: "X", Kind: "zero", Components: []tax.Component{vat(undated("5"))}})},
		{"recoverable above 100", tax.Document{}, tax.ErrInvalidCode, bad(one(comp("vat", "", tax.KeyInput, "100.5", undated("5"))))},
		{"recoverable missing", tax.Document{}, tax.ErrInvalidCode, bad(one(comp("vat", "", tax.KeyInput, "", undated("5"))))},
		{"self_output alone", tax.Document{}, tax.ErrInvalidCode, bad(one(comp("o", "", tax.KeySelfOutput, "100", undated("5"))))},
		{"self pair at different rates", tax.Document{}, tax.ErrInvalidCode, bad(one(comp("o", "", tax.KeySelfOutput, "100", undated("5")),
			comp("i", "", tax.KeySelfInput, "100", undated("6"))))},
		{"document tax out of range", tax.Document{Lines: []tax.Line{{Amount: big, Code: "X"}, {Amount: big, Code: "X"}, {Amount: big, Code: "X"}}}, money.ErrOverflow,
			bad(one(vat(undated("99"))))},
		{"rate of 100%", tax.Document{}, tax.ErrInvalidCode, bad(one(vat(undated("100"))))},
		{"line tax total out of range", tax.Document{Rounding: tax.RoundLine, Lines: []tax.Line{{Amount: big, Code: "X"}, {Amount: big, Code: "X"}}},
			money.ErrOverflow, bad(one(vat(undated("99"))))},
		{"self-assessed tax out of range", tax.Document{Lines: []tax.Line{{Amount: big, Code: "X"}, {Amount: big, Code: "X"}, {Amount: big, Code: "X"}}}, money.ErrOverflow,
			bad(one(comp("o", "", tax.KeySelfOutput, "100", undated("99")), comp("i", "", tax.KeySelfInput, "100", undated("99"))))},
		{"base total out of range", tax.Document{Lines: []tax.Line{{Amount: big, Code: "X"}, {Amount: big, Code: "X"}}}, money.ErrOverflow,
			bad(one(vat(undated("0"))))},
		{"local amount out of range", tax.Document{Local: &tax.Conversion{Rate: "4", FromExp: 2, ToExp: 2},
			Lines: []tax.Line{{Amount: big, Code: "X"}}}, money.ErrOverflow, bad(one(vat(undated("0"))))},
		{"local exponent", tax.Document{Local: &tax.Conversion{Rate: "1", FromExp: 2, ToExp: 99}}, tax.ErrInvalid, bad(one(vat(undated("0"))))},
		{"negative local exponent", tax.Document{Local: &tax.Conversion{Rate: "1", FromExp: -1, ToExp: 2}}, tax.ErrInvalid, bad(one(vat(undated("0"))))},
		{"local tax out of range", tax.Document{Local: &tax.Conversion{Rate: "4", FromExp: 2, ToExp: 2},
			Lines: []tax.Line{{Amount: big, Code: "X"}}}, money.ErrOverflow, bad(one(vat(undated("50"))))},
		{"not an amount", tax.Document{Lines: []tax.Line{{Amount: math.MinInt64, Code: "X"}}}, money.ErrOverflow, bad(one(vat(undated("5"))))},
		{"code listed under another", tax.Document{}, tax.ErrInvalidCode, map[string]tax.Code{"X": {Code: "Y", Components: []tax.Component{vat(undated("5"))}}}},
		{"unknown account key", tax.Document{}, tax.ErrInvalidCode, bad(one(comp("vat", "", "elsewhere", "100", undated("5"))))},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.doc.Codes = c.codes
			for i := range c.doc.Lines {
				if c.doc.Lines[i].Code == "" {
					c.doc.Lines[i].Code = "X"
				}
			}
			if len(c.doc.Lines) == 0 && c.codes != nil {
				c.doc.Lines = []tax.Line{{Amount: 100, Code: "X"}}
			}
			_, err := tax.Calculate(c.doc)
			if !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}
