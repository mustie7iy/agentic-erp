package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/mrp"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// MRP is the material requirements planning context as the API wires it
// (ADR-0084 §1, §2): a leaf that reads tenancy's plants and companies,
// materials, the ledger's accounts and the configuration's company days,
// and, through mrpSupply, sales' demand and that of the intercompany
// and stock transport orders the plant supplies, inventory's stock and
// prices and procurement's receipts (purchase and stock transport orders)
// and requisitions, which it proposes and
// withdraws through procurement's published API, and for S&OP reviews
// inventory's goods issues, sales' and intercompany orders' demand by
// the day it is due and procurement's stock in transit (S2n). Its runs'
// Operations are the operations context's.
func MRP(cfg Config) *mrp.Endpoints {
	return &mrp.Endpoints{Settings: configuration.Settings{}, Plants: tenancy.Directory{}, Materials: materials.Materials{}, Accounts: ledger.Accounts{},
		Supply: mrpSupply{inventory: Inventory(cfg), procurement: Procurement(cfg)}, Runs: runOperations{kind: mrp.KindRun}, Requests: opener(), Pages: cfg.Pages, Now: cfg.Now}
}

// mrpSupply gives the mrp context what it reads and writes of the plant
// through the other contexts' published APIs, in their types.
type mrpSupply struct {
	inventory   *inventory.Endpoints
	procurement *procurement.Endpoints
}

// opens are rows in the mrp context's type.
func opens[T, U any](rows []T, of func(T) U) []U {
	out := make([]U, len(rows))
	for i, r := range rows {
		out[i] = of(r)
	}
	return out
}

func (s mrpSupply) Stock(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) (map[uuid.UUID]string, error) {
	return s.inventory.PlantStock(ctx, plant, ms)
}

func (s mrpSupply) Prices(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) (map[uuid.UUID]mrp.Price, error) {
	ps, err := s.inventory.PlantPrices(ctx, plant, ms)
	out := make(map[uuid.UUID]mrp.Price, len(ps))
	for id, p := range ps {
		out[id] = mrp.Price{Price: p.Price, PriceUnit: p.PriceUnit}
	}
	return out, err
}

// onOrder is a procurement quantity in the mrp context's type.
func onOrder(o procurement.OnOrder) mrp.Open {
	return mrp.Open{MaterialID: o.MaterialID, Unit: o.Unit, Quantity: o.Quantity}
}

func (mrpSupply) Receipts(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Open, error) {
	rs, err := procurement.Orders{}.OpenReceipts(ctx, plant, ms)
	return opens(rs, onOrder), err
}

func (mrpSupply) Requisitions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]mrp.Requisition, error) {
	rs, err := procurement.Orders{}.Requisitions(ctx, ids)
	out := make(map[uuid.UUID]mrp.Requisition, len(rs))
	for id, r := range rs {
		out[id] = mrp.Requisition{Status: r.Status, Version: r.Version, Items: opens(r.Items, onOrder)}
	}
	return out, err
}

func (s mrpSupply) Propose(ctx context.Context, p mrp.Proposal) (mrp.Proposed, error) {
	material := p.MaterialID
	items := make([]procurement.ItemInput, len(p.Lots))
	for i, q := range p.Lots {
		items[i] = procurement.ItemInput{MaterialID: &material, Quantity: q, Unit: p.Unit, Price: p.Price, Currency: p.Currency, PriceUnit: p.PriceUnit,
			AccountID: p.AccountID}
	}
	r, err := s.procurement.ProposeRequisition(ctx, procurement.NewRequisition{LegalEntityID: p.LegalEntityID, Currency: p.Currency, NeededBy: &p.NeededBy,
		Items: items})
	return mrp.Proposed{ID: r.ID, Number: r.DocumentNumber, Version: r.Version}, err
}

func (s mrpSupply) Withdraw(ctx context.Context, requisition uuid.UUID, version int32) (bool, error) {
	return s.procurement.WithdrawProposal(ctx, requisition, version)
}

func (s mrpSupply) Issues(ctx context.Context, plant uuid.UUID, ms []uuid.UUID, from, to time.Time) (map[uuid.UUID]string, error) {
	return s.inventory.PlantIssues(ctx, plant, ms, from, to)
}

// due is a quantity due on a day in the mrp context's type.
func due(material uuid.UUID, unit, quantity string, day time.Time) mrp.Due {
	return mrp.Due{Open: mrp.Open{MaterialID: material, Unit: unit, Quantity: quantity}, Day: day}
}

func (mrpSupply) DemandDue(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Due, error) {
	rs, err := sales.Orders{}.PlantRequirementsDue(ctx, plant, ms)
	return opens(rs, func(r sales.DueRequirement) mrp.Due { return due(r.MaterialID, r.Unit, r.Quantity, r.Due) }), err
}

func (mrpSupply) Transports(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Open, error) {
	rs, err := procurement.Orders{}.TransportReceipts(ctx, plant, ms)
	return opens(rs, onOrder), err
}

func (mrpSupply) IntercompanyDue(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Due, error) {
	rs, err := procurement.Orders{}.IntercompanyDue(ctx, plant, ms)
	return opens(rs, func(r procurement.DueOnOrder) mrp.Due { return due(r.MaterialID, r.Unit, r.Quantity, r.Due) }), err
}

func (mrpSupply) TransportsDue(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Open, error) {
	rs, err := procurement.Orders{}.TransportsDue(ctx, plant, ms)
	return opens(rs, onOrder), err
}

func (mrpSupply) TransportsOnOrder(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Open, error) {
	rs, err := procurement.Orders{}.TransportsOnOrder(ctx, plant, ms)
	return opens(rs, onOrder), err
}
