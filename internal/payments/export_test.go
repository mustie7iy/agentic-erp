package payments

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// FiguresOf is figures for the house bank, for the tests of what it reads.
func (e *Endpoints) FiguresOf(ctx context.Context, houseBank uuid.UUID, dates []time.Time, closing []money.Minor) ([]Figures, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	b, err := scanReconBank(tx.QueryRow(ctx, reconBankSelect, houseBank))
	if err != nil {
		return nil, err
	}
	return figures(ctx, tx, e.Accounts, b, dates, closing)
}
