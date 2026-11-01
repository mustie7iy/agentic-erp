package payments

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
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
// not valid on the run date or whose tax no longer matches the run date's
// rate (invalid) with tax_code_invalid, naming the code and the rates,
// each in an exclusion row of its own, apart from the supplier's items (a
// debit balance of those does not stop its requests); one no ranked house
// bank can pay with NoHouseBank. It goes through the bank and method its
// supplier's items would (router).
func (pr *proposal) payRequests(requests []DownPayment, invalid map[uuid.UUID]taxCheck, payees map[uuid.UUID]partner.Payee, r Run, entityCode string,
	holdDays int) error {
	for _, d := range requests {
		request := d.ID
		payee, found := payees[d.SupplierID]
		reason := exclusion(payee, found, r.RunDate, holdDays)
		check, bad := invalid[d.ID]
		if reason == "" && bad {
			reason = TaxCodeInvalid
		}
		v, routed := pr.route.route(payee)
		if reason == "" && !routed {
			reason = NoHouseBank
		}
		if reason != "" {
			if reason != TaxCodeInvalid {
				check = taxCheck{}
			}
			pr.exclude(d.SupplierID, &request, reason, d.Amount, check)
			continue
		}
		total, err := money.Sum(pr.total, d.Amount)
		if err != nil {
			return errTooLarge("the run")
		}
		pr.total = total
		pr.payment(d.SupplierID, *payee.BankAccountID, v, [3]money.Minor{d.Amount, 0, 0}, KindDownPayment, &request, r, entityCode)
	}
	return nil
}

// exclude excludes the supplier's items (request nil) or one of its
// requests for the reason, a request left out with tax_code_invalid
// naming its code and rates (check; zero otherwise).
func (pr *proposal) exclude(supplier uuid.UUID, request *uuid.UUID, reason string, amount money.Minor, check taxCheck) {
	pr.exSuppliers, pr.exRequests = append(pr.exSuppliers, supplier), append(pr.exRequests, request)
	pr.exReasons, pr.exAmounts = append(pr.exReasons, reason), append(pr.exAmounts, amount)
	pr.exTaxCodes, pr.exRequestRates, pr.exRunRates = append(pr.exTaxCodes, check.code), append(pr.exRequestRates, check.requestRate),
		append(pr.exRunRates, check.runRate)
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
	hb, err := payingBank(ctx, tx, r, p)
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

// taxCheck is why a request's tax cannot be paid on the run date: its
// code, the code's rate on the request's due date, at which its tax was
// calculated, and on the run date (nil where the code has no rate a down
// payment can post that day).
type taxCheck struct {
	code                 *string
	requestRate, runRate *string
}

// rates names a check's rates for people: "19% when requested, 16% on the
// run date".
func (c taxCheck) rates() string {
	pct := func(r *string, none string) string {
		if r == nil {
			return none
		}
		return *r + "%"
	}
	return pct(c.requestRate, "no rate") + " when requested, " + pct(c.runRate, "not valid") + " on the run date"
}

// invalidTaxCodes are the requests whose tax cannot be paid on the run
// date (taxCheck): their payment's posting would be refused at release, or
// would claim tax at a rate no longer charged, maybe after other payments
// posted; the proposal leaves them out (payRequests).
func (e *Endpoints) invalidTaxCodes(ctx context.Context, r Run, requests []DownPayment) (map[uuid.UUID]taxCheck, error) {
	out := map[uuid.UUID]taxCheck{}
	for _, d := range requests {
		check, ok, err := e.checkTax(ctx, r, d)
		if err != nil {
			return nil, err
		}
		if !ok {
			out[d.ID] = check
		}
	}
	return out, nil
}

// checkTax says whether a request's input tax can be paid on the run date
// (ADR-0049 §4): its code valid that day as an input code a down payment
// can post (one component on an account of its own), and its stated tax
// the tax its amount includes at that day's rate (tax.Included); a request
// without tax, or whose code is no longer valid but states none, posts
// none. Otherwise it returns the check naming the code and rates.
func (e *Endpoints) checkTax(ctx context.Context, r Run, d DownPayment) (taxCheck, bool, error) {
	if d.TaxCode == nil {
		return taxCheck{}, true, nil
	}
	run, err := e.inputRate(ctx, r.LegalEntityID, *d.TaxCode, r.RunDate)
	if err != nil {
		return taxCheck{}, false, err
	}
	if run == nil && d.Tax == 0 {
		return taxCheck{}, true, nil
	}
	if run != nil {
		t, err := tax.Included(d.Amount, r.Exponent, *run) // a configured rate: below 100%
		if err != nil || t == d.Tax {
			return taxCheck{}, err == nil, err
		}
	}
	requested, err := e.inputRate(ctx, r.LegalEntityID, *d.TaxCode, d.DueDate)
	return taxCheck{code: d.TaxCode, requestRate: requested, runRate: run}, false, err
}

// inputRate is an input tax code's rate in percent on a date, nil unless
// the code is then valid as an input code with one component on an
// account of its own (until T61d's components reach down payments).
func (e *Endpoints) inputRate(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (*string, error) {
	t, ok, err := e.Accounts.TaxCode(ctx, legalEntity, code, date)
	if err != nil || !ok {
		return nil, err
	}
	c, single := t.Single()
	if t.Direction != "input" || !single {
		return nil, nil
	}
	rate := money.ParseRat(c.RatePct).FloatString(6) // read from numeric: a decimal
	rate = strings.TrimRight(strings.TrimRight(rate, "0"), ".")
	return &rate, nil
}

// recheckRequests refuses the release of a proposed run that pays a
// request whose tax can no longer be paid on the run date (checkTax; 409
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
		check, ok, err := e.checkTax(ctx, r, d)
		if err != nil {
			return err
		}
		if !ok {
			return problem.Conflict("payments.proposal_outdated", "Down payment request "+d.DocumentNumber+" has tax code "+*d.TaxCode+" ("+check.rates()+
				"), whose tax can no longer be paid on the run date; reject the run, and propose again, which leaves the request out.")
		}
	}
	return nil
}
