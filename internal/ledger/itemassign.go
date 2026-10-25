package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ItemAssignment is a document item's account and the centers it gives,
// as procurement and payables check them before the item posts (ADR-0050
// §6, T56e): Index is the item's position in its document's items, which a
// refusal names (items[Index]).
type ItemAssignment struct {
	Index                        int
	AccountID                    uuid.UUID
	CostCenterID, ProfitCenterID *uuid.UUID
}

// CheckAssignments checks the items of a document of legal entity le
// dated date as a posting on that date derives and validates its lines
// (assign, §5), without locking the centers: the posting checks them
// again. With defaults, an item without a cost center takes its account's
// default, as an invoice item posts; without, an item on a primary cost
// account names its own while cost center accounting applies (a
// requisition or order item, §6). The refusals are the posting's, naming
// the items instead of lines. A date in none of the legal entity's fiscal
// years has nothing to check: no switch applies to it, and its posting is
// refused for the date (ledger.no_fiscal_period), so a document that needs
// no fiscal year until it posts, such as an order released ahead of the
// next year's set-up, is not refused for it here.
func (Accounts) CheckAssignments(ctx context.Context, le uuid.UUID, date time.Time, items []ItemAssignment, defaults bool) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	var year int16
	err = tx.QueryRow(ctx, `SELECT f.fiscal_year FROM ledger.fiscal_period f
		WHERE f.calendar_id = (SELECT s.calendar_id FROM ledger.period_status s WHERE s.legal_entity_id = $1 LIMIT 1)
			AND NOT f.is_special AND $2 BETWEEN f.start_date AND f.end_date`, le, date).Scan(&year)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ledger: fiscal year of %s on %s: %w", le, date.Format(time.DateOnly), err)
	}
	lines := make([]Line, len(items))
	for i, it := range items {
		lines[i] = Line{AccountID: it.AccountID, CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID}
	}
	r, err := readAssignRules(ctx, tx, le, year, lines)
	if err != nil || r.accounts == nil {
		return err
	}
	r.ownCostCenter = !defaults
	return asItems(r.apply(ctx, tx, le, date, lines, false), items)
}

// asItems makes a refusal of the lines built from items name the items:
// each problem of assign has one field error, lines[j].<field>, and a
// detail starting "Line <j+1>", which become items[Index].<field> and
// "Item <Index+1>". Other errors pass unchanged.
func asItems(err error, items []ItemAssignment) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	for k := range p.Errors {
		for j, it := range items {
			if rest, ok := strings.CutPrefix(p.Errors[k].Field, fmt.Sprintf("lines[%d].", j)); ok {
				p.Errors[k].Field = fmt.Sprintf("items[%d].%s", it.Index, rest)
				if after, ok := strings.CutPrefix(p.Detail, fmt.Sprintf("Line %d", j+1)); ok {
					p.Detail = fmt.Sprintf("Item %d%s", it.Index+1, after)
				}
				break
			}
		}
	}
	return p
}
