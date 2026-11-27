package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// CashItem is a customer open item as the liquidity forecast reads it
// (ADR-0051 §4, §5, As built T53l1): the ledger's item, the day it is due
// (ledger.ReceivableItem.NetDueDate, the one rule the AR aging, dunning
// and the worklist keep).
type CashItem struct {
	ledger.ReceivableItem
	Due time.Time
}

// CashItems is the receivables context's published read for the
// liquidity forecast (ADR-0051 §4, As built T53l1): the company's
// ordinary customer open items in every currency, credits (a credit memo,
// a payment on account, a residual credit) negative as booked, so the
// forecast nets them as the customer's payments would. Special G/L items
// (a down payment received, a guarantee) are left out: they are no
// receipt the customer still owes in the ordinary course. Company access
// is the caller's to check.
func (e *Endpoints) CashItems(ctx context.Context, legalEntity uuid.UUID) ([]CashItem, error) {
	items, err := e.OpenItems.Receivable(ctx, legalEntity, nil)
	if err != nil {
		return nil, fmt.Errorf("receivables: open items of %s for the forecast: %w", legalEntity, err)
	}
	var out []CashItem
	for _, it := range items {
		if it.SpecialGL == nil {
			out = append(out, CashItem{ReceivableItem: it, Due: it.NetDueDate()})
		}
	}
	return out, nil
}
