package payables

import (
	"math"
	"math/big"
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// inEUR is the conversion of a EUR invoice of a EUR company.
var inEUR = ledger.Conversion{From: "EUR", To: "EUR", Rate: "1", FromExp: 2, ToExp: 2}

// noLimits are the limits of a company the configuration does not have.
func noLimits(t *testing.T) limits {
	t.Helper()
	lim, err := companyLimits(nil)
	if err != nil {
		t.Fatal(err)
	}
	return lim
}

// The match flags amounts whose sums are out of range instead of comparing
// wrapped values. Before, items of 92233720368547758.07 twice and 0.02
// summed to zero and matched a zero gross amount; an order price whose
// extension was out of range was truncated, and an invoice item at the
// truncated amount passed without a variance.
func TestMatchOutOfRange(t *testing.T) {
	top := money.Minor(math.MaxInt64)
	one, two, three := int32(1), int32(2), int32(3)
	order := procurement.Order{Status: "released", Items: []procurement.OrderItem{
		{ItemNo: 1, Quantity: "2", UnitPrice: top, PriceUnit: 1, Received: "2"},
		{ItemNo: 2, Quantity: "1", UnitPrice: top, PriceUnit: 1, Received: "1"},
		{ItemNo: 3, Quantity: "1", UnitPrice: top / 2, PriceUnit: 1, Received: "1"},
		{ItemNo: 4, Quantity: "1", UnitPrice: top / 2, PriceUnit: 1, Received: "1"},
	}}
	four := int32(4)
	pct300 := noLimits(t)
	pct300.pricePercent = 300
	// 1 JPY is 1,000,000 EUR: a variance of top/2 yen is out of range in euro cents.
	huge := ledger.Conversion{From: "JPY", To: "EUR", Rate: "1000000", FromExp: 0, ToExp: 2}
	cases := map[string]struct {
		items      []Item
		gross, tax money.Minor
		lim        limits
		conv       ledger.Conversion
		want       string
	}{
		"items wrap to the gross amount": {[]Item{{ItemNo: 1, Amount: top}, {ItemNo: 2, Amount: top}, {ItemNo: 3, Amount: 2}}, 0, 0,
			noLimits(t), inEUR, IssueTotalMismatch},
		"items plus tax out of range": {[]Item{{ItemNo: 1, Amount: top}}, top, 1, noLimits(t), inEUR, IssueTotalMismatch},
		// 2 × (2^63-1) truncated to int64 is -2.
		"extended price out of range": {[]Item{{ItemNo: 1, POItemNo: &one, Quantity: "2", Amount: -2}}, -2, 0, noLimits(t), inEUR, IssuePriceVariance},
		"variance out of range":       {[]Item{{ItemNo: 1, POItemNo: &two, Quantity: "1", Amount: -2}}, -2, 0, noLimits(t), inEUR, IssuePriceVariance},
		"percentage out of range":     {[]Item{{ItemNo: 1, POItemNo: &three, Quantity: "1", Amount: 0}}, 0, 0, pct300, inEUR, IssuePriceVariance},
		"variance out of range in the company's currency": {[]Item{{ItemNo: 1, POItemNo: &four, Quantity: "1", Amount: 0}}, 0, 0,
			noLimits(t), huge, IssuePriceVariance},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			v := Invoice{Exponent: 2, Items: c.items, Gross: c.gross, Tax: c.tax}
			var codes []string
			for _, is := range evaluate(v, order, map[int32]string{}, c.lim, c.conv) {
				codes = append(codes, is.Code)
			}
			if !slices.Contains(codes, c.want) {
				t.Errorf("issues: got %v, want %s", codes, c.want)
			}
		})
	}
	// Items plus tax are summed exactly: items out of range alone, with a
	// credit tax bringing the total in range, match their gross amount.
	v := Invoice{Exponent: 2, Items: []Item{{ItemNo: 1, Amount: top}, {ItemNo: 2, Amount: 1}}, Tax: -1, Gross: top}
	if why := totalMismatch(v, money.Minor.String); why != "" {
		t.Errorf("exact total: got %q, want a match", why)
	}
}

// The variance message names both tolerances: the percentage in the
// invoice's currency and the absolute one in the company's.
func TestVarianceMessage(t *testing.T) {
	no := int32(1)
	lim, err := companyLimits(&config.Company{Code: "2000", InvoiceTolerance: config.Tolerance{PricePercent: 2, PriceAbsolute: " 5.50 "}, AutoPostLimit: "1.00"})
	if err != nil {
		t.Fatal(err)
	}
	jpy := ledger.Conversion{From: "JPY", To: "EUR", Rate: "0.0062", FromExp: 0, ToExp: 2}
	why := priceVariance(Item{ItemNo: 1, POItemNo: &no, Quantity: "10", Amount: 10900}, procurement.OrderItem{ItemNo: 1, UnitPrice: 1000, PriceUnit: 1},
		lim, jpy, func(m money.Minor) string { return m.Plain(0) })
	if want := "invoiced 10900 but the order price gives 10000 (variance 900, tolerance 200 or 5.50 EUR)"; why != want {
		t.Errorf("message: got %q, want %q", why, want)
	}
}

// A company's configured amounts that do not read as decimals are
// reported, never taken as zero.
func TestCompanyLimitsNotDecimals(t *testing.T) {
	for _, c := range []struct {
		name string
		co   config.Company
		want string
	}{
		{"tolerance", config.Company{Code: "2000", InvoiceTolerance: config.Tolerance{PriceAbsolute: "5,50"}, AutoPostLimit: "1"}, "invoice_tolerance.price_absolute"},
		{"limit", config.Company{Code: "2000", InvoiceTolerance: config.Tolerance{PriceAbsolute: "5.50"}, AutoPostLimit: ""}, "auto_post_limit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := companyLimits(&c.co)
			pgtest.WantErr(t, err, "payables: company 2000: "+c.want)
		})
	}
}

// near compares within 20% of the auto-post limit exactly, in the
// company's currency: a limit of 10,000,000,000,000,000.00 or the largest
// amount is near an invoice at it, without wrapping.
func TestNearTheAutoPostLimit(t *testing.T) {
	top := money.Minor(math.MaxInt64)
	limit := func(s string) limits {
		lim, err := companyLimits(&config.Company{Code: "2000", InvoiceTolerance: config.Tolerance{PriceAbsolute: "0"}, AutoPostLimit: s})
		if err != nil {
			t.Fatal(err)
		}
		return lim
	}
	cases := []struct {
		name  string
		limit string
		gross money.Minor
		want  bool
	}{
		{"at a large limit", "10000000000000000.00", 1_000_000_000_000_000_000, true},
		{"at the largest limit", "92233720368547758.07", top, true},
		{"below a large limit", "92233720368547758.07", top / 2, false},
		{"lower bound", "1000.00", 80000, true},
		{"below the lower bound", "1000.00", 79999, false},
		{"upper bound", "1000.00", 120000, true},
		{"above the upper bound", "1000.00", 120001, false},
		{"no limit", "0.00", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := near(replayed{Gross: tc.gross, Conversion: inEUR}, limit(tc.limit)); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	if near(replayed{Gross: 1, Conversion: inEUR}, noLimits(t)) {
		t.Error("without a company: got near, want not")
	}
	// Near is exactly l×8/10 ≤ gross ≤ l×12/10.
	for l := money.Minor(1); l <= 300; l++ {
		lim := limit(l.Plain(2))
		for gross := money.Minor(0); gross <= 400; gross++ {
			if got, want := near(replayed{Gross: gross, Conversion: inEUR}, lim), gross*10 >= l*8 && gross*10 <= l*12; got != want {
				t.Fatalf("limit %d, gross %d: got %v, want %v", l, gross, got, want)
			}
		}
	}
}

// The auto-post limit is compared with the gross amount in the company's
// currency, exactly: 50,000.00 EUR takes 8,064,516 JPY (49,999.9992 EUR,
// 50,000.00 rounded) and not 8,064,517.
func TestDecideAtTheLimit(t *testing.T) {
	lim := noLimits(t)
	lim.autoPost = big.NewRat(50000, 1)
	jpy := ledger.Conversion{From: "JPY", To: "EUR", Rate: "0.0062", FromExp: 0, ToExp: 2}
	for gross, want := range map[money.Minor]string{8064516: DecisionAutoPost, 8064517: DecisionReview} {
		t.Run(gross.String(), func(t *testing.T) {
			eur, err := jpy.Convert(gross)
			if err != nil {
				t.Fatal(err)
			}
			r := replayed{Invoice: Invoice{Exponent: 0, Gross: gross, Items: []Item{{ItemNo: 1, Amount: gross}}}, Order: procurement.Order{Status: "released"},
				Conversion: jpy, Gross: eur}
			if got, issues := decide(lim, r); got.Outcome != want {
				t.Errorf("decision: got %s (%v), want %s", got, issues, want)
			}
		})
	}
}
