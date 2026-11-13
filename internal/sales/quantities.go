package sales

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ItemQuantity is a quantity of one item of a document as a client sends
// it: the item's number in that document and a plain decimal string in
// the item's unit. A delivery names a sales order's item, a returns
// order a billing document's, a return receipt a returns order's.
type ItemQuantity struct {
	ItemNo         int32
	Quantity, Unit string
}

// taking is how the quantities a request takes of a document's items are
// checked (deliveries, returns orders, return receipts): each names an
// item, once, with a quantity more than zero in the item's unit that is
// at most what is open of it. The document was read under the row lock
// that serializes the requests taking of it (ADR-0008). Every refusal is
// a 422 naming the request's own field.
type taking struct {
	// document is what takes the quantities, as a refusal begins with
	// it: "A delivery".
	document string
	// noField is the request's field that names the item, under items[i].
	noField string
	// find gives the item of the number: its unit and what is open of it.
	find func(no int32) (unit string, open *big.Rat, ok bool)
	// otherUnit words the refusal of a quantity in another unit than the
	// item's.
	otherUnit func(no int32, unit string) string
}

// untaken is the first quantity of a request that its document cannot
// take for a reason of the document's own, which the caller refuses with
// its own code: it names an item the document does not have (open nil),
// or it is more than what is open of the item.
type untaken struct {
	// noField and quantityField are the request's fields that name the
	// item and its quantity.
	noField, quantityField string
	no                     int32
	open                   *big.Rat
	quantity               string
}

// check refuses the first quantity that cannot be taken whatever the
// document: none at all (422 sales.items_required), an item named twice
// (sales.item_repeated), a quantity that is no plain decimal above zero
// (sales.invalid_quantity), another unit than the item's
// (sales.unit_mismatch). One the document cannot take is returned for
// the caller to refuse (untaken); nil, nil: every quantity can be taken.
func (t taking) check(in []ItemQuantity) (*untaken, error) {
	if len(in) == 0 {
		return nil, problem.Invalid("sales.items_required", "items", t.document+" has at least one item.")
	}
	named := make(map[int32]bool, len(in))
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		short := untaken{noField: field + "." + t.noField, quantityField: field + ".quantity.value", no: it.ItemNo, quantity: it.Quantity}
		unit, open, ok := t.find(it.ItemNo)
		switch {
		case !ok:
			return &short, nil
		case named[it.ItemNo]:
			return nil, problem.Invalid("sales.item_repeated", short.noField,
				fmt.Sprintf("%s names an item once; item %d is named more than once.", t.document, it.ItemNo))
		case money.PositiveQuantity(it.Quantity) != nil:
			return nil, problem.Invalid("sales.invalid_quantity", short.quantityField, fmt.Sprintf(
				"A quantity is a plain decimal more than zero of at most %d integer digits and %d decimals.", money.QuantityIntDigits, money.QuantityDecimals))
		case it.Unit != unit:
			return nil, problem.Invalid("sales.unit_mismatch", field+".quantity.unit", t.otherUnit(it.ItemNo, unit))
		}
		named[it.ItemNo] = true
		if money.ParseRat(it.Quantity).Cmp(open) > 0 {
			short.open = open
			return &short, nil
		}
	}
	return nil, nil
}

// confirmation names the tables of the sales schema a confirmation of
// quantities is written to (a delivery, a return receipt): its table
// with the columns of its document, of its number within that document
// and of its date, and its items' table with the columns of the
// confirmation and of the document's item.
type confirmation struct {
	table, document, number, date string
	items, parent, item           string
}

// insert writes a confirmation of the document dated date with its
// quantities and returns its id. Its number is the next within the
// document, whose row lock the caller holds. The database's triggers add
// each quantity to the document's item and move the document's version.
func (c confirmation) insert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, document uuid.UUID, date time.Time, in []ItemQuantity) (uuid.UUID, error) {
	name := func(s ...string) string { return pgx.Identifier(s).Sanitize() }
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO `+name("sales", c.table)+` (tenant_id, id, `+name(c.document)+`, `+name(c.number)+`, `+name(c.date)+`, created_by)
		SELECT $1, $2, $3, coalesce(max(`+name(c.number)+`), 0) + 1, $4, $5 FROM `+name("sales", c.table)+` WHERE `+name(c.document)+` = $3`,
		p.TenantID, id, document, date, p.ID); err != nil {
		return uuid.Nil, fmt.Errorf("sales: insert a sales.%s of %s: %w", c.table, document, err)
	}
	nos, quantities := make([]int32, len(in)), make([]string, len(in))
	for i, it := range in {
		nos[i], quantities[i] = it.ItemNo, it.Quantity
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+name("sales", c.items)+` (tenant_id, `+name(c.parent)+`, `+name(c.document)+`, `+name(c.item)+`, quantity)
		SELECT $1, $2, $3, n, q::numeric FROM unnest($4::int[], $5::text[]) AS x(n, q)`, p.TenantID, id, document, nos, quantities); err != nil {
		return uuid.Nil, fmt.Errorf("sales: write items of sales.%s %s: %w", c.table, id, err)
	}
	return id, nil
}

// listed is how the confirmations of one document are listed in pages by
// date: the query that reads those of the document $1, how a row is
// scanned, a confirmation's date and id, and how the API gives one.
type listed[T, A any] struct {
	what  string
	sort  pagination.Sort
	query string
	scan  func(pgx.Row) (T, error)
	key   func(T) (time.Time, uuid.UUID)
	api   func(T) A
}

// page reads one page of the document's confirmations for the operation.
func (l listed[T, A]) page(ctx context.Context, e *Endpoints, tx pgx.Tx, operation string, document uuid.UUID, page pagination.Request) ([]A, gen.OptString, error) {
	ks, err := e.Pages.Keyset(l.sort, fmt.Sprintf("%s|%s", operation, document), page)
	if err != nil {
		return nil, gen.OptString{}, fmt.Errorf("sales: page of %s: %w", l.what, err)
	}
	query, args := ks.SQL(l.query, []any{document})
	rows, _ := tx.Query(ctx, query, args...)
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return l.scan(r) })
	if err != nil {
		return nil, gen.OptString{}, fmt.Errorf("sales: list the %s of %s: %w", l.what, document, err)
	}
	items, next := apitypes.PageOf(ks, found, func(t T) (string, uuid.UUID) {
		day, id := l.key(t)
		return day.Format(time.DateOnly), id
	}, l.api)
	return items, next, nil
}
