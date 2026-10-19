package payments

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// dueRequests are the legal entity's open down payment requests due by
// the run's due-by date in its currency (T46c, ADR-0037), leaving out
// those another live run pays. The caller holds the proposals lock
// (build), which a cancellation takes too.
func (e *Endpoints) dueRequests(ctx context.Context, tx pgx.Tx, r Run) ([]DownPayment, error) {
	due, err := e.DownPayments.Due(ctx, r.LegalEntityID, r.Currency, r.DueBy)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT p.down_payment_request_id FROM payments.payment p
		JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
		WHERE r.legal_entity_id = $1 AND p.kind = 'down_payment' AND `+liveRun, r.LegalEntityID)
	held, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("payments: read the down payment requests live runs of %s pay: %w", r.LegalEntityID, err)
	}
	out := make([]DownPayment, 0, len(due))
	for _, d := range due {
		if !slices.Contains(held, d.ID) {
			out = append(out, d)
		}
	}
	return out, nil
}

// payRequests adds a payment per due request, whole and never netted with
// the supplier's items: SAP pays special G/L items separately. A request
// of a supplier that cannot be paid (exclusion: blocked, no bank account,
// a bank change) is left out with that reason, and one whose tax code is
// not valid on the run date (invalid) with tax_code_invalid, each in an
// exclusion row of its own, apart from the supplier's items (a debit
// balance of those does not stop its requests).
func (pr *proposal) payRequests(requests []DownPayment, invalid map[uuid.UUID]bool, payees map[uuid.UUID]partner.Payee, r Run, entityCode string, holdDays int) error {
	for _, d := range requests {
		request := d.ID
		payee, found := payees[d.SupplierID]
		reason := exclusion(payee, found, r.RunDate, holdDays)
		if reason == "" && invalid[d.ID] {
			reason = TaxCodeInvalid
		}
		if reason != "" {
			pr.exclude(d.SupplierID, &request, reason, d.Amount)
			continue
		}
		total, err := money.Sum(pr.total, d.Amount)
		if err != nil {
			return errTooLarge("the run")
		}
		pr.total = total
		pr.payment(d.SupplierID, *payee.BankAccountID, [3]money.Minor{d.Amount, 0, 0}, KindDownPayment, &request, r, entityCode)
	}
	return nil
}

// exclude excludes the supplier's items (request nil) or one of its
// requests for the reason.
func (pr *proposal) exclude(supplier uuid.UUID, request *uuid.UUID, reason string, amount money.Minor) {
	pr.exSuppliers, pr.exRequests = append(pr.exSuppliers, supplier), append(pr.exRequests, request)
	pr.exReasons, pr.exAmounts = append(pr.exReasons, reason), append(pr.exAmounts, amount)
}

// postDownPayment posts a released run's payment of a down payment
// request (ledger.Postings.PayDownPayment: the down payment as the
// supplier's special G/L item, its input tax, the bank) and marks the
// request paid, in the request's transaction, so a retry finds the
// payment posted and posts nothing twice.
func (e *Endpoints) postDownPayment(ctx context.Context, tx pgx.Tx, r Run, p Payment) error {
	d, err := e.DownPayments.Request(ctx, *p.RequestID)
	if err != nil {
		return err
	}
	// The run's house bank exists: payment_run has a foreign key to it
	// (migration 000008), so the found flag is always true here.
	hb, _, err := loadHouseBank(ctx, tx, r.HouseBankID)
	if err != nil {
		return err
	}
	text := "Down payment " + d.DocumentNumber + ", payment run " + r.DocumentNumber
	entry, err := e.Postings.PayDownPayment(ctx, ledger.DownPaymentPayment{LegalEntityID: r.LegalEntityID, SupplierID: p.SupplierID,
		BankAccountID: hb.GLAccountID, DocumentType: DocumentType, PostingDate: r.RunDate, Currency: r.Currency, Amount: p.Amount,
		TaxCode: d.TaxCode, Tax: d.Tax, Reference: &p.EndToEndID, HeaderText: &text, SourceDocumentID: p.ID})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment SET journal_entry_year = $2, journal_entry_id = $3 WHERE id = $1`, p.ID, entry.FiscalYear, entry.ID); err != nil {
		return fmt.Errorf("payments: record the posting of payment %s: %w", p.ID, err)
	}
	return e.DownPayments.Paid(ctx, d.ID, entry.FiscalYear, entry.ID)
}

// requestRemittance is the remittance text of a down payment's payment:
// the request's number and reference.
func (e *Endpoints) requestRemittance(ctx context.Context, p Payment) (string, error) {
	d, err := e.DownPayments.Request(ctx, *p.RequestID)
	if err != nil {
		return "", err
	}
	parts := []string{"Down payment " + d.DocumentNumber}
	if d.Reference != nil {
		parts = append(parts, *d.Reference)
	}
	return strings.Join(parts, " "), nil
}

// invalidTaxCodes are the requests whose input tax code is not valid on
// the run date: their payment's posting would be refused at release, maybe
// after other payments posted, leaving the run releasing; the proposal
// leaves them out (payRequests).
func (e *Endpoints) invalidTaxCodes(ctx context.Context, r Run, requests []DownPayment) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, d := range requests {
		valid, err := e.taxCodeValid(ctx, r, d)
		if err != nil {
			return nil, err
		}
		if !valid {
			out[d.ID] = true
		}
	}
	return out, nil
}

// taxCodeValid says whether a request's input tax code, if it has tax, is
// valid on the run date.
func (e *Endpoints) taxCodeValid(ctx context.Context, r Run, d DownPayment) (bool, error) {
	if d.TaxCode == nil || d.Tax == 0 {
		return true, nil
	}
	_, ok, err := e.Accounts.TaxCode(ctx, r.LegalEntityID, *d.TaxCode, r.RunDate)
	return ok, err
}

// recheckRequests refuses the release of a proposed run that pays a
// request whose tax code is no longer valid on the run date (409
// payments.proposal_outdated): nothing is posted yet, so the run is
// rejected and proposed again, which leaves the request out.
func (e *Endpoints) recheckRequests(ctx context.Context, r Run, pays []Payment) error {
	if r.Status != Proposed {
		return nil
	}
	var requests []DownPayment
	for _, p := range pays {
		if p.Kind != KindDownPayment {
			continue
		}
		d, err := e.DownPayments.Request(ctx, *p.RequestID)
		if err != nil {
			return err
		}
		requests = append(requests, d)
	}
	for _, d := range requests {
		valid, err := e.taxCodeValid(ctx, r, d)
		if err != nil {
			return err
		}
		if !valid {
			return problem.Conflict("payments.proposal_outdated", "Down payment request "+d.DocumentNumber+" has tax code "+*d.TaxCode+
				", which is no longer valid on the run date; reject the run, and propose again, which leaves the request out.")
		}
	}
	return nil
}
