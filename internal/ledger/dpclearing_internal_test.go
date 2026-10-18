package ledger

import (
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestPayablesDebitTooLarge: down payments whose total is more than an
// amount holds are not cleared together (ledger.amount_too_large); posting
// them would also put the balances out of range, so no database test
// builds them.
func TestPayablesDebitTooLarge(t *testing.T) {
	huge := Line{AccountID: uuid.New(), TC: math.MaxInt64, LC: 1, GC: 1}
	_, err := payablesDebit([]Line{huge, huge}, DownPaymentClearing{})
	if got := problem.From(err).Code; got != "ledger.amount_too_large" {
		t.Fatalf("got %s (%v), want ledger.amount_too_large", got, err)
	}
}
