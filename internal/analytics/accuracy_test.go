package analytics

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// accuracyCells are hand-checked cells of company 3000: at lag 7 in CAD
// on 16 April supplier payments forecast out and received in (mixed
// signs) and customer receipts short, on 17 April rent paid with nothing
// forecast (a cell of no forecast) and taxes forecast and not paid; in USD
// a payment of nothing forecast; at lag 0 bank charges whose variance is
// 12.345% of the forecast, a half.
func accuracyCells() []AccuracyCell {
	day := func(d int) time.Time { return time.Date(2026, 4, d, 0, 0, 0, 0, time.UTC) }
	cell := func(d int, lag int32, cur, item string, actual, forecast money.Minor) AccuracyCell {
		return AccuracyCell{ID: uuid.New(), ValueDate: day(d), LagDays: lag, Currency: cur, Exponent: 2, Item: item, ItemName: strings.ToLower(item),
			Actual: actual, Forecast: forecast, Variance: actual - forecast}
	}
	return []AccuracyCell{
		cell(16, 7, "CAD", "OP_SUPPLIERS", 5000, -3000), cell(16, 7, "CAD", "REC_CUSTOMERS", 30000, 25000),
		cell(17, 7, "CAD", "OP_RENT", -10000, 0), cell(17, 7, "CAD", "TAX", 0, -20000), cell(17, 7, "USD", "OP_SUPPLIERS", -10, 0),
		cell(16, 0, "CAD", "BANK_CHARGES", -112345, -100000),
	}
}

// accuracyFacts are the facts of accuracyCells as the database path builds
// them.
func accuracyFacts(t *testing.T) *Facts {
	t.Helper()
	m, _ := DefaultModel()
	le := uuid.New()
	f, err := Source{API: fakeAPI(t, cashPages(le)), Datasets: []string{"cash_accuracy"}, Cash: &fakeCash{cells: accuracyCells()}}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestAccuracyFigures: the variance and accuracy metrics by hand (product
// owner 2026-11-04): accuracy is 1 less WAPE, Σ|actual - forecast| /
// Σ|forecast| over the cells, each cell's size taken before the cells are
// added, the quotient rounded to four digits (two of a percentage) half
// away from zero before it is subtracted; none where nothing was forecast;
// below zero where the variance outgrows the forecast. Two currencies are
// never added up.
func TestAccuracyFigures(t *testing.T) {
	m, _ := DefaultModel()
	f := accuracyFacts(t)
	cad := []Filter{{Dim: "currency", Op: "in", Values: []string{"CAD"}}}
	for name, c := range map[string]struct {
		q    Query
		want string
	}{
		// |8000| + |5000| + |-10000| + |20000| = 43000 over 3000 + 25000 + 0 + 20000 = 48000: 0.895833 rounds to 0.8958.
		"lag 7 in CAD": {Query{Measures: []string{"variance_lag_7", "absolute_variance_lag_7", "absolute_forecast_lag_7", "accuracy_lag_7"}, Where: cad},
			"[[230.00 430.00 480.00 0.1042]]"},
		// 13000 / 28000 = 0.464286 and 30000 / 20000 = 1.5.
		"lag 7 in CAD by day": {Query{Measures: []string{"accuracy_lag_7"}, By: []string{"value_date"}, Where: cad},
			"[[2026-04-16 0.5357] [2026-04-17 -0.5000]]"},
		// Rent was not forecast: no accuracy, as the variance report shows no percent.
		"lag 7 by item": {Query{Measures: []string{"variance_lag_7", "accuracy_lag_7"}, By: []string{"currency", "liquidity_item"}},
			"[[CAD OP_RENT -100.00 <nil>] [CAD OP_SUPPLIERS 80.00 -1.6667] [CAD REC_CUSTOMERS 50.00 0.8000] [CAD TAX 200.00 0.0000] [USD OP_SUPPLIERS -0.10 <nil>]]"},
		// 12345 / 100000 is 0.12345, 12.35%: 87.65%, not 0.87655 rounded to 0.8766.
		"lag 0 on a half": {Query{Measures: []string{"accuracy_lag_0", "variance_lag_0"}, By: []string{"liquidity_item_name"}},
			"[[bank_charges 0.8765 -123.45]]"},
		"lag 30 has no cell": {Query{Measures: []string{"accuracy_lag_30"}}, "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Execute(m, f, Principal{}, c.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(res.Rows); got != c.want {
				t.Errorf("rows: got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("two currencies", func(t *testing.T) {
		_, err := Execute(m, f, Principal{}, Query{Measures: []string{"accuracy_lag_7"}})
		if err == nil || !strings.Contains(err.Error(), "would add amounts in CAD and USD together") {
			t.Errorf("got %v, want amounts of two currencies refused", err)
		}
	})
}

// TestAccuracyOfOneCell: for every cell forecast, its accuracy is 100 less
// the variance report's variance_percent of it (money.AbsPercentOf), and
// its variance the actual less the forecast (product owner 2026-11-04).
func TestAccuracyOfOneCell(t *testing.T) {
	m, _ := DefaultModel()
	f := accuracyFacts(t)
	for _, c := range accuracyCells() {
		if c.Forecast == 0 {
			continue
		}
		t.Run(c.Item+" "+c.ValueDate.Format(time.DateOnly), func(t *testing.T) {
			lag := fmt.Sprint(c.LagDays)
			res, err := Execute(m, f, Principal{}, Query{Measures: []string{"accuracy_lag_" + lag, "variance_lag_" + lag}, Where: []Filter{
				{Dim: "value_date", Op: "in", Values: []string{c.ValueDate.Format(time.DateOnly)}}, {Dim: "liquidity_item", Op: "in", Values: []string{c.Item}},
				{Dim: "currency", Op: "in", Values: []string{c.Currency}}}})
			if err != nil || len(res.Rows) != 1 {
				t.Fatalf("got %v, %v; want one row", res, err)
			}
			pct, _ := new(big.Rat).SetString(money.AbsPercentOf(c.Variance, c.Forecast))
			want, _ := money.Round(new(big.Rat).Quo(new(big.Rat).Sub(big.NewRat(100, 1), pct), big.NewRat(100, 1)), ratioScale)
			if got := res.Rows[0][0]; got != (Value{Units: want, Scale: ratioScale}) {
				t.Errorf("accuracy: got %v, want %s", got, Value{Units: want, Scale: ratioScale})
			}
			if got := res.Rows[0][1]; got != (Value{Units: c.Variance, Scale: 2, Currency: c.Currency}) {
				t.Errorf("variance: got %v, want %d minor", got, c.Variance)
			}
		})
	}
}
