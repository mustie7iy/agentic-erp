package collections

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Holds is the collections context's published read for the liquidity
// forecast (ADR-0051 §4, As built T53l1): what collections holds back of
// a company's customer items on a day, so the forecast shows their
// receipts apart, as it shows what blocked suppliers are owed.
type Holds struct{}

// Held are the company's customer items held back on the day, by item,
// and the customers held back whole: an item or a customer under a
// dunning block in force then (not released, ending on or after the day,
// as a dunning run on that key date reads them, T76e), and an item in an
// open dispute case (T86c), whatever part of it the case disputes, as a
// run leaves the whole item out. Company access is the caller's to check.
func (Holds) Held(ctx context.Context, legalEntity uuid.UUID, on time.Time) (map[ledger.ItemRef]bool, map[uuid.UUID]bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, nil, err
	}
	blockedItems, blockedCustomers, err := blocksInForce(ctx, tx, legalEntity, on)
	if err != nil {
		return nil, nil, err
	}
	disputed, err := openCases(ctx, tx, legalEntity)
	if err != nil {
		return nil, nil, err
	}
	items, customers := map[ledger.ItemRef]bool{}, map[uuid.UUID]bool{}
	for ref := range blockedItems {
		items[ref] = true
	}
	for ref := range disputed {
		items[ref] = true
	}
	for c := range blockedCustomers {
		customers[c] = true
	}
	return items, customers, nil
}
