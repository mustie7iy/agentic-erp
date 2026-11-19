package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// documentSelect reads material documents with their movements in line
// order and the documents they reverse and that reverse them.
const documentSelect = `SELECT d.id, d.legal_entity_id, d.fiscal_year, d.number, d.movement_type, d.posting_date, d.header_text, d.reversal_of_id,
		o.number AS reversal_of_number, r.id AS reversed_by_id, r.number AS reversed_by_number, d.created_at, d.created_by,
		coalesce(d.currency, '') AS currency, coalesce(c.exponent, 0) AS exponent, d.journal_entry_id, d.journal_entry_year, d.journal_entry_number,
		d.reference, (SELECT json_agg(json_build_object('line_no', m.line_no, 'item_no', m.item_no, 'material_id', m.material_id, 'plant_id', m.plant_id,
				'storage_location_id', m.storage_location_id, 'quantity', trim_scale(m.quantity)::text,
				'entry_quantity', trim_scale(m.entry_quantity)::text, 'entry_unit', m.entry_uom_code, 'cost_center_id', m.cost_center_id,
				'amount', m.amount, 'difference', m.difference) ORDER BY m.line_no)
			FROM inventory.movement m WHERE m.tenant_id = d.tenant_id AND m.material_document_id = d.id) AS lines
	FROM inventory.material_document d
	LEFT JOIN inventory.material_document o ON o.tenant_id = d.tenant_id AND o.id = d.reversal_of_id
	LEFT JOIN inventory.material_document r ON r.tenant_id = d.tenant_id AND r.reversal_of_id = d.id
	LEFT JOIN ref.currency c ON c.code = d.currency`

func scanDocument(row pgx.Row) (Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.LegalEntityID, &d.FiscalYear, &d.Number, &d.MovementType, &d.PostingDate, &d.HeaderText, &d.ReversalOfID,
		&d.ReversalOfNumber, &d.ReversedByID, &d.ReversedByNumber, &d.CreatedAt, &d.CreatedBy, &d.Currency, &d.Exponent, &d.EntryID, &d.EntryYear,
		&d.EntryNumber, &d.Reference, &d.Lines)
	return d, errs.Wrapf(err, "scan material document")
}

// load reads a material document of a company the caller may act in;
// another company's is not found.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Document, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Document{}, fmt.Errorf("inventory: company access for material document %s: %w", id, err)
	}
	d, err := scanDocument(tx.QueryRow(ctx, documentSelect+` WHERE d.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(d.LegalEntityID)) {
		return Document{}, problem.NotFoundOf("material document")
	}
	if err != nil {
		return Document{}, fmt.Errorf("inventory: read material document %s: %w", id, err)
	}
	return d, nil
}
