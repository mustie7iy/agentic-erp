package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Amount is an API amount: a decimal string and its currency.
type Amount struct{ Value, Currency string }

// ItemInput is an item of a quotation or a sales order as a client sends
// it: one shape for both documents (ADR-0067 §3).
type ItemInput struct {
	ItemKey     *string
	Description string
	// Quantity is a plain decimal string in the unit of measure Unit.
	Quantity, Unit string
	// UnitPrice is the price entered with the item; nil: the item takes
	// the price recorded for its item key (ADR-0067 §10).
	UnitPrice *Amount
	// PriceUnit is the quantity an entered unit price refers to; 0: the
	// request gave none, and an entered price is per 1. A recorded price
	// comes with its own, so one sent without a unit price is refused
	// (unitPrice).
	PriceUnit int32
	AccountID uuid.UUID
	TaxCode   *string
}

// Item is an item of a sales document as it is stored: what is sold, its
// quantity (a decimal string with up to six decimals) and unit, its unit
// price per price unit and its net value in minor units of the document's
// currency, where the price came from (Manual or PriceList), and the
// revenue account and output tax code its billing posts with.
type Item struct {
	ItemNo      int32       `json:"item_no"`
	ItemKey     *string     `json:"item_key"`
	Description string      `json:"description"`
	Quantity    string      `json:"quantity"`
	Unit        string      `json:"unit"`
	UnitPrice   money.Minor `json:"unit_price"`
	PriceUnit   int32       `json:"price_unit"`
	PriceSource string      `json:"price_source"`
	// Conditions are what the pricing procedure applied, in its order;
	// none for an item priced without one (ADR-0067 §11).
	Conditions []Condition `json:"conditions"`
	Net        money.Minor `json:"net_amount"`
	AccountID  uuid.UUID   `json:"account_id"`
	TaxCode    *string     `json:"tax_code"`
}

// document is what a sales document's items are checked and priced
// against: its company, its customer, whose prices its items take, its
// currency with its minor-unit digits, its date, the company's today in
// the time zone it is told in, and the tenant's pricing procedure, its
// condition types in the order they apply, none for a tenant without one.
type document struct {
	le        tenancy.Entity
	customer  uuid.UUID
	procedure []config.ConditionType
	currency  string
	exp       int
	date      time.Time
	zone      *time.Location
}

// errTooLarge refuses a document whose amounts are out of range.
func errTooLarge() *problem.Problem {
	return problem.Invalid("sales.amount_too_large", "items", "The items' net values, or their sum, are too large.")
}

// items checks and prices a document's items (ADR-0067 §3): at least one,
// each a positive quantity in a known unit of measure, priced (price), on
// an account postable in the company, with an output tax code in force on
// the document's date when it names one. It returns them numbered from 1
// in the request's order with their sum, exact in minor units. Every
// refusal names the request's own field.
func (e *Endpoints) items(ctx context.Context, tx pgx.Tx, doc document, in []ItemInput) ([]Item, money.Minor, error) {
	if len(in) == 0 {
		return nil, 0, problem.Invalid("sales.items_required", "items", "A sales document has at least one item.")
	}
	named := make([]string, len(in))
	for i, it := range in {
		named[i] = it.Unit
	}
	units, err := knownUnits(ctx, tx, named)
	if err != nil {
		return nil, 0, err
	}
	check := itemCheck{e: e, tx: tx, doc: doc, units: units, accounts: map[uuid.UUID]bool{}, codes: map[string]bool{}}
	items, nets := make([]Item, len(in)), make([]money.Minor, len(in))
	for i, it := range in {
		if items[i], err = check.item(ctx, i, it); err != nil {
			return nil, 0, err
		}
		nets[i] = items[i].Net
	}
	net, err := money.Sum(nets...)
	if err != nil { // money.Sum's only error is the range
		return nil, 0, errTooLarge()
	}
	return items, net, nil
}

// knownUnits are the units of measure among those named that the
// reference data knows.
func knownUnits(ctx context.Context, tx pgx.Tx, named []string) (map[string]bool, error) {
	rows, _ := tx.Query(ctx, `SELECT code FROM ref.unit_of_measure WHERE code = ANY($1) ORDER BY code`, named)
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("sales: read units of measure: %w", err)
	}
	known := make(map[string]bool, len(codes))
	for _, c := range codes {
		known[c] = true
	}
	return known, nil
}

// itemCheck checks the items of one document, asking the ledger about
// each account and tax code once.
type itemCheck struct {
	e        *Endpoints
	tx       pgx.Tx
	doc      document
	units    map[string]bool
	accounts map[uuid.UUID]bool // postable in the company
	codes    map[string]bool    // output tax codes in force on the document's date
}

// item checks and prices item i.
func (c itemCheck) item(ctx context.Context, i int, it ItemInput) (Item, error) {
	field := fmt.Sprintf("items[%d]", i)
	if err := namesWhatIsSold(field, it); err != nil {
		return Item{}, err
	}
	if money.PositiveQuantity(it.Quantity) != nil {
		return Item{}, problem.Invalid("sales.invalid_quantity", field+".quantity.value", fmt.Sprintf(
			"A quantity is a plain decimal more than zero of at most %d integer digits and %d decimals.", money.QuantityIntDigits, money.QuantityDecimals))
	}
	if !c.units[it.Unit] {
		return Item{}, problem.Invalid("sales.unknown_unit", field+".quantity.unit", "Unit of measure "+it.Unit+" is not known.")
	}
	p, err := price(ctx, c.tx, c.doc, i, it)
	if err != nil {
		return Item{}, err
	}
	if err := c.account(ctx, i, it.AccountID); err != nil {
		return Item{}, err
	}
	if it.TaxCode != nil {
		if err := c.outputCode(ctx, i, *it.TaxCode); err != nil {
			return Item{}, err
		}
	}
	return Item{ItemNo: int32(i + 1), ItemKey: it.ItemKey, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit,
		UnitPrice: p.UnitPrice, PriceUnit: p.PriceUnit, PriceSource: p.Source, Conditions: p.Conditions, Net: p.Net, AccountID: it.AccountID, TaxCode: it.TaxCode}, nil
}

// namesWhatIsSold refuses an item whose description, or whose item key
// when it gives one, is white space alone (422 request.validation_failed
// naming the field), as a purchasing item's description is refused: the
// description is all that says what is sold, and prices are found by the
// key (T80a), so a blank one would be a key of its own.
func namesWhatIsSold(field string, it ItemInput) error {
	if err := problem.RequireText(field+".description", it.Description); err != nil {
		return fmt.Errorf("sales: description of an item: %w", err)
	}
	if it.ItemKey == nil {
		return nil
	}
	if err := problem.RequireText(field+".item_key", *it.ItemKey); err != nil {
		return fmt.Errorf("sales: item key of an item: %w", err)
	}
	return nil
}

// account requires item i's revenue account to be postable in the
// company (422 sales.account_not_postable naming the item's field).
func (c itemCheck) account(ctx context.Context, i int, id uuid.UUID) error {
	ok, asked := c.accounts[id]
	if !asked {
		var err error
		if ok, err = c.e.Accounts.IsPostable(ctx, c.doc.le.ID, id); err != nil {
			return fmt.Errorf("sales: account of item %d: %w", i+1, err)
		}
		c.accounts[id] = ok
	}
	if !ok {
		return problem.Invalid("sales.account_not_postable", fmt.Sprintf("items[%d].account_id", i), "The account cannot be posted in this legal entity.")
	}
	return nil
}

// outputCode requires item i's tax code to be an output tax code of the
// company with a rate in force on the document's date (422
// sales.unknown_tax_code naming the item's field). A code whose component
// levies the subdivision its lines name (one code for every emirate,
// ADR-0049 §7) is refused as a customer invoice refuses it (ADR-0063 §4):
// an item names no jurisdiction, so it could never be billed. The tax
// itself is calculated when the item is billed (ADR-0067 §3).
func (c itemCheck) outputCode(ctx context.Context, i int, code string) error {
	if c.codes[code] {
		return nil
	}
	field := fmt.Sprintf("items[%d].tax_code", i)
	t, ok, err := c.e.Accounts.TaxCode(ctx, c.doc.le.ID, code, c.doc.date)
	if err != nil {
		return fmt.Errorf("sales: tax code %s of item %d: %w", code, i+1, err)
	}
	if !ok || t.Direction != config.TaxOutput {
		return problem.Invalid("sales.unknown_tax_code", field,
			"Tax code "+code+" is not an output tax code of the legal entity on "+c.doc.date.Format(time.DateOnly)+".")
	}
	for _, component := range t.Components {
		if component.JurisdictionOnLine {
			return problem.Invalid("sales.tax_jurisdiction_not_supported", field,
				"Tax code "+code+" levies the tax of the subdivision each line names; a sales document's items name none.")
		}
	}
	c.codes[code] = true
	return nil
}

// insertItems writes a new document's items; table is an item table of
// the sales schema and column its document's id. The items' conditions go
// as one JSON array of arrays, each item's picked by its place; an item
// priced without a procedure has none and is stored with an empty array.
func insertItems(ctx context.Context, tx pgx.Tx, table, column string, tenant, doc uuid.UUID, items []Item) error {
	n := len(items)
	nos, keys, descriptions, quantities, units := make([]int32, n), make([]*string, n), make([]string, n), make([]string, n), make([]string, n)
	prices, priceUnits, nets, accounts, codes := make([]money.Minor, n), make([]int32, n), make([]money.Minor, n), make([]uuid.UUID, n), make([]*string, n)
	sources, conditions := make([]string, n), make([][]Condition, n)
	for i, it := range items {
		nos[i], keys[i], descriptions[i], quantities[i], units[i] = it.ItemNo, it.ItemKey, it.Description, it.Quantity, it.Unit
		prices[i], priceUnits[i], nets[i], accounts[i], codes[i] = it.UnitPrice, it.PriceUnit, it.Net, it.AccountID, it.TaxCode
		sources[i], conditions[i] = it.PriceSource, it.Conditions
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{"sales", table}.Sanitize()+` (tenant_id, `+pgx.Identifier{column}.Sanitize()+`, item_no, item_key,
			description, quantity, uom_code, unit_price, price_unit, net_amount, account_id, tax_code, price_source, conditions)
		SELECT $1, $2, n, k, d, q::numeric, u, p, pu, na, ac, tc, ps, coalesce(nullif($14::jsonb -> (ord::int - 1), 'null'), '[]')
		FROM unnest($3::int[], $4::text[], $5::text[], $6::text[], $7::text[], $8::bigint[], $9::int[], $10::bigint[], $11::uuid[], $12::text[],
			$13::text[]) WITH ORDINALITY AS x(n, k, d, q, u, p, pu, na, ac, tc, ps, ord)`,
		tenant, doc, nos, keys, descriptions, quantities, units, prices, priceUnits, nets, accounts, codes, sources, conditions); err != nil {
		return fmt.Errorf("sales: write items of sales.%s %s: %w", table, doc, err)
	}
	return nil
}

// itemsJSON is the SQL that reads a document's items by item number, so
// every read lists them in the order they were entered; table is an item
// table of the sales schema, column its document's id, doc the alias of
// the document's row and extra further key and value pairs of the item
// (an order item's delivered and billed quantities), led by a comma.
func itemsJSON(table, column, doc, extra string) string {
	return `coalesce((SELECT json_agg(json_build_object('item_no', t.item_no, 'item_key', t.item_key, 'description', t.description,
			'quantity', trim_scale(t.quantity)::text, 'unit', t.uom_code, 'unit_price', t.unit_price, 'price_unit', t.price_unit,
			'price_source', t.price_source, 'conditions', t.conditions, 'net_amount', t.net_amount, 'account_id', t.account_id, 'tax_code', t.tax_code` + extra + `) ORDER BY t.item_no)
		 FROM ` + pgx.Identifier{"sales", table}.Sanitize() + ` t WHERE t.tenant_id = ` + doc + `.tenant_id AND t.` + pgx.Identifier{column}.Sanitize() + ` = ` + doc + `.id), '[]')`
}
