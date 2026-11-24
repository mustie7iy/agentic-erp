package payables

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Attachable is a supplier invoice as the einvoicing context checks it
// before a person links a held e-invoice to it (ADR-0061 §7, T65f): its
// company, supplier and supplier reference, its source and whether it
// has a document of its own.
type Attachable struct {
	LegalEntityID, SupplierID uuid.UUID
	SupplierReference, Source string
	HasDocument               bool
}

// Attachable reads the invoice FOR UPDATE, so a link of it and its
// attachment are serialized with any other change of it; found false for
// none, or one of a company the caller may not act in.
func (Invoices) Attachable(ctx context.Context, id uuid.UUID) (Attachable, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Attachable{}, false, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Attachable{}, false, err
	}
	var a Attachable
	err = tx.QueryRow(ctx, `SELECT legal_entity_id, supplier_id, supplier_reference, source, capture_sha256 IS NOT NULL
		FROM payables.supplier_invoice WHERE id = $1 FOR UPDATE`, id).Scan(&a.LegalEntityID, &a.SupplierID, &a.SupplierReference, &a.Source, &a.HasDocument)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Attachable{}, false, nil
	case err != nil:
		return Attachable{}, false, fmt.Errorf("payables: read invoice %s to attach a document: %w", id, err)
	}
	return a, access.Allows(a.LegalEntityID), nil
}

// Attach makes the bytes kept under ref, whose SHA-256 is sha, the
// document of an invoice a person entered by hand that has none (ADR-0061
// §7, T65f: a held e-invoice linked to it), whatever its status, as a
// document never changes what was posted: getSupplierInvoiceDocument
// serves them, compared with the hash, as a captured invoice's. The
// update is guarded on the invoice still entered by hand without a
// document, which the caller checked under Attachable's lock, and is
// audited (payables.supplier_invoice.document_attached, the hash and no
// content). Bytes that are another invoice's document already are refused
// by supplier_invoice_capture_once (409 payables.already_captured).
func (Invoices) Attach(ctx context.Context, id uuid.UUID, sha []byte, ref string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	var number string
	err = tx.QueryRow(ctx, `UPDATE payables.supplier_invoice SET capture_sha256 = $2, capture_object_ref = $3, version = version + 1, updated_by = $4
		WHERE id = $1 AND source = 'manual' AND capture_sha256 IS NULL RETURNING document_number`, id, sha, ref, p.ID).Scan(&number)
	if err != nil {
		return fmt.Errorf("payables: attach a document to invoice %s: %w", id, err)
	}
	return record(ctx, "payables.supplier_invoice.document_attached", id, map[string]any{"document_number": number, "sha256": hex.EncodeToString(sha)})
}
