package payments

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
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

func fileNotFound() error { return problem.NotFoundOf("bank file") }

// getFile reads a bank file of a legal entity the caller may act in.
func getFile(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (BankFile, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return BankFile{}, err
	}
	q := fileSelect + ` WHERE f.id = $1`
	if lock {
		q += ` FOR UPDATE OF f`
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

// TransmitFile asks for a created or failed bank file to be sent through
// its house bank's channel (ADR-0038), as an operation the transmitter
// completes (T44e): the request only marks the file to_send, in its own
// transaction, as the outbox records an event (ADR-0007), so no
// transaction is open while the bank takes the file. The channel and the
// file's content are checked when the transmitter claims it; a file whose
// content hash its bank already took counts as sent (T44c). A failed file
// may be asked for again, with its attempts counted afresh.
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
	if f.Status != FileCreated && f.Status != FileFailed {
		return operations.Operation{}, problem.Conflict("payments.invalid_status", "Only a created or failed bank file can be transmitted; this one is "+f.Status+".")
	}
	if err := e.configured(ctx, tx, f.HouseBankID); err != nil {
		return operations.Operation{}, err
	}
	op, err := operations.StartFor(ctx, TransmitKind, map[string]any{"bank_file_id": f.ID}, "/v1/bank-files/"+f.ID.String())
	if err != nil {
		return operations.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'to_send', attempts = 0, next_attempt_at = $2, last_error = NULL,
		transmit_operation_id = $3, transmit_requested_by = $4, version = version + 1 WHERE id = $1`, f.ID, clock.Now(e.Now), op.ID, p.ID); err != nil {
		return operations.Operation{}, fmt.Errorf("payments: queue bank file %s: %w", f.ID, err)
	}
	return op, record(ctx, "payments.bank_file.queue", f.ID, map[string]any{"bank_file_id": f.ID, "operation_id": op.ID, "from_status": f.Status})
}

// configured refuses a transmission no channel could take: a house bank
// that names no connector on a server without a directory for its files.
// A bank's own connector is resolved when the file is claimed.
func (e *Endpoints) configured(ctx context.Context, tx pgx.Tx, bank uuid.UUID) error {
	var named bool
	if err := tx.QueryRow(ctx, `SELECT connector_id IS NOT NULL FROM payments.house_bank WHERE id = $1`, bank).Scan(&named); err != nil {
		return fmt.Errorf("payments: channel of house bank %s: %w", bank, err)
	}
	if !named && e.Connector == nil {
		return problem.Unavailable("Bank file transmission is not configured on this server.", 0)
	}
	return nil
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
