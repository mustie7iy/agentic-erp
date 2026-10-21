package payables

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The amount a posting rule moves is summed exactly, once: a total out of
// range fails the simulation instead of wrapping to a wrong amount, and the
// first overflow in the order of the rules is the one reported.
func TestSimulatedMovesOutOfRange(t *testing.T) {
	top := money.Minor(math.MaxInt64)
	ta := &tally{moves: map[string]*Posting{}}
	ta.move("1000", "account_determination.input_tax", "140000", "140100", top, inEUR)
	ta.move("1000", "account_determination.input_tax", "140000", "140100", 1, inEUR)
	ta.move("1000", "account_determination.ap_reconciliation", "211000", "211100", top, inEUR)
	ta.move("1000", "account_determination.ap_reconciliation", "211000", "211100", 1, inEUR)
	_, err := ta.result(90 * 24 * time.Hour)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("result: got %v, want %v", err, money.ErrOverflow)
	}
	if want := "payables: simulate: amount moved by 1000/account_determination.ap_reconciliation: " + money.ErrOverflow.Error(); err.Error() != want {
		t.Errorf("result: got %q, want %q", err, want)
	}
}

// An amount out of range in the company's currency fails the simulation,
// and the first such amount is the one reported.
func TestSimulatedMoveConvertedOutOfRange(t *testing.T) {
	huge := ledger.Conversion{From: "JPY", To: "EUR", Rate: "1000000", FromExp: 0, ToExp: 2}
	ta := &tally{moves: map[string]*Posting{}}
	ta.move("2000", "account_determination.input_tax", "140000", "140100", math.MaxInt64/2, huge)
	ta.move("2000", "account_determination.ap_reconciliation", "211000", "211100", math.MaxInt64/2, huge)
	_, err := ta.result(90 * 24 * time.Hour)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("result: got %v, want %v", err, money.ErrOverflow)
	}
	if want := "payables: simulate: amount moved by 2000/account_determination.input_tax: ledger: convert JPY into EUR: " + money.ErrOverflow.Error(); err.Error() != want {
		t.Errorf("result: got %q, want %q", err, want)
	}
}
