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
// stated as one amount whose split is unknown. Nothing is posted.

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

// UnrecordedExpense is what one invoice or credit memo entry put on the
// price difference account without a recorded order item, in local
// currency, and the order items the entry posts on.
type UnrecordedExpense struct {
	Amount money.Minor
	Items  []OrderItemKey
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
// invoice entries expensed without a recorded order item, per entry; and
// the items of orders, removed ones included, an unknown order having
// none.
type Purchases interface {
	Recorded(ctx context.Context, legalEntity uuid.UUID, year int16) ([]OrderItemExpense, error)
	Unrecorded(ctx context.Context, legalEntity uuid.UUID, year int16) ([]UnrecordedExpense, error)
	Items(ctx context.Context, orders []uuid.UUID) ([]OrderItem, error)
}

// orderExpenseKey is an order item's place in the report's order.
type orderExpenseKey struct {
	Number string    `json:"n"`
	Order  uuid.UUID `json:"o"`
	Item   int32     `json:"i"`
}

// orderExpenseRow is an asset order item with what was expensed on it.
type orderExpenseRow struct {
	item    OrderItem
	account *config.Account
	expense OrderItemExpense
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
	rows, unattributed, entries, err := e.orderExpenses(ctx, co, int16(params.FiscalYear))
	if err != nil {
		return nil, err
	}
	exp, err := currency.Exponent(ctx, tx, co.Currency, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: currency %s: %w", co.Currency, err)
	}
	m := func(a money.Minor) gen.Money { return apitypes.Money(a, co.Currency, exp) }
	var total OrderItemExpense
	for _, r := range rows {
		total.Amount, total.TaxBorne = total.Amount+r.expense.Amount, total.TaxBorne+r.expense.TaxBorne
	}
	out := &gen.AssetOrderExpenses{LegalEntityID: co.ID, CompanyCode: co.Code, CompanyName: co.Name, FiscalYear: params.FiscalYear,
		StartDate: periods[0].Start, EndDate: periods[len(periods)-1].End, Currency: co.Currency, Items: []gen.AssetOrderExpenseItem{},
		Totals:       gen.AssetOrderExpenseAmounts{TaxNotRecovered: m(total.TaxBorne), PriceDifference: m(total.Amount - total.TaxBorne), Expensed: m(total.Amount)},
		Unattributed: gen.AssetOrderExpensesUnattributed{Expensed: m(unattributed), Entries: int32(entries)}}
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
			Expensed: m(r.expense.Amount)})
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

// orderExpenses reads the company's asset order items with a recorded
// price difference line in the year, in the report's order, and what its
// invoice entries touching an asset order item expensed without a
// recorded order item, with the number of those entries. An order item
// is an asset's when its account is an asset clearing account of the
// company in the active document (ADR-0057 §6): the account an asset
// bought on an order is received on.
func (e *Endpoints) orderExpenses(ctx context.Context, co Company, year int16) ([]orderExpenseRow, money.Minor, int, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("assets: configuration: %w", err)
	}
	clearing, err := e.clearingAccounts(ctx, doc, co)
	if err != nil {
		return nil, 0, 0, err
	}
	recorded, err := e.Purchases.Recorded(ctx, co.ID, year)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("assets: recorded price differences of %s in %d: %w", co.Code, year, err)
	}
	unrecorded, err := e.Purchases.Unrecorded(ctx, co.ID, year)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("assets: unrecorded price differences of %s in %d: %w", co.Code, year, err)
	}
	var orders []uuid.UUID
	for _, r := range recorded {
		orders = append(orders, r.OrderID)
	}
	for _, u := range unrecorded {
		for _, k := range u.Items {
			orders = append(orders, k.OrderID)
		}
	}
	slices.SortFunc(orders, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	items, err := e.Purchases.Items(ctx, slices.Compact(orders))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("assets: order items of %s: %w", co.Code, err)
	}
	// The asset order items: those on an asset clearing account.
	assets := map[OrderItemKey]orderExpenseRow{}
	for _, it := range items {
		if account := clearing[it.AccountID]; account != nil {
			assets[it.OrderItemKey] = orderExpenseRow{item: it, account: account}
		}
	}
	var rows []orderExpenseRow
	for _, r := range recorded {
		if row, ok := assets[r.OrderItemKey]; ok {
			row.expense = r
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b orderExpenseRow) int { return compareOrderExpenseKeys(a.key(), b.key()) })
	var unattributed money.Minor
	entries := 0
	for _, u := range unrecorded {
		if slices.ContainsFunc(u.Items, func(k OrderItemKey) bool { _, ok := assets[k]; return ok }) {
			unattributed += u.Amount
			entries++
		}
	}
	return rows, unattributed, entries, nil
}
