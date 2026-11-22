package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Intercompany billing (S2i2; ADR-0082 As built S2i): the supplying
// company of an intercompany stock transport order bills the order's
// posted goods issue (643) to the business partner that is the receiving
// company, every item at the order's price, the transfer price, and posts
// it at once as a customer invoice through receivables (SAP's
// intercompany billing, billing type IV, from memory). The order is the
// receiving company's purchase order (procurement), which sales reads
// through a port the composition root wires (IntercompanyOrders). The
// billing is no billing document: those bill a sales order's delivered
// items. It is written once its invoice has posted and never changes; a
// goods issue is billed once (intercompany_billing_once). The receiving
// company's supplier invoice from it is S2i3.

func init() {
	// The handler reads first; the unique key is the rule for two
	// requests racing for one goods issue, and answers alike.
	problem.RegisterConstraint("intercompany_billing_once", func() *problem.Problem {
		return problem.Conflict("sales.intercompany_billed", "The goods issue of the intercompany order is billed already; a goods issue is billed once.")
	})
}

// IntercompanyOrders reads the intercompany stock transport orders the
// supplying company bills (procurement context, through the composition
// root): an order of a company the caller may act in, under a share lock
// until the transaction ends, or a 404 problem.
type IntercompanyOrders interface {
	IssuedOrder(ctx context.Context, id uuid.UUID) (IssuedOrder, error)
}

// IssuedOrder is an intercompany stock transport order as its billing
// reads it: the receiving company's order, its supplying plant (nil for an
// order that is no intercompany one), its currency with the currency's
// minor-unit digits and, once posted, its goods issue (IssueDocumentID nil
// until then).
type IssuedOrder struct {
	ID                  uuid.UUID
	DocumentNumber      string
	LegalEntityID       uuid.UUID
	SupplyingPlantID    *uuid.UUID
	Currency            string
	Exponent            int
	IssueDocumentID     *uuid.UUID
	IssueDocumentNumber *string
	IssueDate           *time.Time
	Items               []IntercompanyOrderItem
}

// IntercompanyOrderItem is an item of an intercompany order: its material, what the
// order calls it, its ordered quantity (which the goods issue issued) in
// its unit, its price per price unit in minor units of the order's
// currency and its value, as the order values it.
type IntercompanyOrderItem struct {
	No          int32
	MaterialID  uuid.UUID
	Description string
	Quantity    string
	Unit        string
	UnitPrice   money.Minor
	PriceUnit   int32
	Net         money.Minor
}

// IntercompanyItem is an item of an intercompany billing: what Item says
// of a sales item (its material, description, quantity, price, amount,
// revenue account, tax code and jurisdiction), the order item it bills
// and the tax its invoice item bore.
type IntercompanyItem struct {
	Item
	POItemNo int32       `json:"po_item_no"`
	Tax      money.Minor `json:"tax_amount"`
}

// IntercompanyBilling is a row of sales.intercompany_billing with its
// items: the supplying company's (LegalEntityID) bill of an intercompany
// order's goods issue to the business partner that is the receiving
// company (ReceivingID), posted as the customer invoice InvoiceID with the
// entry EntryID. Amounts are minor units of Currency, whose minor-unit
// digits are Exponent; Net is the items' sum and Tax the invoice's.
type IntercompanyBilling struct {
	ID                  uuid.UUID
	DocumentNumber      string
	LegalEntityID       uuid.UUID
	CustomerID          uuid.UUID
	ReceivingID         uuid.UUID
	OrderID             uuid.UUID
	OrderNumber         string
	IssueDocumentID     uuid.UUID
	IssueDocumentNumber string
	BillingDate         time.Time
	PostingDate         time.Time
	Currency            string
	Exponent            int
	Net, Tax            money.Minor
	InvoiceID           uuid.UUID
	InvoiceNumber       string
	EntryYear           int16
	EntryID             uuid.UUID
	Items               []IntercompanyItem
	CreatedAt           time.Time
	CreatedBy           uuid.UUID
}

// Gross is net and tax: what the receiving company owes (in range: the
// invoice's gross, which receivables checked).
func (b IntercompanyBilling) Gross() money.Minor { return b.Net + b.Tax }

// IntercompanyInput is an intercompany billing as a client sends it: the
// order, the customer, optional dates and how each order item is billed.
type IntercompanyInput struct {
	OrderID, CustomerID      uuid.UUID
	BillingDate, PostingDate *time.Time
	Items                    []IntercompanyItemInput
}

// IntercompanyItemInput names the supplying company's revenue account and
// output tax an order item is billed on.
type IntercompanyItemInput struct {
	POItemNo                 int32
	AccountID                uuid.UUID
	TaxCode, TaxJurisdiction *string
}

const intercompanySelect = `SELECT b.id, b.document_number, b.legal_entity_id, b.customer_id, b.receiving_legal_entity_id, b.purchase_order_id,
		b.purchase_order_number, b.issue_document_id, b.issue_document_number, b.billing_date, b.posting_date, b.currency, c.exponent, b.net_amount,
		b.tax_amount, b.customer_invoice_id, b.customer_invoice_number, b.journal_entry_year, b.journal_entry_id,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'po_item_no', i.po_item_no, 'material_id', i.material_id, 'description', i.description,
			'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code, 'unit_price', i.unit_price, 'price_unit', i.price_unit, 'net_amount', i.net_amount,
			'account_id', i.account_id, 'tax_code', i.tax_code, 'tax_jurisdiction', i.tax_jurisdiction, 'tax_amount', i.tax_amount) ORDER BY i.item_no)
		 FROM sales.intercompany_billing_item i WHERE i.tenant_id = b.tenant_id AND i.billing_id = b.id),
		b.created_at, b.created_by
	FROM sales.intercompany_billing b JOIN ref.currency c ON c.code = b.currency`

func scanIntercompany(row pgx.Row) (IntercompanyBilling, error) {
	var b IntercompanyBilling
	err := row.Scan(&b.ID, &b.DocumentNumber, &b.LegalEntityID, &b.CustomerID, &b.ReceivingID, &b.OrderID, &b.OrderNumber, &b.IssueDocumentID,
		&b.IssueDocumentNumber, &b.BillingDate, &b.PostingDate, &b.Currency, &b.Exponent, &b.Net, &b.Tax, &b.InvoiceID, &b.InvoiceNumber, &b.EntryYear,
		&b.EntryID, &b.Items, &b.CreatedAt, &b.CreatedBy)
	return b, errs.Wrapf(err, "scan intercompany billing")
}

// GetIntercompany returns an intercompany billing the caller may
// act in either company of: the supplying company's bill, the receiving
// company's invoice to enter (S2i3). Another is not found, so ids cannot
// be probed (04 §4.9).
func GetIntercompany(ctx context.Context, id uuid.UUID) (IntercompanyBilling, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return IntercompanyBilling{}, fmt.Errorf("sales: company access for intercompany billing %s: %w", id, err)
	}
	b, err := scanIntercompany(tx.QueryRow(ctx, intercompanySelect+` WHERE b.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !access.Allows(b.LegalEntityID) && !access.Allows(b.ReceivingID) {
		return IntercompanyBilling{}, problem.NotFoundOf("intercompany billing")
	}
	if err != nil {
		return IntercompanyBilling{}, fmt.Errorf("sales: read intercompany billing %s: %w", id, err)
	}
	return b, nil
}

// intercompanyOrder is what a billing bills: the order, read under its
// share lock, its supplying plant and the supplying company, which bills.
type intercompanyOrder struct {
	IssuedOrder
	plant    tenancy.Unit
	supplier tenancy.Entity
}

// BillIntercompany bills an intercompany order's goods issue and
// posts it as a customer invoice, in the request's transaction (S2i2):
// the order of a company the caller may act in, supplied by a plant of a
// company the caller may act in too (422 request.reference_not_found
// naming purchase_order_id otherwise), an intercompany one (422
// sales.not_intercompany) whose goods issue is posted (409
// sales.intercompany_not_issued) and not billed (409
// sales.intercompany_billed); the customer the business partner that is
// the order's company (422 sales.intercompany_customer); every order item
// once (intercompanyItems). It is dated the billing date given, else the
// supplying company's date today, not before the goods issue (422
// sales.billing_before_issue), and numbered from that company's
// billing_document range. The invoice (intercompanyInvoice) and the
// billing are written together, or nothing is; the audit record names
// the billing, the order and the invoice.
func (e *Endpoints) BillIntercompany(ctx context.Context, in IntercompanyInput) (IntercompanyBilling, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	o, err := e.billedOrder(ctx, tx, in.OrderID)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	if err := e.intercompanyCustomer(ctx, o, in.CustomerID); err != nil {
		return IntercompanyBilling{}, err
	}
	date, err := e.billingDate(ctx, o.supplier.Code, in.BillingDate)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	if date.Before(*o.IssueDate) { // an issued order has its issue's day (purchase_order_issue)
		return IntercompanyBilling{}, problem.Invalid("sales.billing_before_issue", "billing_date", fmt.Sprintf(
			"on or after %s, the day of goods issue %s", o.IssueDate.Format(time.DateOnly), *o.IssueDocumentNumber))
	}
	items, err := e.intercompanyItems(ctx, o, date, in.Items)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	number, err := e.number(ctx, o.supplier.ID, BillingRange, date)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	b := IntercompanyBilling{ID: uuid.Must(uuid.NewV7()), DocumentNumber: number, LegalEntityID: o.supplier.ID, CustomerID: in.CustomerID,
		ReceivingID: o.LegalEntityID, OrderID: o.ID, OrderNumber: o.DocumentNumber, IssueDocumentID: *o.IssueDocumentID,
		IssueDocumentNumber: *o.IssueDocumentNumber, BillingDate: date, Currency: o.Currency, Exponent: o.Exponent, Items: items}
	v, err := e.intercompanyInvoice(ctx, b, in.PostingDate)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	billed, err := insertIntercompany(ctx, tx, p, b, v)
	if err != nil {
		return IntercompanyBilling{}, err
	}
	return billed, recordIntercompany(ctx, billed, o.supplier)
}

// billedOrder reads the order a billing bills and refuses one that cannot
// be billed (BillIntercompany): the share lock it is read under
// holds what is billed of it, and the goods issue is billed once, which
// the unique key decides for two requests racing.
func (e *Endpoints) billedOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID) (intercompanyOrder, error) {
	const field, missing = "purchase_order_id", "no intercompany order supplied by a company you may act in, of a company you may act in"
	o, err := e.Intercompany.IssuedOrder(ctx, id)
	if err != nil {
		return intercompanyOrder{}, problem.Reference(err, field, "purchase order", missing)
	}
	if o.SupplyingPlantID == nil {
		return intercompanyOrder{}, problem.Invalid("sales.not_intercompany", field, "Purchase order "+o.DocumentNumber+
			" names no supplying plant; only an intercompany stock transport order's goods issue is billed.")
	}
	units, err := e.LegalEntities.OrgUnits(ctx, []uuid.UUID{*o.SupplyingPlantID})
	if err != nil {
		return intercompanyOrder{}, fmt.Errorf("sales: supplying plant of purchase order %s: %w", o.DocumentNumber, err)
	}
	plant := units[*o.SupplyingPlantID] // an order's supplying plant is a plant of the tenant (supplyingPlant)
	access, err := authz.Companies(ctx)
	if err != nil {
		return intercompanyOrder{}, fmt.Errorf("sales: company access for purchase order %s: %w", o.DocumentNumber, err)
	}
	if !access.Allows(plant.LegalEntityID) {
		return intercompanyOrder{}, problem.ReferenceNotFound(field, "purchase order", missing)
	}
	if o.IssueDocumentID == nil {
		return intercompanyOrder{}, problem.Conflict("sales.intercompany_not_issued", "The goods issue of purchase order "+o.DocumentNumber+
			" is not posted yet; its billing bills what the goods issue issued.")
	}
	var earlier string
	switch err := tx.QueryRow(ctx, `SELECT document_number FROM sales.intercompany_billing WHERE issue_document_id = $1`, *o.IssueDocumentID).Scan(&earlier); {
	case err == nil:
		return intercompanyOrder{}, problem.Conflict("sales.intercompany_billed", "Goods issue "+*o.IssueDocumentNumber+" of purchase order "+
			o.DocumentNumber+" is billed already, by "+earlier+"; a goods issue is billed once.")
	case !errors.Is(err, pgx.ErrNoRows):
		return intercompanyOrder{}, fmt.Errorf("sales: billing of goods issue %s: %w", *o.IssueDocumentNumber, err)
	}
	le, _, err := e.LegalEntities.Entity(ctx, plant.LegalEntityID) // a plant's company exists
	if err != nil {
		return intercompanyOrder{}, fmt.Errorf("sales: company of supplying plant %s: %w", plant.Code, err)
	}
	return intercompanyOrder{IssuedOrder: o, plant: plant, supplier: le}, nil
}

// intercompanyCustomer refuses a customer that is not the business partner
// whose group company is the order's company (422
// sales.intercompany_customer naming customer_id); one that does not exist
// is not found (422 request.reference_not_found). Whether it may be
// invoiced in the supplying company is receivables' check.
func (e *Endpoints) intercompanyCustomer(ctx context.Context, o intercompanyOrder, id uuid.UUID) error {
	c, ok, err := e.CustomerData.Customer(ctx, id, o.supplier.ID)
	if err != nil {
		return fmt.Errorf("sales: customer %s of an intercompany billing: %w", id, err)
	}
	if !ok {
		return problem.ReferenceNotFound("customer_id", "business partner", "no business partner with this id")
	}
	if c.GroupCompanyID == nil || *c.GroupCompanyID != o.LegalEntityID {
		return problem.Invalid("sales.intercompany_customer", "customer_id",
			"the business partner that is purchase order "+o.DocumentNumber+"'s company (its group company)")
	}
	return nil
}

// intercompanyItems are the billing's items, in the request's order and
// numbered from 1: each order item once (422 sales.intercompany_items
// naming items otherwise), its quantity, price and value the order's, on
// the account and tax code the request names. An item whose code levies
// the UAE's emirates and that names none takes the supplying plant's
// (defaultedTo): the seller's side, as a sales item's default.
func (e *Endpoints) intercompanyItems(ctx context.Context, o intercompanyOrder, date time.Time, in []IntercompanyItemInput) ([]IntercompanyItem, error) {
	ordered := make(map[int32]IntercompanyOrderItem, len(o.Items))
	for _, it := range o.Items {
		ordered[it.No] = it
	}
	once := fmt.Sprintf("each of purchase order %s's %d items once", o.DocumentNumber, len(o.Items))
	if len(in) != len(o.Items) {
		return nil, problem.Invalid("sales.intercompany_items", "items", once)
	}
	codes := map[string]ledger.TaxCode{}
	out := make([]IntercompanyItem, len(in))
	for i, it := range in {
		src, ok := ordered[it.POItemNo]
		if !ok {
			return nil, problem.Invalid("sales.intercompany_items", fmt.Sprintf("items[%d].po_item_no", i), once)
		}
		delete(ordered, it.POItemNo)
		jurisdiction, err := e.intercompanyJurisdiction(ctx, o, codes, it, date)
		if err != nil {
			return nil, err
		}
		material := src.MaterialID
		out[i] = IntercompanyItem{Item: Item{ItemNo: int32(i + 1), MaterialID: &material, Description: src.Description, Quantity: src.Quantity,
			Unit: src.Unit, UnitPrice: src.UnitPrice, PriceUnit: src.PriceUnit, Net: src.Net, AccountID: it.AccountID, TaxCode: it.TaxCode,
			TaxJurisdiction: jurisdiction}, POItemNo: src.No}
	}
	return out, nil
}

// intercompanyJurisdiction is the item's jurisdiction: the one given,
// else the supplying plant's region where the item's code levies the
// UAE's emirates (defaultedTo); a code that is no output tax code of the
// company on the date takes none, and receivables refuses it.
func (e *Endpoints) intercompanyJurisdiction(ctx context.Context, o intercompanyOrder, codes map[string]ledger.TaxCode, it IntercompanyItemInput,
	date time.Time) (*string, error) {
	if it.TaxJurisdiction != nil || it.TaxCode == nil {
		return it.TaxJurisdiction, nil
	}
	code, ok, err := e.outputCodeOn(ctx, codes, o.supplier.ID, *it.TaxCode, date)
	if err != nil {
		return nil, fmt.Errorf("sales: tax code %s of item %d of purchase order %s: %w", *it.TaxCode, it.POItemNo, o.DocumentNumber, err)
	}
	if !ok {
		return nil, nil
	}
	return defaultedTo(code, o.plant.Region), nil
}

// intercompanyInvoice enters the billing as a customer invoice of the
// supplying company to the customer and posts it (receivables, as
// invoice does for a billing document): the billing date as invoice and
// tax date, the order's currency, the customer's payment term in the
// company, an item per item of the billing with its description,
// account, amount, tax code and jurisdiction, and the billing as its
// source. A refusal names the request's fields (asPosting).
func (e *Endpoints) intercompanyInvoice(ctx context.Context, b IntercompanyBilling, postingDate *time.Time) (receivables.Invoice, error) {
	in := receivables.Input{LegalEntityID: b.LegalEntityID, CustomerID: b.CustomerID, InvoiceDate: b.BillingDate, PostingDate: postingDate,
		Currency: b.Currency, Items: make([]receivables.ItemInput, len(b.Items)), Source: &receivables.Source{ID: b.ID, Number: b.DocumentNumber}}
	for i, it := range b.Items {
		in.Items[i] = receivables.ItemInput{Description: &it.Description, AccountID: it.AccountID,
			Net: receivables.Amount{Value: it.Net.Plain(b.Exponent), Currency: b.Currency}, TaxCode: it.TaxCode, TaxJurisdiction: it.TaxJurisdiction}
	}
	lead := "Purchase order " + b.OrderNumber + "'s intercompany billing cannot be posted as a customer invoice: "
	draft, err := e.Invoices.CreateInvoice(ctx, in)
	if err != nil {
		return receivables.Invoice{}, asPosting(fmt.Errorf("sales: enter intercompany billing %s as a customer invoice: %w", b.DocumentNumber, err), lead, len(b.Items))
	}
	v, err := e.Invoices.PostInvoice(ctx, draft.ID, func(int32) error { return nil }, nil) // entered in this transaction: no one else read it
	if err != nil {
		return receivables.Invoice{}, asPosting(fmt.Errorf("sales: post customer invoice %s of intercompany billing %s: %w", draft.DocumentNumber,
			b.DocumentNumber, err), lead, len(b.Items))
	}
	return v, nil
}

// insertIntercompany writes the billing, posted as the invoice v, with its
// items, each with the tax v's item of its number bore, and returns it as
// it reads.
func insertIntercompany(ctx context.Context, tx pgx.Tx, p reqctx.Principal, b IntercompanyBilling, v receivables.Invoice) (IntercompanyBilling, error) {
	n := len(b.Items)
	nos, orderNos, materials, descriptions, quantities, units := make([]int32, n), make([]int32, n), make([]uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n)
	prices, priceUnits, nets, accounts := make([]money.Minor, n), make([]int32, n), make([]money.Minor, n), make([]uuid.UUID, n)
	codes, jurisdictions, taxes := make([]*string, n), make([]*string, n), make([]money.Minor, n)
	for i, it := range b.Items {
		nos[i], orderNos[i], materials[i], descriptions[i], quantities[i], units[i] = it.ItemNo, it.POItemNo, *it.MaterialID, it.Description, it.Quantity, it.Unit
		prices[i], priceUnits[i], nets[i], accounts[i] = it.UnitPrice, it.PriceUnit, it.Net, it.AccountID
		codes[i], jurisdictions[i], taxes[i] = it.TaxCode, it.TaxJurisdiction, v.Items[i].Tax // the invoice's items are the billing's, one for one
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.intercompany_billing (tenant_id, id, document_number, legal_entity_id, customer_id, purchase_order_id,
			purchase_order_number, receiving_legal_entity_id, issue_document_id, issue_document_number, billing_date, posting_date, currency, net_amount,
			tax_amount, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		p.TenantID, b.ID, b.DocumentNumber, b.LegalEntityID, b.CustomerID, b.OrderID, b.OrderNumber, b.ReceivingID, b.IssueDocumentID,
		b.IssueDocumentNumber, b.BillingDate, *v.PostingDate, b.Currency, v.Net, v.Tax, v.ID, v.DocumentNumber, *v.EntryYear, *v.EntryID,
		p.ID); err != nil { // a posted invoice has its posting date and entry (customer_invoice_posted)
		return IntercompanyBilling{}, fmt.Errorf("sales: insert intercompany billing %s: %w", b.DocumentNumber, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.intercompany_billing_item (tenant_id, billing_id, item_no, po_item_no, material_id, description, quantity,
			uom_code, unit_price, price_unit, net_amount, account_id, tax_code, tax_jurisdiction, tax_amount)
		SELECT $1, $2, x.n, x.o, x.m, x.d, x.q::numeric, x.u, x.p, x.pu, x.a, x.acc, x.c, x.j, x.t
		FROM unnest($3::int[], $4::int[], $5::uuid[], $6::text[], $7::text[], $8::text[], $9::bigint[], $10::int[], $11::bigint[], $12::uuid[], $13::text[],
			$14::text[], $15::bigint[]) AS x(n, o, m, d, q, u, p, pu, a, acc, c, j, t)`,
		p.TenantID, b.ID, nos, orderNos, materials, descriptions, quantities, units, prices, priceUnits, nets, accounts, codes, jurisdictions,
		taxes); err != nil {
		return IntercompanyBilling{}, fmt.Errorf("sales: write items of intercompany billing %s: %w", b.DocumentNumber, err)
	}
	billed, err := scanIntercompany(tx.QueryRow(ctx, intercompanySelect+` WHERE b.id = $1`, b.ID))
	if err != nil {
		return IntercompanyBilling{}, fmt.Errorf("sales: read intercompany billing %s: %w", b.DocumentNumber, err)
	}
	return billed, nil
}

// recordIntercompany adds the audit record of an intercompany billing
// (ADR-0011): no event of its own, as the invoice publishes
// CustomerInvoiceChanged and CustomerInvoicePosted; the customer by id,
// never by name (product owner, 2026-11-05).
func recordIntercompany(ctx context.Context, b IntercompanyBilling, le tenancy.Entity) error {
	amount := func(m money.Minor) any { return money.EventAmount(m, b.Currency, b.Exponent) }
	if err := audit.Record(ctx, "sales.intercompany_billing.create", "sales.intercompany_billing", b.ID, map[string]any{
		"document_number": b.DocumentNumber, "legal_entity_code": le.Code, "customer_id": b.CustomerID, "receiving_legal_entity_id": b.ReceivingID,
		"purchase_order_id": b.OrderID, "purchase_order_number": b.OrderNumber, "issue_document_id": b.IssueDocumentID,
		"issue_document_number": b.IssueDocumentNumber, "billing_date": b.BillingDate.Format(time.DateOnly),
		"posting_date": b.PostingDate.Format(time.DateOnly), "net_amount": amount(b.Net), "tax_amount": amount(b.Tax),
		"customer_invoice_id": b.InvoiceID, "customer_invoice_number": b.InvoiceNumber, "journal_entry_id": b.EntryID, "items": len(b.Items)}); err != nil {
		return fmt.Errorf("sales: record intercompany billing %s: %w", b.DocumentNumber, err)
	}
	return nil
}
