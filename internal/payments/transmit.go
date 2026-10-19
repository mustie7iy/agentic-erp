package payments

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

// File is a bank file handed to a connector.
type File struct {
	ID      uuid.UUID
	Name    string // <company code>-<run number>-<bank file id>.xml
	Content []byte
}

// Connector sends bank files to the bank (EBICS, SFTP, host to host).
type Connector interface {
	Transmit(ctx context.Context, f File) error
}

// DirConnector drops files into a directory that a bank's file transfer
// collects from. A file appears complete or not at all.
type DirConnector struct {
	Dir string
}

// Transmit writes the file under a hidden name and renames it.
func (d DirConnector) Transmit(_ context.Context, f File) error {
	tmp := filepath.Join(d.Dir, ".partial-"+f.Name)
	if err := os.WriteFile(tmp, f.Content, 0o600); err != nil {
		return fmt.Errorf("payments: write bank file %s: %w", f.ID, err)
	}
	if err := os.Rename(tmp, filepath.Join(d.Dir, f.Name)); err != nil {
		return fmt.Errorf("payments: publish bank file %s: %w", f.ID, err)
	}
	return nil
}

// BankChannels resolves the connector a house bank names into the SFTP
// endpoint its files go to (integration context, through the application's
// wiring; ADR-0038, T44b). It fails closed: a channel it cannot use is a
// problem, and nothing is sent.
type BankChannels interface {
	Endpoint(ctx context.Context, connector uuid.UUID) (sftpclient.Endpoint, error)
}

// SFTPConnector uploads bank files to a house bank's SFTP server.
type SFTPConnector struct {
	Endpoint sftpclient.Endpoint
}

// Transmit uploads the file under its name; it appears there whole.
func (s SFTPConnector) Transmit(ctx context.Context, f File) error {
	if err := s.Endpoint.Upload(ctx, f.Name, f.Content); err != nil {
		return fmt.Errorf("payments: upload bank file %s: %w", f.ID, err)
	}
	return nil
}

// channel is the connector a house bank's files go through, and its id:
// the bank's own connector, or, for a bank that names none, the server's
// (DirConnector, id nil; 503 when the server has none).
func (e *Endpoints) channel(ctx context.Context, tx pgx.Tx, bank uuid.UUID) (Connector, *uuid.UUID, error) {
	var id *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT connector_id FROM payments.house_bank WHERE id = $1`, bank).Scan(&id); err != nil {
		return nil, nil, fmt.Errorf("payments: channel of house bank %s: %w", bank, err)
	}
	if id == nil {
		if e.Connector == nil {
			return nil, nil, problem.Unavailable("Bank file transmission is not configured on this server.", 0)
		}
		return e.Connector, nil, nil
	}
	ep, err := e.Channels.Endpoint(ctx, *id)
	if err != nil {
		return nil, nil, err
	}
	return SFTPConnector{Endpoint: ep}, id, nil
}

// send uploads the file through conn unless its bank already took a file
// with its content hash, and records the upload in a transaction of its
// own (T44c). It reports whether the file counted as sent already.
func (e *Endpoints) send(ctx context.Context, tx pgx.Tx, p reqctx.Principal, f BankFile, conn Connector, connectorID *uuid.UUID) (bool, error) {
	if dup, err := sent(ctx, tx, f); err != nil || dup {
		return dup, err
	}
	r, err := load(ctx, tx, f.RunID, false)
	if err != nil {
		return false, err
	}
	le, err := e.entityOf(ctx, r)
	if err != nil { // errNoEntity included: an existing run's missing entity is a 500, as on the download
		return false, err
	}
	content, err := e.fileOf(ctx, tx, p.TenantID, f)
	if err != nil {
		return false, err
	}
	if err := conn.Transmit(ctx, File{ID: f.ID, Name: le.Code + "-" + r.DocumentNumber + "-" + f.ID.String() + ".xml", Content: content}); err != nil {
		return false, problem.Unavailable("The bank connector did not take the file; transmit it again.", 30).WithCause(err)
	}
	return false, e.Sent.record(ctx, p, f, connectorID)
}

func fileNotFound() error { return problem.NotFoundOf("bank file") }

// getFile reads a bank file of a legal entity the caller may act in.
func getFile(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (BankFile, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return BankFile{}, err
	}
	q := fileSelect + ` WHERE f.id = $1`
	if lock {
		// Not FOR UPDATE: the sent file's record (T44c), written in its own
		// transaction while this one holds the lock, references the file,
		// and its foreign key check would wait for this transaction.
		q += ` FOR NO KEY UPDATE OF f`
	}
	f, err := scanFile(tx.QueryRow(ctx, q, id))
	if err = authz.Visible(err, access, f.LegalEntityID, fileNotFound(), "payments: read bank file %s", id); err != nil {
		return BankFile{}, err
	}
	return f, nil
}

// GetFile returns a bank file.
func GetFile(ctx context.Context, id uuid.UUID) (BankFile, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return BankFile{}, err
	}
	return getFile(ctx, tx, id, false)
}

// TransmitFile sends a created bank file through its house bank's channel
// (ADR-0038), as an operation that completes within the request. The file comes from object
// storage (or is rendered again from the run) and must match the hash
// recorded at release; a connector failure leaves the file created, to be
// transmitted again. A file whose content hash its bank already took counts
// as sent: it is not uploaded again, and the event says it was a duplicate
// (T44c).
func (e *Endpoints) TransmitFile(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	f, err := getFile(ctx, tx, id, true)
	if err != nil {
		return operations.Operation{}, err
	}
	if err := ifMatch(f.Version); err != nil {
		return operations.Operation{}, err
	}
	if f.Status != FileCreated {
		return operations.Operation{}, problem.Conflict("payments.invalid_status", "Only a created bank file can be transmitted; this one is "+f.Status+".")
	}
	conn, connectorID, err := e.channel(ctx, tx, f.HouseBankID)
	if err != nil {
		return operations.Operation{}, err
	}
	op, err := operations.StartFor(ctx, "payments.transmit", map[string]any{"bank_file_id": f.ID}, "/v1/bank-files/"+f.ID.String())
	if err != nil {
		return operations.Operation{}, err
	}
	duplicate, err := e.send(ctx, tx, p, f, conn, connectorID)
	if err != nil {
		return operations.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'transmitted', version = version + 1 WHERE id = $1`, f.ID); err != nil {
		return operations.Operation{}, fmt.Errorf("payments: mark bank file %s transmitted: %w", f.ID, err)
	}
	if err := record(ctx, "payments.bank_file.transmit", f.ID, map[string]any{"bank_file_id": f.ID, "operation_id": op.ID, "connector_id": connectorID, "duplicate": duplicate},
		fileEvent(f, "bank_file_transmitted", map[string]any{"format": f.Format, "sha256": hex.EncodeToString(f.SHA256), "connector_id": connectorID,
			"integration_message_id": nil, "status": FileTransmitted, "duplicate": duplicate})); err != nil {
		return operations.Operation{}, err
	}
	return operations.Succeed(ctx, op.ID, "/v1/bank-files/"+f.ID.String(), map[string]any{"bank_file_id": f.ID, "status": FileTransmitted})
}

// fileStatusReport is what the bank's status report (pain.002, camt.054)
// says about a transmitted file, applied by treasury on the API.
type fileStatusReport struct {
	action, to, event string
}

var (
	acknowledgeFile = fileStatusReport{"acknowledge", FileAcknowledged, "bank_file_acknowledged"}
	rejectFile      = fileStatusReport{"reject", FileRejected, "bank_file_rejected"}
)

// ReportFile applies the bank's status report to a transmitted file:
// accepted moves it to acknowledged, rejected to rejected with the bank's
// reason. The run stays released with its postings either way: undoing
// them is a reversal, which the ledger does not offer yet.
func ReportFile(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, outcome, reference, reason string) (BankFile, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return BankFile{}, err
	}
	r := acknowledgeFile
	switch outcome {
	case "accepted":
	case "rejected":
		r = rejectFile
		if reason == "" {
			return BankFile{}, problem.Unprocessable("payments.reason_required", "A rejection carries the bank's reason.",
				problem.FieldError{Field: "reason", Code: "required", Message: "the bank's reason"})
		}
	default:
		return BankFile{}, problem.Unprocessable("payments.invalid_outcome", "The outcome "+outcome+" is not one the report can carry.",
			problem.FieldError{Field: "outcome", Code: "invalid", Message: "accepted or rejected"})
	}
	f, err := getFile(ctx, tx, id, true)
	if err != nil {
		return BankFile{}, err
	}
	if err := ifMatch(f.Version); err != nil {
		return BankFile{}, err
	}
	if f.Status != FileTransmitted {
		return BankFile{}, problem.Conflict("payments.invalid_status", "Only a transmitted bank file can be "+r.to+"; this one is "+f.Status+".")
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = $2, version = version + 1 WHERE id = $1`, f.ID, r.to); err != nil {
		return BankFile{}, fmt.Errorf("payments: mark bank file %s %s: %w", f.ID, r.to, err)
	}
	updated, err := getFile(ctx, tx, id, false)
	if err != nil {
		return BankFile{}, err
	}
	detail := map[string]any{"bank_file_id": f.ID, "from_status": f.Status, "report_reference": reference}
	payload := map[string]any{"previous_status": f.Status, "status": r.to, "report_reference": reference}
	if reason != "" {
		detail["reason"], payload["reason"] = reason, reason
	}
	return updated, record(ctx, "payments.bank_file."+r.action, f.ID, detail, fileEvent(f, r.event, payload))
}

// FileContent returns a bank file's content, checked against its hash.
func (e *Endpoints) FileContent(ctx context.Context, id uuid.UUID) ([]byte, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	f, err := getFile(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	return e.fileOf(ctx, tx, p.TenantID, f)
}
