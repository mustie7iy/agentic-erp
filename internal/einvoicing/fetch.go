package einvoicing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
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

// QueueEInvoiceFetch records, in the caller's transaction, the Operation of
// a scheduled fetch from the connector (ADR-0061 §4): the workflow
// ScheduledEInvoiceFetch chose its id and carries it out, so the Operation
// names that workflow at once and no dispatcher starts another. Recording
// it again is not an error, so the step runs any number of times. The
// connector is resolved by the fetch's first step, which fails the
// Operation with the problem when it cannot be fetched from.
func (e *Endpoints) QueueEInvoiceFetch(ctx context.Context, operation, connector uuid.UUID, workflowID string) error {
	return e.Runs.Queue(ctx, operation, connectorPath(connector), gen.EInvoiceFetch{ConnectorID: connector}, workflowID)
}

// FetchEInvoices implements POST /v1/e-invoices:fetch (ADR-0061 §4): the
// connector is read as a fetch would read it, without calling the
// provider, so one that cannot be fetched from is refused now, and the
// fetch is queued as its Operation, which the workflows dispatcher (or the
// in-process runner) carries out. Its credentials are not resolved here,
// where the request's transaction is open: the fetch's first step resolves
// them with none open and fails the Operation when they do not resolve
// (As built E3). The request is audited in the caller's name
// (einvoicing.fetch.request, on the Operation; ADR-0011), since what the
// fetch records it records as the system: the trail shows who fetched.
func (e *Endpoints) FetchEInvoices(ctx context.Context, req *gen.EInvoiceFetch, _ gen.FetchEInvoicesParams) (gen.FetchEInvoicesRes, error) {
	if e.Objects == nil {
		return nil, noStore()
	}
	if _, err := e.Connectors.Provider(ctx, req.ConnectorID); err != nil {
		return nil, err
	}
	op, err := e.Runs.Start(ctx, connectorPath(req.ConnectorID), req)
	if err != nil {
		return nil, fmt.Errorf("einvoicing: operation of a fetch from connector %s: %w", req.ConnectorID, err)
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: op},
		audit.Record(ctx, "einvoicing.fetch.request", "platform.operation", op.ID, map[string]any{"connector_id": req.ConnectorID, "operation_id": op.ID})
}

// noStore refuses work on a server without object storage, where an
// e-invoice's bytes could not be kept.
func noStore() *problem.Problem {
	return problem.Unavailable("E-invoices are kept in object storage, which this server does not have.", 0)
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

// provider reads the connector in a transaction of its own and, once that
// committed, resolves its credentials, with no transaction open while the
// vault is called (As built E3), into the provider its adapter names; it
// says the fetch's tenant.
func (e *Endpoints) provider(ctx context.Context, connector uuid.UUID, in Within) (Connector, Provider, uuid.UUID, error) {
	var c Connector
	if err := inTx(ctx, in, func(ctx context.Context, _ pgx.Tx, _ reqctx.Principal) error {
		var err error
		c, err = e.Connectors.Provider(ctx, connector)
		return err
	}); err != nil {
		return Connector{}, nil, uuid.Nil, err
	}
	secret, err := e.Connectors.Credentials(ctx, c)
	if err != nil {
		return Connector{}, nil, uuid.Nil, err
	}
	c.Secret = secret
	p, err := e.Adapter(c)
	return c, p, c.Tenant, err
}

// PendingEInvoices lists the documents the connector's provider holds and
// has not had acknowledged: the first step of a fetch.
//
// A server without object storage refuses it before the provider is asked,
// as FetchEInvoices refuses to queue a fetch there: a scheduled fetch
// reaches this step without that request. A provider that keeps no
// acknowledgement (LocalAcknowledgement: Evatra) lists documents already
// kept: those recorded and acknowledged are left out, in a transaction
// of their own after the provider answered, so the fetch takes only the
// new ones and those whose acknowledgement was lost.
func (e *Endpoints) PendingEInvoices(ctx context.Context, connector uuid.UUID, in Within) ([]Message, error) {
	if e.Objects == nil {
		return nil, noStore()
	}
	_, p, _, err := e.provider(ctx, connector, in)
	if err != nil {
		return nil, err
	}
	ms, err := p.Pending(ctx)
	if _, local := p.(LocalAcknowledgement); err != nil || !local || len(ms) == 0 {
		return ms, err
	}
	var out []Message
	return out, inTx(ctx, in, func(ctx context.Context, tx pgx.Tx, _ reqctx.Principal) error {
		out, err = unacknowledged(ctx, tx, connector, ms)
		return err
	})
}

// unacknowledged are the messages of ms that the connector's documents do
// not hold as recorded and acknowledged, in ms's order.
func unacknowledged(ctx context.Context, tx pgx.Tx, connector uuid.UUID, ms []Message) ([]Message, error) {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	rows, _ := tx.Query(ctx, `SELECT message_id FROM einvoicing.einvoice
		WHERE connector_id = $1 AND message_id = ANY($2) AND acknowledged_at IS NOT NULL`, connector, ids)
	kept := map[string]bool{}
	var id string
	if _, err := pgx.ForEachRow(rows, []any{&id}, func() error { kept[id] = true; return nil }); err != nil {
		return nil, fmt.Errorf("einvoicing: the documents of connector %s kept already: %w", connector, err)
	}
	return slices.DeleteFunc(ms, func(m Message) bool { return kept[m.ID] }), nil
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
// A message recorded before whose bytes differ is recorded as changed
// (redelivered), once per different bytes.
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
		return e.redelivered(ctx, tx, p, c.ID, operation, m.ID, d.sum)
	}
	if err != nil {
		return "", fmt.Errorf("einvoicing: record message %s of connector %s: %w", m.ID, c.ID, err)
	}
	return Recorded, recordChange(ctx, doc, company, received)
}

// redelivered handles a message recorded before (ADR-0061 §4). Bytes other
// than those kept and than the change last recorded set changed_hash, by
// an update guarded on that, with the audit record and EInvoiceChanged:
// the row keeps its first bytes, the legal original (§8), and the same
// changed bytes delivered again (their acknowledgement lost) change and
// publish nothing again. Otherwise it says who recorded what was found.
func (e *Endpoints) redelivered(ctx context.Context, tx pgx.Tx, p reqctx.Principal, connector, operation uuid.UUID, message string, sum []byte) (string, error) {
	doc, err := scanEInvoice(tx.QueryRow(ctx, `UPDATE einvoicing.einvoice SET changed_hash = $3, changed_at = now(), changed_fetch_operation_id = $4,
			version = version + 1, updated_by = $5
		WHERE connector_id = $1 AND message_id = $2 AND sha256 <> $3 AND changed_hash IS DISTINCT FROM $3 RETURNING `+einvoiceColumns,
		connector, message, sum, operation, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return recordedBy(ctx, tx, connector, message, sum, operation)
	}
	if err != nil {
		return "", fmt.Errorf("einvoicing: record the change of message %s of connector %s: %w", message, connector, err)
	}
	company, err := e.companyOf(ctx, doc)
	if err != nil {
		return "", err
	}
	return Changed, recordChange(ctx, doc, company, bytesChanged)
}

// companyOf is the document's company; none when it has none.
func (e *Endpoints) companyOf(ctx context.Context, d EInvoice) (*Company, error) {
	if d.LegalEntityID == nil {
		return nil, nil
	}
	named, err := idref.Resolve(ctx, e.Companies.Named, []uuid.UUID{*d.LegalEntityID}, companyRef)
	if err != nil {
		return nil, err
	}
	co := named[*d.LegalEntityID]
	return &co, nil
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

// recordedBy says, of a message recorded before with the bytes sum (the
// first, or the change last recorded), whether this fetch recorded them
// (on an earlier attempt of the step: recorded or changed) or an earlier
// one (already recorded).
func recordedBy(ctx context.Context, tx pgx.Tx, connector uuid.UUID, message string, sum []byte, operation uuid.UUID) (string, error) {
	var first bool
	var by *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT sha256 = $3, CASE WHEN sha256 = $3 THEN fetch_operation_id ELSE changed_fetch_operation_id END
		FROM einvoicing.einvoice WHERE connector_id = $1 AND message_id = $2`, connector, message, sum).Scan(&first, &by); err != nil {
		return "", fmt.Errorf("einvoicing: read message %s of connector %s: %w", message, connector, err)
	}
	switch {
	case by == nil || *by != operation:
		return AlreadyRecorded, nil
	case first:
		return Recorded, nil
	}
	return Changed, nil
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

// change is what changed of a document: its receipt, its bytes, its
// validation (status unchanged) or its refusal (T65c), its entering or its
// hold (T65d).
type change int

const (
	received change = iota
	bytesChanged
	validated
	refused
	entered
	held
)

// changeActions are the audit actions of the changes.
var changeActions = map[change]string{received: "einvoicing.einvoice.received", bytesChanged: "einvoicing.einvoice.bytes_changed",
	validated: "einvoicing.einvoice.validated", refused: "einvoicing.einvoice.refused", entered: "einvoicing.einvoice.entered",
	held: "einvoicing.einvoice.held"}

// recordChange audits a document's change and, but for its validation,
// publishes EInvoiceChanged (api/asyncapi.yaml): its receipt (status_before
// nil), a change of its bytes (status_before its status, which does not
// move) or its refusal (status_before received, with its problem code).
// Every one carries the change of bytes the document holds, if any, the
// fetch that found it and the problem code, null but for a refusal; never
// the problem's detail, which may quote the document (ADR-0079). A
// validation moves no status, so it publishes nothing, as an
// acknowledgement does not (§12).
func recordChange(ctx context.Context, d EInvoice, company *Company, what change) error {
	payload := map[string]any{"einvoice_id": d.ID, "connector_id": d.ConnectorID, "message_id": d.MessageID, "legal_entity_id": d.LegalEntityID,
		"legal_entity_code": nil, "status": d.Status, "status_before": nil, "sha256": hex.EncodeToString(d.SHA256), "changed_sha256": nil,
		"changed_fetch_operation_id": d.ChangedFetchedBy, "fetch_operation_id": d.FetchOperationID, "problem_code": d.ProblemCode,
		"supplier_invoice_id": d.SupplierInvoiceID, "version": d.Version}
	if company != nil {
		payload["legal_entity_code"] = company.Code
	}
	if d.ChangedHash != nil {
		payload["changed_sha256"] = hex.EncodeToString(d.ChangedHash)
	}
	switch what {
	case received: // status_before stays null: it had none
	case bytesChanged:
		payload["status_before"] = d.Status
	case refused, validated, entered, held:
		payload["status_before"] = StatusReceived
	}
	if what == validated {
		return audit.Record(ctx, changeActions[what], "einvoicing.einvoice", d.ID, payload)
	}
	return audit.Record(ctx, changeActions[what], "einvoicing.einvoice", d.ID, payload,
		outbox.Event{Context: "einvoicing", Name: "einvoice_changed", AggregateType: "einvoicing.einvoice", AggregateID: d.ID, Payload: payload})
}

// Failure is a message a fetch did not record: its id and the problem code
// that stopped it (platform.unavailable for a failure of the machinery).
type Failure struct {
	ID, Code string
}

// Counts are what a fetch found, its Operation's result: the documents it
// recorded, found recorded already or changed, refused, entered and held
// (T65d), and the connector's documents still waiting to be processed
// (T65c).
type Counts struct {
	Recorded        int `json:"recorded"`
	AlreadyRecorded int `json:"already_recorded"`
	Changed         int `json:"changed"`
	Refused         int `json:"refused"`
	Entered         int `json:"entered"`
	Held            int `json:"held"`
	Waiting         int `json:"waiting"`
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
		"platform.unavailable is taken again by the next fetch, a refused one is refused again. Recorded %d, already recorded %d, changed %d, "+
		"refused %d, entered %d, held %d: GET %s.", strings.Join(named, ", "), counts.Recorded, counts.AlreadyRecorded, counts.Changed, counts.Refused,
		counts.Entered, counts.Held, fetchedPath(operation)), errs...)
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
