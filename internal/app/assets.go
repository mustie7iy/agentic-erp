package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Assets is the assets context as the API wires it (ADR-0057 §2): a leaf
// that reads the configuration, tenancy, ledger and operations contexts
// through the adapters below, so none of them imports it and it imports
// none of them.
func Assets(cfg Config) *assets.Endpoints {
	return &assets.Endpoints{Settings: configuration.Settings{}, Companies: assetCompanies{}, Ledger: assetLedger{}, Centers: assetCenters{},
		Numbers: configuration.Numbers{}, Runs: runOperations{kind: assets.KindDepreciate}, Invoices: assetInvoices{}, Purchases: assetPurchases{}, Pages: cfg.Pages, Now: cfg.Now}
}

// assetCompanies gives the assets context a legal entity by company code
// or id, with its local currency.
type assetCompanies struct{}

func assetCompany(le tenancy.Entity) assets.Company {
	return assets.Company{ID: le.ID, Code: le.Code, Name: le.Name, Currency: le.LocalCurrency}
}

func (assetCompanies) ByCode(ctx context.Context, code string) (assets.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.EntityByCode(ctx, code)
	return assetCompany(le), ok, err
}

func (assetCompanies) Company(ctx context.Context, id uuid.UUID) (assets.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return assetCompany(le), ok, err
}

func (assetCompanies) GroupCurrency(ctx context.Context) (string, error) {
	return tenancy.Directory{}.GroupCurrency(ctx)
}

// assetLedger gives the assets context the ledgers and fiscal years.
type assetLedger struct{}

func (assetLedger) Ledgers(ctx context.Context, legalEntity *uuid.UUID) ([]assets.LedgerRef, error) {
	ls, err := ledger.AssetReads{}.Ledgers(ctx, legalEntity)
	out := make([]assets.LedgerRef, len(ls))
	for i, l := range ls {
		out[i] = assets.LedgerRef{ID: l.ID, Code: l.Code, Name: l.Name}
	}
	return out, err
}

func (assetLedger) FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error) {
	return ledger.Calendar{LegalEntities: tenancy.Directory{}}.FiscalYear(ctx, legalEntity, date)
}

func (assetLedger) Period(ctx context.Context, legalEntity uuid.UUID, date time.Time) (assets.Period, error) {
	p, err := ledger.Calendar{LegalEntities: tenancy.Directory{}}.Period(ctx, legalEntity, date)
	return assets.Period{FiscalYear: p.FiscalYear, Number: p.Number, Start: p.Start, End: p.End}, err
}

func (assetLedger) Periods(ctx context.Context, legalEntity uuid.UUID, year int16) ([]assets.Period, error) {
	ps, err := ledger.Calendar{LegalEntities: tenancy.Directory{}}.Periods(ctx, legalEntity, year)
	out := make([]assets.Period, len(ps))
	for i, p := range ps {
		out[i] = assets.Period{FiscalYear: p.FiscalYear, Number: p.Number, Start: p.Start, End: p.End}
	}
	return out, err
}

func (assetLedger) Accounts(ctx context.Context, legalEntity uuid.UUID, numbers []string) (map[string]uuid.UUID, error) {
	return ledger.AssetReads{}.Accounts(ctx, legalEntity, numbers)
}

func assetLine(l ledger.AssetLine) assets.Line {
	return assets.Line{LineRef: assets.LineRef{FiscalYear: l.FiscalYear, EntryID: l.EntryID, LineNo: l.LineNo}, LegalEntityID: l.LegalEntityID,
		AccountID: l.AccountID, Currency: l.Currency, CurrencyLC: l.CurrencyLC, CurrencyGC: l.CurrencyGC, Amount: l.Amount, LC: l.LC, GC: l.GC,
		PostingDate: l.PostingDate, DocumentNumber: l.DocumentNumber, SourceContext: l.SourceContext, SourceDocumentID: l.SourceDocumentID,
		ProfitCenterID: l.ProfitCenterID, Leading: l.Leading, InEveryLedger: l.InEveryLedger, Reverses: reversalEntry(l.Reverses),
		ReversedBy: reversalEntry(l.ReversedBy)}
}

func reversalEntry(r *ledger.ReversalEntry) *assets.ReversalEntry {
	if r == nil {
		return nil
	}
	return &assets.ReversalEntry{FiscalYear: r.FiscalYear, EntryID: r.EntryID, DocumentNumber: r.DocumentNumber, PostingDate: r.PostingDate}
}

func (assetLedger) Line(ctx context.Context, entry uuid.UUID, lineNo int32) (assets.Line, bool, error) {
	l, ok, err := ledger.AssetReads{}.Line(ctx, entry, lineNo)
	return assetLine(l), ok, err
}

func itemRefs(refs []assets.LineRef) []ledger.ItemRef {
	out := make([]ledger.ItemRef, len(refs))
	for i, r := range refs {
		out[i] = ledger.ItemRef{FiscalYear: r.FiscalYear, EntryID: r.EntryID, LineNo: r.LineNo}
	}
	return out
}

func (assetLedger) ClearingLines(ctx context.Context, q assets.ClearingQuery) ([]assets.Line, error) {
	in := ledger.ClearingQuery{LegalEntityID: q.LegalEntityID, Accounts: q.Accounts, ExceptContext: q.ExceptContext, FiscalYear: q.FiscalYear, Limit: q.Limit,
		Skip: itemRefs(q.Skip), Keep: itemRefs(q.Keep)}
	if q.After != nil {
		in.After = &ledger.ClearingKey{PostingDate: q.After.PostingDate, EntryID: q.After.EntryID, LineNo: q.After.LineNo}
	}
	ls, err := ledger.AssetReads{}.ClearingLines(ctx, in)
	out := make([]assets.Line, len(ls))
	for i, l := range ls {
		out[i] = assetLine(l)
	}
	return out, err
}

// Post posts through the ledger's published posting API (ADR-0057 §4),
// source context assets, to every ledger the company keeps, each line
// keeping the local and group amounts it gives, and reads the entries back
// per ledger.
func (assetLedger) Post(ctx context.Context, p assets.Posting) ([]assets.PostedEntry, error) {
	in := ledger.Posting{LegalEntityID: p.LegalEntityID, DocumentType: p.DocumentType, DocumentDate: p.DocumentDate, PostingDate: p.PostingDate,
		Currency: p.Currency, HeaderText: p.HeaderText, SourceContext: "assets", SourceDocumentID: p.DocumentID, LedgerID: p.LedgerID}
	for _, l := range p.Lines {
		in.Lines = append(in.Lines, ledger.PostingLine{AccountID: l.AccountID, Amount: l.Amount, LC: l.LC, GC: l.GC, CostCenterID: l.CostCenterID,
			ProfitCenterID: l.ProfitCenterID, Text: l.Text})
	}
	posted, err := postings().Post(ctx, in)
	if err != nil {
		return nil, err
	}
	return groupEntries(ctx, posted)
}

// groupEntries reads the entries of an entry's posting group per ledger.
func groupEntries(ctx context.Context, first ledger.Entry) ([]assets.PostedEntry, error) {
	es, err := ledger.AssetReads{}.Entries(ctx, first.FiscalYear, first.ID)
	out := make([]assets.PostedEntry, len(es))
	for i, e := range es {
		out[i] = assets.PostedEntry{ID: e.ID, LedgerID: e.LedgerID, FiscalYear: e.FiscalYear, Period: e.Period, DocumentNumber: e.DocumentNumber}
	}
	return out, err
}

// Reverse reverses an assets entry's posting group through the ledger's
// published reversal path on the date, and reads the reversal's entries
// back per ledger.
func (assetLedger) Reverse(ctx context.Context, entry uuid.UUID, postingDate time.Time) ([]assets.PostedEntry, error) {
	rev, err := postings().Reverse(ctx, entry, &postingDate, nil)
	if err != nil {
		return nil, err
	}
	return groupEntries(ctx, rev)
}

func (assetLedger) OffsetAccount(ctx context.Context, legalEntity, account uuid.UUID) (assets.AccountRef, bool, error) {
	a, ok, err := ledger.AssetReads{}.OffsetAccount(ctx, legalEntity, account)
	return assets.AccountRef{ID: a.ID, Number: a.Number, Name: a.Name}, ok, err
}

func (assetLedger) AccountNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]assets.AccountRef, error) {
	names, err := ledger.AssetReads{}.AccountNames(ctx, ids)
	out := make(map[uuid.UUID]assets.AccountRef, len(names))
	for id, a := range names {
		out[id] = assets.AccountRef{ID: a.ID, Number: a.Number, Name: a.Name}
	}
	return out, err
}

func (assetLedger) PeriodOpen(ctx context.Context, legalEntity, ledgerID uuid.UUID, year, period int16) error {
	return ledger.AssetReads{}.PeriodOpen(ctx, legalEntity, ledgerID, year, period)
}

func (assetLedger) GroupAmounts(ctx context.Context, from, to string, date time.Time, amounts []money.Minor) ([]money.Minor, error) {
	return ledger.AssetReads{}.GroupAmounts(ctx, from, to, date, amounts)
}

// assetInvoices names the supplier invoices acquisitions come from, with
// their suppliers' names.
type assetInvoices struct{}

func (assetInvoices) Sources(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]assets.InvoiceSource, error) {
	srcs, err := payables.Invoices{}.Sources(ctx, ids)
	if err != nil {
		return nil, err
	}
	suppliers := make([]uuid.UUID, 0, len(srcs))
	for _, s := range srcs {
		suppliers = append(suppliers, s.SupplierID)
	}
	names, err := partner.Payees{}.Names(ctx, suppliers)
	out := make(map[uuid.UUID]assets.InvoiceSource, len(srcs))
	for id, s := range srcs {
		out[id] = assets.InvoiceSource{DocumentNumber: s.DocumentNumber, SupplierReference: s.SupplierReference, SupplierID: s.SupplierID,
			SupplierName: names[s.SupplierID].Name}
	}
	return out, err
}

// assetPurchases gives the assets context what order items expensed on
// the price difference account (ledger) and the order items themselves
// (procurement), for its report on asset purchase orders (B10).
type assetPurchases struct{}

func (assetPurchases) Recorded(ctx context.Context, legalEntity uuid.UUID, year int16) ([]assets.OrderItemExpense, error) {
	rows, err := ledger.AssetReads{}.OrderItemExpenses(ctx, legalEntity, year)
	out := make([]assets.OrderItemExpense, len(rows))
	for i, r := range rows {
		out[i] = assets.OrderItemExpense{OrderItemKey: assets.OrderItemKey{OrderID: r.PurchaseOrderID, ItemNo: r.POItemNo}, Amount: r.Amount, TaxBorne: r.TaxBorne}
	}
	return out, err
}

func (assetPurchases) Unrecorded(ctx context.Context, legalEntity uuid.UUID, year int16) ([]assets.UnrecordedExpense, error) {
	rows, err := ledger.AssetReads{}.UnrecordedExpenses(ctx, legalEntity, year)
	out := make([]assets.UnrecordedExpense, len(rows))
	for i, r := range rows {
		out[i].Debits, out[i].Credits = r.Debits, r.Credits
		for _, it := range r.Items {
			out[i].Items = append(out[i].Items, assets.OrderItemKey{OrderID: it.PurchaseOrderID, ItemNo: it.POItemNo})
		}
	}
	return out, err
}

func (assetPurchases) Items(ctx context.Context, orders []uuid.UUID) ([]assets.OrderItem, error) {
	rows, err := procurement.Orders{}.ItemAccounts(ctx, orders)
	out := make([]assets.OrderItem, len(rows))
	for i, r := range rows {
		out[i] = assets.OrderItem{OrderItemKey: assets.OrderItemKey{OrderID: r.PurchaseOrderID, ItemNo: r.ItemNo}, OrderNumber: r.DocumentNumber,
			Description: r.Description, AccountID: r.AccountID}
	}
	return out, err
}

// assetCenters gives the assets context an asset's centers derived and
// checked by the ledger, and centers' names.
type assetCenters struct{}

func (assetCenters) Assignment(ctx context.Context, legalEntity uuid.UUID, date time.Time, year int16, account string,
	costCenter, profitCenter *uuid.UUID) (assets.Assignment, error) {
	a, err := ledger.AssetReads{}.Assignment(ctx, legalEntity, date, year, account, costCenter, profitCenter)
	return assets.Assignment{ProfitCenterID: a.ProfitCenterID, CostCenterRequired: a.CostCenterRequired}, err
}

func (assetCenters) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]assets.CenterName, error) {
	names, err := ledger.AssetReads{}.CenterNames(ctx, ids)
	out := make(map[uuid.UUID]assets.CenterName, len(names))
	for id, n := range names {
		out[id] = assets.CenterName{Code: n.Code, Name: n.Name}
	}
	return out, err
}

// runOperations keeps the Operation of a run of the kind: a
// depreciation run's (ADR-0057 §7), a consolidation run's (ADR-0059 §6).
type runOperations struct{ kind string }

func (r runOperations) Start(ctx context.Context, resource string, request any) (gen.Operation, error) {
	o, err := operations.StartFor(ctx, r.kind, request, resource)
	if err != nil {
		return gen.Operation{}, err
	}
	return operations.ToAPI(o), nil
}

func (r runOperations) Queue(ctx context.Context, id uuid.UUID, resource string, request any, workflowID string) error {
	return operations.StartTaken(ctx, id, r.kind, request, resource, workflowID)
}

func (runOperations) Report(ctx context.Context, id uuid.UUID, done, total int64) error {
	return operations.Report(ctx, id, done, total)
}

func (runOperations) Cancelled(ctx context.Context, id uuid.UUID, lock bool) (bool, error) {
	return operations.IsCancelled(ctx, id, lock)
}

func (runOperations) Succeed(ctx context.Context, id uuid.UUID, resource string, result any) error {
	_, err := operations.Succeed(ctx, id, resource, result)
	return err
}

func (runOperations) Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) error {
	_, err := operations.Fail(ctx, id, p)
	return err
}
