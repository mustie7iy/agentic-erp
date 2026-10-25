package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestCheckAssignments: document items are checked as a posting checks
// its lines (T56e, ADR-0050 §6), naming the items; an order item takes no
// default.
func TestCheckAssignments(t *testing.T) {
	a := setupAssigned(t)
	check := func(t *testing.T, le uuid.UUID, day string, defaults bool, items ...ledger.ItemAssignment) error {
		t.Helper()
		return apitest.InRequest(t, a.ten, a.ten.Human, func(ctx context.Context) error {
			return ledger.Accounts{}.CheckAssignments(ctx, le, 2026, date(day), items, defaults)
		})
	}
	item := func(index int, account uuid.UUID, cc, pc *uuid.UUID) ledger.ItemAssignment {
		return ledger.ItemAssignment{Index: index, AccountID: account, CostCenterID: cc, ProfitCenterID: pc}
	}
	t.Run("accepted", func(t *testing.T) {
		for name, c := range map[string]struct {
			le       uuid.UUID
			defaults bool
			items    []ledger.ItemAssignment
		}{
			"a cost center valid on the date":     {a.de, false, []ledger.ItemAssignment{item(0, a.it, &a.cc1000, nil)}},
			"an invoice item taking the default":  {a.de, true, []ledger.ItemAssignment{item(0, a.office, nil, nil)}},
			"a company without center accounting": {a.us, false, []ledger.ItemAssignment{item(0, a.office, nil, nil)}},
		} {
			t.Run(name, func(t *testing.T) {
				if err := check(t, c.le, "2026-05-10", c.defaults, c.items...); err != nil {
					t.Errorf("got %v, want none", err)
				}
			})
		}
	})
	t.Run("refused", func(t *testing.T) {
		for name, c := range map[string]struct {
			defaults            bool
			items               []ledger.ItemAssignment
			code, field, reason string
			detail              string
		}{
			"an order item without a cost center": {false, []ledger.ItemAssignment{item(0, a.it, &a.cc1000, nil), item(3, a.office, nil, nil)},
				"ledger.cost_center_required", "items[3].cost_center_id", "required", "Item 4 posts to a primary cost account and needs a cost center; " +
					"while cost center accounting applies, an order item names its own."},
			"a cost center locked for costs": {true, []ledger.ItemAssignment{item(1, a.it, &a.cc2000, nil)},
				"ledger.cost_center_invalid", "items[1].cost_center_id", "locked_for_costs", "Item 2: The cost center is locked for actual costs on the posting date."},
			"another profit center than the cost center's": {true, []ledger.ItemAssignment{item(0, a.it, &a.cc1000, &a.dummy)},
				"ledger.profit_center_mismatch", "items[0].profit_center_id", "mismatch", ""},
		} {
			t.Run(name, func(t *testing.T) {
				err := check(t, a.de, "2026-02-10", c.defaults, c.items...)
				var p *problem.Problem
				if !errors.As(err, &p) {
					t.Fatalf("got %v, want a problem", err)
				}
				if p.Code != c.code {
					t.Errorf("code: got %s, want %s", p.Code, c.code)
				}
				if len(p.Errors) != 1 || p.Errors[0].Field != c.field || p.Errors[0].Code != c.reason {
					t.Errorf("field errors: got %v, want %s %s", p.Errors, c.field, c.reason)
				}
				if c.detail != "" && p.Detail != c.detail {
					t.Errorf("detail: got %q, want %q", p.Detail, c.detail)
				}
			})
		}
	})
	t.Run("outside a unit of work", func(t *testing.T) {
		if err := (ledger.Accounts{}).CheckAssignments(context.Background(), a.de, 2026, date("2026-05-10"), nil, true); err == nil {
			t.Error("got nil error, want one")
		}
	})
	apitest.EveryFailure(t, a.ten, a.ten.Human, "check assignments", func(ctx context.Context) error {
		return ledger.Accounts{}.CheckAssignments(ctx, a.de, 2026, date("2026-05-10"), []ledger.ItemAssignment{item(0, a.it, &a.cc1000, nil)}, false)
	})
}
