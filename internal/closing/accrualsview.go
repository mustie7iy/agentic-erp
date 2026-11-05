package closing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// supplierReadPermission is the permission reading business partners
// requires (the partner context's, which the closing context may not
// import): a supplier's number and name are a copy of the partner, and a
// copy never crosses permissions (04 §4.4).
const supplierReadPermission = "partner.supplier.read"

var accrualSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "a.created_at", Type: "timestamptz"}}}

// GetAccrualProposal implements GET /v1/accrual-proposals/{id}.
func (e *Endpoints) GetAccrualProposal(ctx context.Context, params gen.GetAccrualProposalParams) (gen.GetAccrualProposalRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	a, err := readAccrual(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.accrualOut(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	return &gen.GetAccrualProposalOKHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Response: out}, nil
}

// ListAccrualProposals implements GET /v1/accrual-proposals: the
// proposals of the closes of the legal entities the caller may act in,
// oldest first.
func (e *Endpoints) ListAccrualProposals(ctx context.Context, params gen.ListAccrualProposalsParams) (gen.ListAccrualProposalsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	closeID, le := apitypes.Ptr[uuid.UUID](params.CloseID), apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	session, entry := apitypes.Ptr[uuid.UUID](params.SessionID), apitypes.Ptr[uuid.UUID](params.ParkedEntryID)
	scope := fmt.Sprintf("listAccrualProposals|%v|%v|%v|%v", closeID, le, session, entry)
	ks, err := e.Pages.Keyset(accrualSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// A subquery, so that the keyset's id is the proposal's alone.
	q, args := ks.SQL(`SELECT * FROM (`+accrualSelect+`) a WHERE ($1 OR a.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR a.close_id = $3)
		AND ($4::uuid IS NULL OR a.legal_entity_id = $4) AND ($5::uuid IS NULL OR a.session_id = $5) AND ($6::uuid IS NULL OR a.parked_entry_id = $6)`,
		[]any{access.All, access.IDs, closeID, le, session, entry})
	rows, _ := tx.Query(ctx, q, args...)
	as, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accrual, error) { return scanAccrual(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: list accrual proposals: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, as, func(a accrual) (string, uuid.UUID) { return a.CreatedAt.UTC().Format(time.RFC3339Nano), a.ID },
		func(page []accrual) (accrualView, error) { return e.accrualView(ctx, tx, page) }, func(a accrual, v accrualView) gen.AccrualProposal { return v.toAPI(a) })
	if err != nil {
		return nil, err
	}
	return &gen.AccrualProposalPage{Items: items, NextPageToken: next}, nil
}

// accrualView is what proposals are shown with: their companies and
// agents, their currencies' digits, and their suppliers' numbers and
// names, none for a caller who may not read business partners.
type accrualView struct {
	view
	exps      map[string]int
	suppliers map[uuid.UUID]Supplier
}

// accrualView reads what the proposals are shown with.
func (e *Endpoints) accrualView(ctx context.Context, tx pgx.Tx, as []accrual) (accrualView, error) {
	v, err := e.view(ctx, idref.Of(as, func(a accrual) *uuid.UUID { return &a.LegalEntityID }), nil,
		idref.Of(as, func(a accrual) *uuid.UUID { return &a.AgentID })...)
	if err != nil {
		return accrualView{}, err
	}
	out := accrualView{view: v, exps: map[string]int{}}
	var suppliers []uuid.UUID
	for _, a := range as {
		if _, ok := out.exps[a.Currency]; !ok {
			if out.exps[a.Currency], err = currency.Exponent(ctx, tx, a.Currency, nil); err != nil {
				return accrualView{}, fmt.Errorf("closing: currency of accrual proposal %s: %w", a.ID, err)
			}
		}
		suppliers = append(suppliers, idref.Of(a.Evidence, func(ev evidence) *uuid.UUID { return ev.SupplierID })...)
		if a.SupplierID != nil {
			suppliers = append(suppliers, *a.SupplierID)
		}
	}
	may, err := authz.MayUse(ctx, supplierReadPermission)
	if err != nil {
		return accrualView{}, fmt.Errorf("closing: may the caller read the proposals' suppliers: %w", err)
	}
	if may && len(suppliers) > 0 {
		if out.suppliers, err = e.Evidence.Suppliers(ctx, suppliers); err != nil {
			return accrualView{}, fmt.Errorf("closing: suppliers of %d accrual proposals: %w", len(as), err)
		}
	}
	return out, nil
}

// accrualOut is one proposal as the API shows it.
func (e *Endpoints) accrualOut(ctx context.Context, tx pgx.Tx, a accrual) (gen.AccrualProposal, error) {
	v, err := e.accrualView(ctx, tx, []accrual{a})
	if err != nil {
		return gen.AccrualProposal{}, err
	}
	return v.toAPI(a), nil
}

// toAPI shows a proposal with its company, agent, accounts, centers and
// suppliers named, its amount with its currency, and its parked entry.
func (v accrualView) toAPI(a accrual) gen.AccrualProposal {
	co := v.companies[a.LegalEntityID]
	out := gen.AccrualProposal{ID: a.ID, CloseID: a.CloseID, LegalEntityID: a.LegalEntityID, CompanyCode: co.Code, CompanyName: co.Name,
		FiscalYear: int32(a.FiscalYear), FiscalPeriod: int32(a.FiscalPeriod), SessionID: a.SessionID, AgentID: a.AgentID,
		AgentName: v.people[a.AgentID].Name, AccountID: a.AccountID, AccountNumber: a.AccountNumber, AccountName: a.AccountName,
		AccrualAccountID: a.AccrualAccountID, AccrualAccountNumber: a.AccrualAccountNumber, AccrualAccountName: a.AccrualAccountName,
		CostCenterID: apitypes.OptUUID(a.CostCenterID), CostCenterCode: apitypes.OptString(a.CostCenterCode), CostCenterName: apitypes.OptString(a.CostCenterName),
		ProfitCenterID: apitypes.OptUUID(a.ProfitCenterID), ProfitCenterCode: apitypes.OptString(a.ProfitCenterCode),
		ProfitCenterName: apitypes.OptString(a.ProfitCenter), SupplierID: apitypes.OptUUID(a.SupplierID),
		Amount: apitypes.Money(a.Amount, a.Currency, v.exps[a.Currency]), Description: a.Description, Reason: a.Reason,
		Evidence: make([]gen.AccrualEvidence, len(a.Evidence)), ParkedEntryID: a.ParkedEntryID, PostingDate: a.PostingDate, Status: a.Status,
		Version: a.Version, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
	if s, ok := v.supplier(a.SupplierID); ok {
		out.SupplierNumber.SetTo(s.Number)
		out.SupplierName.SetTo(s.Name)
	}
	for i, ev := range a.Evidence {
		out.Evidence[i] = gen.AccrualEvidence{Kind: ev.Kind, DocumentID: ev.DocumentID, DocumentNumber: ev.Number, SupplierID: apitypes.OptUUID(ev.SupplierID)}
		if ev.FiscalYear != nil {
			out.Evidence[i].FiscalYear.SetTo(int32(*ev.FiscalYear))
		}
		if s, ok := v.supplier(ev.SupplierID); ok {
			out.Evidence[i].SupplierNumber.SetTo(s.Number)
			out.Evidence[i].SupplierName.SetTo(s.Name)
		}
	}
	return out
}

// supplier is the named supplier of an id, ok false for none or one the
// caller may not see named.
func (v accrualView) supplier(id *uuid.UUID) (Supplier, bool) {
	if id == nil {
		return Supplier{}, false
	}
	s, ok := v.suppliers[*id]
	return s, ok
}
