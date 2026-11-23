// Package mrp is the material requirements planning context (ADR-0084;
// plan-supply-chain.md §S2k): materials' planning data at plants (SAP
// MARC's MRP view) and planning runs per plant, which compute each planned
// material's net requirement from its plant's confirmed sales orders and
// the intercompany and stock transport orders it supplies and has not
// issued, its stock and open receipts (stock transport orders to it among
// them) against its reorder point and propose purchase requisitions for
// the shortages, replacing the run's own earlier proposals and never a
// person's requisition. A run is long work behind an Operation
// (ADR-0012), carried out by the workflows context through its port. It
// is a leaf (ADR-0084 §1): it reads tenancy's plants, materials, the
// ledger's accounts, sales' demand, inventory's stock and prices and
// procurement's intercompany and stock transport orders, receipts and
// requisitions, and writes requisitions, through ports the composition
// root wires, in the published types of materials and tenancy; no context
// imports it. Its
// S&OP plan versions (ADR-0084 As built S2n, plan-supply-chain.md §S2n)
// review a company's demand forecast against supply by material group
// and month, and fix the consensus a planner agrees.
// Tenancy is row-level security (ADR-0003), quantities exact decimals
// (ADR-0082 §5), state changes guarded (ADR-0008) and audited (ADR-0011).
package mrp

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// KindRun is the kind of a planning run's Operation (ADR-0084 §3).
const KindRun = "mrp.run"

// MRP types (SAP MARC-DISMM, from memory).
const (
	ReorderPoint = "reorder_point" // SAP VB
	NoPlanning   = "no_planning"   // SAP ND
)

// Run statuses.
const (
	RunQueued    = "queued"
	RunCompleted = "completed"
	RunFailed    = "failed"
)

// Proposal statuses (mrp.proposal).
const (
	proposalOpen     = "open"
	proposalReplaced = "replaced"
	proposalTaken    = "taken_over"
	proposalClosed   = "closed"
)

func init() {
	problem.RegisterConstraint("material_plan_once", func() *problem.Problem {
		return problem.Conflict("mrp.material_plan_exists", "The material is planned at the plant already; change its planning data instead "+
			"(GET /v1/material-plans?plant_id=&material_id=).")
	})
	// Two runs of a plant started at once: the second's insert meets the
	// first's queued row.
	problem.RegisterConstraint("run_one_at_a_time", func() *problem.Problem {
		return problem.Conflict("mrp.run_in_progress", "Another planning run of the plant was started at the same time and is queued "+
			"(GET /v1/mrp-runs?status=queued).")
	})
	// Two reviews of a company started at once: the second's insert meets
	// the first's number.
	problem.RegisterConstraint("sop_plan_number_once", func() *problem.Problem {
		return problem.Conflict("mrp.sop_plan_concurrent", "Another S&OP review of the company was started at the same time and took this "+
			"version's number; start it again.")
	})
}

// Plants reads tenancy's org units and legal entities (its published
// API): the plants by id, and a company.
type Plants interface {
	OrgUnits(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]tenancy.Unit, error)
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Materials reads materials (the materials context's published API).
type Materials interface {
	Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]materials.Material, error)
}

// Accounts reads the ledger's accounts: whether a company may post to
// one, and their numbers.
type Accounts interface {
	IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error)
	AccountNumbers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Open is a quantity of a material in one unit, as a plain decimal: what
// sales orders demand or purchasing documents have open.
type Open struct {
	MaterialID uuid.UUID
	Unit       string
	Quantity   string
}

// Price is a material's valuation price at a plant, in the company's
// currency (a valuation's, ADR-0082 §8) per PriceUnit base units.
type Price struct {
	Price     money.Minor
	PriceUnit int32
}

// Requisition is a requisition a run proposed, as it stands now: its
// status, version and items of materials.
type Requisition struct {
	Status  string
	Version int32
	Items   []Open
}

// Proposal is a requisition a run proposes: one item of the material in
// its base unit, at its price, naming the account, needed by the day.
type Proposal struct {
	LegalEntityID, MaterialID, AccountID uuid.UUID
	Currency, Quantity, Unit, Price      string
	PriceUnit                            int32
	NeededBy                             time.Time
}

// Proposed is a requisition created for a proposal.
type Proposed struct {
	ID      uuid.UUID
	Number  string
	Version int32
}

// Due is an Open quantity due on a day: what sales orders due then
// demand.
type Due struct {
	Open
	Day time.Time
}

// Supply is what a run reads of the plant through the other contexts'
// published APIs, in the run's transaction: the demand of the confirmed
// sales orders (sales) and of the intercompany orders (IntercompanyDue,
// its days not needed) and stock transport orders (TransportsDue) the
// plant supplies (procurement), the stock and prices (inventory), the
// open receipts of purchase orders (Receipts) and stock transport orders
// (TransportsOnOrder) (procurement), and the requisitions it proposed,
// which it proposes and withdraws through procurement. An S&OP review (S2n) reads besides the plant's goods
// issues of the days (inventory), its demand by the day it is due
// (sales' confirmed orders, procurement's intercompany orders it
// supplies) and the stock in transit to it (procurement).
type Supply interface {
	Demand(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Open, error)
	Stock(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) (map[uuid.UUID]string, error)
	Prices(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) (map[uuid.UUID]Price, error)
	Receipts(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Open, error)
	Requisitions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Requisition, error)
	Propose(ctx context.Context, p Proposal) (Proposed, error)
	Withdraw(ctx context.Context, requisition uuid.UUID, version int32) (bool, error)
	Issues(ctx context.Context, plant uuid.UUID, materials []uuid.UUID, from, to time.Time) (map[uuid.UUID]string, error)
	DemandDue(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Due, error)
	IntercompanyDue(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Due, error)
	Transports(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Open, error)
	TransportsDue(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Open, error)
	TransportsOnOrder(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Open, error)
}

// Runs keeps a run's Operation (operations context): Start writes it
// queued for the run's resource path in the request's transaction;
// Cancelled says whether it was cancelled (under its lock with lock);
// Succeed and Fail finish it from the run's work.
type Runs interface {
	Start(ctx context.Context, resource string, request any) (gen.Operation, error)
	Cancelled(ctx context.Context, id uuid.UUID, lock bool) (bool, error)
	Succeed(ctx context.Context, id uuid.UUID, resource string, result any) error
	Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) error
}

// Endpoints implements the mrp context's operations of the public API,
// the planning run's work and S&OP's plan versions.
type Endpoints struct {
	Settings  config.Documents
	Plants    Plants
	Materials Materials
	Accounts  Accounts
	Supply    Supply
	Runs      Runs
	Pages     *pagination.Codec
	Now       func() time.Time // the clock of a run's company day; nil: the wall clock
}
