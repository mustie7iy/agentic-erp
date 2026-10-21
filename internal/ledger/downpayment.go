package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// DownPaymentPayment is the payment of a down payment request (T46c,
// ADR-0037): the payments context posts it when its run is released.
type DownPaymentPayment struct {
	LegalEntityID    uuid.UUID
	SupplierID       uuid.UUID
	BankAccountID    uuid.UUID // the house bank's ledger account
	DocumentType     string
	PostingDate      time.Time
	Currency         string
	Amount           money.Minor // paid, tax included
	TaxCode          *string     // the request's input tax code, or nil
	Tax              money.Minor // the input tax the amount includes
	Reference        *string
	HeaderText       *string
	SourceDocumentID uuid.UUID // the payment
}

// PayDownPayment posts a down payment request's payment: Dr the legal
// entity's down payment account, as the supplier's special G/L item A, net
// of tax; Dr input tax on the request's tax code; Cr bank. A payment
// already posted is not posted again: the entry posted for it (its source
// document, in the payments context) is returned instead.
func (p Postings) PayDownPayment(ctx context.Context, in DownPaymentPayment) (Entry, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	if entry, found, err := postedFor(ctx, tx, "payments", in.SourceDocumentID); err != nil || found {
		return entry, err
	}
	lines, err := downPaymentLines(ctx, in)
	if err != nil {
		return Entry{}, err
	}
	return p.Post(ctx, Posting{LegalEntityID: in.LegalEntityID, DocumentType: in.DocumentType, DocumentDate: in.PostingDate, PostingDate: in.PostingDate,
		Currency: in.Currency, Reference: in.Reference, HeaderText: in.HeaderText, SourceContext: "payments", SourceDocumentID: in.SourceDocumentID,
		Lines: lines})
}

// downPaymentLines are a down payment's lines: the special G/L item on
// the down payment account, the input tax when there is any, and the bank.
func downPaymentLines(ctx context.Context, in DownPaymentPayment) ([]PostingLine, error) {
	account, ok, err := Accounts{}.Determination(ctx, in.LegalEntityID, "down_payment")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, problem.Unprocessable("ledger.no_down_payment_account", "The legal entity has no down_payment account in its account determination.")
	}
	if in.Tax < 0 || in.Tax >= in.Amount {
		return nil, problem.Unprocessable("ledger.invalid_down_payment", "A down payment's tax is 0 or more and less than the amount paid.")
	}
	net := in.Amount - in.Tax // no overflow: 0 <= Tax < Amount
	special := SpecialDownPayment
	lines := []PostingLine{{AccountID: account, Amount: net, PartnerID: &in.SupplierID, SpecialGL: &special}}
	if in.TaxCode != nil && in.Tax != 0 {
		code, ok, err := Accounts{}.TaxCode(ctx, in.LegalEntityID, *in.TaxCode, in.PostingDate)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, problem.Unprocessable("ledger.unknown_tax_code", "Tax code "+*in.TaxCode+" is not valid in the legal entity on the posting date.")
		}
		single, ok := code.Single()
		if !ok {
			return nil, problem.Unprocessable("ledger.tax_code_components", "Tax code "+*in.TaxCode+" has components a down payment cannot post yet "+
				"(several, non-deductible or partly recoverable; T61d).")
		}
		lines = append(lines, PostingLine{AccountID: *single.AccountID, Amount: in.Tax, TaxCode: in.TaxCode})
	}
	return append(lines, PostingLine{AccountID: in.BankAccountID, Amount: -in.Amount}), nil
}

// SpecialDownPayment is the special G/L indicator of down payments.
const SpecialDownPayment = "A"

// postedFor finds the entry posted for a source document, not a reversal:
// its posting group's first entry, the leading ledger's, not a copy.
func postedFor(ctx context.Context, tx pgx.Tx, source string, document uuid.UUID) (Entry, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM ledger.journal_entry WHERE source_context = $1 AND source_document_id = $2 AND reversal_of_id IS NULL
			AND (posting_group_id IS NULL OR posting_group_id = id)
		ORDER BY posted_at LIMIT 1`, source, document).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("ledger: the entry posted for %s document %s: %w", source, document, err)
	}
	entry, err := getEntry(ctx, tx, id)
	return entry, err == nil, err
}
