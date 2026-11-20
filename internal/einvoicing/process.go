package einvoicing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// StatusRefused is a received document refused by validation (ADR-0061
// §5, §7): kept, never entered, final.
const StatusRefused = "refused"

// Processed is what a fetch's processing found: the documents it refused,
// and the connector's documents still waiting to be processed.
type Processed struct{ Refused, Waiting int }

// unprocessed is a document of the connector not yet processed, as the
// process step reads it before its own transaction.
type unprocessed struct {
	id  uuid.UUID
	ref string
	sum []byte
}

// ProcessEInvoices processes the connector's received documents not yet
// processed, oldest first (ADR-0061 §4 Process, §5; T65c): each one's
// bytes are read from the store with no transaction open, then, in a
// transaction of its own, the document is read FOR UPDATE where it is
// still received and unprocessed, validated (Received, then addressed) and
// either refused, with its problem, the audit record
// einvoicing.einvoice.refused and EInvoiceChanged, or left received with
// what it read and the audit record einvoicing.einvoice.validated (no
// event: its status does not move), for entering (T65d). A document
// already processed is skipped, so the step runs any number of times; it
// answers how many documents this fetch refused, counted from the rows, so
// a step run again after a commit still counts them, and how many of the
// connector's documents are still waiting (left unprocessed until the
// company's setup is completed), so a person sees the gap in the fetch's
// result; a document whose bytes are gone fails the step instead.
func (e *Endpoints) ProcessEInvoices(ctx context.Context, connector, operation uuid.UUID, in Within) (Processed, error) {
	var c Connector
	var docs []unprocessed
	if err := inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		var err error
		if c, err = e.Connectors.Provider(ctx, connector); err != nil {
			return err
		}
		docs, err = unprocessedOf(ctx, tx, connector)
		return err
	}); err != nil {
		return Processed{}, err
	}
	if err := e.processAll(ctx, c, operation, docs, in); err != nil {
		return Processed{}, err
	}
	var out Processed
	return out, inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE processed_fetch_operation_id = $2 AND status = 'refused'),
				count(*) FILTER (WHERE processed_at IS NULL AND status = 'received')
			FROM einvoicing.einvoice WHERE connector_id = $1`, connector, operation).Scan(&out.Refused, &out.Waiting); err != nil {
			return fmt.Errorf("einvoicing: count the documents fetch %s refused: %w", operation, err)
		}
		return nil
	})
}

// processAll processes each document; one that fails does not stop the
// others (as one message's failure does not stop a fetch's recording).
// Afterwards a document whose stored bytes are gone or differ, which no
// retry repairs, fails the step with einvoicing.document_changed naming
// each; any other failure (the store or the database) with its error, for
// the step's retries.
func (e *Endpoints) processAll(ctx context.Context, c Connector, operation uuid.UUID, docs []unprocessed, in Within) error {
	var failed []error
	var changed []string
	for _, d := range docs {
		err := e.processOne(ctx, c, operation, d, in)
		switch {
		case errors.Is(err, errStoredBytes):
			changed = append(changed, d.id.String())
		case err != nil:
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		return errors.Join(failed...)
	}
	if len(changed) > 0 {
		return problem.Conflict("einvoicing.document_changed", "The stored bytes of e-invoices "+atMost(changed)+
			" are gone or do not match the SHA-256 recorded when they were received; they are not processed, the others were.")
	}
	return nil
}

// errStoredBytes is a document whose stored bytes are gone or are not
// those recorded: no retry repairs it.
var errStoredBytes = errors.New("the stored bytes are gone or not those recorded")

// unprocessedOf lists the connector's documents not yet processed, oldest
// received first.
func unprocessedOf(ctx context.Context, tx pgx.Tx, connector uuid.UUID) ([]unprocessed, error) {
	rows, _ := tx.Query(ctx, `SELECT id, content_ref, sha256 FROM einvoicing.einvoice WHERE connector_id = $1 AND processed_at IS NULL
		AND status = 'received' ORDER BY received_at, id`, connector)
	docs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (unprocessed, error) {
		var d unprocessed
		return d, r.Scan(&d.id, &d.ref, &d.sum)
	})
	if err != nil {
		return nil, fmt.Errorf("einvoicing: the documents of connector %s to process: %w", connector, err)
	}
	return docs, nil
}

// processOne validates one document: its bytes read and compared with the
// hash recorded (a store that holds other bytes fails the step, as the
// download refuses them), then its row locked and its outcome written.
func (e *Endpoints) processOne(ctx context.Context, c Connector, operation uuid.UUID, u unprocessed, in Within) error {
	content, _, err := e.Objects.Get(ctx, u.ref)
	switch {
	case errors.Is(err, blobstore.ErrNotFound):
		return fmt.Errorf("einvoicing: read the bytes of e-invoice %s: %w", u.id, errStoredBytes)
	case err != nil:
		return fmt.Errorf("einvoicing: read the bytes of e-invoice %s: %w", u.id, err)
	}
	if sum := sha256.Sum256(content); !bytes.Equal(sum[:], u.sum) {
		return fmt.Errorf("einvoicing: e-invoice %s: %w", u.id, errStoredBytes)
	}
	read, refusal := Received(content, currency.SeededExponent)
	return inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal) error {
		doc, err := scanEInvoice(tx.QueryRow(ctx, `SELECT `+einvoiceColumns+` FROM einvoicing.einvoice WHERE id = $1 AND status = 'received'
			AND processed_at IS NULL FOR UPDATE`, u.id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // processed meanwhile by another fetch
		}
		if err != nil {
			return fmt.Errorf("einvoicing: lock e-invoice %s: %w", u.id, err)
		}
		company, err := e.companyOf(ctx, doc)
		if err != nil {
			return err
		}
		if refusal == nil {
			var later bool
			if refusal, later, err = e.addressed(ctx, c, company, read); err != nil || later {
				return err // later: left unprocessed for a fetch once the setup is complete
			}
		}
		return e.processed(ctx, tx, p, operation, doc, company, read, refusal)
	})
}

// addressed refuses a document not addressed to the company the provider
// listed it for (ADR-0061 §5 C6): its buyer's electronic address (IBT-049,
// scheme 0235 and the company's TIN) must be the connector's participant
// of that company, and its buyer's VAT TRN (IBT-048), checked apart, the
// number of the company's FTA registration on the issue date (a tax
// group's for a member, as the registration holds it). Neither TRN is
// put in the detail. A gap in the ERP's own setup is no ground to refuse
// the supplier's document for good: when the connector no longer names a
// participant of the company, or the company holds no FTA registration on
// the issue date, the document is left unprocessed (later), and a fetch
// after the setup is completed processes it.
func (e *Endpoints) addressed(ctx context.Context, c Connector, company *Company, d Document) (*Refusal, bool, error) {
	if company == nil {
		return &Refusal{Code: RefusedNotAddressed, Detail: "The provider listed it for a participant of none of connector " + c.Name + "'s companies."},
			false, nil
	}
	endpoint := d.Buyer.EndpointScheme + ":" + d.Buyer.Endpoint
	participant, ok := c.Participants[company.Code]
	switch {
	case !ok:
		return nil, true, nil
	case endpoint != participant:
		return &Refusal{Code: RefusedNotAddressed, Detail: fmt.Sprintf("Its buyer's electronic address (IBT-049) is %s, not company %s's participant %s.",
			clip(endpoint), company.Code, participant)}, false, nil
	}
	s, err := e.Sales.Seller(ctx, company.ID, d.IssueDate)
	switch {
	case err != nil:
		return nil, false, err
	case s.TRN == "":
		return nil, true, nil
	case s.TRN != d.Buyer.TRN:
		return &Refusal{Code: RefusedNotAddressed, Detail: "Its buyer's TRN (IBT-048), ending " + d.Buyer.TRN[len(d.Buyer.TRN)-4:] +
			", is not the number of company " + company.Code + "'s FTA registration on " + d.IssueDate.Format(time.DateOnly) + ", its issue date."}, false, nil
	}
	return nil, false, nil
}

// processed writes the outcome of processing the locked document: refused
// with its problem, or still received; either way what it read, when, by
// which fetch and against which release. The update is guarded on the
// document still unprocessed, which the lock holds.
func (e *Endpoints) processed(ctx context.Context, tx pgx.Tx, p reqctx.Principal, operation uuid.UUID, doc EInvoice, company *Company, d Document,
	refusal *Refusal) error {
	status, code, detail := StatusReceived, (*string)(nil), (*string)(nil)
	if refusal != nil {
		status, code, detail = StatusRefused, &refusal.Code, &refusal.Detail
	}
	f := readFields(d)
	after, err := scanEInvoice(tx.QueryRow(ctx, `UPDATE einvoicing.einvoice SET status = $2, problem_code = $3, problem_detail = $4,
			processed_at = now(), processed_fetch_operation_id = $5, validated_against = $6, document_number = $7, document_kind = $8,
			issue_date = $9, currency = $10, gross_amount = $11, specification = $12, version = version + 1, updated_by = $13
		WHERE id = $1 AND status = 'received' AND processed_at IS NULL RETURNING `+einvoiceColumns,
		doc.ID, status, code, detail, operation, PINTAERelease, f.number, f.kind, f.issued, f.currency, f.gross, f.specification, p.ID))
	if err != nil {
		return fmt.Errorf("einvoicing: record the processing of e-invoice %s: %w", doc.ID, err)
	}
	if refusal == nil {
		return recordChange(ctx, after, company, validated)
	}
	return recordChange(ctx, after, company, refused)
}

// read are what a processed document's row keeps of what it says: none of
// it for a document that could not be read.
type read struct {
	number, kind, currency, specification *string
	issued                                *time.Time
	gross                                 *money.Minor
}

func readFields(d Document) read {
	if d.Number == "" {
		return read{}
	}
	gross := d.TaxInclusive
	return read{number: &d.Number, kind: &d.Kind, currency: &d.Currency, specification: &d.Customization, issued: &d.IssueDate, gross: &gross}
}
