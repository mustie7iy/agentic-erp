package einvoicing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// GetEInvoiceContent implements GET /v1/e-invoices/{id}/content (ADR-0061
// §8, §15): the document's bytes, the legal original, as the provider
// first delivered them, compared with the hash recorded on every read, so
// a store that no longer holds what was received answers 409
// einvoicing.document_changed rather than other bytes. It is visible as
// GetEInvoice is; reading it is not audited, as other downloads are not.
func (e *Endpoints) GetEInvoiceContent(ctx context.Context, params gen.GetEInvoiceContentParams) (gen.GetEInvoiceContentRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	var d EInvoice
	var ref string
	err = tx.QueryRow(ctx, `SELECT `+einvoiceColumns+`, content_ref FROM einvoicing.einvoice WHERE id = $1`, params.ID).Scan(append(d.fields(), &ref)...)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !visible(d, access)):
		return nil, problem.NotFoundOf("e-invoice")
	case err != nil:
		return nil, fmt.Errorf("einvoicing: read e-invoice %s: %w", params.ID, err)
	}
	if e.Objects == nil {
		return nil, problem.Unavailable("E-invoices are kept in object storage, which this server does not have.", 0)
	}
	content, _, err := e.Objects.Get(ctx, ref)
	switch {
	case errors.Is(err, blobstore.ErrNotFound):
		return nil, problem.NotFound("The e-invoice's bytes are not in the store.")
	case err != nil:
		return nil, problem.Unavailable("The document store did not answer; try again.", 30).WithCause(err)
	}
	if sum := sha256.Sum256(content); !bytes.Equal(sum[:], d.SHA256) {
		return nil, problem.Conflict("einvoicing.document_changed", "The stored bytes of e-invoice "+d.ID.String()+" do not match the SHA-256 recorded when it was received.")
	}
	return &gen.GetEInvoiceContentOK{Data: bytes.NewReader(content)}, nil
}
