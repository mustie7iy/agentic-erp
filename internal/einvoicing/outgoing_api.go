package einvoicing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CreateOutgoingEInvoice implements POST /v1/outgoing-e-invoices
// (ADR-0061 §9, As built E2): the posted customer document's PINT AE
// e-invoice, checked against the mandatory fields before anything is
// kept. A valid one is answered unchanged; otherwise the document is
// rendered: valid, its XML parsed back and checked again (a render that
// its own parser or rules refuse is a defect, never kept as valid) and
// kept write-once before its row is written, so no row lock is held
// while the store is called; or invalid with its problems and no XML.
// 201 when the row is new, 200 when it existed.
func (e *Endpoints) CreateOutgoingEInvoice(ctx context.Context, req *gen.OutgoingEInvoiceRender, _ gen.CreateOutgoingEInvoiceParams) (gen.CreateOutgoingEInvoiceRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	s, reason, err := e.renderable(ctx, req)
	if err != nil {
		return nil, err
	}
	o, found, err := outgoingOf(ctx, tx, s.ID)
	if err != nil {
		return nil, err
	}
	created := false
	if !found || o.Status != StatusValid {
		if o, created, err = e.render(ctx, tx, p, s, reason); err != nil {
			return nil, err
		}
	}
	out, err := e.outgoingAPI(ctx, o)
	if err != nil {
		return nil, err
	}
	tag := gen.NewOptString(etag.Format(o.Version))
	if created {
		return &gen.CreateOutgoingEInvoiceCreatedHeaders{ETag: tag, Location: gen.NewOptString("/v1/outgoing-e-invoices/" + o.ID.String()),
			Response: out}, nil
	}
	return &gen.OutgoingEInvoiceHeaders{ETag: tag, Response: out}, nil
}

// renderable reads the customer document and refuses one the format does
// not render: not found, not posted, not an invoice or credit memo, not
// of a UAE company in AED, or a credit memo without its reason (an
// invoice with one).
func (e *Endpoints) renderable(ctx context.Context, req *gen.OutgoingEInvoiceRender) (Sale, *Reason, error) {
	s, ok, err := e.Sales.Sale(ctx, req.CustomerInvoiceID)
	switch {
	case err != nil:
		return Sale{}, nil, err
	case !ok:
		return Sale{}, nil, problem.ReferenceNotFound("customer_invoice_id", "customer invoice", "no customer invoice or credit memo with this id")
	case s.Status != salePosted:
		return Sale{}, nil, problem.Conflict("einvoicing.document_not_posted", "Customer document "+s.Number+" is "+s.Status+
			"; only a posted one is rendered as an e-invoice.")
	case s.Kind != saleInvoice && s.Kind != saleCreditMemo:
		return Sale{}, nil, problem.Invalid("einvoicing.unsupported_document", "customer_invoice_id",
			"a "+s.Kind+" is not rendered: PINT AE renders tax invoices and tax credit notes")
	case s.Location != "AE" || s.LocalCurrency != accountingCurrency:
		return Sale{}, nil, problem.Invalid("einvoicing.not_a_uae_company", "customer_invoice_id",
			"the customer document's company is not a UAE company in AED, which PINT AE e-invoices are issued by")
	}
	reason, err := reasonOf(s, req.CreditNoteReason)
	if err != nil {
		return Sale{}, nil, err
	}
	if e.Objects == nil {
		return Sale{}, nil, noStore()
	}
	return s, reason, nil
}

// reasonOf is a credit memo's reason (BTAE-03), required for it and
// refused for an invoice (422 einvoicing.credit_note_reason).
func reasonOf(s Sale, given gen.OptCreditNoteReason) (*Reason, error) {
	if s.Kind != saleCreditMemo {
		if given.Set {
			return nil, problem.Invalid("einvoicing.credit_note_reason", "credit_note_reason", "a tax invoice carries no credit note reason")
		}
		return nil, nil
	}
	if !given.Set {
		return nil, problem.Invalid("einvoicing.credit_note_reason", "credit_note_reason", "a tax credit note needs its reason (BTAE-03)")
	}
	if err := problem.RequireText("credit_note_reason.text", given.Value.Text); err != nil {
		return nil, err
	}
	return &Reason{Code: given.Value.Code, Text: strings.TrimSpace(given.Value.Text)}, nil
}

// render builds, checks and keeps the e-invoice and writes its row,
// saying whether the row is new.
func (e *Endpoints) render(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Sale, reason *Reason) (OutgoingEInvoice, bool, error) {
	seller, err := e.Sales.Seller(ctx, s.CompanyID, s.IssueDate)
	if err != nil {
		return OutgoingEInvoice{}, false, err
	}
	buyer, _, err := e.Sales.Buyer(ctx, s.CustomerID, s.IssueDate) // a customer that is gone renders as one without name or TRN, which the rules name
	if err != nil {
		return OutgoingEInvoice{}, false, err
	}
	d, found, err := buildDocument(s, seller, buyer, reason)
	if err != nil {
		return OutgoingEInvoice{}, false, err
	}
	o := OutgoingEInvoice{ID: uuid.Must(uuid.NewV7()), LegalEntityID: s.CompanyID, CustomerInvoiceID: s.ID, CustomerInvoiceNumber: s.Number,
		SourceDocumentID: s.SourceID, SourceDocumentNumber: s.SourceNumber, CustomerID: s.CustomerID, Kind: d.Kind, TypeCode: d.TypeCode,
		IssueDate: s.IssueDate, Currency: s.Currency, Exponent: s.Exponent, Net: s.Net, Tax: s.Tax, Gross: s.Gross,
		ReferenceNumber: s.ReferenceNumber, Specification: PINTAECustomization, Status: StatusInvalid, Problems: append(found, Check(d)...)}
	if reason != nil {
		o.ReasonCode, o.ReasonText = &reason.Code, &reason.Text
	}
	if len(o.Problems) == 0 {
		if err := e.keep(ctx, p.TenantID, d, &o); err != nil {
			return OutgoingEInvoice{}, false, err
		}
	}
	return e.write(ctx, tx, p, o, seller.Company, buyer.Number)
}

// keep renders the valid document, parses its XML back and checks it
// again, and keeps it write-once under its SHA-256 (ADR-0016): the same
// bytes are the same object.
func (e *Endpoints) keep(ctx context.Context, tenant uuid.UUID, d Document, o *OutgoingEInvoice) error {
	content := RenderPINTAE(d)
	back, err := ParsePINTAE(content, currency.SeededExponent)
	if err != nil {
		return fmt.Errorf("einvoicing: render of %s does not parse back: %w", o.CustomerInvoiceNumber, err)
	}
	if found := Check(back); len(found) > 0 {
		return fmt.Errorf("einvoicing: render of %s fails %s once parsed back: %s", o.CustomerInvoiceNumber, found[0].Rule, found[0].Message)
	}
	sum := sha256.Sum256(content)
	ref := blobstore.Key(tenant, blobstore.KindDocuments, hex.EncodeToString(sum[:]))
	if err := e.Objects.Put(ctx, ref, content, blobstore.PutOptions{ContentType: "application/xml"}); err != nil && !errors.Is(err, blobstore.ErrExists) {
		return problem.Unavailable("The document store did not take the e-invoice; render it again.", 30).WithCause(err)
	}
	size := int64(len(content))
	o.Status, o.SHA256, o.Size, o.ContentRef = StatusValid, sum[:], &size, &ref
	return nil
}

// write inserts the e-invoice, or renders an invalid one again in its
// row (ADR-0008: the upsert's guard on status = invalid; a valid row is
// final, which its trigger holds too), with its audit record and
// OutgoingEInvoiceRendered. A row another request made valid meanwhile is
// answered as it is.
func (e *Endpoints) write(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o OutgoingEInvoice, company Company, customerNumber string) (OutgoingEInvoice, bool, error) {
	problems, err := problemsJSON(o.Problems)
	if err != nil {
		return OutgoingEInvoice{}, false, err
	}
	var inserted bool
	err = tx.QueryRow(ctx, `INSERT INTO einvoicing.outgoing_einvoice AS o (tenant_id, id, legal_entity_id, customer_invoice_id,
			customer_invoice_number, source_document_id, source_document_number, customer_id, kind, type_code, issue_date, currency,
			net_amount, tax_amount, gross_amount, reference_invoice_number, credit_reason_code, credit_reason_text, specification, status,
			problems, sha256, size, content_ref, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $25)
		ON CONFLICT ON CONSTRAINT outgoing_einvoice_once DO UPDATE SET source_document_id = EXCLUDED.source_document_id,
			source_document_number = EXCLUDED.source_document_number, type_code = EXCLUDED.type_code, issue_date = EXCLUDED.issue_date,
			currency = EXCLUDED.currency, net_amount = EXCLUDED.net_amount, tax_amount = EXCLUDED.tax_amount, gross_amount = EXCLUDED.gross_amount,
			reference_invoice_number = EXCLUDED.reference_invoice_number, credit_reason_code = EXCLUDED.credit_reason_code,
			credit_reason_text = EXCLUDED.credit_reason_text, specification = EXCLUDED.specification, status = EXCLUDED.status,
			problems = EXCLUDED.problems, sha256 = EXCLUDED.sha256, size = EXCLUDED.size, content_ref = EXCLUDED.content_ref, rendered_at = now(),
			version = o.version + 1, updated_by = EXCLUDED.updated_by
		WHERE o.status = 'invalid' RETURNING o.id, o.rendered_at, o.version, o.xmax = 0`,
		p.TenantID, o.ID, o.LegalEntityID, o.CustomerInvoiceID, o.CustomerInvoiceNumber, o.SourceDocumentID, o.SourceDocumentNumber, o.CustomerID,
		o.Kind, o.TypeCode, o.IssueDate, o.Currency, o.Net, o.Tax, o.Gross, o.ReferenceNumber, o.ReasonCode, o.ReasonText, o.Specification,
		o.Status, problems, o.SHA256, o.Size, o.ContentRef, p.ID).Scan(&o.ID, &o.RenderedAt, &o.Version, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		kept, _, err := outgoingOf(ctx, tx, o.CustomerInvoiceID) // valid meanwhile: the row exists
		return kept, false, err
	}
	if err != nil {
		return OutgoingEInvoice{}, false, fmt.Errorf("einvoicing: write the e-invoice of customer document %s: %w", o.CustomerInvoiceNumber, err)
	}
	var before *string
	if !inserted {
		was := StatusInvalid
		before = &was
	}
	payload := o.event(company, customerNumber, before)
	return o, inserted, audit.Record(ctx, "einvoicing.outgoing.rendered", "einvoicing.outgoing_einvoice", o.ID, payload,
		outbox.Event{Context: "einvoicing", Name: "outgoing_einvoice_rendered", AggregateType: "einvoicing.outgoing_einvoice", AggregateID: o.ID,
			Payload: payload})
}

// outgoingOf reads the e-invoice of a customer document, found false for
// none.
func outgoingOf(ctx context.Context, tx pgx.Tx, customerInvoice uuid.UUID) (OutgoingEInvoice, bool, error) {
	var o OutgoingEInvoice
	err := tx.QueryRow(ctx, `SELECT `+outgoingColumns+outgoingFrom+` WHERE o.customer_invoice_id = $1`, customerInvoice).Scan(o.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, false, nil
	}
	if err != nil {
		return o, false, fmt.Errorf("einvoicing: read the e-invoice of customer document %s: %w", customerInvoice, err)
	}
	return o, true, nil
}

// outgoing reads an e-invoice of a company the caller may act in; another
// company's is not found, so ids cannot be probed.
func outgoing(ctx context.Context, id uuid.UUID) (OutgoingEInvoice, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return OutgoingEInvoice{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return OutgoingEInvoice{}, err
	}
	var o OutgoingEInvoice
	err = tx.QueryRow(ctx, `SELECT `+outgoingColumns+outgoingFrom+` WHERE o.id = $1`, id).Scan(o.fields()...)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(o.LegalEntityID)):
		return o, problem.NotFoundOf("outgoing e-invoice")
	case err != nil:
		return o, fmt.Errorf("einvoicing: read outgoing e-invoice %s: %w", id, err)
	}
	return o, nil
}

// GetOutgoingEInvoice implements GET /v1/outgoing-e-invoices/{id}.
func (e *Endpoints) GetOutgoingEInvoice(ctx context.Context, params gen.GetOutgoingEInvoiceParams) (gen.GetOutgoingEInvoiceRes, error) {
	o, err := outgoing(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.outgoingAPI(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.GetOutgoingEInvoiceOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: out}, nil
}

// GetOutgoingEInvoiceContent implements GET
// /v1/outgoing-e-invoices/{id}/content: a valid e-invoice's XML as
// rendered, compared with its SHA-256 on every read (409
// einvoicing.document_changed otherwise); an invalid one has none (409
// einvoicing.document_invalid). Not audited, as other downloads are not.
func (e *Endpoints) GetOutgoingEInvoiceContent(ctx context.Context, params gen.GetOutgoingEInvoiceContentParams) (gen.GetOutgoingEInvoiceContentRes, error) {
	o, err := outgoing(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	if o.Status != StatusValid {
		return nil, problem.Conflict("einvoicing.document_invalid", "Outgoing e-invoice "+o.CustomerInvoiceNumber+
			" is invalid and has no XML; correct what its problems name and render it again.")
	}
	if e.Objects == nil {
		return nil, noStore()
	}
	content, _, err := e.Objects.Get(ctx, *o.ContentRef) // a valid row has its content_ref (outgoing_einvoice_content)
	switch {
	case errors.Is(err, blobstore.ErrNotFound):
		return nil, problem.NotFound("The e-invoice's XML is not in the store.")
	case err != nil:
		return nil, problem.Unavailable("The document store did not answer; try again.", 30).WithCause(err)
	}
	if sum := sha256.Sum256(content); !bytes.Equal(sum[:], o.SHA256) {
		return nil, problem.Conflict("einvoicing.document_changed", "The stored XML of outgoing e-invoice "+o.CustomerInvoiceNumber+
			" does not match the SHA-256 recorded when it was rendered.")
	}
	return &gen.GetOutgoingEInvoiceContentOK{Data: bytes.NewReader(content)}, nil
}

// outgoingAPI is the e-invoice with its company's and customer's names.
func (e *Endpoints) outgoingAPI(ctx context.Context, o OutgoingEInvoice) (gen.OutgoingEInvoice, error) {
	companies, err := e.Companies.Named(ctx, []uuid.UUID{o.LegalEntityID})
	if err != nil {
		return gen.OutgoingEInvoice{}, err
	}
	customers, err := e.Sales.Customers(ctx, []uuid.UUID{o.CustomerID})
	if err != nil {
		return gen.OutgoingEInvoice{}, err
	}
	co, cu := companies[o.LegalEntityID], customers[o.CustomerID]
	out := gen.OutgoingEInvoice{ID: o.ID, CustomerInvoiceID: o.CustomerInvoiceID, CustomerInvoiceNumber: o.CustomerInvoiceNumber,
		LegalEntityID: o.LegalEntityID, CompanyCode: co.Code, CompanyName: co.Name, CustomerID: o.CustomerID, CustomerNumber: cu.Number,
		CustomerName: cu.Name, Kind: o.Kind, TypeCode: o.TypeCode, IssueDate: o.IssueDate, Currency: o.Currency,
		NetAmount: apitypes.Money(o.Net, o.Currency, o.Exponent), TaxAmount: apitypes.Money(o.Tax, o.Currency, o.Exponent),
		GrossAmount: apitypes.Money(o.Gross, o.Currency, o.Exponent), Specification: o.Specification, Status: o.Status,
		Problems: make([]gen.OutgoingEInvoiceProblem, len(o.Problems)), RenderedAt: o.RenderedAt, Version: o.Version}
	for i, f := range o.Problems {
		out.Problems[i] = gen.OutgoingEInvoiceProblem{Rule: f.Rule, Term: f.Term, Message: f.Message}
	}
	out.SourceDocumentID = nilUUID(o.SourceDocumentID)
	out.SourceDocumentNumber = nilString(o.SourceDocumentNumber)
	out.ReferenceInvoiceNumber = nilString(o.ReferenceNumber)
	if o.ReasonCode != nil {
		out.CreditNoteReason.SetTo(gen.CreditNoteReason{Code: *o.ReasonCode, Text: *o.ReasonText})
	}
	if o.SHA256 != nil {
		out.SHA256.SetTo(hex.EncodeToString(o.SHA256))
	} else {
		out.SHA256.SetToNull()
	}
	if o.Size != nil {
		out.Size.SetTo(*o.Size)
	} else {
		out.Size.SetToNull()
	}
	return out, nil
}

func nilUUID(v *uuid.UUID) gen.NilUUID {
	var out gen.NilUUID
	if v != nil {
		out.SetTo(*v)
	} else {
		out.SetToNull()
	}
	return out
}

func nilString(v *string) gen.NilString {
	var out gen.NilString
	if v != nil {
		out.SetTo(*v)
	} else {
		out.SetToNull()
	}
	return out
}
