package revenue

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// references is what contracts name by id, as their consumer reads it
// (REVIEW.md, API-consumer classes): their companies and customers.
type references struct {
	companies map[uuid.UUID]tenancy.Entity
	customers map[uuid.UUID]partner.Named
}

// references reads the contracts' companies, once each, and their
// customers' numbers and names.
func (e *Endpoints) references(ctx context.Context, contracts []Contract) (references, error) {
	refs := references{companies: map[uuid.UUID]tenancy.Entity{}}
	customers := make([]uuid.UUID, len(contracts))
	for i, c := range contracts {
		customers[i] = c.CustomerID
		if _, ok := refs.companies[c.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID) // a stored contract's legal entity exists
		if err != nil {
			return references{}, fmt.Errorf("revenue: company of contract %s: %w", c.ID, err)
		}
		refs.companies[c.LegalEntityID] = le
	}
	var err error
	if refs.customers, err = e.Partners.Names(ctx, customers); err != nil {
		return references{}, fmt.Errorf("revenue: customers of %d contracts: %w", len(contracts), err)
	}
	return refs, nil
}

// contractAPI is a contract as the API gives it, with what it names by
// id and every amount in its currency.
func contractAPI(c Contract, refs references) gen.RevenueContract {
	le, customer := refs.companies[c.LegalEntityID], refs.customers[c.CustomerID]
	out := gen.RevenueContract{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), LegalEntityID: c.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name,
		CustomerID: c.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name, SalesOrderID: c.SalesOrderID,
		SalesOrderNumber: c.SalesOrderNumber, Currency: c.Currency, TransactionPrice: apitypes.Money(c.TransactionPrice, c.Currency, c.Exponent),
		Status: c.Status, Obligations: make([]gen.PerformanceObligation, len(c.Obligations))}
	for i, o := range c.Obligations {
		out.Obligations[i] = gen.PerformanceObligation{ObligationNo: o.No, OrderItemNo: o.OrderItemNo, ItemKey: apitypes.OptString(o.ItemKey),
			Description: o.Description, NetAmount: apitypes.Money(o.Net, c.Currency, c.Exponent), Timing: o.Timing,
			ServiceFrom: apitypes.OptDate(o.ServiceFrom), ServiceTo: apitypes.OptDate(o.ServiceTo), SatisfiedOn: apitypes.OptDate(o.SatisfiedOn),
			StandaloneSellingPrice: apitypes.Money(o.StandalonePrice, c.Currency, c.Exponent)}
		if o.Allocated != nil {
			out.Obligations[i].AllocatedAmount = gen.NewOptMoney(apitypes.Money(*o.Allocated, c.Currency, c.Exponent))
		}
	}
	return out
}

// answer is a contract as an action answers it.
func (e *Endpoints) answer(ctx context.Context, c Contract) (gen.RevenueContract, error) {
	refs, err := e.references(ctx, []Contract{c})
	if err != nil {
		return gen.RevenueContract{}, err
	}
	return contractAPI(c, refs), nil
}

// CreateRevenueContract implements POST /v1/revenue-contracts.
func (e *Endpoints) CreateRevenueContract(ctx context.Context, req *gen.RevenueContractCreate, _ gen.CreateRevenueContractParams) (gen.CreateRevenueContractRes, error) {
	c, err := e.Create(ctx, req.SalesOrderID)
	if err != nil {
		return nil, err
	}
	out, err := e.answer(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.RevenueContractHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/revenue-contracts/" + c.ID.String()),
		Response: out}, nil
}

// GetRevenueContract implements GET /v1/revenue-contracts/{id}.
func (e *Endpoints) GetRevenueContract(ctx context.Context, params gen.GetRevenueContractParams) (gen.GetRevenueContractRes, error) {
	c, err := GetContract(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.answer(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetRevenueContractOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// UpdateRevenueContract implements PATCH /v1/revenue-contracts/{id}.
func (e *Endpoints) UpdateRevenueContract(ctx context.Context, req *gen.RevenueContractUpdate, params gen.UpdateRevenueContractParams) (gen.UpdateRevenueContractRes, error) {
	changes := make([]ObligationChange, len(req.Obligations))
	for i, o := range req.Obligations {
		changes[i] = ObligationChange{No: o.ObligationNo, Timing: apitypes.Ptr[string](o.Timing), ServiceFrom: apitypes.OptNilDate(o.ServiceFrom), ServiceTo: apitypes.OptNilDate(o.ServiceTo),
			SatisfiedOn: apitypes.OptNilDate(o.SatisfiedOn)}
		if price, ok := o.StandaloneSellingPrice.Get(); ok {
			changes[i].Price = &Amount{Value: price.Amount, Currency: price.Currency}
		}
	}
	c, err := e.Update(ctx, params.ID, changes, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.answer(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateRevenueContractOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// ActivateRevenueContract implements POST /v1/revenue-contracts/{id}:activate.
func (e *Endpoints) ActivateRevenueContract(ctx context.Context, params gen.ActivateRevenueContractParams) (gen.ActivateRevenueContractRes, error) {
	c, err := e.Activate(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.answer(ctx, c)
	if err != nil {
		return nil, err
	}
	// The generated answer of the update: both answer the contract with its ETag.
	return &gen.UpdateRevenueContractOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

var contractSort = pagination.Sort{Default: "sales_order_number", Fields: []pagination.Field{
	{Name: "sales_order_number", Column: "k.sales_order_number", Type: "text"},
}}

// ListRevenueContracts implements GET /v1/revenue-contracts: the
// contracts of the companies the caller may act in, by their sales
// orders' numbers.
func (e *Endpoints) ListRevenueContracts(ctx context.Context, params gen.ListRevenueContractsParams) (gen.ListRevenueContractsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer, order := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID), apitypes.Ptr[uuid.UUID](params.SalesOrderID)
	scope := fmt.Sprintf("listRevenueContracts|%s|%s|%s|%s", le, customer, order, params.Status.Value)
	ks, err := e.Pages.Keyset(contractSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("revenue: page of contracts: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("revenue: company access for contracts: %w", err)
	}
	query, args := ks.SQL(contractSelect+` WHERE ($1 OR k.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR k.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR k.customer_id = $4) AND ($5::uuid IS NULL OR k.sales_order_id = $5) AND ($6 = '' OR k.status = $6)`,
		[]any{access.All, access.IDs, le, customer, order, params.Status.Value})
	rows, _ := tx.Query(ctx, query, args...)
	contracts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contract, error) { return scanContract(r) })
	if err != nil {
		return nil, fmt.Errorf("revenue: list contracts: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, contracts, func(c Contract) (string, uuid.UUID) { return c.SalesOrderNumber, c.ID },
		func(page []Contract) (references, error) { return e.references(ctx, page) }, contractAPI)
	if err != nil {
		return nil, err
	}
	return &gen.RevenueContractPage{Items: items, NextPageToken: next}, nil
}

// periodsAPI are a schedule's periods as the API gives them; unit is the
// unit of the quantities delivered.
func periodsAPI(periods []PeriodAmount, unit, currency string, exp int) []gen.RevenueSchedulePeriod {
	out := make([]gen.RevenueSchedulePeriod, len(periods))
	for i, p := range periods {
		out[i] = gen.RevenueSchedulePeriod{FiscalYear: int32(p.Period.FiscalYear), FiscalPeriod: int32(p.Period.Number), PeriodStart: p.Period.Start,
			PeriodEnd: p.Period.End, Amount: apitypes.Money(p.Amount, currency, exp), RecognizedAmount: apitypes.Money(p.Recognized, currency, exp),
			PlannedAmount: apitypes.Money(p.Amount-p.Recognized, currency, exp)}
		if p.Days > 0 {
			out[i].Days = gen.NewOptInt32(p.Days)
		}
		if p.Delivered != nil {
			out[i].DeliveredQuantity = gen.NewOptQuantity(gen.Quantity{Value: money.PlainQuantity(p.Delivered), Unit: unit})
		}
	}
	return out
}

// GetRevenueSchedule implements GET /v1/revenue-contracts/{id}/schedule.
func (e *Endpoints) GetRevenueSchedule(ctx context.Context, params gen.GetRevenueScheduleParams) (gen.GetRevenueScheduleRes, error) {
	s, err := e.GetSchedule(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	refs, err := e.references(ctx, []Contract{s.Contract})
	if err != nil {
		return nil, err
	}
	c := s.Contract
	le, customer := refs.companies[c.LegalEntityID], refs.customers[c.CustomerID]
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, c.Currency, c.Exponent) }
	out := &gen.RevenueSchedule{RevenueContractID: c.ID, SalesOrderID: c.SalesOrderID, SalesOrderNumber: c.SalesOrderNumber, LegalEntityID: c.LegalEntityID,
		CompanyCode: le.Code, CompanyName: le.Name, CustomerID: c.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name,
		Currency: c.Currency, AsOf: s.AsOf, TransactionPrice: amount(c.TransactionPrice), RecognizedAmount: amount(s.Recognized),
		PlannedAmount: amount(s.Planned), UnscheduledAmount: amount(s.Unscheduled), Obligations: make([]gen.RevenueScheduleObligation, len(s.Obligations)),
		Periods: periodsAPI(s.Periods, "", c.Currency, c.Exponent)}
	for i, o := range s.Obligations {
		out.Obligations[i] = gen.RevenueScheduleObligation{ObligationNo: o.No, OrderItemNo: o.OrderItemNo, ItemKey: apitypes.OptString(o.ItemKey),
			Description: o.Description, Timing: o.Timing, ServiceFrom: apitypes.OptDate(o.ServiceFrom), ServiceTo: apitypes.OptDate(o.ServiceTo),
			SatisfiedOn: apitypes.OptDate(o.SatisfiedOn), AllocatedAmount: amount(*o.Allocated), RecognizedAmount: amount(o.Recognized),
			PlannedAmount: amount(o.Planned), UnscheduledAmount: amount(o.Unscheduled), Periods: periodsAPI(o.Periods, o.Unit, c.Currency, c.Exponent)}
	}
	return out, nil
}
