package leases

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
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
// (REVIEW.md, API-consumer classes): their companies, lessors and cost
// centers.
type references struct {
	companies   map[uuid.UUID]tenancy.Entity
	lessors     map[uuid.UUID]partner.Named
	costCenters map[uuid.UUID]ledger.CostCenterRef
}

// references reads the contracts' companies, once each, their lessors'
// numbers and names and their cost centers' codes.
func (e *Endpoints) references(ctx context.Context, contracts []Contract) (references, error) {
	refs := references{companies: map[uuid.UUID]tenancy.Entity{}}
	lessors, centers := make([]uuid.UUID, len(contracts)), make([]uuid.UUID, len(contracts))
	for i, c := range contracts {
		lessors[i], centers[i] = c.LessorID, c.CostCenterID
		if _, ok := refs.companies[c.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID) // a stored contract's legal entity exists
		if err != nil {
			return references{}, fmt.Errorf("leases: company of contract %s: %w", c.ID, err)
		}
		refs.companies[c.LegalEntityID] = le
	}
	var err error
	if refs.lessors, err = e.Partners.Names(ctx, lessors); err != nil {
		return references{}, fmt.Errorf("leases: lessors of %d contracts: %w", len(contracts), err)
	}
	if refs.costCenters, err = e.CostCenters.CostCenterRefs(ctx, centers); err != nil {
		return references{}, fmt.Errorf("leases: cost centers of %d contracts: %w", len(contracts), err)
	}
	return refs, nil
}

// contractAPI is a contract as the API gives it, with what it names by
// id and every amount in its currency.
func contractAPI(c Contract, refs references) gen.LeaseContract {
	le, lessor := refs.companies[c.LegalEntityID], refs.lessors[c.LessorID]
	out := gen.LeaseContract{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), DocumentNumber: c.DocumentNumber, LegalEntityID: c.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, LessorID: c.LessorID, LessorNumber: lessor.Number, LessorName: lessor.Name, CommencementDate: c.CommencementDate,
		EndDate: c.Terms().End(), TermMonths: c.TermMonths, PaymentAmount: apitypes.Money(c.PaymentAmount, c.Currency, c.Exponent),
		PaymentFrequency: c.Frequency, PaymentTiming: c.Timing, AnnualDiscountRatePercent: c.AnnualRate, CostCenterID: c.CostCenterID,
		CostCenterCode: refs.costCenters[c.CostCenterID].Code, Currency: c.Currency, PaymentCount: c.PaymentCount,
		TotalPayments: apitypes.Money(c.Total, c.Currency, c.Exponent), Status: c.Status,
		JournalEntryID: apitypes.OptUUID(c.EntryID), JournalEntryNumber: apitypes.OptString(c.EntryNumber)}
	if c.Liability != nil {
		out.LeaseLiability = gen.NewOptMoney(apitypes.Money(*c.Liability, c.Currency, c.Exponent))
		out.RightOfUseAsset = gen.NewOptMoney(apitypes.Money(*c.RightOfUse, c.Currency, c.Exponent))
		out.JournalEntryYear = gen.NewOptInt32(int32(*c.EntryYear))
	}
	if c.Payments != nil {
		out.Payments = make([]gen.LeasePayment, len(c.Payments))
		for i, p := range c.Payments {
			out.Payments[i] = gen.LeasePayment{PaymentNo: p.No, DueDate: p.Due, Amount: apitypes.Money(p.Amount, c.Currency, c.Exponent)}
		}
	}
	return out
}

// answer is a contract as an action answers it.
func (e *Endpoints) answer(ctx context.Context, c Contract) (gen.LeaseContract, error) {
	refs, err := e.references(ctx, []Contract{c})
	if err != nil {
		return gen.LeaseContract{}, err
	}
	return contractAPI(c, refs), nil
}

// CreateLeaseContract implements POST /v1/lease-contracts.
func (e *Endpoints) CreateLeaseContract(ctx context.Context, req *gen.LeaseContractCreate, _ gen.CreateLeaseContractParams) (gen.CreateLeaseContractRes, error) {
	c, err := e.Create(ctx, Input{LegalEntityID: req.LegalEntityID, LessorID: req.LessorID, CostCenterID: req.CostCenterID,
		Commencement: req.CommencementDate, TermMonths: req.TermMonths, Payment: Amount{Value: req.PaymentAmount.Amount, Currency: req.PaymentAmount.Currency},
		Frequency: req.PaymentFrequency, Timing: req.PaymentTiming, Rate: req.AnnualDiscountRatePercent})
	if err != nil {
		return nil, err
	}
	out, err := e.answer(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.LeaseContractHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/lease-contracts/" + c.ID.String()),
		Response: out}, nil
}

// GetLeaseContract implements GET /v1/lease-contracts/{id}.
func (e *Endpoints) GetLeaseContract(ctx context.Context, params gen.GetLeaseContractParams) (gen.GetLeaseContractRes, error) {
	c, err := GetContract(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.answer(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetLeaseContractOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// ActivateLeaseContract implements POST /v1/lease-contracts/{id}:activate.
func (e *Endpoints) ActivateLeaseContract(ctx context.Context, params gen.ActivateLeaseContractParams) (gen.ActivateLeaseContractRes, error) {
	c, err := e.Activate(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.answer(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gen.ActivateLeaseContractOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// GetLeaseSchedule implements GET /v1/lease-contracts/{id}/schedule: a
// draft's computed on read, an active contract's as stored.
func (e *Endpoints) GetLeaseSchedule(ctx context.Context, params gen.GetLeaseScheduleParams) (gen.GetLeaseScheduleRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := GetContract(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	s, err := c.Schedule(ctx, tx)
	if err != nil {
		return nil, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID) // a stored contract's legal entity exists
	if err != nil {
		return nil, fmt.Errorf("leases: company of contract %s: %w", c.ID, err)
	}
	return scheduleAPI(c, le, s), nil
}

// scheduleAPI is a schedule as the API gives it, every amount in the
// contract's currency, with its totals.
func scheduleAPI(c Contract, le tenancy.Entity, s Schedule) *gen.LeaseSchedule {
	m := func(a money.Minor) gen.Money { return apitypes.Money(a, c.Currency, c.Exponent) }
	t := c.Terms()
	out := &gen.LeaseSchedule{LeaseContractID: c.ID, DocumentNumber: c.DocumentNumber, LegalEntityID: c.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, Status: c.Status, Currency: c.Currency, CommencementDate: c.CommencementDate, PaymentTiming: c.Timing,
		PaymentsPerYear: int32(t.perYear()), AnnualDiscountRatePercent: c.AnnualRate, LeaseLiability: m(s.Liability),
		CommencementPayment: m(s.CommencementPayment), RightOfUseAsset: m(s.RightOfUse), Lines: make([]gen.LeaseScheduleLine, len(s.Lines))}
	var interest, depreciation money.Minor
	for i, l := range s.Lines {
		out.Lines[i] = gen.LeaseScheduleLine{LineNo: l.No, PeriodStart: l.Start, PeriodEnd: l.End, PaymentDate: l.PaymentDate, OpeningLiability: m(l.Opening),
			Payment: m(l.Payment), Interest: m(l.Interest), ClosingLiability: m(l.Closing), Depreciation: m(l.Depreciation),
			RightOfUseClosing: m(l.RightOfUseCarrying)}
		interest += l.Interest
		depreciation += l.Depreciation
	}
	out.Totals = gen.LeaseScheduleTotals{Payments: m(c.Total), Interest: m(interest), Depreciation: m(depreciation)}
	return out
}

var contractSort = pagination.Sort{Default: "document_number", Fields: []pagination.Field{
	{Name: "document_number", Column: "k.document_number", Type: "text"},
}}

// ListLeaseContracts implements GET /v1/lease-contracts: the contracts of
// the companies the caller may act in, by number, without their payments.
func (e *Endpoints) ListLeaseContracts(ctx context.Context, params gen.ListLeaseContractsParams) (gen.ListLeaseContractsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, lessor := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.LessorID)
	scope := fmt.Sprintf("listLeaseContracts|%s|%s|%s", le, lessor, params.Status.Value)
	ks, err := e.Pages.Keyset(contractSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("leases: page of contracts: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("leases: company access for contracts: %w", err)
	}
	query, args := ks.SQL(listSelect+` WHERE ($1 OR k.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR k.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR k.lessor_id = $4) AND ($5 = '' OR k.status = $5)`,
		[]any{access.All, access.IDs, le, lessor, params.Status.Value})
	rows, _ := tx.Query(ctx, query, args...)
	contracts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contract, error) { return scanContract(r) })
	if err != nil {
		return nil, fmt.Errorf("leases: list contracts: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, contracts, func(c Contract) (string, uuid.UUID) { return c.DocumentNumber, c.ID },
		func(page []Contract) (references, error) { return e.references(ctx, page) }, contractAPI)
	if err != nil {
		return nil, err
	}
	return &gen.LeaseContractPage{Items: items, NextPageToken: next}, nil
}
