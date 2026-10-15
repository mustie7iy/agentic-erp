package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Exclusion reasons.
const (
	SupplierBlocked   = "supplier_blocked"
	PaymentBlocked    = "payment_blocked"
	NoBankAccount     = "no_bank_account"
	BankChangePending = "bank_change_pending"
	BankChangeHold    = "bank_change_hold"
	DebitBalance      = "debit_balance"
	// A release drops a payment whose item was reversed, or is no longer
	// open, since the proposal (dropPayment).
	ItemReversed = "item_reversed"
	ItemNotOpen  = "item_not_open"
)

// Proposal is what a client asks to pay.
type Proposal struct {
	LegalEntityID uuid.UUID
	HouseBankID   uuid.UUID
	RunDate       time.Time
	DueBy         time.Time
}

// ProposeRun creates a run in status proposing, numbered from the
// payment_run range of the run date's fiscal year, and queues its
// payments.propose operation, whose resource is the run from the start.
// The Proposer builds it.
func (e *Endpoints) ProposeRun(ctx context.Context, in Proposal) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return operations.Operation{}, err
	}
	if le.Status != "active" {
		return operations.Operation{}, problem.Unprocessable("payments.legal_entity_not_active", "The legal entity is "+le.Status+"; payment runs need it active.")
	}
	hb, ok, err := loadHouseBank(ctx, tx, in.HouseBankID)
	switch {
	case err != nil:
		return operations.Operation{}, err
	case !ok || hb.LegalEntityID != le.ID:
		return operations.Operation{}, problem.ReferenceNotFound("house_bank_id", "house bank of the legal entity", "no house bank of the legal entity with this id that you can access")
	case hb.Status != "active":
		return operations.Operation{}, problem.Unprocessable("payments.house_bank_inactive", "House bank "+hb.Code+" is inactive.",
			problem.FieldError{Field: "house_bank_id", Code: "inactive", Message: "an active house bank"})
	}
	year, err := e.Calendar.FiscalYear(ctx, le.ID, in.RunDate)
	if err != nil {
		return operations.Operation{}, err
	}
	number, err := e.Numbers.Next(ctx, le.ID, RunNumbers, year)
	if err != nil {
		return operations.Operation{}, err
	}
	id := uuid.Must(uuid.NewV7())
	op, err := operations.StartFor(ctx, ProposeKind, map[string]any{"legal_entity_id": le.ID, "house_bank_id": hb.ID,
		"run_date": in.RunDate.Format(time.DateOnly), "due_by": in.DueBy.Format(time.DateOnly)}, "/v1/payment-runs/"+id.String())
	if err != nil {
		return operations.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by,
			currency, total_amount, status, proposed_by, operation_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, 'proposing', $9, $10, $9, $9)`,
		p.TenantID, id, number, le.ID, hb.ID, in.RunDate, in.DueBy, hb.Currency, p.ID, op.ID); err != nil {
		return operations.Operation{}, fmt.Errorf("payments: insert run %s: %w", id, err)
	}
	return op, audit.Add(ctx, audit.Entry{Action: "payments.payment_run.propose", ObjectType: "payments.payment_run", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"document_number": number, "operation_id": op.ID, "run_date": in.RunDate.Format(time.DateOnly),
			"due_by": in.DueBy.Format(time.DateOnly)}})
}

// candidate is a supplier's due items not in another live run.
type candidate struct {
	supplier uuid.UUID
	items    []Item
}

// errTooLarge fails a proposal whose amounts add up to more than an
// amount holds (money.ErrOverflow): a wrapped total would pay the wrong
// amount, and the exclusion reasons are a closed set in the database, so
// the run fails rather than leaving the supplier out. The problem replaces
// money.ErrOverflow by design: it is what the operation records.
func errTooLarge(what string) error {
	return problem.Unprocessable("payments.amount_too_large", "The open items of "+what+" add up to more than an amount can hold.")
}

// total is what is owed the supplier: positive when it is to be paid.
func (c candidate) total() (money.Minor, error) {
	amounts := make([]money.Minor, len(c.items))
	for i, it := range c.items {
		amounts[i] = it.Amount
	}
	return money.Sum(amounts...)
}

// exclusion is why a supplier cannot be paid on the run date, or "".
func exclusion(p partner.Payee, found bool, runDate time.Time, holdDays int) string {
	switch {
	case !found || p.Status != partner.Active || p.PostingBlocked:
		return SupplierBlocked
	case p.PaymentBlocked:
		return PaymentBlocked
	case p.BankAccountID == nil:
		return NoBankAccount
	case p.BankChangePending:
		return BankChangePending
	case p.BankChanged && runDate.Before(p.BankValidFrom.AddDate(0, 0, holdDays)):
		return BankChangeHold
	}
	return ""
}

// candidates groups the legal entity's due open items by supplier, leaving
// out items reserved by another live run.
func (e *Endpoints) candidates(ctx context.Context, tx pgx.Tx, r Run) ([]candidate, map[uuid.UUID]partner.Payee, error) {
	open, err := e.OpenItems.Payable(ctx, r.LegalEntityID, r.Currency, r.DueBy)
	if err != nil {
		return nil, nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.item_fiscal_year, i.item_entry_id, i.item_line_no FROM payments.payment_item i
		JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
		JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
		WHERE r.legal_entity_id = $1 AND i.is_active`, r.LegalEntityID)
	reserved := map[ledger.ItemRef]bool{}
	var ref ledger.ItemRef
	if _, err := pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &ref.LineNo}, func() error { reserved[ref] = true; return nil }); err != nil {
		return nil, nil, fmt.Errorf("payments: read the reserved items of legal entity %s: %w", r.LegalEntityID, err)
	}
	var suppliers []uuid.UUID
	for i, it := range open {
		if i == 0 || it.PartnerID != open[i-1].PartnerID {
			suppliers = append(suppliers, it.PartnerID)
		}
	}
	payees, err := e.Payees.Payees(ctx, r.LegalEntityID, suppliers, r.RunDate)
	if err != nil {
		return nil, nil, err
	}
	var out []candidate
	for _, it := range open {
		due := it.DocumentDate.AddDate(0, 0, payees[it.PartnerID].NetDays)
		if due.After(r.DueBy) || reserved[it.ItemRef] {
			continue
		}
		if len(out) == 0 || out[len(out)-1].supplier != it.PartnerID {
			out = append(out, candidate{supplier: it.PartnerID})
		}
		c := &out[len(out)-1]
		c.items = append(c.items, Item{FiscalYear: it.FiscalYear, EntryID: it.EntryID, LineNo: it.LineNo, Amount: -it.Amount,
			DueDate: partner.Date{Time: due}, Active: true})
	}
	return out, payees, nil
}

// build computes a proposing run's payments and exclusions, reserves the
// paid items and moves the run to proposed. Proposals of one legal entity
// are serialized, so two runs never reserve the same item.
func (e *Endpoints) build(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) (Run, error) {
	if err := lockProposals(ctx, tx, r.LegalEntityID); err != nil {
		return Run{}, err
	}
	// errNoEntity becomes a problem, which fails this run (Process): left an
	// error, the job would be retried first in its tenant's queue forever,
	// holding back every later proposal.
	le, err := e.entityOf(ctx, r)
	if errors.Is(err, errNoEntity) {
		return Run{}, noEntity(r)
	}
	if err != nil {
		return Run{}, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return Run{}, err
	}
	cands, payees, err := e.candidates(ctx, tx, r)
	if err != nil {
		return Run{}, err
	}
	pr, err := propose(cands, payees, r, le.Code, doc.Payments.BankChangeHoldDays)
	if err != nil {
		return Run{}, err
	}
	if err := pr.insert(ctx, tx, p, r); err != nil {
		return Run{}, err
	}
	if err := e.Postings.Reserve(ctx, owner(r), pr.items()); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'proposed', total_amount = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		r.ID, pr.total, p.ID); err != nil {
		return Run{}, fmt.Errorf("payments: propose run %s: %w", r.ID, err)
	}
	n := int64(len(pr.itemPays))
	if err := operations.Report(ctx, *r.OperationID, n, n); err != nil {
		return Run{}, err
	}
	if _, err := operations.Succeed(ctx, *r.OperationID, "/v1/payment-runs/"+r.ID.String(), map[string]any{"payment_run_id": r.ID, "status": Proposed,
		"payment_count": len(pr.payIDs), "exclusion_count": len(pr.exSuppliers), "item_count": n, "total_amount": money.EventAmount(pr.total, r.Currency, r.Exponent)}); err != nil {
		return Run{}, err
	}
	proposed, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	return proposed, record(ctx, "payments.payment_run.build", r.ID, map[string]any{"payments": len(pr.payIDs), "exclusions": len(pr.exSuppliers), "items": n},
		runEvent(proposed, "payment_run_proposed", map[string]any{"house_bank_id": proposed.HouseBankID, "run_date": proposed.RunDate.Format(time.DateOnly),
			"due_by": proposed.DueBy.Format(time.DateOnly), "total_amount": money.EventAmount(pr.total, proposed.Currency, proposed.Exponent),
			"payment_count": len(pr.payIDs), "exclusion_count": len(pr.exSuppliers), "proposed_by": proposed.ProposedBy, "operation_id": proposed.OperationID}))
}

// lockProposals takes the legal entity's proposals lock until the
// transaction ends. Proposals take it so two runs never reserve the same
// item; whoever would change what a proposal pays (an invoice reversal,
// through RunOf) takes it too, so it waits for a proposal under way and then
// sees the items it reserved, and a proposal that starts later waits for it
// and then no longer finds the item open.
func lockProposals(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "payments.propose."+legalEntity.String()); err != nil {
		return fmt.Errorf("payments: lock the proposals of legal entity %s: %w", legalEntity, err)
	}
	return nil
}

// proposal is a run's payments, their items and its exclusions, as the
// columns they are inserted from.
type proposal struct {
	payIDs, suppliers, banks, exSuppliers []uuid.UUID
	amounts, exAmounts                    []money.Minor
	e2e, exReasons                        []string
	itemPays, itemEntries                 []uuid.UUID
	itemYears                             []int16
	itemLines                             []int32
	itemAmounts                           []money.Minor
	itemDue                               []time.Time
	total                                 money.Minor
}

// propose pays each candidate supplier its open items in one payment, or
// excludes it with the reason. Totals out of range fail the proposal
// (errTooLarge).
func propose(cands []candidate, payees map[uuid.UUID]partner.Payee, r Run, entityCode string, holdDays int) (proposal, error) {
	var pr proposal
	for _, c := range cands {
		total, err := c.total()
		if err != nil {
			return proposal{}, errTooLarge("supplier " + c.supplier.String())
		}
		payee, found := payees[c.supplier]
		reason := exclusion(payee, found, r.RunDate, holdDays)
		if reason == "" && total <= 0 {
			reason = DebitBalance
		}
		if reason != "" {
			pr.exSuppliers, pr.exReasons, pr.exAmounts = append(pr.exSuppliers, c.supplier), append(pr.exReasons, reason), append(pr.exAmounts, total)
			continue
		}
		if pr.total, err = money.Sum(pr.total, total); err != nil {
			return proposal{}, errTooLarge("the run")
		}
		id := uuid.Must(uuid.NewV7())
		pr.payIDs, pr.suppliers, pr.banks, pr.amounts = append(pr.payIDs, id), append(pr.suppliers, c.supplier), append(pr.banks, *payee.BankAccountID), append(pr.amounts, total)
		pr.e2e = append(pr.e2e, fmt.Sprintf("%s-%s-%06d", entityCode, r.DocumentNumber, len(pr.payIDs)))
		for _, it := range c.items {
			pr.itemPays, pr.itemYears, pr.itemEntries, pr.itemLines = append(pr.itemPays, id), append(pr.itemYears, it.FiscalYear), append(pr.itemEntries, it.EntryID), append(pr.itemLines, it.LineNo)
			pr.itemAmounts, pr.itemDue = append(pr.itemAmounts, it.Amount), append(pr.itemDue, it.DueDate.Time)
		}
	}
	return pr, nil
}

// items are the open items the proposal pays.
func (pr *proposal) items() []ledger.ItemRef {
	refs := make([]ledger.ItemRef, len(pr.itemEntries))
	for i := range refs {
		refs[i] = ledger.ItemRef{FiscalYear: pr.itemYears[i], EntryID: pr.itemEntries[i], LineNo: pr.itemLines[i]}
	}
	return refs
}

// owner is the run as the holder of the open items it reserves in the
// ledger.
func owner(r Run) ledger.Owner {
	return ledger.Owner{Context: "payments", ID: r.ID, Reference: r.DocumentNumber}
}

// insert stores the proposal's payments, items and exclusions.
func (pr *proposal) insert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) error {
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by)
		SELECT $1, i, $2, s, b, a, $3, x, $4 FROM unnest($5::uuid[], $6::uuid[], $7::uuid[], $8::bigint[], $9::text[]) AS t(i, s, b, a, x)`,
		p.TenantID, r.ID, r.Currency, p.ID, pr.payIDs, pr.suppliers, pr.banks, pr.amounts, pr.e2e); err != nil {
		return fmt.Errorf("payments: insert the payments of run %s: %w", r.ID, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
		SELECT $1, p, y, je, n, a, d FROM unnest($2::uuid[], $3::smallint[], $4::uuid[], $5::int[], $6::bigint[], $7::date[]) AS t(p, y, je, n, a, d)`,
		p.TenantID, pr.itemPays, pr.itemYears, pr.itemEntries, pr.itemLines, pr.itemAmounts, pr.itemDue); err != nil {
		return fmt.Errorf("payments: insert the payment items of run %s: %w", r.ID, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment_exclusion (tenant_id, payment_run_id, supplier_id, reason_code, amount)
		SELECT $1, $2, s, c, a FROM unnest($3::uuid[], $4::text[], $5::bigint[]) AS t(s, c, a)`,
		p.TenantID, r.ID, pr.exSuppliers, pr.exReasons, pr.exAmounts); err != nil {
		return fmt.Errorf("payments: insert the exclusions of run %s: %w", r.ID, err)
	}
	return nil
}

// fail moves a run that could not be proposed, or whose operation was
// cancelled, to failed; the operation records the problem unless it is
// already cancelled.
func fail(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, why *problem.Problem) error {
	if why != nil {
		if _, err := operations.Fail(ctx, *r.OperationID, why); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'failed', version = version + 1, updated_by = $2 WHERE id = $1`, r.ID, p.ID); err != nil {
		return fmt.Errorf("payments: fail run %s: %w", r.ID, err)
	}
	failed, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return err
	}
	detail := map[string]any{"cancelled": why == nil}
	if why != nil {
		detail["code"], detail["detail"] = why.Code, why.Detail
	}
	return record(ctx, "payments.payment_run.fail", r.ID, detail, runEvent(failed, "payment_run_failed", map[string]any{"operation_id": r.OperationID,
		"released_item_count": 0, "previous_status": Proposing}))
}

// Job is a proposal waiting for the Proposer: a queued one to build, or
// a cancelled one whose run is still proposing.
type Job struct {
	RunID        uuid.UUID
	Cancelled    bool
	ProposedBy   uuid.UUID
	ProposerKind string
}

// NextJob finds and locks the tenant's oldest job, skipping jobs another
// worker holds.
func (e *Endpoints) NextJob(ctx context.Context, tx pgx.Tx) (Job, bool, error) {
	var j Job
	var status string
	err := tx.QueryRow(ctx, `SELECT r.id, o.status, r.proposed_by FROM platform.operation o
		JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
		WHERE o.kind = $1 AND r.status = 'proposing' AND o.status IN ('queued', 'cancelled')
		ORDER BY o.id LIMIT 1 FOR UPDATE OF o, r SKIP LOCKED`, ProposeKind).Scan(&j.RunID, &status, &j.ProposedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("payments: find the next proposal job: %w", err)
	}
	return e.proposer(ctx, tx, j, status)
}

// JobFor finds and locks one run's job: the run still proposing with its
// operation queued or cancelled. ok is false when there is nothing left to
// do for the run, so a retried worker does nothing twice.
func (e *Endpoints) JobFor(ctx context.Context, tx pgx.Tx, run uuid.UUID) (Job, bool, error) {
	var j Job
	var status string
	err := tx.QueryRow(ctx, `SELECT r.id, o.status, r.proposed_by FROM payments.payment_run r
		JOIN platform.operation o ON o.tenant_id = r.tenant_id AND o.id = r.operation_id
		WHERE r.id = $1 AND r.status = 'proposing' AND o.status IN ('queued', 'cancelled') FOR UPDATE OF o, r`, run).Scan(&j.RunID, &status, &j.ProposedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("payments: find the proposal job of run %s: %w", run, err)
	}
	return e.proposer(ctx, tx, j, status)
}

// proposer completes a locked job with whether its operation was
// cancelled and the kind of principal its proposer is.
func (e *Endpoints) proposer(ctx context.Context, tx pgx.Tx, j Job, status string) (Job, bool, error) {
	j.Cancelled = status == operations.Cancelled
	p, err := e.Actors.Actor(ctx, tx, j.ProposedBy)
	if err != nil {
		return Job{}, false, fmt.Errorf("payments: read the proposer of run %s: %w", j.RunID, err)
	}
	j.ProposerKind = p.Kind
	return j, true, nil
}

// Process does a job as its proposer: it builds the run, or fails it when
// its operation was cancelled. A problem while building fails the run and
// its operation; other errors leave both for another attempt.
func (e *Endpoints) Process(ctx context.Context, j Job) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := load(ctx, tx, j.RunID, false)
	if err != nil {
		return err
	}
	if j.Cancelled {
		return fail(ctx, tx, p, r, nil)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT propose`); err != nil {
		return fmt.Errorf("payments: savepoint before building run %s: %w", r.ID, err)
	}
	_, err = e.build(ctx, tx, p, r)
	var why *problem.Problem
	if !errors.As(err, &why) || why.Status >= 500 {
		return err
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT propose`); err != nil {
		return fmt.Errorf("payments: roll back the build of run %s: %w", r.ID, err)
	}
	audit.Discard(ctx)
	return fail(ctx, tx, p, r, why)
}
