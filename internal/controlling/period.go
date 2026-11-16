package controlling

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// regularPeriod is the regular posting period of the company's fiscal
// year a settlement or a cycle run names, with its dates: what controlling
// posts is dated the period's last day. A year that is not set up is the
// ledger's refusal (422 ledger.no_fiscal_period); a number that is no
// regular period of the year, a special period among them, is refused
// here.
func (e *Endpoints) regularPeriod(ctx context.Context, le uuid.UUID, year, number int32) (ledger.FiscalPeriod, error) {
	periods, err := e.Calendar.Periods(ctx, le, int16(year))
	if err != nil {
		return ledger.FiscalPeriod{}, fmt.Errorf("controlling: periods of %s in %d: %w", le, year, err)
	}
	return ledger.PeriodNumbered(periods, year, number, "controlling.period_not_regular", "fiscal_period")
}
