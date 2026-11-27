package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CostObjectReferences answers for the custom fields that name a center
// (extensions.CostObjects; ADR-0046 As built T56k): whether a document of a
// company dated a day may name it. It asks what a posting on that day asks
// (validCostCenter, profitCenterState): known, active, the company's (a
// profit center assigned to it) and with a period covering the day, but
// judges activity before the company, unlike centerState, so a pending
// profit center, assigned to its companies only at approval, is named not
// active rather than another company's (costObjectState). It reads without locks, as CheckAssignments reads a document's items:
// the document's next write and its posting check again. The centers'
// locks are not read: they hold back postings of actual costs, revenue or
// plan, and a custom field posts nothing.
type CostObjectReferences struct{}

// CostObject implements extensions.CostObjects.
func (CostObjectReferences) CostObject(ctx context.Context, kind extensions.Kind, company uuid.UUID, date time.Time,
	id uuid.UUID) (extensions.CostObjectState, error) {
	judge, ok := costObjectJudges[kind]
	if !ok {
		return "", fmt.Errorf("ledger: %q is no cost object the ledger holds", kind)
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	return judge(ctx, tx, company, date, id)
}

// A costObjectJudge judges one kind of center for a document.
type costObjectJudge func(ctx context.Context, tx pgx.Tx, company uuid.UUID, date time.Time, id uuid.UUID) (extensions.CostObjectState, error)

var costObjectJudges = map[extensions.Kind]costObjectJudge{extensions.CostCenter: costCenterObject, extensions.ProfitCenter: profitCenterObject}

// costCenterObject judges a cost center: active, the company's, with a
// period on the date.
func costCenterObject(ctx context.Context, tx pgx.Tx, company uuid.UUID, date time.Time, id uuid.UUID) (extensions.CostObjectState, error) {
	centers, err := readCostCenters(ctx, tx, []uuid.UUID{id}, false)
	if err != nil || centers[id] == nil {
		return extensions.CostObjectNotFound, err
	}
	c := centers[id]
	return costObjectState(c.status, c.legalEntity == company, onDate(c.spans, date)), nil
}

// profitCenterObject judges a profit center: active, assigned to the
// company, with a period on the date.
func profitCenterObject(ctx context.Context, tx pgx.Tx, company uuid.UUID, date time.Time, id uuid.UUID) (extensions.CostObjectState, error) {
	centers, err := readProfitCenters(ctx, tx, company, []uuid.UUID{id}, false)
	if err != nil || centers[id] == nil {
		return extensions.CostObjectNotFound, err
	}
	c := centers[id]
	return costObjectState(c.status, c.inCompany, onDate(c.spans, date)), nil
}

// costObjectState is why a known center cannot be named. Its activity
// comes first: a pending profit center is assigned to its companies only
// when it is approved, and is not another company's.
func costObjectState(status string, inCompany, onDate bool) extensions.CostObjectState {
	switch {
	case status != CenterActive:
		return extensions.CostObjectNotActive
	case !inCompany:
		return extensions.CostObjectNotInCompany
	case !onDate:
		return extensions.CostObjectNotValidOnDate
	}
	return extensions.CostObjectValid
}

// onDate reports whether one of the spans covers the day.
func onDate[A comparable](spans []span[A], date time.Time) bool {
	_, ok := spanOn(spans, date, false)
	return ok
}
