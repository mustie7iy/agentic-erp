package ledger

import (
	"context"
	"math"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestKeepLocalOutOfRange: a kept local amount whose group amount, or the
// lines' local or group total, is out of range is refused with 422
// ledger.amount_too_large, before any account is read.
func TestKeepLocalOutOfRange(t *testing.T) {
	huge := money.Minor(math.MaxInt64 / 2)
	for name, c := range map[string]struct {
		lines  []Line
		rateGC exchange
	}{
		"group amount": {[]Line{{keptLC: &huge}}, exchange{exact: big.NewRat(3, 1), shown: "3"}},
		"local total":  {[]Line{{keptLC: &huge}, {keptLC: &huge}, {LC: huge}}, one},
		"group total":  {[]Line{{keptLC: &huge}, {LC: -huge, GC: huge}, {GC: huge}}, one},
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
