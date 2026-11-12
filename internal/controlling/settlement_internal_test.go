package controlling

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// validCenters is the ledger's port finding every cost center valid.
type validCenters struct{ Ledger }

func (validCenters) CostCenterValidOn(context.Context, uuid.UUID, time.Time) (bool, error) {
	return true, nil
}

// TestSettlementBeyondAnAmount: a settlement whose total does not fit in
// an amount ends before anything is posted.
func TestSettlementBeyondAnAmount(t *testing.T) {
	e := &Endpoints{Ledger: validCenters{}}
	_, err := e.settle(context.Background(), nil, reqctx.Principal{}, Order{ID: uuid.New()}, tenancy.Entity{}, settlementRule{ReceiverID: uuid.New()},
		ledger.FiscalPeriod{}, []ledger.OrderAccountBalance{{AccountID: uuid.New(), Balance: math.MaxInt64}, {AccountID: uuid.New(), Balance: 1}})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("got %v, want an overflow and nothing posted", err)
	}
}

// TestSettlementLines: an account's balance is credited on the order and
// debited on the receiver, in the order the balances came in, and a total
// that does not fit in an amount is an error, never a wrapped sum.
func TestSettlementLines(t *testing.T) {
	o, rule := Order{ID: uuid.New()}, settlementRule{ReceiverID: uuid.New()}
	a, b := uuid.New(), uuid.New()
	lines, total, err := settlementLines(o, rule, []ledger.OrderAccountBalance{{AccountID: a, Balance: 700}, {AccountID: b, Balance: -200}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 500 {
		t.Errorf("total: got %d, want 500", total)
	}
	want := []ledger.PostingLine{{AccountID: a, Amount: -700, InternalOrderID: &o.ID}, {AccountID: a, Amount: 700, CostCenterID: &rule.ReceiverID},
		{AccountID: b, Amount: 200, InternalOrderID: &o.ID}, {AccountID: b, Amount: -200, CostCenterID: &rule.ReceiverID}}
	if len(lines) != len(want) {
		t.Fatalf("lines: got %d, want %d", len(lines), len(want))
	}
	for i, l := range lines {
		order, center := l.InternalOrderID != nil && *l.InternalOrderID == o.ID, l.CostCenterID != nil && *l.CostCenterID == rule.ReceiverID
		if l.AccountID != want[i].AccountID || l.Amount != want[i].Amount || order != (want[i].InternalOrderID != nil) || center != (want[i].CostCenterID != nil) {
			t.Errorf("line %d: got %+v, want %+v", i+1, l, want[i])
		}
	}
	_, _, err = settlementLines(o, rule, []ledger.OrderAccountBalance{{AccountID: a, Balance: math.MaxInt64}, {AccountID: b, Balance: 1}})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("a total beyond an amount: got %v, want an overflow", err)
	}
}
