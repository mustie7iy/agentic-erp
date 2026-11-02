package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// TestKeepLocalOutOfRange: a kept local amount whose group amount, of
// lines that all keep theirs or of one that does, or the lines' local or
// group total, is out of range is refused with 422
// ledger.amount_too_large, before any account is read.
func TestKeepLocalOutOfRange(t *testing.T) {
	huge := money.Minor(math.MaxInt64 / 2)
	for name, c := range map[string]struct {
		lines  []Line
		rateGC exchange
	}{
		"group amount":                {[]Line{{keptLC: &huge}}, exchange{exact: big.NewRat(3, 1), shown: "3"}},
		"group amount of a kept line": {[]Line{{keptLC: &huge}, {}}, exchange{exact: big.NewRat(3, 1), shown: "3"}},
		"local total":                 {[]Line{{keptLC: &huge}, {keptLC: &huge}, {LC: huge}}, one},
		"group total":                 {[]Line{{keptLC: &huge}, {LC: -huge, GC: huge}, {GC: huge}}, one},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := keepLocal(context.Background(), uuid.Nil, c.lines, c.rateGC, 2, 2, nil)
			if got := problem.From(err); got == nil || got.Code != "ledger.amount_too_large" {
				t.Errorf("got %v, want ledger.amount_too_large", err)
			}
		})
	}
}

// TestGRIRKeptLocalOutOfRange: a GR/IR posting's kept local amount whose
// group amount is out of range is refused before anything is read.
func TestGRIRKeptLocalOutOfRange(t *testing.T) {
	huge := money.Minor(math.MaxInt64 / 2)
	g := grirPosting{in: Posting{Lines: []PostingLine{{Amount: 1, LC: &huge}}},
		rates: rates{toLC: one, toGC: exchange{exact: big.NewRat(3, 1), shown: "3"}, expTC: 2, expLC: 2, expGC: 2}}
	_, err := g.lines(context.Background(), nil)
	if got := problem.From(err); got == nil || got.Code != "ledger.amount_too_large" {
		t.Errorf("got %v, want ledger.amount_too_large", err)
	}
}

// TestTaxBaseOutOfRange: a declared base whose local amount is out of
// range is refused with 422 ledger.amount_too_large.
func TestTaxBaseOutOfRange(t *testing.T) {
	huge := money.Minor(math.MaxInt64 / 2)
	err := convertLines([]Line{{TC: 1, TaxBase: &huge}, {TC: -1}}, exchange{exact: big.NewRat(3, 1), shown: "3"}, one, 2, 2, 2)
	if got := problem.From(err); got == nil || got.Code != "ledger.amount_too_large" {
		t.Errorf("got %v, want ledger.amount_too_large", err)
	}
}

// TestOnAccountInvalidCode: a code the calculator cannot rate on the date
// is refused with 422 ledger.invalid_tax_code naming it; the calculator's
// other errors (a base that is no amount) are wrapped.
func TestOnAccountInvalidCode(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	code := TaxCode{Code: "IC5", Kind: "import", Components: []TaxComponent{{Key: "gst", AccountKey: "input", RecoverablePct: "100", RatePct: "abc"}}}
	_, err := calculatedOn(code, 0, 100, day)
	if got := problem.From(err); got == nil || got.Code != "ledger.invalid_tax_code" || !strings.Contains(got.Detail, "IC5") {
		t.Errorf("a rate that is no rate: got %v, want ledger.invalid_tax_code naming IC5", err)
	}
	code.Components[0].RatePct = "5"
	_, err = calculatedOn(code, 0, math.MinInt64, day)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("a base that is no amount: got %v, want the calculator's error", err)
	}
	if got := baseMismatch(err, 0, 5, 0, "IC5", 2); !errors.Is(got, err) {
		t.Errorf("refusal with the calculator's error: got %v, want it", got)
	}
}

// TestOutputCalculationFailed: a code the calculator cannot calculate on
// the date is refused with 422, its code naming why (no rate then, or
// components that make no code then); the calculator's other errors are
// wrapped.
func TestOutputCalculationFailed(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		code string
	}{
		"no rate":      {fmt.Errorf("%w: code S5 component tax on 2026-03-10", tax.ErrNoRate), "ledger.no_tax_rate"},
		"invalid code": {fmt.Errorf("%w S5: no components", tax.ErrInvalidCode), "ledger.invalid_tax_code"},
	} {
		t.Run(name, func(t *testing.T) {
			got := problem.From(calculationFailed("S5", c.err))
			if got == nil || got.Code != c.code || got.Status != 422 || !strings.Contains(got.Detail, "S5") {
				t.Errorf("got %v, want 422 %s naming S5", got, c.code)
			}
		})
	}
	if err := calculationFailed("S5", tax.ErrInvalid); !errors.Is(err, tax.ErrInvalid) {
		t.Errorf("got %v, want it wrapped", err)
	}
}
