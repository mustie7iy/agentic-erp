// Package controlling is the controlling context (ADR-0050 §2, §8,
// decision P5; ADR-0064; 03-data-model §3.19): plan values and plan
// against actual, and internal orders, in its own schema. Cost centers, profit centers, hierarchies and the
// actuals by assignment are the ledger's (ADR-0050 §2, §7); controlling
// reads them only through the ledger's published port,
// ledger.AssignmentBalances, run in the request's transaction under
// row-level security (ADR-0003), and keeps no foreign key into the ledger's
// schema.
//
// Plan categories (PLAN, FORECAST, ...) are open or locked; locking and
// unlocking are a person's decision (controlling.plan.lock, an approval
// permission agents cannot hold, ADR-0050 §12), read FOR UPDATE with the
// caller's version (ADR-0008). Plan lines are written by batches: a batch
// replaces the plan of its category, company, fiscal year and centers, and
// its key, once applied, makes the same batch again a no-op that publishes
// nothing. A batch that would write or replace a cost center's plan in a
// period in which the center's period is locked for planning (cost_center_period.lock_plan,
// ADR-0050 §3) is refused (T56g2); the ledger's port holds the centers
// for the batch's transaction, so a lock cannot slip between the check and
// the write. Every change is audited (ADR-0011) and published through the
// controlling outbox (ADR-0007). Amounts are integer minor units of the
// company's local currency (ADR-0005).
//
// Internal orders (ADR-0064 §1, T60a) are controlling's master data: an
// order of a company with a code unique in it and a responsible cost
// center, which the ledger's port names and checks (an active one of the
// company). An order is created and then released for posting by a person
// (controlling.order.release, an approval permission), read FOR UPDATE
// with the caller's version; both are audited and published. Costs are
// posted to a released order on journal lines (T60b): the ledger asks
// this context about the orders through its port ledger.CostObjects
// (CostObjects), and the order's actual costs by account and period are
// the ledger's lines naming it, read through the ledger's port.
//
// An order's budget (ADR-0064 §3, T60c) is one overall amount in the
// company's currency, set by a person holding controlling.order.budget
// with the version the budget's reader saw, the order held FOR UPDATE;
// each change is audited with the amounts before and after and
// published. The read compares it with the order's actual costs over
// every year; nothing is refused for a budget.
package controlling

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func init() {
	// Each constraint by its name: TestProblemCodes reads the names to
	// place each constraint's problem on the operations writing it. A new
	// category's code is checked by its constraint alone; a locked
	// category is refused by the application, which holds the category
	// FOR UPDATE, and the trigger backs that up for any other writer.
	problem.RegisterConstraint("plan_category_code_once", func() *problem.Problem {
		return problem.Conflict("controlling.plan_category_exists", "A plan category with this code already exists.")
	})
	problem.RegisterConstraint("plan_category_open", categoryLocked)
	// Internal orders (ADR-0064 §1): a code is checked by its constraint
	// alone; a released order is refused by the application, which holds
	// the order FOR UPDATE, and the trigger backs that up for any other
	// writer.
	problem.RegisterConstraint("internal_order_code_once", func() *problem.Problem {
		return problem.Conflict("controlling.internal_order_exists", "The legal entity already has an internal order with this code.")
	})
}

// Ledger is what controlling reads from the ledger (ledger.AssignmentBalances).
type Ledger interface {
	PlanObjects(ctx context.Context, le uuid.UUID, year int16, costCenters, profitCenters, accounts []uuid.UUID) (ledger.PlanObjects, error)
	CostCenters(ctx context.Context, center, node *uuid.UUID) ([]uuid.UUID, bool, error)
	CostCenterActuals(ctx context.Context, q ledger.ActualsQuery) ([]ledger.AssignmentActual, error)
	ProfitCenters(ctx context.Context, center, node *uuid.UUID) ([]uuid.UUID, bool, error)
	ProfitCenterActuals(ctx context.Context, q ledger.ActualsQuery) ([]ledger.AssignmentActual, error)
	CostCenterRefs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.CostCenterRef, error)
	OrderActuals(ctx context.Context, q ledger.OrderActualsQuery) (ledger.OrderActuals, error)
	OrderBalance(ctx context.Context, le, order uuid.UUID) (money.Minor, error)
}

// LegalEntities reads legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Endpoints implements the controlling part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Ledger        Ledger
}

func categoryLocked() *problem.Problem {
	return problem.Conflict("controlling.plan_category_locked", "The plan category is locked, so its plan does not move; a person unlocks it first.")
}
