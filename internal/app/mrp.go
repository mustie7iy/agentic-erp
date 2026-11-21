package app

import (
	"context"

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
// and, through mrpSupply, sales' demand, inventory's stock and prices and
// procurement's receipts and requisitions, which it proposes and
// withdraws through procurement's published API. Its runs' Operations are
// the operations context's.
func MRP(cfg Config) *mrp.Endpoints {
	return &mrp.Endpoints{Settings: configuration.Settings{}, Plants: tenancy.Directory{}, Materials: materials.Materials{}, Accounts: ledger.Accounts{},
		Supply: mrpSupply{inventory: Inventory(cfg), procurement: Procurement(cfg)}, Runs: runOperations{kind: mrp.KindRun}, Pages: cfg.Pages, Now: cfg.Now}
}

// mrpSupply gives the mrp context what it reads and writes of the plant
// through the other contexts' published APIs, in their types.
type mrpSupply struct {
	inventory   *inventory.Endpoints
	procurement *procurement.Endpoints
}

// opens are quantities in the mrp context's type.
func opens[T any](rows []T, of func(T) mrp.Open) []mrp.Open {
	out := make([]mrp.Open, len(rows))
	for i, r := range rows {
		out[i] = of(r)
	}
	return out
}

func (mrpSupply) Demand(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Open, error) {
	rs, err := sales.Orders{}.PlantRequirements(ctx, plant, ms)
	return opens(rs, func(r sales.Requirement) mrp.Open {
		return mrp.Open{MaterialID: r.MaterialID, Unit: r.Unit, Quantity: r.Quantity}
	}), err
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
	r, err := s.procurement.ProposeRequisition(ctx, procurement.NewRequisition{LegalEntityID: p.LegalEntityID, Currency: p.Currency, NeededBy: &p.NeededBy,
		Items: []procurement.ItemInput{{MaterialID: &material, Quantity: p.Quantity, Unit: p.Unit, Price: p.Price, Currency: p.Currency,
			PriceUnit: p.PriceUnit, AccountID: p.AccountID}}})
	return mrp.Proposed{ID: r.ID, Number: r.DocumentNumber, Version: r.Version}, err
}

func (s mrpSupply) Withdraw(ctx context.Context, requisition uuid.UUID, version int32) (bool, error) {
	return s.procurement.WithdrawProposal(ctx, requisition, version)
}
