package procurement

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// orderDocument is what a purchase order's document for its supplier
// shows, read from the stored order and the master data it names
// (ADR-0054).
type orderDocument struct {
	order         PurchaseOrder
	buyer         tenancy.Entity
	purchasingOrg *tenancy.Unit
	supplier      partner.Supplier
	address       *partner.Address // nil: the supplier holds no order_from or legal address
	term          partner.PaymentTerm
	neededBy      *time.Time // the requisition's, when the order has one that says
}

// OrderDocument renders the document of a released or closed purchase
// order for its supplier, as plain text. It is a read: nothing is stored,
// audited or published (ADR-0054).
func (e *Endpoints) OrderDocument(ctx context.Context, id uuid.UUID) (string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	o, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return "", err
	}
	if !allows(o.Status, Released, Closed) {
		return "", statusConflict("purchase order", o.Status, "a document for the supplier", Released, Closed)
	}
	d, err := e.documentOf(ctx, tx, o)
	if err != nil {
		return "", err
	}
	return d.render()
}

// documentOf reads what the document names besides the order.
func (e *Endpoints) documentOf(ctx context.Context, tx pgx.Tx, o PurchaseOrder) (orderDocument, error) {
	d := orderDocument{order: o}
	var ok bool
	var err error
	if d.buyer, ok, err = e.LegalEntities.Entity(ctx, o.LegalEntityID); err != nil || !ok {
		return d, missing(err, ok, "legal entity "+o.LegalEntityID.String(), o.ID)
	}
	if o.PurchasingOrgID != nil {
		u, ok, err := e.LegalEntities.OrgUnit(ctx, *o.PurchasingOrgID)
		if err != nil || !ok {
			return d, missing(err, ok, "purchasing organisation "+o.PurchasingOrgID.String(), o.ID)
		}
		d.purchasingOrg = &u
	}
	if d.supplier, ok, err = e.Suppliers.Supplier(ctx, o.SupplierID, o.LegalEntityID); err != nil || !ok {
		return d, missing(err, ok, "supplier "+o.SupplierID.String(), o.ID)
	}
	a, ok, err := e.Suppliers.OrderAddress(ctx, o.SupplierID)
	if err != nil {
		return d, fmt.Errorf("procurement: document of purchase order %s: %w", o.ID, err)
	}
	if ok {
		d.address = &a
	}
	if d.term, ok, err = e.Suppliers.PaymentTerm(ctx, o.PaymentTermCode); err != nil || !ok {
		return d, missing(err, ok, "payment term "+o.PaymentTermCode, o.ID)
	}
	if o.RequisitionID != nil {
		r, err := getRequisition(ctx, tx, *o.RequisitionID, false)
		if err != nil {
			return d, fmt.Errorf("procurement: document of purchase order %s: %w", o.ID, err)
		}
		d.neededBy = r.NeededBy
	}
	return d, nil
}

// missing is the error of master data an order names that could not be
// read: the read's error, or the row's absence, which the references the
// order was written with rule out.
func missing(err error, ok bool, what string, order uuid.UUID) error {
	if err == nil && !ok {
		err = fmt.Errorf("no %s", what)
	}
	return fmt.Errorf("procurement: document of purchase order %s: %w", order, err)
}

// render writes the document. Amounts are in the order's currency with its
// minor-unit digits and no grouping, as the API writes them (ADR-0005);
// items are computed as the order's total is, and an item out of range
// refuses the document rather than print a wrong amount.
func (d orderDocument) render() (string, error) {
	o := d.order
	sum, err := documentTotal(o.Items)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	d.header(&b)
	b.WriteString("\nItems\n")
	for _, it := range o.Items {
		amount, _ := money.Scale(it.UnitPrice, it.Quantity, int64(it.PriceUnit)) // in range: documentTotal computed it above
		line(&b, "  Item %d: %s", it.No, it.Description)
		qty := fmt.Sprintf("    %s %s at %s per %d %s", it.Quantity, it.Unit, d.money(it.UnitPrice), it.PriceUnit, it.Unit)
		if it.TaxCode != nil {
			qty += ", tax code " + *it.TaxCode
		}
		line(&b, "%s", qty)
		line(&b, "    Amount %s", d.money(amount))
	}
	line(&b, "\nTotal %s", d.money(sum))
	if o.Status == Closed {
		b.WriteString("\nThis order is closed: nothing more is to be delivered on it.\n")
	}
	line(&b, "\nPlease quote purchase order %s on your delivery documents and invoices.", o.DocumentNumber)
	return b.String(), nil
}

// header writes the order's identification, the parties and the terms.
func (d orderDocument) header(b *strings.Builder) {
	o := d.order
	line(b, "PURCHASE ORDER %s", o.DocumentNumber)
	revision := fmt.Sprintf("Revision %d", o.Revision)
	if o.Revision > 1 {
		revision += fmt.Sprintf(" (replaces revision %d)", o.Revision-1)
	}
	line(b, "%s, order date %s, currency %s", revision, o.OrderDate.Format(time.DateOnly), o.Currency)
	b.WriteString("\nBuyer\n")
	line(b, "  %s (company %s)", d.buyer.Name, d.buyer.Code)
	line(b, "  Country %s", d.buyer.CountryCode)
	if u := d.purchasingOrg; u != nil {
		line(b, "  Purchasing organisation %s %s", u.Code, u.Name)
	}
	b.WriteString("\nSupplier\n")
	line(b, "  %s (supplier %s)", d.supplier.Name, d.supplier.Number)
	if a := d.address; a != nil {
		line(b, "  %s", a.Line1)
		if a.Line2 != nil {
			line(b, "  %s", *a.Line2)
		}
		place := []string{a.City}
		for _, p := range []*string{a.Region, a.PostalCode} {
			if p != nil {
				place = append(place, *p)
			}
		}
		line(b, "  %s", strings.Join(place, ", "))
		line(b, "  Country %s", a.CountryCode)
	} else {
		line(b, "  Country %s (no order or legal address held)", d.supplier.CountryCode)
	}
	b.WriteString("\n")
	line(b, "Payment terms: %s %s", d.term.Code, d.term.Description)
	if d.neededBy != nil {
		line(b, "Needed by: %s", d.neededBy.Format(time.DateOnly))
	}
}

func (d orderDocument) money(m money.Minor) string {
	return m.Plain(d.order.Exponent) + " " + d.order.Currency
}

// line writes one formatted line. Values come from master data and order
// texts a person typed: control characters, line and paragraph separators
// and bidirectional overrides become spaces, so no value can start a line
// of its own or reorder one that reads as part of the document (a forged
// total, another account to pay).
func line(b *strings.Builder, format string, args ...any) {
	for i, a := range args {
		if s, ok := a.(string); ok {
			args[i] = strings.Map(plain, s)
		}
	}
	fmt.Fprintf(b, format, args...)
	b.WriteByte('\n')
}

// bidiControls are the Unicode bidirectional formatting characters: the
// Arabic letter mark (U+061C), the left-to-right and right-to-left marks
// (U+200E, U+200F), the embeddings and overrides (U+202A-U+202E) and the
// isolates (U+2066-U+2069). Joiners (ZWJ, ZWNJ) stay: names need them.
var bidiControls = &unicode.RangeTable{R16: []unicode.Range16{{Lo: 0x061c, Hi: 0x061c, Stride: 1}, {Lo: 0x200e, Hi: 0x200f, Stride: 1},
	{Lo: 0x202a, Hi: 0x202e, Stride: 1}, {Lo: 0x2066, Hi: 0x2069, Stride: 1}}}

func plain(r rune) rune {
	if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, bidiControls) {
		return ' '
	}
	return r
}
