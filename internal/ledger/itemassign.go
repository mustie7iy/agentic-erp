package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

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

// CheckAssignments checks the items of a document of legal entity le, on
// date in fiscal year year, as a posting on that date derives and
// validates its lines (assign, §5), without locking the centers: the
// posting checks them again. With defaults, an item without a cost center
// takes its account's default, as an invoice item posts; without, an item
// on a primary cost account names its own while cost center accounting
// applies (an order item, §6). The refusals are the posting's, naming the
// items instead of lines.
func (Accounts) CheckAssignments(ctx context.Context, le uuid.UUID, year int16, date time.Time, items []ItemAssignment, defaults bool) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
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
