package payments

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
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

func fileNotFound() error { return problem.NotFound("No bank file with this id.") }

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
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(f.LegalEntityID)) {
		return BankFile{}, fileNotFound()
	}
	if err != nil {
		return BankFile{}, fmt.Errorf("payments: read bank file %s: %w", id, err)
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

// TransmitFile sends a created bank file through the connector, as an
// operation that completes within the request. The file comes from object
// storage (or is rendered again from the run) and must match the hash
// recorded at release; a connector failure leaves the file created, to be
// transmitted again (banks refuse a message id they have already
// processed).
func (e *Endpoints) TransmitFile(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	if e.Connector == nil {
		return operations.Operation{}, problem.Unavailable("Bank file transmission is not configured on this server.", 0)
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
	op, err := operations.StartFor(ctx, "payments.transmit", map[string]any{"bank_file_id": f.ID}, "/v1/bank-files/"+f.ID.String())
	if err != nil {
		return operations.Operation{}, err
	}
	content, err := e.fileOf(ctx, tx, p.TenantID, f)
	if err != nil {
		return operations.Operation{}, err
	}
	r, err := load(ctx, tx, f.RunID, false)
	if err != nil {
		return operations.Operation{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID)
	if err != nil {
		return operations.Operation{}, err
	}
	if err := e.Connector.Transmit(ctx, File{ID: f.ID, Name: le.Code + "-" + r.DocumentNumber + "-" + f.ID.String() + ".xml", Content: content}); err != nil {
		return operations.Operation{}, problem.Unavailable("The bank connector did not take the file; transmit it again.", 30).WithCause(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'transmitted', version = version + 1 WHERE id = $1`, f.ID); err != nil {
		return operations.Operation{}, fmt.Errorf("payments: mark bank file %s transmitted: %w", f.ID, err)
	}
	if err := record(ctx, "payments.bank_file.transmit", f.ID, map[string]any{"bank_file_id": f.ID, "operation_id": op.ID},
		fileEvent(f, "bank_file_transmitted", map[string]any{"format": f.Format, "sha256": hex.EncodeToString(f.SHA256), "connector_id": nil,
			"integration_message_id": nil, "status": FileTransmitted})); err != nil {
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
