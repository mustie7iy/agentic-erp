package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPayingBankOfNeither: a payment that names no house bank in a run
// without one is broken data (a run proposed without a bank names one on
// each payment), an error rather than a guess; the database is not asked.
func TestPayingBankOfNeither(t *testing.T) {
	_, err := payingBank(context.Background(), nil, Run{ID: uuid.New()}, Payment{ID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "names no house bank") {
		t.Errorf("got %v, want the payment refused", err)
	}
}
