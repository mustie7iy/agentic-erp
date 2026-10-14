package payments

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func runToAPI(r Run) gen.PaymentRun {
	out := gen.PaymentRun{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), DocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, HouseBankID: r.HouseBankID,
		RunDate: r.RunDate, DueBy: r.DueBy, TotalAmount: apitypes.Money(r.Total, r.Currency, r.Exponent), Status: r.Status, ProposedBy: r.ProposedBy,
		DecidedBy: apitypes.OptUUID(r.DecidedBy), OperationID: apitypes.OptUUID(r.OperationID), ReleaseOperationID: apitypes.OptUUID(r.ReleaseOpID)}
	if r.DecidedAt != nil {
		out.DecidedAt.SetTo(*r.DecidedAt)
	}
	return out
}

func runOK(r Run) *gen.RejectPaymentRunOKHeaders {
	return &gen.RejectPaymentRunOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: runToAPI(r)}
}

func paymentToAPI(p Payment, currency string, exp int) gen.Payment {
	out := gen.Payment{ID: p.ID, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, PaymentRunID: p.RunID, SupplierID: p.SupplierID,
		BankAccountID: p.BankAccountID, Amount: apitypes.Money(p.Amount, currency, exp), EndToEndID: p.EndToEndID, JournalEntryID: apitypes.OptUUID(p.JournalEntryID),
		Items: make([]gen.PaymentItem, len(p.Items))}
	for i, it := range p.Items {
		out.Items[i] = gen.PaymentItem{FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID, LineNo: it.LineNo,
			Amount: apitypes.Money(it.Amount, currency, exp), DueDate: it.DueDate.Time, IsActive: it.Active}
	}
	return out
}

func fileToAPI(f BankFile) gen.BankFile {
	return gen.BankFile{ID: f.ID, Version: f.Version, CreatedAt: f.CreatedAt, CreatedBy: f.CreatedBy, UpdatedAt: f.UpdatedAt,
		PaymentRunID: f.RunID, Format: f.Format, Status: f.Status}
}

// ProposePaymentRun implements POST /v1/payment-runs:propose.
func (e *Endpoints) ProposePaymentRun(ctx context.Context, req *gen.PaymentRunPropose, _ gen.ProposePaymentRunParams) (gen.ProposePaymentRunRes, error) {
	op, err := e.ProposeRun(ctx, Proposal{LegalEntityID: req.LegalEntityID, HouseBankID: req.HouseBankID, RunDate: req.RunDate, DueBy: req.DueBy})
	if err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: operations.ToAPI(op)}, nil
}

// GetPaymentRun implements GET /v1/payment-runs/{id}.
func (e *Endpoints) GetPaymentRun(ctx context.Context, params gen.GetPaymentRunParams) (gen.GetPaymentRunRes, error) {
	r, err := GetRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.PaymentRunHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: runToAPI(r)}, nil
}

var runSort = pagination.Sort{Default: "run_date", Fields: []pagination.Field{
	{Name: "run_date", Column: "r.run_date", Type: "date"}, {Name: "document_number", Column: "r.document_number", Type: "text"},
}}

// ListPaymentRuns implements GET /v1/payment-runs.
func (e *Endpoints) ListPaymentRuns(ctx context.Context, params gen.ListPaymentRunsParams) (gen.ListPaymentRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, from, to := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[time.Time](params.RunDateFrom), apitypes.Ptr[time.Time](params.RunDateTo)
	scope := fmt.Sprintf("listPaymentRuns|%s|%s|%s|%v|%v", le, params.Status.Value, params.DocumentNumber.Value, from, to)
	ks, err := e.Pages.Keyset(runSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(runSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4 = '' OR r.status = $4) AND ($5 = '' OR r.document_number = $5) AND ($6::date IS NULL OR r.run_date >= $6)
		AND ($7::date IS NULL OR r.run_date <= $7)`, []any{access.All, access.IDs, le, params.Status.Value, params.DocumentNumber.Value, from, to})
	rows, _ := tx.Query(ctx, q, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("payments: list payment runs: %w", err)
	}
	runs, next := pagination.Page(ks, runs, func(r Run) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return r.DocumentNumber, r.ID
		}
		return r.RunDate.Format(time.DateOnly), r.ID
	})
	page := gen.PaymentRunPage{Items: make([]gen.PaymentRun, len(runs))}
	for i, r := range runs {
		page.Items[i] = runToAPI(r)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.PaymentRunPageHeaders{Response: page}, nil
}

// ReleasePaymentRun implements POST /v1/payment-runs/{id}:release.
func (e *Endpoints) ReleasePaymentRun(ctx context.Context, req gen.OptDecisionInput, params gen.ReleasePaymentRunParams) (gen.ReleasePaymentRunRes, error) {
	op, err := e.StartRelease(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Value.Comment.Value)
	if err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: operations.ToAPI(op)}, nil
}

// RejectPaymentRun implements POST /v1/payment-runs/{id}:reject.
func (e *Endpoints) RejectPaymentRun(ctx context.Context, req *gen.ReasonInput, params gen.RejectPaymentRunParams) (gen.RejectPaymentRunRes, error) {
	r, err := RejectRun(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return runOK(r), nil
}

var byID = pagination.Sort{Default: "id", Fields: []pagination.Field{{Name: "id", Column: "id", Type: "uuid"}}}

func idKey[T any](id func(T) uuid.UUID) func(T) (string, uuid.UUID) {
	return func(v T) (string, uuid.UUID) { return id(v).String(), id(v) }
}

// ListPayments implements GET /v1/payment-runs/{id}/payments.
func (e *Endpoints) ListPayments(ctx context.Context, params gen.ListPaymentsParams) (gen.ListPaymentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	sup := apitypes.Ptr[uuid.UUID](params.SupplierID)
	ks, err := e.Pages.Keyset(byID, fmt.Sprintf("listPayments|%s|%s", params.ID, sup), apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	r, err := get(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT * FROM (`+paymentSelect+` WHERE p.payment_run_id = $1 AND ($2::uuid IS NULL OR p.supplier_id = $2)) p WHERE true`,
		[]any{r.ID, sup})
	rows, _ := tx.Query(ctx, q, args...)
	pays, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, fmt.Errorf("payments: list payments of run %s: %w", r.ID, err)
	}
	pays, next := pagination.Page(ks, pays, idKey(func(p Payment) uuid.UUID { return p.ID }))
	page := gen.PaymentPage{Items: make([]gen.Payment, len(pays))}
	for i, p := range pays {
		page.Items[i] = paymentToAPI(p, r.Currency, r.Exponent)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.PaymentPageHeaders{Response: page}, nil
}

// ListPaymentExclusions implements GET /v1/payment-runs/{id}/exclusions.
func (e *Endpoints) ListPaymentExclusions(ctx context.Context, params gen.ListPaymentExclusionsParams) (gen.ListPaymentExclusionsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(byID, "listPaymentExclusions|"+params.ID.String(), apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	r, err := get(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT id, reason_code, amount FROM (SELECT supplier_id AS id, reason_code, amount FROM payments.payment_exclusion
		WHERE payment_run_id = $1) x WHERE true`, []any{r.ID})
	rows, _ := tx.Query(ctx, q, args...)
	exs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Exclusion, error) {
		var x Exclusion
		return x, errs.Wrapf(row.Scan(&x.SupplierID, &x.Reason, &x.Amount), "scan exclusion")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: list exclusions of run %s: %w", r.ID, err)
	}
	exs, next := pagination.Page(ks, exs, idKey(func(x Exclusion) uuid.UUID { return x.SupplierID }))
	page := gen.PaymentExclusionPage{Items: make([]gen.PaymentExclusion, len(exs))}
	for i, x := range exs {
		page.Items[i] = gen.PaymentExclusion{SupplierID: x.SupplierID, ReasonCode: x.Reason, Amount: apitypes.Money(x.Amount, r.Currency, r.Exponent)}
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.PaymentExclusionPageHeaders{Response: page}, nil
}

// GetBankFile implements GET /v1/bank-files/{id}.
func (e *Endpoints) GetBankFile(ctx context.Context, params gen.GetBankFileParams) (gen.GetBankFileRes, error) {
	f, err := GetFile(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.BankFileHeaders{ETag: gen.NewOptString(etag.Format(f.Version)), Response: fileToAPI(f)}, nil
}

var fileSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "f.created_at", Type: "timestamptz"}}}

// ListBankFiles implements GET /v1/bank-files.
func (e *Endpoints) ListBankFiles(ctx context.Context, params gen.ListBankFilesParams) (gen.ListBankFilesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	run := apitypes.Ptr[uuid.UUID](params.PaymentRunID)
	ks, err := e.Pages.Keyset(fileSort, fmt.Sprintf("listBankFiles|%s|%s", run, params.Status.Value), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT * FROM (`+fileSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR f.payment_run_id = $3)
		AND ($4 = '' OR f.status = $4)) f WHERE true`, []any{access.All, access.IDs, run, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	files, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BankFile, error) { return scanFile(r) })
	if err != nil {
		return nil, fmt.Errorf("payments: list bank files: %w", err)
	}
	files, next := pagination.Page(ks, files, func(f BankFile) (string, uuid.UUID) {
		return f.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), f.ID
	})
	page := gen.BankFilePage{Items: make([]gen.BankFile, len(files))}
	for i, f := range files {
		page.Items[i] = fileToAPI(f)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.BankFilePageHeaders{Response: page}, nil
}

// GetBankFileContent implements GET /v1/bank-files/{id}/content.
func (e *Endpoints) GetBankFileContent(ctx context.Context, params gen.GetBankFileContentParams) (gen.GetBankFileContentRes, error) {
	content, err := e.FileContent(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetBankFileContentOKHeaders{Response: gen.GetBankFileContentOK{Data: bytes.NewReader(content)}}, nil
}

// ReportBankFileStatus implements POST /v1/bank-files/{id}:report.
func (e *Endpoints) ReportBankFileStatus(ctx context.Context, req *gen.BankStatusReport, params gen.ReportBankFileStatusParams) (gen.ReportBankFileStatusRes, error) {
	f, err := ReportFile(ctx, params.ID, etag.IfMatch(params.IfMatch), string(req.Outcome), req.Reference.Value, req.Reason.Value)
	if err != nil {
		return nil, err
	}
	return &gen.ReportBankFileStatusOKHeaders{ETag: gen.NewOptString(etag.Format(f.Version)), Response: fileToAPI(f)}, nil
}

// TransmitBankFile implements POST /v1/bank-files/{id}:transmit.
func (e *Endpoints) TransmitBankFile(ctx context.Context, params gen.TransmitBankFileParams) (gen.TransmitBankFileRes, error) {
	op, err := e.TransmitFile(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: operations.ToAPI(op)}, nil
}
