package payables

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
)

// badRate is the ledger's accounts with a withholding tax code whose rate
// is no decimal, which the database never holds.
type badRate struct{ ledger.Accounts }

func (badRate) WithholdingTaxCode(context.Context, uuid.UUID, string) (ledger.WithholdingTaxCode, bool, error) {
	return ledger.WithholdingTaxCode{Code: "W1", RatePct: "x", Posting: config.WithholdingAtInvoice}, true, nil
}

// TestWithholdingRateRefused: a code's rate that is no decimal fails the
// posting rather than withholding nothing.
func TestWithholdingRateRefused(t *testing.T) {
	e := &Endpoints{Accounts: badRate{}}
	if _, _, _, err := e.withholding(context.Background(), Invoice{Gross: 100}, partner.Supplier{WithholdingTaxCodes: []string{"W1"}}); err == nil {
		t.Error("a rate that is no decimal: got nil error, want an error")
	}
}
