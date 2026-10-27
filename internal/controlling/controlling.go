// Package controlling is the controlling context (ADR-0050 §2, §8,
// decision P5; 03-data-model §3.19): plan values and plan against actual,
// in its own schema. Cost centers, profit centers, hierarchies and the
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
// nothing. Every change is audited (ADR-0011) and published through the
// controlling outbox (ADR-0007). Amounts are integer minor units of the
// company's local currency (ADR-0005).
package controlling

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func init() {
	// Each constraint by its name: TestProblemCodes reads the names to
	// place each constraint's problem on the operations writing it. A new
	// category's code is checked by its constraint alone; a locked
	// category is refused by the application first, and by the trigger
	// only when a check loses a race.
	problem.RegisterConstraint("plan_category_code_once", func() *problem.Problem {
		return problem.Conflict("controlling.plan_category_exists", "A plan category with this code already exists.")
	})
	problem.RegisterConstraint("plan_category_open", categoryLocked)
}

// Ledger is what controlling reads from the ledger (ledger.AssignmentBalances).
type Ledger interface {
	PlanObjects(ctx context.Context, le uuid.UUID, year int16, costCenters, profitCenters, accounts []uuid.UUID) (ledger.PlanObjects, error)
	CostCenters(ctx context.Context, center, node *uuid.UUID) ([]uuid.UUID, bool, error)
	CostCenterActuals(ctx context.Context, q ledger.ActualsQuery) ([]ledger.AssignmentActual, error)
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
