package einvoicing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
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
	tag := gen.NewOptString(out.Etag)
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
	d, found := buildDocument(s, seller, buyer, reason)
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
	var inserted bool
	err := tx.QueryRow(ctx, `INSERT INTO einvoicing.outgoing_einvoice AS o (tenant_id, id, legal_entity_id, customer_invoice_id,
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
		o.Status, problemsJSON(o.Problems), o.SHA256, o.Size, o.ContentRef, p.ID).Scan(&o.ID, &o.RenderedAt, &o.Version, &inserted)
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
	return &gen.GetOutgoingEInvoiceOKHeaders{ETag: gen.NewOptString(out.Etag), Response: out}, nil
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

// tagOf is an e-invoice's ETag, its representation's etag: its version
// and its submission's, so it moves with its transmission (As built E3);
// both only grow.
func tagOf(o gen.OutgoingEInvoice) string {
	return etag.Format(o.Version + o.Transmission.Value.Version)
}

// outgoingNames are the companies and customers of a page of e-invoices,
// read once each (REVIEW.md: an identifier with the name its consumer
// needs), and their submissions with their connectors' names (As built
// E3).
type outgoingNames struct {
	companies  map[uuid.UUID]Company
	customers  map[uuid.UUID]CustomerName
	sent       map[uuid.UUID]submission
	connectors map[uuid.UUID]string
}

func (e *Endpoints) outgoingNames(ctx context.Context, docs []OutgoingEInvoice) (outgoingNames, error) {
	var n outgoingNames
	var err error
	if n.companies, err = e.Companies.Named(ctx, idref.Of(docs, func(o OutgoingEInvoice) *uuid.UUID { return &o.LegalEntityID })); err != nil {
		return n, err
	}
	if n.customers, err = e.Sales.Customers(ctx, idref.Of(docs, func(o OutgoingEInvoice) *uuid.UUID { return &o.CustomerID })); err != nil {
		return n, err
	}
	if n.sent, err = submissions(ctx, idref.Of(docs, func(o OutgoingEInvoice) *uuid.UUID { return &o.ID })); err != nil {
		return n, err
	}
	var conns []uuid.UUID
	for _, s := range n.sent {
		conns = append(conns, s.ConnectorID)
	}
	n.connectors, err = e.Connectors.Names(ctx, conns)
	return n, err
}

// submissions are the e-invoices' submissions by e-invoice, each with its
// delivery's outcome.
func submissions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]submission, error) {
	out := map[uuid.UUID]submission{}
	if len(ids) == 0 {
		return out, nil
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT `+submissionColumns+`, `+deliveryOutcome+` FROM einvoicing.outgoing_submission s
		JOIN einvoicing.outgoing_einvoice x ON x.tenant_id = s.tenant_id AND x.id = s.outgoing_einvoice_id WHERE s.outgoing_einvoice_id = ANY($1)`, ids)
	var s submission
	if _, err := pgx.ForEachRow(rows, append(s.fields(), &s.Outcome), func() error { out[s.OutgoingID] = s; return nil }); err != nil {
		return nil, fmt.Errorf("einvoicing: read the submissions of outgoing e-invoices: %w", err)
	}
	return out, nil
}

// deliveryOutcome is what the delivery statuses of the submission s of
// the e-invoice x mean (As built E4): null until completed; rejected
// (by the buyer's access point or the authority, the worklist to correct
// by credit note) until a credit note of the company naming it (IBT-025),
// rendered after it, is transmitted, then corrected; undelivered,
// accepted, or pending while no response is final.
const deliveryOutcome = `CASE WHEN s.status <> 'completed' THEN NULL
	WHEN ` + deliveryRejected + ` THEN CASE WHEN ` + deliveryCorrected + ` THEN 'corrected' ELSE 'rejected' END
	WHEN s.delivery_status = 'DELIVERY_FAILURE' THEN 'undelivered'
	WHEN s.delivery_status IN ('ACCEPTANCE', 'BUSINESS_RULE_WARNING') THEN 'accepted'
	ELSE 'pending' END`

// deliveryRejected holds when the buyer's access point or the authority
// rejected the e-invoice of submission s.
const deliveryRejected = `(s.delivery_status IN ('REJECTION', 'BUSINESS_RULE_FATAL', 'SYNTAX_VIOLATION') OR s.tax_data_status = 'TDD_REJECTED')`

// deliveryCorrected holds when a credit note c of x's company naming x's
// invoice (IBT-025; a credit note's own reference for a credit note),
// rendered after x, is transmitted.
const deliveryCorrected = `EXISTS (SELECT FROM einvoicing.outgoing_einvoice c
	JOIN einvoicing.outgoing_submission cs ON cs.tenant_id = c.tenant_id AND cs.outgoing_einvoice_id = c.id AND cs.status = 'completed'
	WHERE c.tenant_id = x.tenant_id AND c.legal_entity_id = x.legal_entity_id AND c.type_code = '381' AND c.id <> x.id
	  AND c.reference_invoice_number = coalesce(x.reference_invoice_number, x.customer_invoice_number) AND c.rendered_at > x.rendered_at)`

// transmissionAPI is a submission as the e-invoice shows it.
func (n outgoingNames) transmissionAPI(s submission) gen.OutgoingEInvoiceTransmission {
	out := gen.OutgoingEInvoiceTransmission{ConnectorID: s.ConnectorID, ConnectorName: n.connectors[s.ConnectorID], OperationID: s.OperationID,
		BusinessType: s.BusinessType, Status: s.Status, ProviderStatus: nilString(s.ProviderStatus), TrackingID: nilString(s.TrackingID),
		Attempts: int32(s.Attempts), Resends: int32(s.Resends), NextCheckAt: nilTime(s.NextCheckAt), SubmittedAt: nilTime(s.SubmittedAt),
		CompletedAt: nilTime(s.CompletedAt), DeliveryStatus: nilString(s.DeliveryStatus), DeliveryDetail: nilString(s.DeliveryDetail),
		DeliveredAt: nilTime(s.DeliveredAt), DeliveryOutcome: nilString(s.Outcome), TaxDataStatus: nilString(s.TaxDataStatus),
		TaxDataAt: nilTime(s.TaxDataAt), TaxDataRequest: nilString(s.TaxDataRequest),
		TaxDataAwaited: nilString(s.TaxDataAwaited), FollowCheckAt: nilTime(s.FollowCheckAt), Version: s.Version}
	if s.ProblemCode != nil {
		out.Problem.SetTo(gen.OutgoingEInvoiceTransmissionProblem{Code: *s.ProblemCode, Detail: *s.ProblemDetail})
	} else {
		out.Problem.SetToNull()
	}
	return out
}

func nilTime(v *time.Time) gen.NilDateTime {
	var out gen.NilDateTime
	if v != nil {
		out.SetTo(*v)
	} else {
		out.SetToNull()
	}
	return out
}

// outgoingAPI is the e-invoice with its company's and customer's names.
func (e *Endpoints) outgoingAPI(ctx context.Context, o OutgoingEInvoice) (gen.OutgoingEInvoice, error) {
	n, err := e.outgoingNames(ctx, []OutgoingEInvoice{o})
	return n.toAPI(o), err
}

func (n outgoingNames) toAPI(o OutgoingEInvoice) gen.OutgoingEInvoice {
	co, cu := n.companies[o.LegalEntityID], n.customers[o.CustomerID]
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
	if s, ok := n.sent[o.ID]; ok {
		out.Transmission.SetTo(n.transmissionAPI(s))
	} else {
		out.Transmission.SetToNull()
	}
	out.Etag = tagOf(out)
	return out
}

var outgoingSort = pagination.Sort{Default: "rendered_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "rendered_at", Column: "o.rendered_at", Type: "timestamptz"}}}

// ListOutgoingEInvoices implements GET /v1/outgoing-e-invoices: the
// e-invoices of the companies the caller may act in, newest rendered
// first, cursor-paged.
func (e *Endpoints) ListOutgoingEInvoices(ctx context.Context, params gen.ListOutgoingEInvoicesParams) (gen.ListOutgoingEInvoicesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listOutgoingEInvoices|%s|%s|%s|%s|%s|%s", optUUID(params.LegalEntityID), optUUID(params.CustomerInvoiceID), params.Status.Value,
		params.TransmissionStatus.Value, params.DeliveryOutcome.Value, params.TaxDataStatus.Value)
	ks, err := e.Pages.Keyset(outgoingSort, scope, apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+outgoingColumns+outgoingFrom+`
		WHERE ($1::uuid IS NULL OR o.legal_entity_id = $1) AND ($2::uuid IS NULL OR o.customer_invoice_id = $2) AND ($3 = '' OR o.status = $3)
		  AND ($4 OR o.legal_entity_id = ANY($5))
		  AND ($6 = '' OR coalesce((SELECT s.status FROM einvoicing.outgoing_submission s WHERE s.tenant_id = o.tenant_id AND s.outgoing_einvoice_id = o.id),
		    'none') = $6)
		  AND ($7 = '' OR (SELECT `+deliveryOutcome+` FROM einvoicing.outgoing_submission s, einvoicing.outgoing_einvoice x
		    WHERE s.tenant_id = o.tenant_id AND s.outgoing_einvoice_id = o.id AND x.tenant_id = o.tenant_id AND x.id = o.id) = $7)
		  AND ($8 = '' OR EXISTS (SELECT FROM einvoicing.outgoing_submission s WHERE s.tenant_id = o.tenant_id AND s.outgoing_einvoice_id = o.id
		    AND s.tax_data_status = $8 AND s.tax_data_awaited IS NULL))`,
		[]any{apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerInvoiceID), params.Status.Value, access.All, access.IDs,
			string(params.TransmissionStatus.Value), params.DeliveryOutcome.Value, params.TaxDataStatus.Value})
	rows, _ := tx.Query(ctx, q, args...)
	docs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OutgoingEInvoice, error) {
		var o OutgoingEInvoice
		return o, r.Scan(o.fields()...)
	})
	if err != nil {
		return nil, fmt.Errorf("einvoicing: list outgoing e-invoices: %w", err)
	}
	docs, next := pagination.Page(ks, docs, func(o OutgoingEInvoice) (string, uuid.UUID) { return pagination.Instant(o.RenderedAt), o.ID })
	n, err := e.outgoingNames(ctx, docs)
	if err != nil {
		return nil, err
	}
	page := &gen.OutgoingEInvoicePage{Items: make([]gen.OutgoingEInvoice, len(docs))}
	for i, o := range docs {
		page.Items[i] = n.toAPI(o)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return page, nil
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
