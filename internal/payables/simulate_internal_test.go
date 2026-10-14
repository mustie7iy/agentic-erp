package payables

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The amount a posting rule moves is summed exactly: a total out of range
// fails the simulation instead of wrapping to a wrong amount, and the first
// overflow is the one reported.
func TestSimulatedMovesOutOfRange(t *testing.T) {
	top := money.Minor(math.MaxInt64)
	ta := &tally{moves: map[string]*Posting{}}
	ta.move("1000", "account_determination.ap_reconciliation", "211000", "211100", top)
	ta.move("1000", "account_determination.ap_reconciliation", "211000", "211100", 1)
	ta.move("1000", "account_determination.input_tax", "140000", "140100", top)
	ta.move("1000", "account_determination.input_tax", "140000", "140100", 1)
	_, err := ta.result(90*24*time.Hour, 2)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("result: got %v, want %v", err, money.ErrOverflow)
	}
	if want := "payables: simulate: amount moved by 1000/account_determination.ap_reconciliation: " + money.ErrOverflow.Error(); err.Error() != want {
		t.Errorf("result: got %q, want %q", err, want)
	}
}
