package procurement

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Supplier quotations (ADR-0069 §7, T82b): what an invited supplier
// quoted for a published RFQ, recorded by the buyer, one per supplier. A
// quotation prices items of the RFQ in the RFQ's currency and may leave
// some out; its total is computed from its prices and the RFQ's
// quantities, which never change, and is not stored.

// Recorded is the status of a supplier quotation as the buyer recorded it.
const Recorded = "recorded"

func init() {
	problem.RegisterConstraint("supplier_quotation_once", func() *problem.Problem {
		return problem.Conflict("procurement.quotation_exists", "The supplier already has a quotation for this RFQ.")
	})
}

// QuotedPrice is the price a quotation gives one RFQ item: minor units of
// the RFQ's currency per PriceUnit of the RFQ item's unit.
type QuotedPrice struct {
	RFQItemNo int32       `json:"rfq_item_no"`
	UnitPrice money.Minor `json:"unit_price"`
	PriceUnit int32       `json:"price_unit"`
}

// Quotation is a row of procurement.supplier_quotation with its prices by
// RFQ item number.
type Quotation struct {
	ID, RFQID, SupplierID uuid.UUID
	Reference             string
	ValidUntil            time.Time
	Status                string
	Prices                []QuotedPrice
	Version               int32
	CreatedAt             time.Time
	CreatedBy             uuid.UUID
	UpdatedAt             time.Time
	UpdatedBy             uuid.UUID
}

const quotationSelect = `SELECT q.id, q.rfq_id, q.supplier_id, q.supplier_reference, q.valid_until, q.status, q.version, q.created_at, q.created_by,
		q.updated_at, q.updated_by,
		coalesce((SELECT json_agg(json_build_object('rfq_item_no', i.rfq_item_no, 'unit_price', i.unit_price, 'price_unit', i.price_unit)
			ORDER BY i.rfq_item_no)
		 FROM procurement.supplier_quotation_item i WHERE i.tenant_id = q.tenant_id AND i.supplier_quotation_id = q.id), '[]')
	FROM procurement.supplier_quotation q`

func scanQuotation(row pgx.Row) (Quotation, error) {
	var q Quotation
	if err := row.Scan(&q.ID, &q.RFQID, &q.SupplierID, &q.Reference, &q.ValidUntil, &q.Status, &q.Version, &q.CreatedAt, &q.CreatedBy,
		&q.UpdatedAt, &q.UpdatedBy, &q.Prices); err != nil {
		return Quotation{}, fmt.Errorf("scan supplier quotation: %w", err)
	}
	return q, nil
}

// priced are the items of RFQ r the quotation prices, each the RFQ's item
// at the quoted price, by item number; unquoted are the item numbers it
// leaves out. A price always names an item of its RFQ (the foreign key
// supplier_quotation_item_rfq_item_fkey).
func (q Quotation) priced(r RFQ) (priced []Item, unquoted []int32) {
	prices := map[int32]QuotedPrice{}
	for _, p := range q.Prices {
		prices[p.RFQItemNo] = p
	}
	unquoted = []int32{}
	for _, it := range r.Items {
		p, ok := prices[it.No]
		if !ok {
			unquoted = append(unquoted, it.No)
			continue
		}
		it.UnitPrice, it.PriceUnit = p.UnitPrice, p.PriceUnit
		priced = append(priced, it)
	}
	return priced, unquoted
}

// NewQuotation is the input of recordQuotation.
type NewQuotation struct {
	SupplierID uuid.UUID
	Reference  string
	ValidUntil time.Time
	Prices     []PriceInput
}

// PriceInput is a quoted price as the API sends it.
type PriceInput struct {
	RFQItemNo int32
	Price     string // decimal amount of Currency
	Currency  string
	PriceUnit int32
}

// quotedPrices validates a quotation's prices against RFQ r: each names
// an item of the RFQ, once, at a positive price in the RFQ's currency.
// They come back by RFQ item number.
func quotedPrices(r RFQ, in []PriceInput) ([]QuotedPrice, error) {
	out := make([]QuotedPrice, len(in))
	seen := map[int32]bool{}
	for i, p := range in {
		field := fmt.Sprintf("items[%d]", i)
		if !slices.ContainsFunc(r.Items, func(it Item) bool { return it.No == p.RFQItemNo }) {
			return nil, invalid("procurement.unknown_rfq_item", field+".rfq_item_no", fmt.Sprintf("RFQ %s has no item %d", r.DocumentNumber, p.RFQItemNo))
		}
		if seen[p.RFQItemNo] {
			return nil, invalid("procurement.quotation_item_repeated", field+".rfq_item_no", "each RFQ item is priced once per quotation")
		}
		seen[p.RFQItemNo] = true
		price, err := amount(p.Price, p.Currency, r.Currency, r.Exponent, field+".unit_price", true)
		if err != nil {
			return nil, err
		}
		out[i] = QuotedPrice{RFQItemNo: p.RFQItemNo, UnitPrice: price, PriceUnit: max(p.PriceUnit, 1)}
	}
	slices.SortFunc(out, func(a, b QuotedPrice) int { return int(a.RFQItemNo - b.RFQItemNo) })
	return out, nil
}

// recordQuotation records an invited supplier's quotation for a published
// RFQ, read under its row lock: quotations for one RFQ are recorded one
// after another, and an award (T82d) waits for them. The supplier's second
// quotation fails on supplier_quotation_once (409). It returns the RFQ
// named, which the quotation is shown beside.
func (e *Endpoints) recordQuotation(ctx context.Context, rfq uuid.UUID, in NewQuotation) (RFQ, Quotation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return RFQ{}, Quotation{}, err
	}
	r, err := getRFQ(ctx, tx, rfq, true)
	if err != nil {
		return RFQ{}, Quotation{}, err
	}
	if r.Status != RFQPublished {
		return RFQ{}, Quotation{}, statusConflict("RFQ", r.Status, "recording a quotation", RFQPublished)
	}
	if !slices.Contains(r.supplierIDs(), in.SupplierID) {
		return RFQ{}, Quotation{}, invalid("procurement.supplier_not_invited", "supplier_id", "RFQ "+r.DocumentNumber+" does not invite this supplier")
	}
	if err := problem.RequireText("supplier_reference", in.Reference); err != nil {
		return RFQ{}, Quotation{}, err
	}
	q := Quotation{ID: uuid.Must(uuid.NewV7()), RFQID: r.ID, SupplierID: in.SupplierID, Reference: in.Reference, ValidUntil: in.ValidUntil}
	if q.Prices, err = quotedPrices(r, in.Prices); err != nil {
		return RFQ{}, Quotation{}, err
	}
	priced, _ := q.priced(r)
	if err := checkTotal(priced); err != nil {
		return RFQ{}, Quotation{}, err
	}
	if err := writeQuotation(ctx, tx, p, q); err != nil {
		return RFQ{}, Quotation{}, err
	}
	named, err := e.named(ctx, r)
	if err != nil {
		return RFQ{}, Quotation{}, err
	}
	stored, err := scanQuotation(tx.QueryRow(ctx, quotationSelect+` WHERE q.id = $1`, q.ID))
	if err != nil {
		return RFQ{}, Quotation{}, fmt.Errorf("procurement: read supplier quotation %s: %w", q.ID, err)
	}
	return named[0], stored, quotationRecorded(ctx, named[0], stored)
}

// writeQuotation inserts a quotation with its prices.
func writeQuotation(ctx context.Context, tx pgx.Tx, p reqctx.Principal, q Quotation) error {
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.supplier_quotation (tenant_id, id, rfq_id, supplier_id, supplier_reference, valid_until,
			status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, p.TenantID, q.ID, q.RFQID, q.SupplierID, q.Reference, q.ValidUntil, Recorded, p.ID); err != nil {
		return fmt.Errorf("procurement: record a quotation of supplier %s for RFQ %s: %w", q.SupplierID, q.RFQID, err)
	}
	n := len(q.Prices)
	nos, prices, units := make([]int32, n), make([]money.Minor, n), make([]int32, n)
	for i, pr := range q.Prices {
		nos[i], prices[i], units[i] = pr.RFQItemNo, pr.UnitPrice, pr.PriceUnit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.supplier_quotation_item (tenant_id, supplier_quotation_id, rfq_id, rfq_item_no, unit_price, price_unit)
		SELECT $1, $2, $3, n, p, u FROM unnest($4::int[], $5::bigint[], $6::int[]) AS x(n, p, u)`,
		p.TenantID, q.ID, q.RFQID, nos, prices, units); err != nil {
		return fmt.Errorf("procurement: write prices of supplier quotation %s: %w", q.ID, err)
	}
	return nil
}

// quotationRecorded audits a recorded quotation and publishes
// SupplierQuotationRecorded, naming the supplier by id and number, never
// by name (product owner 2026-11-05). r is the quotation's RFQ, named.
func quotationRecorded(ctx context.Context, r RFQ, q Quotation) error {
	priced, unquoted := q.priced(r)
	sum := money.EventAmount(total(priced), r.Currency, r.Exponent)
	supplier := r.Suppliers[slices.Index(r.supplierIDs(), q.SupplierID)]
	detail := map[string]any{"rfq_id": r.ID, "supplier_id": q.SupplierID, "supplier_reference": q.Reference,
		"valid_until": q.ValidUntil.Format(time.DateOnly), "items": len(q.Prices), "total": sum}
	return audit.Record(ctx, "procurement.supplier_quotation.record", "procurement.supplier_quotation", q.ID, detail,
		event("supplier_quotation_recorded", "supplier_quotation", q.ID, map[string]any{"supplier_quotation_id": q.ID, "rfq_id": r.ID,
			"document_number": r.DocumentNumber, "legal_entity_id": r.LegalEntityID, "legal_entity_code": r.CompanyCode, "supplier_id": q.SupplierID, "supplier_number": supplier.Number,
			"supplier_reference": q.Reference, "valid_until": q.ValidUntil.Format(time.DateOnly), "total": sum, "complete": len(unquoted) == 0,
			"item_count": len(q.Prices)}))
}
