package assets

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The materiality report on asset purchase orders (accounting specialist
// B10, po:2026-11-10; ADR-0057 §6, As built B10): what invoices and
// credit memos expensed on the price difference account for order items
// that are assets, the tax not recovered apart from the price difference.
// Exact for the lines the ledger records with their order item
// (ledger.price_difference_item); what earlier postings expensed is
// stated as one amount whose split is unknown; and, a figure of its own,
// what GR/IR clearings of those items put in profit or loss. The report
// is history (product owner 2026-11-10): a document or clearing posted
// now puts an asset's item's amounts on its asset clearing account, and
// correcting an earlier posting is a journal the accountants decide from
// this report. Nothing is posted here.

// OrderItemKey names a purchase order item.
type OrderItemKey struct {
	OrderID uuid.UUID
	ItemNo  int32
}

// OrderItemExpense is what a company's recorded price difference lines of
// a fiscal year expensed for an order item, in local currency, debit
// positive: Amount in all, TaxBorne of it the input tax the item bore.
type OrderItemExpense struct {
	OrderItemKey
	Amount, TaxBorne money.Minor
}

// OrderItemCleared is what a company's GR/IR clearings of a fiscal year
// put in profit or loss for an order item, in local currency, debit
// positive.
type OrderItemCleared struct {
	OrderItemKey
	Amount money.Minor
}

// UnrecordedExpense is what one invoice or credit memo entry put on the
// price difference account without a recorded order item, in local
// currency, its debits and its credits (negative) apart, and the order
// items the entry clears GR/IR for.
type UnrecordedExpense struct {
	Debits, Credits money.Minor
	Items           []OrderItemKey
}

// unattributedExpenses are the unrecorded amounts of the entries that
// touch an asset order item, and how many those entries are.
type unattributedExpenses struct {
	debits, credits money.Minor
	entries         int
}

// OrderItem is a purchase order item as the report shows it: its order's
// number, its description and the account it is assigned to.
type OrderItem struct {
	OrderItemKey
	OrderNumber, Description string
	AccountID                uuid.UUID
}

// Purchases reads what purchase order items expensed (ledger context) and
// the items themselves (procurement context), ADR-0057 §2: the recorded
// price differences of a company and fiscal year per order item; what its
// invoice entries expensed without a recorded order item, per entry; what
// its GR/IR clearings put in profit or loss per order item; and the items
// of orders, removed ones included, an unknown order having none.
type Purchases interface {
	Recorded(ctx context.Context, legalEntity uuid.UUID, year int16) ([]OrderItemExpense, error)
	Unrecorded(ctx context.Context, legalEntity uuid.UUID, year int16) ([]UnrecordedExpense, error)
	Cleared(ctx context.Context, legalEntity uuid.UUID, year int16) ([]OrderItemCleared, error)
	Items(ctx context.Context, orders []uuid.UUID) ([]OrderItem, error)
}

// orderExpenseKey is an order item's place in the report's order.
type orderExpenseKey struct {
	Number string    `json:"n"`
	Order  uuid.UUID `json:"o"`
	Item   int32     `json:"i"`
}

// orderExpenseRow is an asset order item with what was expensed on it
// and what its GR/IR clearings put in profit or loss.
type orderExpenseRow struct {
	item    OrderItem
	account *config.Account
	expense OrderItemExpense
	cleared money.Minor
}

// orderExpenseTotals sums the rows.
func orderExpenseTotals(rows []orderExpenseRow) (total orderExpenseRow) {
	for _, r := range rows {
		total.expense.Amount, total.expense.TaxBorne = total.expense.Amount+r.expense.Amount, total.expense.TaxBorne+r.expense.TaxBorne
		total.cleared += r.cleared
	}
	return total
}

func (r orderExpenseRow) key() orderExpenseKey {
	return orderExpenseKey{Number: r.item.OrderNumber, Order: r.item.OrderID, Item: r.item.ItemNo}
}

func compareOrderExpenseKeys(a, b orderExpenseKey) int {
	return cmp.Or(cmp.Compare(a.Number, b.Number), slices.Compare(a.Order[:], b.Order[:]), cmp.Compare(a.Item, b.Item))
}

// GetAssetOrderExpenses implements GET /v1/asset-order-expenses (B10):
// the tax not recovered and the price differences expensed on the
// company's asset order items in a fiscal year, with their totals and
// what cannot be attributed.
func (e *Endpoints) GetAssetOrderExpenses(ctx context.Context, params gen.GetAssetOrderExpensesParams) (gen.GetAssetOrderExpensesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("getAssetOrderExpenses|%s|%d", params.LegalEntityID, params.FiscalYear)
	size, after, err := e.orderExpensesPage(params, scope)
	if err != nil {
		return nil, err
	}
	co, err := e.company(ctx, params.LegalEntityID)
	if err != nil {
		return nil, err
	}
	periods, err := e.Ledger.Periods(ctx, co.ID, int16(params.FiscalYear))
	if err != nil {
		return nil, fmt.Errorf("assets: periods of %s in %d: %w", co.Code, params.FiscalYear, err)
	}
	rows, un, err := e.orderExpenses(ctx, co, int16(params.FiscalYear))
	if err != nil {
		return nil, err
	}
	exp, err := currency.Exponent(ctx, tx, co.Currency, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: currency %s: %w", co.Currency, err)
	}
	m := func(a money.Minor) gen.Money { return apitypes.Money(a, co.Currency, exp) }
	total := orderExpenseTotals(rows)
	out := &gen.AssetOrderExpenses{LegalEntityID: co.ID, CompanyCode: co.Code, CompanyName: co.Name, FiscalYear: params.FiscalYear,
		StartDate: periods[0].Start, EndDate: periods[len(periods)-1].End, Currency: co.Currency, Items: []gen.AssetOrderExpenseItem{},
		Totals: gen.AssetOrderExpenseAmounts{TaxNotRecovered: m(total.expense.TaxBorne), PriceDifference: m(total.expense.Amount - total.expense.TaxBorne),
			Expensed: m(total.expense.Amount), GrIrCleared: m(total.cleared)},
		Unattributed: gen.AssetOrderExpensesUnattributed{Expensed: m(un.debits + un.credits), Debits: m(un.debits), Credits: m(un.credits),
			Entries: int32(un.entries)}}
	rows = rowsAfter(rows, after)
	if len(rows) > size {
		rows = rows[:size]
		tok, _ := e.Pages.Encode(scope, rows[size-1].key()) // a string, a uuid and a number always encode
		out.NextPageToken = gen.NewOptString(tok)
	}
	for _, r := range rows {
		out.Items = append(out.Items, gen.AssetOrderExpenseItem{PurchaseOrderID: r.item.OrderID, PurchaseOrderNumber: r.item.OrderNumber,
			PoItemNo: r.item.ItemNo, Description: r.item.Description, AccountID: r.item.AccountID, AccountNumber: r.account.Number,
			AccountName: r.account.Name, TaxNotRecovered: m(r.expense.TaxBorne), PriceDifference: m(r.expense.Amount - r.expense.TaxBorne),
			Expensed: m(r.expense.Amount), GrIrCleared: m(r.cleared)})
	}
	return out, nil
}

// orderExpensesPage reads the page size and the order item a page starts
// after (none: the first page).
func (e *Endpoints) orderExpensesPage(params gen.GetAssetOrderExpensesParams, scope string) (int, *orderExpenseKey, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil || !params.PageToken.Set {
		return size, nil, err
	}
	after := &orderExpenseKey{}
	return size, after, e.Pages.Decode(scope, params.PageToken.Value, after)
}

// rowsAfter are the rows after the key (nil: all of them): none when it
// names the last one.
func rowsAfter(rows []orderExpenseRow, after *orderExpenseKey) []orderExpenseRow {
	if after == nil {
		return rows
	}
	next := slices.IndexFunc(rows, func(r orderExpenseRow) bool { return compareOrderExpenseKeys(r.key(), *after) > 0 })
	if next < 0 {
		return nil
	}
	return rows[next:]
}

// orderExpenseReads are the ledger's reads the report is built from.
type orderExpenseReads struct {
	recorded   []OrderItemExpense
	unrecorded []UnrecordedExpense
	cleared    []OrderItemCleared
}

// readOrderExpenses reads what the company's order items expensed in the
// year, recorded and not, and what its GR/IR clearings put in profit or
// loss.
func (e *Endpoints) readOrderExpenses(ctx context.Context, co Company, year int16) (r orderExpenseReads, err error) {
	if r.recorded, err = e.Purchases.Recorded(ctx, co.ID, year); err != nil {
		return r, fmt.Errorf("assets: recorded price differences of %s in %d: %w", co.Code, year, err)
	}
	if r.unrecorded, err = e.Purchases.Unrecorded(ctx, co.ID, year); err != nil {
		return r, fmt.Errorf("assets: unrecorded price differences of %s in %d: %w", co.Code, year, err)
	}
	if r.cleared, err = e.Purchases.Cleared(ctx, co.ID, year); err != nil {
		return r, fmt.Errorf("assets: GR/IR clearings of %s in %d: %w", co.Code, year, err)
	}
	return r, nil
}

// orders are the orders the reads name, each once.
func (r orderExpenseReads) orders() []uuid.UUID {
	var orders []uuid.UUID
	for _, x := range r.recorded {
		orders = append(orders, x.OrderID)
	}
	for _, u := range r.unrecorded {
		for _, k := range u.Items {
			orders = append(orders, k.OrderID)
		}
	}
	for _, x := range r.cleared {
		orders = append(orders, x.OrderID)
	}
	slices.SortFunc(orders, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(orders)
}

// rows are the asset order items (assets) with a recorded price
// difference line or a GR/IR clearing in profit or loss, in the report's
// order.
func (r orderExpenseReads) rows(assets map[OrderItemKey]orderExpenseRow) []orderExpenseRow {
	byItem := map[OrderItemKey]*orderExpenseRow{}
	row := func(k OrderItemKey) *orderExpenseRow {
		asset, ok := assets[k]
		if !ok {
			return &orderExpenseRow{} // not an asset's item: its amounts go nowhere
		}
		if byItem[k] == nil {
			byItem[k] = &asset
		}
		return byItem[k]
	}
	for _, x := range r.recorded {
		row(x.OrderItemKey).expense = x
	}
	for _, x := range r.cleared {
		row(x.OrderItemKey).cleared = x.Amount
	}
	rows := make([]orderExpenseRow, 0, len(byItem))
	for _, x := range byItem {
		rows = append(rows, *x)
	}
	slices.SortFunc(rows, func(a, b orderExpenseRow) int { return compareOrderExpenseKeys(a.key(), b.key()) })
	return rows
}

// orderExpenses reads the company's asset order items with a recorded
// price difference line or a GR/IR clearing in profit or loss in the
// year, in the report's order, and what its invoice entries clearing
// GR/IR for an asset order item expensed without a recorded order item.
// An order item is an asset's when its account is an asset clearing
// account of the company in the active document (ADR-0057 §6): the
// account an asset bought on an order is received on.
func (e *Endpoints) orderExpenses(ctx context.Context, co Company, year int16) ([]orderExpenseRow, unattributedExpenses, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, unattributedExpenses{}, fmt.Errorf("assets: configuration: %w", err)
	}
	clearing, err := e.clearingAccounts(ctx, doc, co)
	if err != nil {
		return nil, unattributedExpenses{}, err
	}
	reads, err := e.readOrderExpenses(ctx, co, year)
	if err != nil {
		return nil, unattributedExpenses{}, err
	}
	items, err := e.Purchases.Items(ctx, reads.orders())
	if err != nil {
		return nil, unattributedExpenses{}, fmt.Errorf("assets: order items of %s: %w", co.Code, err)
	}
	// The asset order items: those on an asset clearing account.
	assets := map[OrderItemKey]orderExpenseRow{}
	for _, it := range items {
		if account := clearing[it.AccountID]; account != nil {
			assets[it.OrderItemKey] = orderExpenseRow{item: it, account: account}
		}
	}
	var un unattributedExpenses
	for _, u := range reads.unrecorded {
		if slices.ContainsFunc(u.Items, func(k OrderItemKey) bool { _, ok := assets[k]; return ok }) {
			un.debits, un.credits, un.entries = un.debits+u.Debits, un.credits+u.Credits, un.entries+1
		}
	}
	return reads.rows(assets), un, nil
}
