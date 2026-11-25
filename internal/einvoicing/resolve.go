package einvoicing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A person resolves a held e-invoice (ADR-0061 §7, T65f): retries its
// entering once its cause is corrected, links it to the invoice they
// entered by hand, or dismisses it. Each reads the document FOR UPDATE at
// the caller's ETag and refuses one no longer held; the bytes a retry or a
// link reads are read before the lock, so no row lock is held across the
// call to the store. People only: agents never retry, link or dismiss
// (x-agent: false, §13).

func init() {
	// At most one document names an invoice (einvoice_invoice_once): the
	// database's guard behind linkable's check.
	problem.RegisterConstraint("einvoice_invoice_once", func() *problem.Problem {
		return problem.Conflict("einvoicing.invoice_already_linked", "The supplier invoice is another e-invoice's already; an invoice is at most one "+
			"e-invoice's.")
	})
}

// StatusDismissed is a held document a person closed (T65f): final,
// keeping the problem it was held with and the reason given.
const StatusDismissed = "dismissed"

// A person's resolutions of a held document, the last one kept
// (resolution): retried (entered, or held again with that attempt's
// problem), linked (entered as the invoice the person entered by hand) or
// dismissed.
const (
	ResolutionRetried   = "retried"
	ResolutionLinked    = "linked"
	ResolutionDismissed = "dismissed"
)

// LinkTarget is a supplier invoice as a link checks it: its company, its
// supplier and reference, how it was entered, its status, its kind and
// the kind of e-invoice one of that kind is entered from ("" for none,
// the caller's mapping, so that entering and a link share it), its
// invoice date, currency, gross and tax amounts, and whether it has a
// document of its own.
type LinkTarget struct {
	LegalEntityID, SupplierID         uuid.UUID
	SupplierReference, Source, Status string
	Kind, EnteredFrom, Currency       string
	InvoiceDate                       time.Time
	Gross, Tax                        money.Minor
	HasDocument                       bool
}

// sourceManual is the source of an invoice a person entered by hand, the
// only one a held document is linked to.
const sourceManual = "manual"

// endedStatuses are the statuses of an invoice that will never be posted
// or that a reversal took back: a held document linked to one would be
// entered as an invoice that does not stand (coordinator, 24 Nov).
var endedStatuses = map[string]bool{"rejected": true, "cancelled": true, "reversed": true}

// RetryEInvoice implements POST /v1/e-invoices/{id}:retry: the held
// document entered again in the request's transaction, as a fetch enters
// one (enter), and answered entered or still held with this attempt's
// problem.
func (e *Endpoints) RetryEInvoice(ctx context.Context, params gen.RetryEInvoiceParams) (gen.RetryEInvoiceRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	content, err := e.heldContent(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	doc, company, err := e.lockHeld(ctx, tx, params.ID, params.IfMatch)
	if err != nil {
		return nil, err
	}
	read, refusal := Received(content.bytes, currency.SeededExponent)
	out := entering{held: refusal} // valid when processed, refused by the rules since: held with that refusal, as a fetch holds it
	if refusal == nil {
		out.held = heldBefore(read)
	}
	if out.held == nil {
		entry := Entry{LegalEntityID: company.ID, Document: read, SHA256: doc.SHA256, ContentRef: content.ref}
		if out, err = e.tryEnter(ctx, tx, doc.ID, entry); err != nil {
			return nil, err
		}
	}
	r := resolution{kind: ResolutionRetried, status: StatusHeld}
	if out.entered != nil {
		r.status, r.invoice, r.supplier = StatusEntered, &out.entered.InvoiceID, &out.entered.SupplierID
	} else {
		d := clipDetail(out.held.Detail)
		r.code, r.detail = &out.held.Code, &d
	}
	return e.resolved(ctx, tx, p, doc, company, r)
}

// LinkEInvoice implements POST /v1/e-invoices/{id}:link: the held document
// entered as the invoice a person entered by hand, which takes its bytes
// as its document (payables' Attach).
func (e *Endpoints) LinkEInvoice(ctx context.Context, req *gen.EInvoiceLink, params gen.LinkEInvoiceParams) (gen.LinkEInvoiceRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	content, err := e.heldContent(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	doc, company, err := e.lockHeld(ctx, tx, params.ID, params.IfMatch)
	if err != nil {
		return nil, err
	}
	target, err := e.linkable(ctx, tx, doc, req.SupplierInvoiceID, content.bytes)
	if err != nil {
		return nil, err
	}
	if err := e.Payables.Attach(ctx, req.SupplierInvoiceID, doc.SHA256, content.ref); err != nil {
		return nil, err
	}
	return e.resolved(ctx, tx, p, doc, company, resolution{kind: ResolutionLinked, status: StatusEntered, invoice: &req.SupplierInvoiceID,
		supplier: &target.SupplierID})
}

// DismissEInvoice implements POST /v1/e-invoices/{id}:dismiss: the held
// document closed with the reason given, final.
func (e *Endpoints) DismissEInvoice(ctx context.Context, req *gen.EInvoiceDismissal, params gen.DismissEInvoiceParams) (gen.DismissEInvoiceRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if err := problem.RequireText("reason", req.Reason); err != nil {
		return nil, err
	}
	doc, company, err := e.lockHeld(ctx, tx, params.ID, params.IfMatch)
	if err != nil {
		return nil, err
	}
	return e.resolved(ctx, tx, p, doc, company, resolution{kind: ResolutionDismissed, status: StatusDismissed, code: doc.ProblemCode,
		detail: doc.ProblemDetail, reason: &req.Reason})
}

// stored is a document's bytes as received, read and compared with the
// hash recorded, and their key.
type stored struct {
	bytes []byte
	ref   string
}

// heldContent reads the bytes of a held document the caller may see, with
// no row locked: a document of another company is not found, one not held
// is refused before the store is read, and bytes gone or other than those
// received are refused (409 einvoicing.document_changed), as the download
// refuses them.
func (e *Endpoints) heldContent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (stored, error) {
	d, ref, err := visibleEInvoice(ctx, tx, id, false)
	if err != nil {
		return stored{}, err
	}
	if err := heldNow(d); err != nil {
		return stored{}, err
	}
	if e.Objects == nil {
		return stored{}, noStore()
	}
	content, _, err := e.Objects.Get(ctx, ref)
	switch {
	case errors.Is(err, blobstore.ErrNotFound):
		return stored{}, problem.Conflict("einvoicing.document_changed", "The stored bytes of e-invoice "+d.ID.String()+" are gone.")
	case err != nil:
		return stored{}, problem.Unavailable("The document store did not answer; try again.", 30).WithCause(err)
	}
	if sum := sha256.Sum256(content); !bytes.Equal(sum[:], d.SHA256) {
		return stored{}, problem.Conflict("einvoicing.document_changed", "The stored bytes of e-invoice "+d.ID.String()+
			" do not match the SHA-256 recorded when it was received.")
	}
	return stored{bytes: content, ref: ref}, nil
}

// lockHeld reads the document FOR UPDATE, as the caller may see it, at the
// caller's ETag (412 request.version_mismatch otherwise), still held (409
// einvoicing.einvoice_not_held otherwise), with its company.
func (e *Endpoints) lockHeld(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch string) (EInvoice, *Company, error) {
	d, _, err := visibleEInvoice(ctx, tx, id, true)
	if err != nil {
		return EInvoice{}, nil, err
	}
	if err := etag.Check(ifMatch, d.Version); err != nil {
		return EInvoice{}, nil, err
	}
	if err := heldNow(d); err != nil {
		return EInvoice{}, nil, err
	}
	company, err := e.companyOf(ctx, d) // a held document was addressed to a company of the connector
	return d, company, err
}

// visibleEInvoice reads a document and its object key, FOR UPDATE when
// lock: one of a company the caller may not act in is not found, as one
// that does not exist.
func visibleEInvoice(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (EInvoice, string, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return EInvoice{}, "", err
	}
	q := `SELECT ` + einvoiceColumns + `, content_ref FROM einvoicing.einvoice WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	var d EInvoice
	var ref string
	err = tx.QueryRow(ctx, q, id).Scan(append(d.fields(), &ref)...)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !visible(d, access)):
		return EInvoice{}, "", problem.NotFoundOf("e-invoice")
	case err != nil:
		return EInvoice{}, "", fmt.Errorf("einvoicing: read e-invoice %s: %w", id, err)
	}
	return d, ref, nil
}

// heldNow refuses a document that is not held: only a held one is
// resolved by a person.
func heldNow(d EInvoice) error {
	if d.Status == StatusHeld {
		return nil
	}
	return problem.Conflict("einvoicing.einvoice_not_held", "E-invoice "+d.MessageID+" is "+d.Status+", not held; only a held e-invoice is "+
		"retried, linked or dismissed.")
}

// linkable checks the invoice a person would link the locked document to,
// read FOR UPDATE: one the tenant has none of, or of a company the caller
// may not act in, is no reference (422); one another document names
// already is 409 einvoicing.invoice_already_linked (read under the
// invoice's lock, which a concurrent link of it waits for); one not
// entered by hand, rejected, cancelled or reversed, of another company,
// supplier (the seller's TRN not its tax identifier), kind, date,
// currency, gross or tax amount or reference, or with a document of its
// own, is 422
// einvoicing.link_mismatch naming each that differs. The invoice's lock
// is held to the attach, so what is compared is what is attached to.
func (e *Endpoints) linkable(ctx context.Context, tx pgx.Tx, doc EInvoice, invoice uuid.UUID, content []byte) (LinkTarget, error) {
	target, ok, err := e.Payables.Linkable(ctx, invoice)
	switch {
	case err != nil:
		return LinkTarget{}, err
	case !ok:
		return LinkTarget{}, problem.ReferenceNotFound("supplier_invoice_id", "supplier invoice", "a supplier invoice of a company you may act in")
	}
	var other string
	err = tx.QueryRow(ctx, `SELECT message_id FROM einvoicing.einvoice WHERE supplier_invoice_id = $1 AND id <> $2`, invoice, doc.ID).Scan(&other)
	switch {
	case err == nil:
		return LinkTarget{}, problem.Conflict("einvoicing.invoice_already_linked", "Supplier invoice "+invoice.String()+" is e-invoice "+other+
			"'s already; an invoice is at most one e-invoice's.")
	case !errors.Is(err, pgx.ErrNoRows):
		return LinkTarget{}, fmt.Errorf("einvoicing: the e-invoice naming supplier invoice %s: %w", invoice, err)
	}
	differs, err := e.differences(ctx, doc, target, content)
	if err != nil || len(differs) == 0 {
		return target, err
	}
	fields := make([]problem.FieldError, len(differs))
	msgs := make([]string, len(differs))
	for i, d := range differs {
		fields[i], msgs[i] = problem.FieldError{Field: "supplier_invoice_id", Code: "mismatch", Message: d}, d
	}
	return LinkTarget{}, problem.Unprocessable("einvoicing.link_mismatch", "Supplier invoice "+invoice.String()+" is not the one entered by hand "+
		"from e-invoice "+doc.MessageID+": "+strings.Join(msgs, "; ")+".", fields...)
}

// differences are what makes the invoice another than the document's:
// not entered by hand, rejected, cancelled or reversed, another company,
// what the document states and the invoice does not (statedDifferences),
// another supplier reference than the document's number, or a document
// of its own.
func (e *Endpoints) differences(ctx context.Context, doc EInvoice, target LinkTarget, content []byte) ([]string, error) {
	var out []string
	if target.Source != sourceManual {
		out = append(out, "it was not entered by hand (source "+target.Source+")")
	}
	if endedStatuses[target.Status] {
		out = append(out, "it is "+target.Status)
	}
	if doc.LegalEntityID == nil || target.LegalEntityID != *doc.LegalEntityID {
		out = append(out, "it is another company's")
	}
	read, refusal := Received(content, currency.SeededExponent)
	switch {
	case refusal != nil: // validated when processed, refused by the rules since
		out = append(out, "the e-invoice's seller and amounts cannot be read now ("+refusal.Code+")")
	default:
		stated, err := e.statedDifferences(ctx, read, target)
		if err != nil {
			return nil, err
		}
		out = append(out, stated...)
	}
	if doc.DocumentNumber == nil || target.SupplierReference != *doc.DocumentNumber {
		out = append(out, "its supplier reference "+target.SupplierReference+" is not the e-invoice's number (IBT-001)")
	}
	if target.HasDocument {
		out = append(out, "it has a document of its own")
	}
	return out, nil
}

// statedDifferences are what the e-invoice states that the invoice does
// not, each a figure an entered e-invoice takes (ADR-0061 §6): a supplier
// not holding the seller's TRN; another kind than the document's
// (IBT-003: a tax invoice is an invoice, a tax credit note a credit
// memo); another invoice date than its issue date (IBT-002, which sets
// the VAT period); another currency (IBT-005); in the same currency,
// another gross amount than its total with VAT (IBT-112) or another tax
// amount than its VAT total (IBT-110), compared exactly in minor units.
func (e *Endpoints) statedDifferences(ctx context.Context, read Document, target LinkTarget) ([]string, error) {
	var out []string
	holds, err := e.Payables.HoldsTaxNumber(ctx, target.SupplierID, read.Seller.TRN)
	if err != nil {
		return nil, err
	}
	if !holds {
		out = append(out, "its supplier does not hold the seller's TRN (IBT-031) ending "+read.Seller.TRN[max(0, len(read.Seller.TRN)-4):])
	}
	if target.EnteredFrom != read.Kind {
		out = append(out, "its kind "+target.Kind+" is not the kind the e-invoice's "+read.Kind+" (IBT-003) is entered as")
	}
	if day := read.IssueDate.Format(time.DateOnly); target.InvoiceDate.Format(time.DateOnly) != day {
		out = append(out, "its invoice date "+target.InvoiceDate.Format(time.DateOnly)+" is not the e-invoice's issue date "+day+" (IBT-002)")
	}
	if target.Currency != read.Currency {
		return append(out, "its currency "+target.Currency+" is not the e-invoice's "+read.Currency+" (IBT-005)"), nil
	}
	if target.Gross != read.TaxInclusive {
		out = append(out, "its gross amount "+target.Gross.Plain(read.Exponent)+" "+target.Currency+" is not the e-invoice's total with VAT "+
			read.TaxInclusive.Plain(read.Exponent)+" "+read.Currency+" (IBT-112)")
	}
	if target.Tax != read.Tax {
		out = append(out, "its tax amount "+target.Tax.Plain(read.Exponent)+" "+target.Currency+" is not the e-invoice's VAT total "+
			read.Tax.Plain(read.Exponent)+" "+read.Currency+" (IBT-110)")
	}
	return out, nil
}

// resolution is what a person's resolution writes on a held document:
// its kind and the status it leaves, the invoice and supplier of an
// entered one, the problem of one held again or dismissed, and a
// dismissal's reason.
type resolution struct {
	kind, status      string
	invoice, supplier *uuid.UUID
	code, detail      *string
	reason            *string
}

// resolved writes the resolution on the locked document, guarded on it
// still held, audits and publishes it, and answers the document with its
// ETag.
func (e *Endpoints) resolved(ctx context.Context, tx pgx.Tx, p reqctx.Principal, doc EInvoice, company *Company, r resolution) (*gen.DismissEInvoiceOKHeaders, error) {
	after, err := scanEInvoice(tx.QueryRow(ctx, `UPDATE einvoicing.einvoice SET status = $2, problem_code = $3, problem_detail = $4, supplier_invoice_id = $5,
			supplier_id = $6, resolution = $7, resolved_at = now(), dismissal_reason = $8, version = version + 1, updated_by = $9
		WHERE id = $1 AND status = 'held' RETURNING `+einvoiceColumns,
		doc.ID, r.status, r.code, r.detail, r.invoice, r.supplier, r.kind, r.reason, p.ID))
	if err != nil {
		return nil, fmt.Errorf("einvoicing: record the %s of e-invoice %s: %w", r.kind, doc.ID, err)
	}
	if err := recordChange(ctx, after, company, resolutionChanges[r.kind]); err != nil {
		return nil, err
	}
	n, err := e.names(ctx, []EInvoice{after})
	if err != nil {
		return nil, err
	}
	return &gen.DismissEInvoiceOKHeaders{ETag: gen.NewOptString(etag.Format(after.Version)), Response: n.toAPI(after)}, nil
}
