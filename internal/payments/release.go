package payments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// StartRelease is the F110 release, by a person other than the proposer,
// as an operation (ADR-0012): every supplier is checked again on the run
// date here, then the run moves to releasing and the operation is queued;
// the release workflow posts each payment with the entry that clears its
// items, one transaction per payment, and writes the bank file, after which
// the run is released. A run whose release operation failed or was
// cancelled (nothing or
// some payments posted) is released again by the same call, which resumes
// the work: payments already posted are skipped.
func (e *Endpoints) StartRelease(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, comment string) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	r, err := get(ctx, tx, id, true)
	if err != nil {
		return operations.Operation{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return operations.Operation{}, err
	}
	if err := releasable(ctx, tx, r); err != nil {
		return operations.Operation{}, err
	}
	if err := decider(r, p); err != nil {
		return operations.Operation{}, err
	}
	pays, err := paymentsOf(ctx, tx, r.ID)
	if err != nil {
		return operations.Operation{}, err
	}
	if len(pays) == 0 {
		return operations.Operation{}, problem.Conflict("payments.nothing_to_pay", "The run has no payments; reject it.")
	}
	if err := e.recheck(ctx, r, pays); err != nil {
		return operations.Operation{}, err
	}
	op, err := operations.StartFor(ctx, ReleaseKind, map[string]any{"payment_run_id": r.ID, "comment": comment}, "/v1/payment-runs/"+r.ID.String())
	if err != nil {
		return operations.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'releasing', decided_by = $2, decided_at = coalesce(decided_at, now()), release_operation_id = $3,
		version = version + 1, updated_by = $2 WHERE id = $1`, r.ID, p.ID, op.ID); err != nil {
		return operations.Operation{}, fmt.Errorf("payments: release run %s: %w", r.ID, err)
	}
	return op, record(ctx, "payments.payment_run.release", r.ID, map[string]any{"comment": comment, "payments": len(pays), "operation_id": op.ID, "resumed": r.Status == Releasing})
}

// releasable: a proposed run, or a releasing one whose release operation
// failed or was cancelled (the work resumes); a release under way or done
// is refused.
func releasable(ctx context.Context, tx pgx.Tx, r Run) error {
	switch {
	case r.Status == Proposed:
		return nil
	case r.Status != Releasing:
		return problem.Conflict("payments.invalid_status", "Only a proposed payment run can be released; this one is "+r.Status+".")
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM platform.operation WHERE id = $1`, r.ReleaseOpID).Scan(&status); err != nil {
		return fmt.Errorf("payments: read release operation %s of run %s: %w", r.ReleaseOpID, r.ID, err)
	}
	if status != operations.Failed && status != operations.Cancelled {
		return problem.Conflict("payments.release_in_progress", "The run is being released (operation "+r.ReleaseOpID.String()+" is "+status+").")
	}
	return nil
}

// Releaser is who posts a run's payments: the person who released it. The
// run is read FOR UPDATE, so the release activities of a run, even those of
// two overlapping workflows, run one at a time and each sees what the one
// before it committed. A released run is returned too: an activity run
// again after its completion was lost finds its work done and does nothing
// (ADR-0012).
func (e *Endpoints) Releaser(ctx context.Context, tx pgx.Tx, run uuid.UUID) (reqctx.Principal, Run, error) {
	r, err := load(ctx, tx, run, true)
	if err != nil {
		return reqctx.Principal{}, Run{}, err
	}
	if r.Status != Releasing && r.Status != Released || r.DecidedBy == nil {
		return reqctx.Principal{}, Run{}, fmt.Errorf("payments: run %s is %s, not releasing", run, r.Status)
	}
	p, err := e.Actors.Actor(ctx, tx, *r.DecidedBy)
	if err != nil {
		return reqctx.Principal{}, Run{}, fmt.Errorf("payments: read releaser %s of run %s: %w", *r.DecidedBy, run, err)
	}
	return p, r, nil
}

// PaymentsToPost lists the run's payments without a posting yet, in
// end-to-end id order, and how many there are in all.
func PaymentsToPost(ctx context.Context, tx pgx.Tx, run uuid.UUID) (todo []uuid.UUID, total int, err error) {
	pays, err := paymentsOf(ctx, tx, run)
	if err != nil {
		return nil, 0, err
	}
	for _, pay := range pays {
		if pay.JournalEntryID == nil {
			todo = append(todo, pay.ID)
		}
	}
	return todo, len(pays), nil
}

// PostPayment posts one payment of a releasing run: the entry that clears
// its items, as the releaser, in the request's transaction; a payment
// already posted is left alone (posted = false), so a retry posts nothing
// twice. Progress is recorded on the release operation.
func (e *Endpoints) PostPayment(ctx context.Context, r Run, pay uuid.UUID) (posted bool, err error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	p, err := paymentOf(ctx, tx, r.ID, pay)
	if err != nil {
		return false, err
	}
	if p.JournalEntryID != nil {
		return false, nil
	}
	items := make([]ledger.ItemRef, len(p.Items))
	for i, it := range p.Items {
		items[i] = ledger.ItemRef{FiscalYear: it.FiscalYear, EntryID: it.EntryID, LineNo: it.LineNo}
	}
	hb, _, err := loadHouseBank(ctx, tx, r.HouseBankID)
	if err != nil {
		return false, err
	}
	text := "Payment run " + r.DocumentNumber
	entry, err := e.Postings.Settle(ctx, ledger.Settlement{LegalEntityID: r.LegalEntityID, DocumentType: DocumentType, PostingDate: r.RunDate,
		Currency: r.Currency, BankAccountID: hb.GLAccountID, Reference: &p.EndToEndID, HeaderText: &text, SourceContext: "payments",
		SourceDocumentID: p.ID, Items: items, ReservedFor: r.ID})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment SET journal_entry_year = $2, journal_entry_id = $3 WHERE id = $1`, p.ID, entry.FiscalYear, entry.ID); err != nil {
		return false, fmt.Errorf("payments: record the posting of payment %s: %w", p.ID, err)
	}
	var done, total int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE journal_entry_id IS NOT NULL), count(*) FROM payments.payment WHERE payment_run_id = $1`, r.ID).Scan(&done, &total); err != nil {
		return false, fmt.Errorf("payments: count the posted payments of run %s: %w", r.ID, err)
	}
	return true, operations.Report(ctx, *r.ReleaseOpID, done, total)
}

// paymentOf reads one payment of the run.
func paymentOf(ctx context.Context, tx pgx.Tx, run, pay uuid.UUID) (Payment, error) {
	pays, err := paymentsOf(ctx, tx, run)
	if err != nil {
		return Payment{}, err
	}
	for _, p := range pays {
		if p.ID == pay {
			return p, nil
		}
	}
	return Payment{}, fmt.Errorf("payments: run %s has no payment %s", run, pay)
}

// FinishRelease ends a release whose payments are all posted: the bank file
// is produced, the run is released, the operation succeeds and the events
// go out, in the request's transaction. A run already released is left
// alone: the release was finished before, and its completion lost.
func (e *Endpoints) FinishRelease(ctx context.Context, r Run) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	if r.Status == Released {
		return r, nil
	}
	pays, err := paymentsOf(ctx, tx, r.ID)
	if err != nil {
		return Run{}, err
	}
	for _, pay := range pays {
		if pay.JournalEntryID == nil {
			return Run{}, fmt.Errorf("payments: run %s: payment %s is not posted yet", r.ID, pay.ID)
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'released', version = version + 1, updated_by = $2 WHERE id = $1 AND status = 'releasing'`, r.ID, p.ID)
	if err != nil {
		return Run{}, fmt.Errorf("payments: complete the release of run %s: %w", r.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return Run{}, fmt.Errorf("payments: complete the release of run %s: the run is no longer releasing", r.ID)
	}
	hb, _, err := loadHouseBank(ctx, tx, r.HouseBankID)
	if err != nil {
		return Run{}, err
	}
	file, err := e.createFile(ctx, tx, p, r, hb, pays)
	if err != nil {
		return Run{}, err
	}
	released, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	if _, err := operations.Succeed(ctx, *r.ReleaseOpID, "/v1/payment-runs/"+r.ID.String(), map[string]any{"payment_run_id": r.ID, "status": Released,
		"payment_count": len(pays), "bank_file_id": file.ID, "total_amount": money.EventAmount(released.Total, released.Currency, released.Exponent)}); err != nil {
		return Run{}, err
	}
	return released, record(ctx, "payments.payment_run.complete", r.ID, map[string]any{"payments": len(pays), "bank_file_id": file.ID},
		fileEvent(file, "bank_file_created", map[string]any{"format": file.Format, "sha256": hex.EncodeToString(file.SHA256), "status": file.Status}),
		runEvent(released, "payment_run_released", map[string]any{"house_bank_id": released.HouseBankID, "run_date": released.RunDate.Format(time.DateOnly),
			"total_amount": money.EventAmount(released.Total, released.Currency, released.Exponent), "payment_count": len(pays), "workflow_request_id": nil,
			"released_at": released.DecidedAt.UTC().Format(time.RFC3339Nano)}))
}

// FailRelease records a release that could not be done: the operation fails
// with the problem; a run with nothing posted goes back to proposed for
// another decision, one with payments posted stays releasing, to be
// resumed by :release once the cause is fixed (posted payments stand). A
// release operation that already succeeded or failed (the failure recorded
// before and its completion lost, or the run released) is left alone. One
// cancelled while the release went on keeps its status: a run with nothing
// posted still goes back to proposed, one with payments posted stays
// releasing, which :release resumes as it does a failed one.
func (e *Endpoints) FailRelease(ctx context.Context, r Run, why *problem.Problem) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	op, err := operations.Get(ctx, *r.ReleaseOpID)
	if err != nil {
		return Run{}, err
	}
	if op.Status == operations.Succeeded || op.Status == operations.Failed {
		return r, nil
	}
	var posted int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payments.payment WHERE payment_run_id = $1 AND journal_entry_id IS NOT NULL`, r.ID).Scan(&posted); err != nil {
		return Run{}, fmt.Errorf("payments: count the posted payments of run %s: %w", r.ID, err)
	}
	cancelled := op.Status == operations.Cancelled
	if cancelled && posted > 0 {
		return r, nil
	}
	if !cancelled {
		if _, err := operations.Fail(ctx, *r.ReleaseOpID, why); err != nil {
			return Run{}, err
		}
	}
	if posted == 0 {
		if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'proposed', decided_by = NULL, decided_at = NULL, version = version + 1, updated_by = $2 WHERE id = $1`, r.ID, p.ID); err != nil {
			return Run{}, fmt.Errorf("payments: return run %s to proposed: %w", r.ID, err)
		}
	}
	after, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	return after, record(ctx, "payments.payment_run.fail", r.ID, map[string]any{"stage": "release", "code": why.Code, "detail": why.Detail, "posted": posted, "status": after.Status,
		"cancelled": cancelled})
}

// recheck refuses a release when a supplier can no longer be paid as
// proposed: blocked since, bank details changed or about to, or held.
func (e *Endpoints) recheck(ctx context.Context, r Run, pays []Payment) error {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return err
	}
	suppliers := make([]uuid.UUID, len(pays))
	for i, pay := range pays {
		suppliers[i] = pay.SupplierID
	}
	payees, err := e.Payees.Payees(ctx, r.LegalEntityID, suppliers, r.RunDate)
	if err != nil {
		return err
	}
	for _, pay := range pays {
		payee, found := payees[pay.SupplierID]
		reason := exclusion(payee, found, r.RunDate, doc.Payments.BankChangeHoldDays)
		if reason == "" && *payee.BankAccountID != pay.BankAccountID {
			reason = "bank_account_changed"
		}
		if reason != "" {
			return problem.Conflict("payments.proposal_outdated", "Supplier "+pay.SupplierID.String()+" can no longer be paid as proposed ("+reason+
				"); reject the run and propose again.")
		}
	}
	return nil
}

// RejectRun rejects a proposed run, by a person other than the proposer,
// and frees its items for later runs, here and in the ledger.
func (e *Endpoints) RejectRun(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := get(ctx, tx, id, true)
	if err != nil {
		return Run{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Run{}, err
	}
	if err := requireProposed(r, "rejected"); err != nil {
		return Run{}, err
	}
	if err := decider(r, p); err != nil {
		return Run{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE payments.payment_item i SET is_active = false FROM payments.payment p
		WHERE p.tenant_id = i.tenant_id AND p.id = i.payment_id AND p.payment_run_id = $1 AND i.is_active`, r.ID)
	if err != nil {
		return Run{}, fmt.Errorf("payments: free the items of run %s: %w", r.ID, err)
	}
	if err := e.Postings.Release(ctx, r.ID, nil); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'rejected', decided_by = $2, decided_at = now(), version = version + 1,
		updated_by = $2 WHERE id = $1`, r.ID, p.ID); err != nil {
		return Run{}, fmt.Errorf("payments: reject run %s: %w", r.ID, err)
	}
	rejected, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	return rejected, record(ctx, "payments.payment_run.reject", r.ID, map[string]any{"reason": reason, "released_items": tag.RowsAffected()},
		runEvent(rejected, "payment_run_rejected", map[string]any{"workflow_request_id": nil, "released_item_count": tag.RowsAffected()}))
}

// BankFile is a row of payments.bank_file with its run's legal entity.
type BankFile struct {
	ID            uuid.UUID
	RunID         uuid.UUID
	LegalEntityID uuid.UUID
	Format        string
	ObjectRef     string
	SHA256        []byte
	Status        string
	Version       int32
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     time.Time
}

const fileSelect = `SELECT f.id, f.payment_run_id, r.legal_entity_id, f.format, f.object_ref, f.sha256, f.status, f.version, f.created_at,
		f.created_by, f.updated_at
	FROM payments.bank_file f JOIN payments.payment_run r ON r.tenant_id = f.tenant_id AND r.id = f.payment_run_id`

func scanFile(row pgx.Row) (BankFile, error) {
	var f BankFile
	err := row.Scan(&f.ID, &f.RunID, &f.LegalEntityID, &f.Format, &f.ObjectRef, &f.SHA256, &f.Status, &f.Version, &f.CreatedAt, &f.CreatedBy, &f.UpdatedAt)
	return f, errs.Wrapf(err, "scan bank file")
}

// createFile renders the run's pain.001 file and records it with its hash.
// The file is stamped with the transaction's time, which becomes the
// record's creation time, so it can be rendered again byte for byte. With
// object storage the file is kept under the bank file's id, and the record
// refers to it; without, the reference is the hash alone.
func (e *Endpoints) createFile(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, hb houseBank, pays []Payment) (BankFile, error) {
	var created time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&created); err != nil {
		return BankFile{}, fmt.Errorf("payments: read the time of the bank file of run %s: %w", r.ID, err)
	}
	content, err := e.render(ctx, p.TenantID, r, hb, pays, created)
	if err != nil {
		return BankFile{}, err
	}
	sum := sha256.Sum256(content)
	id := uuid.Must(uuid.NewV7())
	ref := "sha256:" + hex.EncodeToString(sum[:])
	if e.Objects != nil {
		ref = blobstore.Key(p.TenantID, blobstore.KindBankFiles, id.String()+".xml")
		if err := e.Objects.Put(ctx, ref, content, blobstore.PutOptions{ContentType: "application/xml"}); err != nil {
			return BankFile{}, problem.Unavailable("The file store did not take the bank file; release the run again.", 30).WithCause(err)
		}
	}
	f, err := scanFile(tx.QueryRow(ctx, `WITH f AS (
			INSERT INTO payments.bank_file (tenant_id, id, payment_run_id, format, object_ref, sha256, status, created_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, 'created', $7, $8) RETURNING *
		) SELECT f.id, f.payment_run_id, $9::uuid, f.format, f.object_ref, f.sha256, f.status, f.version, f.created_at, f.created_by, f.updated_at FROM f`,
		p.TenantID, id, r.ID, Format, ref, sum[:], created, p.ID, r.LegalEntityID))
	if err != nil {
		return BankFile{}, fmt.Errorf("payments: record the bank file of run %s: %w", r.ID, err)
	}
	return f, nil
}

// render writes the run's payments as a pain.001.001.03 credit transfer
// from the house bank, in end-to-end id order, each telling the supplier
// the references of the items it settles.
func (e *Endpoints) render(ctx context.Context, tenant uuid.UUID, r Run, hb houseBank, pays []Payment, created time.Time) ([]byte, error) {
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID)
	if err != nil {
		return nil, err
	}
	iban, err := e.Keys.Open(ctx, tenant, hb.IBANCipher)
	if err != nil {
		return nil, err
	}
	banks, entries := make([]uuid.UUID, len(pays)), []uuid.UUID{}
	for i, pay := range pays {
		banks[i] = pay.BankAccountID
		for _, it := range pay.Items {
			entries = append(entries, it.EntryID)
		}
	}
	details, err := e.Payees.BankDetails(ctx, banks)
	if err != nil {
		return nil, err
	}
	refs, err := e.OpenItems.References(ctx, entries)
	if err != nil {
		return nil, err
	}
	b := bankfile.Batch{MessageID: le.Code + "-" + r.DocumentNumber, InitiatorName: le.Name, DebtorName: le.Name, DebtorIBAN: iban, DebtorBIC: hb.BIC,
		Currency: r.Currency, Decimals: r.Exponent, ExecutionDate: r.RunDate.Format(time.DateOnly), Created: created}
	for _, pay := range pays {
		var remittance []string
		for _, it := range pay.Items {
			remittance = append(remittance, refs[it.EntryID])
		}
		d := details[pay.BankAccountID]
		b.Payments = append(b.Payments, bankfile.Payment{EndToEndID: pay.EndToEndID, CreditorName: d.Holder, CreditorAccount: d.Account,
			Amount: pay.Amount, Remittance: strings.Join(remittance, " ")})
	}
	content, err := bankfile.Pain001(b)
	if err != nil {
		return nil, problem.Unprocessable("payments.bank_file_invalid", "The bank file cannot be written: "+err.Error()+".")
	}
	return content, nil
}

// fileOf is the bank file's content: read from object storage when it was
// kept there, else rendered again from its run. Either way it must match
// the hash recorded at release (409 payments.bank_file_changed).
func (e *Endpoints) fileOf(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, f BankFile) ([]byte, error) {
	content, err := e.rendered(ctx, tx, tenant, f)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(content); !bytes.Equal(sum[:], f.SHA256) {
		return nil, problem.Conflict("payments.bank_file_changed",
			"The bank file no longer matches the hash recorded at release; it is not handed out.")
	}
	return content, nil
}

func (e *Endpoints) rendered(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, f BankFile) ([]byte, error) {
	if blobstore.IsKey(f.ObjectRef) {
		if e.Objects == nil {
			return nil, problem.Unavailable("Object storage is not configured on this server.", 0)
		}
		content, _, err := e.Objects.Get(ctx, f.ObjectRef)
		if err != nil {
			return nil, problem.Unavailable("The file store did not answer; try again.", 30).WithCause(err)
		}
		return content, nil
	}
	r, err := load(ctx, tx, f.RunID, false)
	if err != nil {
		return nil, err
	}
	hb, _, err := loadHouseBank(ctx, tx, r.HouseBankID)
	if err != nil {
		return nil, err
	}
	pays, err := paymentsOf(ctx, tx, r.ID)
	if err != nil {
		return nil, err
	}
	return e.render(ctx, tenant, r, hb, pays, f.CreatedAt)
}
