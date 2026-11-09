package einvoicing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// connectorPath is a connector's resource path: a fetch's Operation names
// it as its resource until it succeeds (operations.QueuedOf reads it).
func connectorPath(id uuid.UUID) string { return "/v1/connectors/" + id.String() }

// fetchedPath is where a fetch's documents are listed: its Operation's
// resource once it succeeded.
func fetchedPath(operation uuid.UUID) string {
	return "/v1/e-invoices?fetch_operation_id=" + operation.String()
}

// FetchEInvoices implements POST /v1/e-invoices:fetch (ADR-0061 §4): the
// connector is resolved as a fetch would resolve it, so one that cannot
// be fetched from is refused now, and the fetch is queued as its
// Operation, which the workflows dispatcher (or the in-process runner)
// carries out.
func (e *Endpoints) FetchEInvoices(ctx context.Context, req *gen.EInvoiceFetch, _ gen.FetchEInvoicesParams) (gen.FetchEInvoicesRes, error) {
	if e.Objects == nil {
		return nil, problem.Unavailable("E-invoices are kept in object storage, which this server does not have.", 0)
	}
	if _, err := e.Connectors.Provider(ctx, req.ConnectorID); err != nil {
		return nil, err
	}
	op, err := e.Runs.Start(ctx, connectorPath(req.ConnectorID), req)
	if err != nil {
		return nil, fmt.Errorf("einvoicing: operation of a fetch from connector %s: %w", req.ConnectorID, err)
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: op}, nil
}

// inTx runs fn in a transaction of in, with its transaction and
// principal.
func inTx(ctx context.Context, in Within, fn func(ctx context.Context, tx pgx.Tx, p reqctx.Principal) error) error {
	return in(ctx, func(ctx context.Context) error {
		tx, p, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, tx, p)
	})
}

// provider resolves the connector, in a transaction of its own, into the
// provider its adapter names, and says the fetch's tenant.
func (e *Endpoints) provider(ctx context.Context, connector uuid.UUID, in Within) (Connector, Provider, uuid.UUID, error) {
	var c Connector
	var tenant uuid.UUID
	if err := inTx(ctx, in, func(ctx context.Context, _ pgx.Tx, p reqctx.Principal) error {
		var err error
		tenant = p.TenantID
		c, err = e.Connectors.Provider(ctx, connector)
		return err
	}); err != nil {
		return Connector{}, nil, uuid.Nil, err
	}
	p, err := e.Adapter(c)
	return c, p, tenant, err
}

// PendingEInvoices lists the documents the connector's provider holds and
// has not had acknowledged: the first step of a fetch.
func (e *Endpoints) PendingEInvoices(ctx context.Context, connector uuid.UUID, in Within) ([]Message, error) {
	_, p, _, err := e.provider(ctx, connector, in)
	if err != nil {
		return nil, err
	}
	return p.Pending(ctx)
}

// RecordEInvoice records one message of the connector's provider for the
// fetch (ADR-0061 §4): its bytes, read with no transaction open, are kept
// write-once under their SHA-256 (a key already there is the same bytes);
// the document is inserted received, once per connector and message id,
// with its audit record and EInvoiceChanged in one transaction; after that
// commit the provider is told it is kept and acknowledged_at is set. It
// is idempotent: a message recorded before is not inserted again and its
// acknowledgement is sent again, so a lost acknowledgement is repaired;
// one this fetch recorded on an earlier attempt still counts as recorded.
func (e *Endpoints) RecordEInvoice(ctx context.Context, connector, operation uuid.UUID, m Message, in Within) (string, error) {
	c, p, tenant, err := e.provider(ctx, connector, in)
	if err != nil {
		return "", err
	}
	content, err := p.Fetch(ctx, m.ID)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	ref := blobstore.Key(tenant, blobstore.KindDocuments, hex.EncodeToString(sum[:]))
	if err := e.Objects.Put(ctx, ref, content, blobstore.PutOptions{ContentType: "application/xml"}); err != nil && !errors.Is(err, blobstore.ErrExists) {
		return "", fmt.Errorf("einvoicing: keep message %s: %w", m.ID, err)
	}
	var outcome string
	if err := inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal) error {
		outcome, err = e.record(ctx, tx, p, c, operation, m, document{sum: sum[:], size: int64(len(content)), ref: ref})
		return err
	}); err != nil {
		return "", err
	}
	if err := p.Acknowledge(ctx, m.ID); err != nil {
		return "", err
	}
	return outcome, inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal) error {
		return acknowledged(ctx, tx, p, connector, m.ID)
	})
}

// document is a message's bytes as kept.
type document struct {
	sum  []byte
	size int64
	ref  string
}

// record inserts the message's document unless the connector's message is
// recorded already, and says which.
func (e *Endpoints) record(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Connector, operation uuid.UUID, m Message, d document) (string, error) {
	company, err := e.addressee(ctx, c, m.Receiver)
	if err != nil {
		return "", err
	}
	var receiver *string
	if m.Receiver != "" {
		receiver = &m.Receiver
	}
	doc, err := scanEInvoice(tx.QueryRow(ctx, `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, receiver, legal_entity_id, status,
			sha256, size, content_ref, fetch_operation_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'received', $7, $8, $9, $10, $11, $11)
		ON CONFLICT ON CONSTRAINT einvoice_message_once DO NOTHING RETURNING `+einvoiceColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), c.ID, m.ID, receiver, companyID(company), d.sum, d.size, d.ref, operation, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return recordedBy(ctx, tx, c.ID, m.ID, operation)
	}
	if err != nil {
		return "", fmt.Errorf("einvoicing: record message %s of connector %s: %w", m.ID, c.ID, err)
	}
	return Recorded, recordChange(ctx, doc, company)
}

// addressee is the connector's company of the participant the message is
// addressed to; none when it names none of them, or a company the tenant
// no longer has.
func (e *Endpoints) addressee(ctx context.Context, c Connector, receiver string) (*Company, error) {
	for code, participant := range c.Participants {
		if participant != receiver || receiver == "" {
			continue
		}
		co, ok, err := e.Companies.ByCode(ctx, code)
		if err != nil || !ok {
			return nil, err
		}
		return &co, nil
	}
	return nil, nil
}

func companyID(c *Company) *uuid.UUID {
	if c == nil {
		return nil
	}
	return &c.ID
}

// recordedBy says whether the message recorded before was recorded by
// this fetch (on an earlier attempt of the step) or an earlier one.
func recordedBy(ctx context.Context, tx pgx.Tx, connector uuid.UUID, message string, operation uuid.UUID) (string, error) {
	var by uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT fetch_operation_id FROM einvoicing.einvoice WHERE connector_id = $1 AND message_id = $2`, connector, message).
		Scan(&by); err != nil {
		return "", fmt.Errorf("einvoicing: read message %s of connector %s: %w", message, connector, err)
	}
	if by == operation {
		return Recorded, nil
	}
	return AlreadyRecorded, nil
}

// acknowledged records that the provider was told the message is kept,
// once.
func acknowledged(ctx context.Context, tx pgx.Tx, p reqctx.Principal, connector uuid.UUID, message string) error {
	if _, err := tx.Exec(ctx, `UPDATE einvoicing.einvoice SET acknowledged_at = now(), version = version + 1, updated_by = $3
		WHERE connector_id = $1 AND message_id = $2 AND acknowledged_at IS NULL`, connector, message, p.ID); err != nil {
		return fmt.Errorf("einvoicing: acknowledge message %s of connector %s: %w", message, connector, err)
	}
	return nil
}

// recordChange audits a document's change of status and publishes
// EInvoiceChanged (api/asyncapi.yaml); in T65a the one change is its
// receipt, so status_before is null (the processing slices add the others).
func recordChange(ctx context.Context, d EInvoice, company *Company) error {
	payload := map[string]any{"einvoice_id": d.ID, "connector_id": d.ConnectorID, "message_id": d.MessageID, "legal_entity_id": d.LegalEntityID,
		"legal_entity_code": nil, "status": d.Status, "status_before": nil, "sha256": hex.EncodeToString(d.SHA256), "fetch_operation_id": d.FetchOperationID,
		"version": d.Version}
	if company != nil {
		payload["legal_entity_code"] = company.Code
	}
	return audit.Record(ctx, "einvoicing.einvoice."+d.Status, "einvoicing.einvoice", d.ID, payload,
		outbox.Event{Context: "einvoicing", Name: "einvoice_changed", AggregateType: "einvoicing.einvoice", AggregateID: d.ID, Payload: payload})
}

// Failure is a message a fetch did not record: its id and the problem code
// that stopped it (platform.unavailable for a failure of the machinery).
type Failure struct {
	ID, Code string
}

// Counts are what a fetch found, its Operation's result.
type Counts struct {
	Recorded        int `json:"recorded"`
	AlreadyRecorded int `json:"already_recorded"`
}

// FinishEInvoiceFetch ends the fetch's Operation in the caller's
// transaction: succeeded with the counts and the list of the documents it
// recorded as its resource, or, when messages were not recorded (failed),
// failed with einvoicing.fetch_incomplete naming each with its code; the
// next fetch takes them again. An Operation already finished (cancelled)
// is left as it is, so finishing twice changes nothing.
func (e *Endpoints) FinishEInvoiceFetch(ctx context.Context, operation uuid.UUID, counts Counts, failed []Failure) error {
	var err error
	if len(failed) > 0 {
		err = e.Runs.Fail(ctx, operation, incomplete(operation, counts, failed))
	} else {
		err = e.Runs.Succeed(ctx, operation, fetchedPath(operation), counts)
	}
	if problem.From(err).Code == "platform.operation_finished" {
		return nil
	}
	return err
}

// incomplete is the problem a fetch fails with when messages were not
// recorded: each message is a field error, messages.<id>, with the code
// that stopped it, so a consumer tells a refusal (einvoicing.document_too_large,
// which the next fetch meets again) from a failure of the machinery
// (platform.unavailable, which the next fetch may get past).
func incomplete(operation uuid.UUID, counts Counts, failed []Failure) *problem.Problem {
	errs := make([]problem.FieldError, len(failed))
	named := make([]string, len(failed))
	for i, f := range failed {
		errs[i] = problem.FieldError{Field: "messages." + f.ID, Code: f.Code, Message: "Not recorded: " + f.Code + "."}
		named[i] = f.ID + " (" + f.Code + ")"
	}
	return problem.Unprocessable("einvoicing.fetch_incomplete", fmt.Sprintf("The provider's messages %s were not recorded; a message that failed on "+
		"platform.unavailable is taken again by the next fetch, a refused one is refused again. Recorded %d, already recorded %d: GET %s.",
		strings.Join(named, ", "), counts.Recorded, counts.AlreadyRecorded, fetchedPath(operation)), errs...)
}

// FailEInvoiceFetch fails the fetch's Operation with the problem that
// stopped it before any message (the connector refused since it was
// queued), in the caller's transaction; one already finished is left.
func (e *Endpoints) FailEInvoiceFetch(ctx context.Context, operation uuid.UUID, why *problem.Problem) error {
	err := e.Runs.Fail(ctx, operation, why)
	if problem.From(err).Code == "platform.operation_finished" {
		return nil
	}
	return err
}
