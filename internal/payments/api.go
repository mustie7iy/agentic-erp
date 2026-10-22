package payments

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func runToAPI(r Run) gen.PaymentRun {
	out := gen.PaymentRun{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), DocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, HouseBankID: apitypes.OptUUID(r.HouseBankID),
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

// paymentToAPI is payment p of run r; a payment that names no house bank
// is paid by its run's.
func paymentToAPI(p Payment, r Run) gen.Payment {
	currency, exp := r.Currency, r.Exponent
	out := gen.Payment{ID: p.ID, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, PaymentRunID: p.RunID, SupplierID: p.SupplierID,
		BankAccountID: p.BankAccountID, Amount: apitypes.Money(p.Amount, currency, exp), DiscountAmount: gen.NewOptMoney(apitypes.Money(p.Discount, currency, exp)),
		WithholdingAmount: gen.NewOptMoney(apitypes.Money(p.Withholding, currency, exp)), EndToEndID: p.EndToEndID, JournalEntryID: apitypes.OptUUID(p.JournalEntryID), Kind: gen.NewOptString(p.Kind),
		DownPaymentRequestID: apitypes.OptUUID(p.RequestID), HouseBankID: apitypes.OptUUID(p.HouseBankID), Items: make([]gen.PaymentItem, len(p.Items))}
	if p.HouseBankID == nil {
		out.HouseBankID = apitypes.OptUUID(r.HouseBankID)
	}
	if p.Method != nil {
		out.PaymentMethod.SetTo(*p.Method)
	}
	if p.DiscountUntil != nil {
		out.DiscountUntil.SetTo(*p.DiscountUntil)
	}
	for i, it := range p.Items {
		out.Items[i] = gen.PaymentItem{FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID, LineNo: it.LineNo,
			Amount: apitypes.Money(it.Amount, currency, exp), DiscountAmount: gen.NewOptMoney(apitypes.Money(it.Discount, currency, exp)),
			WithholdingAmount: gen.NewOptMoney(apitypes.Money(it.Withholding, currency, exp)), DueDate: it.DueDate.Time, IsActive: it.Active}
	}
	return out
}

func fileToAPI(f BankFile) gen.BankFile {
	out := gen.BankFile{ID: f.ID, Version: f.Version, CreatedAt: f.CreatedAt, CreatedBy: f.CreatedBy, UpdatedAt: f.UpdatedAt,
		PaymentRunID: f.RunID, HouseBankID: gen.NewOptUUID(f.HouseBankID), Format: f.Format, Status: f.Status}
	if f.Method != nil {
		out.PaymentMethod.SetTo(*f.Method)
	}
	return out
}

// ProposePaymentRun implements POST /v1/payment-runs:propose.
func (e *Endpoints) ProposePaymentRun(ctx context.Context, req *gen.PaymentRunPropose, _ gen.ProposePaymentRunParams) (gen.ProposePaymentRunRes, error) {
	op, err := e.ProposeRun(ctx, Proposal{LegalEntityID: req.LegalEntityID, HouseBankID: apitypes.Ptr[uuid.UUID](req.HouseBankID), Currency: req.Currency.Value,
		RunDate: req.RunDate, DueBy: req.DueBy})
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
	items, next := apitypes.PageOf(ks, runs, func(r Run) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return r.DocumentNumber, r.ID
		}
		return r.RunDate.Format(time.DateOnly), r.ID
	}, runToAPI)
	return &gen.PaymentRunPageHeaders{Response: gen.PaymentRunPage{Items: items, NextPageToken: next}}, nil
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
	r, err := e.RejectRun(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
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
	q, args := ks.SQL(`SELECT * FROM (`+paymentSelect+` WHERE p.payment_run_id = $1 AND NOT `+dropped+` AND ($2::uuid IS NULL OR p.supplier_id = $2)) p WHERE true`,
		[]any{r.ID, sup})
	rows, _ := tx.Query(ctx, q, args...)
	pays, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, fmt.Errorf("payments: list payments of run %s: %w", r.ID, err)
	}
	items, next := apitypes.PageOf(ks, pays, idKey(func(p Payment) uuid.UUID { return p.ID }),
		func(p Payment) gen.Payment { return paymentToAPI(p, r) })
	return &gen.PaymentPageHeaders{Response: gen.PaymentPage{Items: items, NextPageToken: next}}, nil
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
	// The suppliers' items, keyed by supplier as before, and the requests
	// left out (T46c), keyed by request.
	q, args := ks.SQL(`SELECT id, supplier_id, request_id, reason_code, amount FROM (
		SELECT supplier_id AS id, supplier_id, NULL::uuid AS request_id, reason_code, amount FROM payments.payment_exclusion WHERE payment_run_id = $1
		UNION ALL
		SELECT down_payment_request_id, supplier_id, down_payment_request_id, reason_code, amount FROM payments.request_exclusion WHERE payment_run_id = $1) x
		WHERE true`, []any{r.ID})
	rows, _ := tx.Query(ctx, q, args...)
	exs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Exclusion, error) {
		var x Exclusion
		return x, errs.Wrapf(row.Scan(&x.ID, &x.SupplierID, &x.RequestID, &x.Reason, &x.Amount), "scan exclusion")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: list exclusions of run %s: %w", r.ID, err)
	}
	items, next := apitypes.PageOf(ks, exs, idKey(func(x Exclusion) uuid.UUID { return x.ID }),
		func(x Exclusion) gen.PaymentExclusion {
			return gen.PaymentExclusion{SupplierID: x.SupplierID, DownPaymentRequestID: apitypes.OptUUID(x.RequestID), ReasonCode: x.Reason,
				Amount: apitypes.Money(x.Amount, r.Currency, r.Exponent)}
		})
	return &gen.PaymentExclusionPageHeaders{Response: gen.PaymentExclusionPage{Items: items, NextPageToken: next}}, nil
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
	items, next := apitypes.PageOf(ks, files, func(f BankFile) (string, uuid.UUID) {
		return f.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), f.ID
	}, fileToAPI)
	return &gen.BankFilePageHeaders{Response: gen.BankFilePage{Items: items, NextPageToken: next}}, nil
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

// statementToAPI is the statement as the caller sees it: masked, an agent.
func statementToAPI(s Statement, masked bool) gen.BankStatement {
	out := gen.BankStatement{ID: s.ID, Version: s.Version, CreatedAt: s.CreatedAt, CreatedBy: s.CreatedBy, UpdatedAt: s.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(s.UpdatedBy), HouseBankID: s.HouseBankID, LegalEntityID: s.LegalEntityID, Format: s.Format,
		FileSHA256: sha256Hex(s.FileSHA256), FilePosition: s.FilePosition, StatementRef: s.Ref, Sequence: s.Sequence, Account: s.Account,
		OpeningBalance: apitypes.Money(s.Opening, s.Currency, s.Exponent), OpeningDate: s.OpeningDate,
		ClosingBalance: apitypes.Money(s.Closing, s.Currency, s.Exponent), ClosingDate: s.ClosingDate, LineCount: s.LineCount,
		PreviousStatementID: apitypes.OptUUID(s.PreviousID), Status: s.Status, ReconciledBy: apitypes.OptUUID(s.ReconciledBy)}
	if s.ReconciledAt != nil {
		out.ReconciledAt.SetTo(*s.ReconciledAt)
	}
	if masked {
		out.Account = "****" + fieldcrypt.Last4(s.Account)
	}
	if s.MessageID != nil {
		out.MessageID.SetTo(*s.MessageID)
	}
	if s.Lines != nil { // read with the statement: [] when it has none
		out.Lines = make([]gen.BankStatementLine, 0, len(s.Lines))
	}
	for _, l := range s.Lines {
		out.Lines = append(out.Lines, lineToAPI(l, s, masked))
	}
	return out
}

func lineToAPI(l StatementLine, s Statement, masked bool) gen.BankStatementLine {
	line := gen.BankStatementLine{LineNo: l.LineNo, Amount: apitypes.Money(l.Amount, s.Currency, s.Exponent), IsReversal: l.Reversal,
		BookingDate: l.BookingDate, ValueDate: l.ValueDate, TransactionCode: l.TransactionCode, BankReference: l.BankReference,
		OwnerReference: l.OwnerReference, Text: l.Text, Status: l.Status, Transactions: make([]gen.BankStatementTransaction, len(l.Details))}
	for i, d := range l.Details {
		t := gen.BankStatementTransaction{Amount: apitypes.Money(d.Amount, s.Currency, s.Exponent),
			MessageID: optText(d.MessageID), PaymentInformationID: optText(d.PaymentInfoID), InstructionID: optText(d.InstructionID),
			EndToEndID: optText(d.EndToEndID), TransactionCode: optText(d.TransactionCode), CounterpartyName: optText(d.CounterpartyName),
			CounterpartyIban: optText(d.CounterpartyIBAN), CounterpartyAccount: optText(d.CounterpartyAccount), CounterpartyBic: optText(d.CounterpartyBIC),
			Remittance: optText(d.Remittance), CounterpartyAccountLast4: optText(fieldcrypt.Last4(d.counterpartyAccount()))}
		if masked {
			t.CounterpartyIban, t.CounterpartyAccount = gen.OptString{}, gen.OptString{}
		}
		line.Transactions[i] = t
	}
	return line
}

// optText is a text the bank may not give: absent when empty.
func optText(s string) gen.OptString {
	if s == "" {
		return gen.OptString{}
	}
	return gen.NewOptString(s)
}

// ImportBankStatements implements POST /v1/bank-statements.
func (e *Endpoints) ImportBankStatements(ctx context.Context, req *gen.BankStatementUploadMultipart, _ gen.ImportBankStatementsParams) (gen.ImportBankStatementsRes, error) {
	statements, sum, created, err := e.ImportStatements(ctx, req.HouseBankID, req.Format, []byte(req.File))
	if err != nil {
		return nil, err
	}
	out := gen.BankStatementImport{FileSHA256: sha256Hex(sum), Statements: make([]gen.BankStatement, len(statements))}
	for i, s := range statements {
		out.Statements[i] = statementToAPI(s, false) // agents import nothing
	}
	if created {
		return &gen.ImportBankStatementsCreated{Response: out}, nil
	}
	return &gen.ImportBankStatementsOK{Response: out}, nil
}

// GetBankStatement implements GET /v1/bank-statements/{id}.
func (e *Endpoints) GetBankStatement(ctx context.Context, params gen.GetBankStatementParams) (gen.GetBankStatementRes, error) {
	s, err := GetStatement(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.BankStatementHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: statementToAPI(s, reqctx.Masked(ctx))}, nil
}

// GetBankStatementFile implements GET /v1/bank-statements/{id}/file.
func (e *Endpoints) GetBankStatementFile(ctx context.Context, params gen.GetBankStatementFileParams) (gen.GetBankStatementFileRes, error) {
	content, err := e.StatementFile(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetBankStatementFileOKHeaders{Response: gen.GetBankStatementFileOK{Data: bytes.NewReader(content)}}, nil
}

var statementSort = pagination.Sort{Default: "closing_date", Fields: []pagination.Field{{Name: "closing_date", Column: "s.closing_date", Type: "date"}}}

// ListBankStatements implements GET /v1/bank-statements.
func (e *Endpoints) ListBankStatements(ctx context.Context, params gen.ListBankStatementsParams) (gen.ListBankStatementsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, bank := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.HouseBankID)
	scope := fmt.Sprintf("listBankStatements|%s|%s|%s", le, bank, params.Status.Value)
	ks, err := e.Pages.Keyset(statementSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(statementSelect+` WHERE ($1 OR s.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR s.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR s.house_bank_id = $4) AND ($5 = '' OR s.status = $5)`, []any{access.All, access.IDs, le, bank, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	statements, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Statement, error) { return scanStatement(r) })
	if err != nil {
		return nil, fmt.Errorf("payments: list bank statements: %w", err)
	}
	masked := reqctx.Masked(ctx)
	items, next := apitypes.PageOf(ks, statements, func(s Statement) (string, uuid.UUID) { return s.ClosingDate.Format(time.DateOnly), s.ID },
		func(s Statement) gen.BankStatement { return statementToAPI(s, masked) })
	return &gen.BankStatementPageHeaders{Response: gen.BankStatementPage{Items: items, NextPageToken: next}}, nil
}

// MatchBankStatement implements POST /v1/bank-statements/{id}:match.
func (e *Endpoints) MatchBankStatement(ctx context.Context, params gen.MatchBankStatementParams) (gen.MatchBankStatementRes, error) {
	s, err := e.MatchStatement(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.MatchBankStatementOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: statementToAPI(s, reqctx.Masked(ctx))}, nil
}

func matchToAPI(m Match) gen.StatementMatch {
	out := gen.StatementMatch{ID: m.ID, Version: m.Version, CreatedAt: m.CreatedAt, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(m.UpdatedBy), BankStatementID: m.StatementID, LineNo: m.LineNo, Kind: m.Kind, Reason: m.Reason, Status: m.Status,
		Targets: make([]gen.StatementMatchTarget, len(m.Targets))}
	if m.Rule != nil {
		out.Rule.SetTo(*m.Rule)
	}
	if m.PostingDate != nil {
		out.PostingDate.SetTo(*m.PostingDate)
	}
	out.PostingReason, out.ApprovalRequestID, out.JournalEntryID = apitypes.OptString(m.PostingReason), apitypes.OptUUID(m.ApprovalRequestID), apitypes.OptUUID(m.JournalEntryID)
	if m.JournalEntryYear != nil {
		out.JournalEntryYear.SetTo(int32(*m.JournalEntryYear))
	}
	out.SessionID, out.Proposal, out.Outcome = apitypes.OptUUID(m.SessionID), apitypes.OptString(m.Proposal), apitypes.OptString(m.Outcome)
	for i, t := range m.Targets {
		out.Targets[i] = gen.StatementMatchTarget{Candidate: int32(t.Candidate), PaymentID: apitypes.OptUUID(t.PaymentID), BankFileID: apitypes.OptUUID(t.BankFileID)}
		if t.PostingRule != nil {
			out.Targets[i].PostingRule.SetTo(*t.PostingRule)
		}
		if t.Incoming {
			out.Targets[i].Incoming.SetTo(true)
		}
	}
	return out
}

// ListBankStatementMatches implements GET /v1/bank-statements/{id}/matches.
func (e *Endpoints) ListBankStatementMatches(ctx context.Context, params gen.ListBankStatementMatchesParams) (gen.ListBankStatementMatchesRes, error) {
	matches, err := StatementMatches(ctx, params.ID, params.LineNo.Value)
	if err != nil {
		return nil, err
	}
	out := gen.StatementMatchList{Items: make([]gen.StatementMatch, len(matches))}
	for i, m := range matches {
		out.Items[i] = matchToAPI(m)
	}
	return &gen.StatementMatchListHeaders{Response: out}, nil
}

// MatchBankStatementLine implements POST /v1/bank-statements/{id}/lines/{line_no}:match.
func (e *Endpoints) MatchBankStatementLine(ctx context.Context, req *gen.StatementLineMatchInput, params gen.MatchBankStatementLineParams) (gen.MatchBankStatementLineRes, error) {
	m, err := e.MatchLine(ctx, params.ID, params.LineNo, etag.IfMatch(params.IfMatch), ManualMatch{PaymentIDs: req.PaymentIds, PostingRule: req.PostingRule.Value,
		Incoming: req.Incoming.Value, Reason: req.Reason})
	if err != nil {
		return nil, err
	}
	return &gen.StatementMatchHeaders{ETag: gen.NewOptString(etag.Format(m.StatementVersion)), Response: matchToAPI(m)}, nil
}

// ExcludeBankStatementLine implements POST /v1/bank-statements/{id}/lines/{line_no}:exclude.
func (e *Endpoints) ExcludeBankStatementLine(ctx context.Context, req *gen.ReasonInput, params gen.ExcludeBankStatementLineParams) (gen.ExcludeBankStatementLineRes, error) {
	m, err := e.ExcludeLine(ctx, params.ID, params.LineNo, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return &gen.StatementMatchHeaders{ETag: gen.NewOptString(etag.Format(m.StatementVersion)), Response: matchToAPI(m)}, nil
}

// PostBankStatementLine implements POST /v1/bank-statements/{id}/lines/{line_no}:post.
func (e *Endpoints) PostBankStatementLine(ctx context.Context, req gen.OptStatementLinePostInput, params gen.PostBankStatementLineParams) (gen.PostBankStatementLineRes, error) {
	m, err := e.PostLine(ctx, params.ID, params.LineNo, etag.IfMatch(params.IfMatch),
		LinePosting{Date: apitypes.Ptr[time.Time](req.Value.PostingDate), Reason: req.Value.Reason.Value})
	if err != nil {
		return nil, err
	}
	return &gen.StatementMatchHeaders{ETag: gen.NewOptString(etag.Format(m.StatementVersion)), Response: matchToAPI(m)}, nil
}

// ProposeBankStatementLine implements POST /v1/bank-statements/{id}/lines/{line_no}:propose.
func (e *Endpoints) ProposeBankStatementLine(ctx context.Context, req *gen.StatementLineProposalInput, params gen.ProposeBankStatementLineParams) (gen.ProposeBankStatementLineRes, error) {
	m, created, err := e.ProposeLine(ctx, params.ID, params.LineNo, etag.IfMatch(params.IfMatch), LineProposal{SessionID: req.SessionID,
		PaymentIDs: req.PaymentIds, PostingRule: req.PostingRule.Value, Exclude: req.Exclude.Value, Reason: req.Reason})
	if err != nil {
		return nil, err
	}
	if !created {
		return &gen.ProposeBankStatementLineOK{ETag: gen.NewOptString(etag.Format(m.StatementVersion)), Response: matchToAPI(m)}, nil
	}
	return &gen.ProposeBankStatementLineCreated{ETag: gen.NewOptString(etag.Format(m.StatementVersion)), Response: matchToAPI(m)}, nil
}

// AcceptBankStatementLineProposal implements POST /v1/bank-statements/{id}/lines/{line_no}:accept.
func (e *Endpoints) AcceptBankStatementLineProposal(ctx context.Context, req gen.OptStatementLineProposalAccept, params gen.AcceptBankStatementLineProposalParams) (gen.AcceptBankStatementLineProposalRes, error) {
	m, err := e.AcceptProposal(ctx, params.ID, params.LineNo, etag.IfMatch(params.IfMatch), req.Value.Comment.Value)
	if err != nil {
		return nil, err
	}
	return &gen.StatementMatchHeaders{ETag: gen.NewOptString(etag.Format(m.StatementVersion)), Response: matchToAPI(m)}, nil
}

// RejectBankStatementLineProposal implements POST /v1/bank-statements/{id}/lines/{line_no}:reject.
func (e *Endpoints) RejectBankStatementLineProposal(ctx context.Context, req *gen.ReasonInput, params gen.RejectBankStatementLineProposalParams) (gen.RejectBankStatementLineProposalRes, error) {
	m, err := e.RejectProposal(ctx, params.ID, params.LineNo, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return &gen.StatementMatchHeaders{ETag: gen.NewOptString(etag.Format(m.StatementVersion)), Response: matchToAPI(m)}, nil
}

var houseBankSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "h.code", Type: "text"}}}

// ListHouseBanks implements GET /v1/house-banks: the house banks of the
// legal entities the caller may access, the account by its last four
// characters.
func (e *Endpoints) ListHouseBanks(ctx context.Context, params gen.ListHouseBanksParams) (gen.ListHouseBanksRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	ks, err := e.Pages.Keyset(houseBankSort, fmt.Sprintf("listHouseBanks|%s", le), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT h.id, h.legal_entity_id, h.code, h.currency, h.iban_last4, h.bic, h.status FROM payments.house_bank h
		WHERE ($1 OR h.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR h.legal_entity_id = $3)`, []any{access.All, access.IDs, le})
	rows, _ := tx.Query(ctx, q, args...)
	banks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (gen.HouseBank, error) {
		var b gen.HouseBank
		err := r.Scan(&b.ID, &b.LegalEntityID, &b.Code, &b.Currency, &b.AccountLast4, &b.Bic, &b.Status)
		return b, errs.Wrapf(err, "scan house bank")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: list house banks: %w", err)
	}
	items, next := apitypes.PageOf(ks, banks, func(b gen.HouseBank) (string, uuid.UUID) { return b.Code, b.ID }, func(b gen.HouseBank) gen.HouseBank { return b })
	return &gen.HouseBankPageHeaders{Response: gen.HouseBankPage{Items: items, NextPageToken: next}}, nil
}

// GetBankStatementRules implements GET /v1/bank-statements/{id}/rules: the
// posting and matching rules of the statement's company.
func (e *Endpoints) GetBankStatementRules(ctx context.Context, params gen.GetBankStatementRulesParams) (gen.GetBankStatementRulesRes, error) {
	s, _, err := visibleStatement(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	co, err := e.statementCompany(ctx, s.LegalEntityID)
	if err != nil {
		return nil, err
	}
	out := gen.BankStatementRules{PostingRules: []gen.BankStatementRulesPostingRulesItem{}, MatchingRules: []gen.BankStatementRulesMatchingRulesItem{}}
	if co == nil {
		return &gen.BankStatementRulesHeaders{Response: out}, nil
	}
	for _, r := range co.BankStatements.PostingRules {
		out.PostingRules = append(out.PostingRules, gen.BankStatementRulesPostingRulesItem{TransactionCode: r.TransactionCode, Description: r.Description, Account: r.Account})
	}
	for _, r := range co.BankStatements.MatchingRules {
		m := gen.BankStatementRulesMatchingRulesItem{Kind: r.Kind, AmountTolerance: optText(r.AmountTolerance)}
		if r.Kind == config.MatchAmount {
			m.DateWindowDays, m.Counterparty = gen.NewOptInt32(int32(r.DateWindowDays)), gen.NewOptBool(r.Counterparty)
		}
		out.MatchingRules = append(out.MatchingRules, m)
	}
	return &gen.BankStatementRulesHeaders{Response: out}, nil
}

// GetBankReconciliation implements GET /v1/house-banks/{id}/reconciliation:
// one page of the house bank's reconciliation report (T49h).
func (e *Endpoints) GetBankReconciliation(ctx context.Context, params gen.GetBankReconciliationParams) (gen.GetBankReconciliationRes, error) {
	from, to := apitypes.Ptr[time.Time](params.ClosingDateFrom), apitypes.Ptr[time.Time](params.ClosingDateTo)
	scope := fmt.Sprintf("getBankReconciliation|%s|%v|%v|%s", params.ID, from, to, params.LineStatus.Value)
	ks, err := e.Pages.Keyset(statementSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	r, err := e.BankReconciliation(ctx, params.ID, ReconciliationQuery{From: from, To: to, LineStatus: params.LineStatus.Value, Page: ks})
	if err != nil {
		return nil, err
	}
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, r.Currency, r.Exponent) }
	out := gen.BankReconciliation{HouseBankID: r.HouseBankID, LegalEntityID: r.LegalEntityID, Currency: r.Currency,
		ClosingDateFrom: apitypes.OptDate(r.From), ClosingDateTo: apitypes.OptDate(r.To), Items: make([]gen.BankReconciliationStatement, len(r.Statements)),
		Summary: gen.BankReconciliationSummary{Statements: r.Summary.Statements, OpeningDate: apitypes.OptDate(r.Summary.OpeningDate), Lines: lineCounts(r.Summary.Counts)}}
	if r.Summary.Opening != nil {
		out.Summary.OpeningBalance.SetTo(amount(*r.Summary.Opening))
	}
	if r.Summary.Figures != nil {
		out.Summary.Figures.SetTo(figuresToAPI(*r.Summary.Figures, amount))
	}
	for i, s := range r.Statements {
		st := gen.BankReconciliationStatement{BankStatementID: s.Statement.ID, StatementRef: s.Statement.Ref, Sequence: s.Statement.Sequence,
			Status: s.Statement.Status, OpeningDate: s.Statement.OpeningDate, OpeningBalance: amount(s.Statement.Opening),
			Figures: figuresToAPI(s.Figures, amount), LineCounts: lineCounts(s.Counts), Lines: make([]gen.BankReconciliationLine, len(s.Lines))}
		for j, l := range s.Lines {
			st.Lines[j] = reconciledLineToAPI(l, amount)
		}
		out.Items[i] = st
	}
	if r.NextPageToken != "" {
		out.NextPageToken.SetTo(r.NextPageToken)
	}
	return &gen.BankReconciliationHeaders{Response: out}, nil
}

func figuresToAPI(f Figures, amount func(money.Minor) gen.Money) gen.BankReconciliationFigures {
	out := gen.BankReconciliationFigures{AsOf: f.AsOf, ClosingBalance: amount(f.Closing), Excluded: amount(f.Excluded), InTransit: amount(f.InTransit),
		PostedLater: amount(f.PostedLater)}
	if f.Ledger != nil {
		out.LedgerBalance.SetTo(amount(*f.Ledger))
		out.Unexplained.SetTo(amount(*f.Unexplained()))
	}
	return out
}

func lineCounts(c LineCounts) gen.BankReconciliationLineCounts {
	return gen.BankReconciliationLineCounts{Unmatched: c["unmatched"], Proposed: c["proposed"], Matched: c[MatchMatched], Excluded: c[MatchExcluded],
		PendingApproval: c["pending_approval"], Posted: c["posted"]}
}

func reconciledLineToAPI(l ReconciledLine, amount func(money.Minor) gen.Money) gen.BankReconciliationLine {
	out := gen.BankReconciliationLine{LineNo: l.LineNo, BookingDate: l.BookingDate, Amount: amount(l.Amount), IsReversal: l.Reversal,
		TransactionCode: l.TransactionCode, Status: l.Status, MatchID: apitypes.OptUUID(l.MatchID), MatchKind: apitypes.OptString(l.MatchKind),
		MatchRule: apitypes.OptString(l.MatchRule), MatchStatus: apitypes.OptString(l.MatchStatus), PostingDate: apitypes.OptDate(l.PostingDate),
		JournalEntryID: apitypes.OptUUID(l.JournalEntryID), ApprovalRequestID: apitypes.OptUUID(l.ApprovalRequestID),
		ProposalOutcome: apitypes.OptString(l.ProposalOutcome)}
	if l.Incoming {
		out.Incoming.SetTo(true)
	}
	if l.JournalEntryYear != nil {
		out.JournalEntryYear.SetTo(int32(*l.JournalEntryYear))
	}
	return out
}
