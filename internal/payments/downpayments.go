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
// the supplier's items: SAP pays special G/L items separately. A supplier
// that cannot be paid at all (exclusion: blocked, no bank account, a bank
// change) is excluded with its requests, which add to its exclusion's
// amount; one whose items are excluded for a debit balance still has its
// requests paid, since they are not netted with those items.
func (pr *proposal) payRequests(requests []DownPayment, payees map[uuid.UUID]partner.Payee, r Run, entityCode string, holdDays int) error {
	for _, d := range requests {
		payee, found := payees[d.SupplierID]
		if reason := exclusion(payee, found, r.RunDate, holdDays); reason != "" {
			if err := pr.exclude(d.SupplierID, reason, d.Amount); err != nil {
				return err
			}
			continue
		}
		total, err := money.Sum(pr.total, d.Amount)
		if err != nil {
			return errTooLarge("the run")
		}
		pr.total = total
		request := d.ID
		pr.payment(d.SupplierID, *payee.BankAccountID, d.Amount, 0, KindDownPayment, &request, r, entityCode)
	}
	return nil
}

// exclude excludes the supplier for the reason, or adds the amount to its
// exclusion when it has one.
func (pr *proposal) exclude(supplier uuid.UUID, reason string, amount money.Minor) error {
	for i, s := range pr.exSuppliers {
		if s == supplier {
			sum, err := money.Sum(pr.exAmounts[i], amount)
			if err != nil {
				return errTooLarge("supplier " + supplier.String())
			}
			pr.exAmounts[i] = sum
			return nil
		}
	}
	pr.exSuppliers, pr.exReasons, pr.exAmounts = append(pr.exSuppliers, supplier), append(pr.exReasons, reason), append(pr.exAmounts, amount)
	return nil
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

// paidRequests are the requests the proposal pays.
func paidRequests(requests []DownPayment, pr proposal) []DownPayment {
	var out []DownPayment
	for _, d := range requests {
		if slices.ContainsFunc(pr.requests, func(id *uuid.UUID) bool { return id != nil && *id == d.ID }) {
			out = append(out, d)
		}
	}
	return out
}

// requireRequestTaxCodes refuses, with the problem refuse makes, a run
// that pays a request whose input tax code is not valid on the run date:
// its payment's posting would be refused at release, maybe after other
// payments posted, leaving the run releasing. A request was checked on its
// due date, so a code that ended since is caught here, at proposal, and
// again before the release starts (recheck).
func (e *Endpoints) requireRequestTaxCodes(ctx context.Context, r Run, requests []DownPayment, refuse func(DownPayment) *problem.Problem) error {
	for _, d := range requests {
		if d.TaxCode == nil || d.Tax == 0 {
			continue
		}
		_, ok, err := e.Accounts.TaxCode(ctx, r.LegalEntityID, *d.TaxCode, r.RunDate)
		if err != nil {
			return err
		}
		if !ok {
			return refuse(d)
		}
	}
	return nil
}

// recheckRequests refuses the release of a proposed run that pays a
// request whose tax code is no longer valid on the run date (409
// payments.proposal_outdated): nothing is posted yet, so the run is
// rejected and the request requested again.
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
	return e.requireRequestTaxCodes(ctx, r, requests, func(d DownPayment) *problem.Problem {
		return problem.Conflict("payments.proposal_outdated", "Down payment request "+d.DocumentNumber+" has tax code "+*d.TaxCode+
			", which is no longer valid on the run date; reject the run, and cancel and request the down payment again.")
	})
}
