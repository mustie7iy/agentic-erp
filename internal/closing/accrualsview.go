package closing

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
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
// oldest first, with the totals of the whole filtered set per derived
// status and currency.
func (e *Endpoints) ListAccrualProposals(ctx context.Context, params gen.ListAccrualProposalsParams) (gen.ListAccrualProposalsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	closeID, le := apitypes.Ptr[uuid.UUID](params.CloseID), apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	session, entry := apitypes.Ptr[uuid.UUID](params.SessionID), apitypes.Ptr[uuid.UUID](params.ParkedEntryID)
	status := apitypes.Ptr[string](params.Status)
	scope := fmt.Sprintf("listAccrualProposals|%v|%v|%v|%v|%s", closeID, le, session, entry, params.Status.Or(""))
	ks, err := e.Pages.Keyset(accrualSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	filter := []any{access.All, access.IDs, closeID, le, session, entry}
	set, statuses, err := e.accrualSet(ctx, tx, filter, status)
	if err != nil {
		return nil, err
	}
	totals, err := accrualTotals(ctx, tx, set, statuses)
	if err != nil {
		return nil, err
	}
	// A subquery, so that the keyset's id is the proposal's alone; the
	// status, derived, filters by the ids of the set that have it.
	q, args := ks.SQL(`SELECT * FROM (`+accrualSelect+`) a WHERE `+accrualFilter+` AND ($7 OR a.id = ANY($8))`,
		append(filter, status == nil, idref.Of(set, func(a accrual) *uuid.UUID { return &a.ID })))
	rows, _ := tx.Query(ctx, q, args...)
	as, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accrual, error) { return scanAccrual(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: list accrual proposals: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, as, func(a accrual) (string, uuid.UUID) { return a.CreatedAt.UTC().Format(time.RFC3339Nano), a.ID },
		func(page []accrual) (accrualView, error) { return e.accrualView(ctx, tx, page, statuses) },
		func(a accrual, v accrualView) gen.AccrualProposal { return v.toAPI(a) })
	if err != nil {
		return nil, err
	}
	return &gen.AccrualProposalPage{Items: items, Totals: totals, NextPageToken: next}, nil
}

// accrualFilter is the list's filters: $1 and $2 the companies the caller
// may act in, then the close, the company, the session and the parked
// entry, each NULL for any.
const accrualFilter = `($1 OR a.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR a.close_id = $3) AND ($4::uuid IS NULL OR a.legal_entity_id = $4)
	AND ($5::uuid IS NULL OR a.session_id = $5) AND ($6::uuid IS NULL OR a.parked_entry_id = $6)`

// accrualSet is the whole filtered set of a list, of the status asked for
// (nil: any), with every proposal's derived status: the totals' and the
// status filter's. It reads only what a status and a total need.
func (e *Endpoints) accrualSet(ctx context.Context, tx pgx.Tx, filter []any, status *string) ([]accrual, map[uuid.UUID]derived, error) {
	rows, _ := tx.Query(ctx, `SELECT a.id, a.parked_entry_id, a.status, a.currency, a.amount FROM (SELECT a.*, c.legal_entity_id
		FROM closing.accrual_proposal a JOIN closing.period_close c ON c.tenant_id = a.tenant_id AND c.id = a.close_id) a WHERE `+accrualFilter, filter...)
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accrual, error) {
		var a accrual
		return a, errs.Wrapf(r.Scan(&a.ID, &a.ParkedEntryID, &a.Status, &a.Currency, &a.Amount), "scan accrual proposal")
	})
	if err != nil {
		return nil, nil, fmt.Errorf("closing: accrual proposals to list: %w", err)
	}
	statuses, err := e.statusesOf(ctx, all)
	if err != nil || status == nil {
		return all, statuses, err
	}
	var set []accrual
	for _, a := range all {
		if statuses[a.ID].status == *status {
			set = append(set, a)
		}
	}
	return set, statuses, nil
}

// accrualTotal sums the proposals of one status and currency.
type accrualTotal struct {
	status, currency string
	count, modified  int32
	amount, posted   money.Minor
}

// accrualTotals sums the set per status, in accrualStatuses' order, and
// currency, alphabetically; a posted amount is in local currency, the
// proposals' (statuses), so it sums with its row's.
func accrualTotals(ctx context.Context, tx pgx.Tx, set []accrual, statuses map[uuid.UUID]derived) ([]gen.AccrualProposalTotal, error) {
	sums := map[[2]string]*accrualTotal{}
	for _, a := range set {
		d := statuses[a.ID]
		k := [2]string{d.status, a.Currency}
		t, ok := sums[k]
		if !ok {
			t = &accrualTotal{status: d.status, currency: a.Currency}
			sums[k] = t
		}
		t.count++
		t.amount += a.Amount
		if d.posted() {
			t.posted += d.entry.Debits
		}
		if d.modified {
			t.modified++
		}
	}
	keys := slices.SortedFunc(maps.Keys(sums), func(x, y [2]string) int {
		return cmp.Or(cmp.Compare(slices.Index(accrualStatuses, x[0]), slices.Index(accrualStatuses, y[0])), cmp.Compare(x[1], y[1]))
	})
	out := make([]gen.AccrualProposalTotal, len(keys))
	for i, k := range keys {
		t := sums[k]
		exp, err := currency.Exponent(ctx, tx, t.currency, nil)
		if err != nil {
			return nil, fmt.Errorf("closing: currency of accrual totals in %s: %w", t.currency, err)
		}
		out[i] = gen.AccrualProposalTotal{Status: t.status, Currency: t.currency, Count: t.count, Amount: apitypes.Money(t.amount, t.currency, exp),
			ModifiedCount: t.modified}
		if t.status == AccrualPosted || t.status == AccrualReversed {
			out[i].PostedAmount.SetTo(apitypes.Money(t.posted, t.currency, exp))
		}
	}
	return out, nil
}

// accrualView is what proposals are shown with: their companies, agents
// and the people who rejected them, their statuses derived from their
// parked entries, their currencies' digits, and their suppliers' numbers
// and names, none for a caller who may not read business partners.
type accrualView struct {
	view
	statuses  map[uuid.UUID]derived
	exps      map[string]int
	suppliers map[uuid.UUID]Supplier
}

// accrualView reads what the proposals are shown with; statuses are their
// derived statuses when the caller read them already, nil to read them.
func (e *Endpoints) accrualView(ctx context.Context, tx pgx.Tx, as []accrual, statuses map[uuid.UUID]derived) (accrualView, error) {
	people := append(idref.Of(as, func(a accrual) *uuid.UUID { return &a.AgentID }), idref.Of(as, func(a accrual) *uuid.UUID { return a.DecidedBy })...)
	v, err := e.view(ctx, idref.Of(as, func(a accrual) *uuid.UUID { return &a.LegalEntityID }), nil, people...)
	if err != nil {
		return accrualView{}, err
	}
	if statuses == nil {
		if statuses, err = e.statusesOf(ctx, as); err != nil {
			return accrualView{}, err
		}
	}
	out := accrualView{view: v, statuses: statuses, exps: map[string]int{}}
	var suppliers []uuid.UUID
	for _, a := range as {
		if err := out.exponents(ctx, tx, a.ID, a.Currency, statuses[a.ID].entry.Currency); err != nil {
			return accrualView{}, err
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

// exponents reads the digits of a proposal's currencies the view lacks.
func (v accrualView) exponents(ctx context.Context, tx pgx.Tx, id uuid.UUID, currencies ...string) error {
	for _, c := range currencies {
		if _, ok := v.exps[c]; ok {
			continue
		}
		exp, err := currency.Exponent(ctx, tx, c, nil)
		if err != nil {
			return fmt.Errorf("closing: currency of accrual proposal %s: %w", id, err)
		}
		v.exps[c] = exp
	}
	return nil
}

// accrualOut is one proposal as the API shows it.
func (e *Endpoints) accrualOut(ctx context.Context, tx pgx.Tx, a accrual) (gen.AccrualProposal, error) {
	v, err := e.accrualView(ctx, tx, []accrual{a}, nil)
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
		Evidence: make([]gen.AccrualEvidence, len(a.Evidence)), ParkedEntryID: a.ParkedEntryID, PostingDate: a.PostingDate,
		Version: a.Version, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
	v.statusToAPI(&out, a)
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

// statusToAPI shows a proposal's derived status: once posted, its journal
// entry and the amount posted, in local currency, and whether it was
// changed; once reversed, the reversal; once rejected here, who rejected
// it, when and why.
func (v accrualView) statusToAPI(out *gen.AccrualProposal, a accrual) {
	d := v.statuses[a.ID]
	out.Status, out.Modified = d.status, d.modified
	if d.posted() {
		out.PostedAmount.SetTo(apitypes.Money(d.entry.Debits, d.entry.Currency, v.exps[d.entry.Currency]))
		out.JournalEntryID, out.JournalEntryNumber = apitypes.OptUUID(d.entry.JournalEntryID), apitypes.OptString(d.entry.DocumentNumber)
		out.ReversalEntryID, out.ReversalEntryNumber = apitypes.OptUUID(d.entry.ReversalID), apitypes.OptString(d.entry.ReversalNumber)
	}
	if a.DecidedBy != nil {
		out.DecidedBy.SetTo(*a.DecidedBy)
		out.DecidedByName.SetTo(v.people[*a.DecidedBy].Name)
		out.DecidedAt, out.DecisionReason = apitypes.OptDateTime(a.DecidedAt), apitypes.OptString(a.DecisionReason)
	}
}
