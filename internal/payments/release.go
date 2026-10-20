package payments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
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
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	p, err := paymentOf(ctx, tx, r.ID, pay)
	if err != nil {
		return false, err
	}
	if p.JournalEntryID != nil || p.Excluded {
		return false, nil
	}
	if p.Kind == KindDownPayment {
		if err := e.postDownPayment(ctx, tx, r, p); err != nil {
			return false, err
		}
		return true, report(ctx, tx, r)
	}
	items := itemRefs(p)
	unpayable, err := e.OpenItems.Unpayable(ctx, items)
	if err != nil {
		return false, err
	}
	if len(unpayable) > 0 {
		return false, e.dropPayment(ctx, tx, actor, r, p, unpayable)
	}
	hb, err := payingBank(ctx, tx, r, p)
	if err != nil {
		return false, err
	}
	text := "Payment run " + r.DocumentNumber
	entry, err := e.Postings.Settle(ctx, ledger.Settlement{LegalEntityID: r.LegalEntityID, DocumentType: DocumentType, PostingDate: r.RunDate,
		Currency: r.Currency, BankAccountID: hb.GLAccountID, Reference: &p.EndToEndID, HeaderText: &text, SourceContext: "payments",
		SourceDocumentID: p.ID, Items: items, Discounts: discounts(p), Withholding: withholding(p), ReservedFor: r.ID})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment SET journal_entry_year = $2, journal_entry_id = $3 WHERE id = $1`, p.ID, entry.FiscalYear, entry.ID); err != nil {
		return false, fmt.Errorf("payments: record the posting of payment %s: %w", p.ID, err)
	}
	return true, report(ctx, tx, r)
}

// report records the release's progress on its operation: the payments
// posted out of those the run makes.
func report(ctx context.Context, tx pgx.Tx, r Run) error {
	var done, total int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE journal_entry_id IS NOT NULL), count(*) FROM payments.payment p
		WHERE payment_run_id = $1 AND NOT `+dropped, r.ID).Scan(&done, &total); err != nil {
		return fmt.Errorf("payments: count the posted payments of run %s: %w", r.ID, err)
	}
	return operations.Report(ctx, *r.ReleaseOpID, done, total)
}

// withholding is the withholding tax a payment deducts from its items, in
// their order (itemRefs).
func withholding(p Payment) []money.Minor {
	out := make([]money.Minor, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Withholding
	}
	return out
}

// discounts are the cash discounts a payment takes on its items, in their
// order (itemRefs).
func discounts(p Payment) []money.Minor {
	out := make([]money.Minor, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Discount
	}
	return out
}

// itemRefs are the open items a payment settles.
func itemRefs(p Payment) []ledger.ItemRef {
	items := make([]ledger.ItemRef, len(p.Items))
	for i, it := range p.Items {
		items[i] = ledger.ItemRef{FiscalYear: it.FiscalYear, EntryID: it.EntryID, LineNo: it.LineNo}
	}
	return items
}

// dropPayment takes a payment that can never post out of its releasing
// run: an item it settles was reversed, or is no longer open, since the
// proposal (neither changes back). Its supplier is excluded with the
// reason, its items are freed here and in the ledger, the run's total no
// longer counts it, and the release goes on with the other payments, so a
// run is never stuck releasing on a payment no retry can post. The run is
// read FOR UPDATE by the release step, and a dropped payment is skipped by
// every later step, so it is dropped once.
func (e *Endpoints) dropPayment(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, r Run, p Payment, unpayable map[ledger.ItemRef]string) error {
	reason := ItemNotOpen
	for _, why := range unpayable {
		if why == ledger.ItemReversed {
			reason = ItemReversed
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment_exclusion (tenant_id, payment_run_id, supplier_id, reason_code, amount)
		VALUES ($1, $2, $3, $4, $5)`, actor.TenantID, r.ID, p.SupplierID, reason, p.Amount); err != nil {
		return fmt.Errorf("payments: exclude supplier %s from run %s: %w", p.SupplierID, r.ID, err)
	}
	freed, err := tx.Exec(ctx, `UPDATE payments.payment_item SET is_active = false WHERE payment_id = $1 AND is_active`, p.ID)
	if err != nil {
		return fmt.Errorf("payments: free the items of payment %s: %w", p.ID, err)
	}
	if err := e.Postings.Release(ctx, r.ID, itemRefs(p)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET total_amount = total_amount - $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		r.ID, p.Amount, actor.ID); err != nil {
		return fmt.Errorf("payments: take payment %s out of the total of run %s: %w", p.ID, r.ID, err)
	}
	if err := report(ctx, tx, r); err != nil {
		return err
	}
	return record(ctx, "payments.payment_run.drop_payment", r.ID, map[string]any{"payment_id": p.ID, "supplier_id": p.SupplierID,
		"reason": reason, "freed_items": freed.RowsAffected()},
		runEvent(r, "payment_excluded", map[string]any{"payment_id": p.ID, "supplier_id": p.SupplierID, "reason_code": reason,
			"amount": money.EventAmount(p.Amount, r.Currency, r.Exponent), "released_item_count": freed.RowsAffected()}))
}

// paymentOf reads one payment of the run, dropped or not.
func paymentOf(ctx context.Context, tx pgx.Tx, run, pay uuid.UUID) (Payment, error) {
	p, err := scanPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id = $1 AND p.payment_run_id = $2`, pay, run))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, fmt.Errorf("payments: run %s has no payment %s", run, pay)
	}
	if err != nil {
		return Payment{}, fmt.Errorf("payments: read payment %s of run %s: %w", pay, run, err)
	}
	return p, nil
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
	if len(pays) == 0 {
		return Run{}, problem.Conflict("payments.nothing_to_pay", "The release dropped every payment of the run, whose items can no longer be paid; reject it.")
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
	files, err := e.createFiles(ctx, tx, p, r, pays)
	if err != nil {
		return Run{}, err
	}
	released, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	// bank_file_id, the first file, stays for clients from before a run
	// had a file per house bank and method (T45c).
	ids := make([]uuid.UUID, len(files))
	events := make([]outbox.Event, 0, len(files)+1)
	for i, f := range files {
		ids[i] = f.ID
		events = append(events, fileEvent(f, "bank_file_created", map[string]any{"format": f.Format, "sha256": hex.EncodeToString(f.SHA256), "status": f.Status,
			"house_bank_id": f.HouseBankID, "payment_method": f.Method}))
	}
	if _, err := operations.Succeed(ctx, *r.ReleaseOpID, "/v1/payment-runs/"+r.ID.String(), map[string]any{"payment_run_id": r.ID, "status": Released,
		"payment_count": len(pays), "bank_file_id": ids[0], "bank_file_ids": ids,
		"total_amount": money.EventAmount(released.Total, released.Currency, released.Exponent)}); err != nil {
		return Run{}, err
	}
	return released, record(ctx, "payments.payment_run.complete", r.ID, map[string]any{"payments": len(pays), "bank_file_ids": ids},
		append(events, runEvent(released, "payment_run_released", map[string]any{"house_bank_id": released.HouseBankID, "run_date": released.RunDate.Format(time.DateOnly),
			"total_amount": money.EventAmount(released.Total, released.Currency, released.Exponent), "payment_count": len(pays), "workflow_request_id": nil,
			"released_at": released.DecidedAt.UTC().Format(time.RFC3339Nano)}))...)
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
	if err := e.recheckDiscounts(r, pays); err != nil {
		return err
	}
	if err := e.recheckRequests(ctx, r, pays); err != nil {
		return err
	}
	return e.recheckItems(ctx, r, pays)
}

// recheckDiscounts refuses the release of a proposed run after the last day
// a payment's cash discounts are earned (409 payments.discount_lapsed):
// paying later would take a discount the supplier no longer grants; the
// run is rejected and proposed again, at the discount then earned. Days
// are compared in UTC. A releasing run being resumed is not refused: some
// of its payments may have posted already.
func (e *Endpoints) recheckDiscounts(r Run, pays []Payment) error {
	if r.Status != Proposed {
		return nil
	}
	today := clock.Now(e.Now).UTC().Truncate(24 * time.Hour)
	for _, pay := range pays {
		if pay.DiscountUntil != nil && today.After(*pay.DiscountUntil) {
			return problem.Conflict("payments.discount_lapsed", fmt.Sprintf("The payment to supplier %s takes cash discount earned until %s; reject the run and propose again.",
				pay.SupplierID, pay.DiscountUntil.Format(time.DateOnly)))
		}
	}
	return nil
}

// unpayableText says why an item can no longer be paid (OpenItems.Unpayable).
var unpayableText = map[string]string{ledger.ItemReversed: "reversed", ledger.ItemNotOpen: "no longer open"}

// recheckItems refuses the release of a proposed run with an item that
// can no longer be paid: reversed, or no longer open, since the proposal.
// Nothing is posted yet, so the run is rejected and proposed again. A
// releasing run being resumed is not refused: its posted payments cleared
// their items, and its release drops a payment that can never post
// (dropPayment), where a refusal would leave the run releasing for good.
func (e *Endpoints) recheckItems(ctx context.Context, r Run, pays []Payment) error {
	if r.Status != Proposed {
		return nil
	}
	var items []ledger.ItemRef
	for _, pay := range pays {
		items = append(items, itemRefs(pay)...)
	}
	unpayable, err := e.OpenItems.Unpayable(ctx, items)
	if err != nil {
		return err
	}
	for _, pay := range pays {
		for _, it := range itemRefs(pay) {
			if why, ok := unpayable[it]; ok {
				return problem.Conflict("payments.proposal_outdated", fmt.Sprintf("An item the payment to supplier %s settles (%d/%s line %d) is %s since the proposal; reject the run and propose again.",
					pay.SupplierID, it.FiscalYear, it.EntryID, it.LineNo, unpayableText[why]))
			}
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
	HouseBankID   uuid.UUID // whose account it pays from: its own, or its run's
	Method        *string   // the payment method of its payments, nil in a run proposed for one bank (T45c)
	Format        string
	ObjectRef     string
	SHA256        []byte
	Status        string
	Version       int32
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     time.Time
}

const fileSelect = `SELECT f.id, f.payment_run_id, r.legal_entity_id, coalesce(f.house_bank_id, r.house_bank_id), f.payment_method, f.format, f.object_ref,
		f.sha256, f.status, f.version, f.created_at, f.created_by, f.updated_at
	FROM payments.bank_file f JOIN payments.payment_run r ON r.tenant_id = f.tenant_id AND r.id = f.payment_run_id`

func scanFile(row pgx.Row) (BankFile, error) {
	var f BankFile
	err := row.Scan(&f.ID, &f.RunID, &f.LegalEntityID, &f.HouseBankID, &f.Method, &f.Format, &f.ObjectRef, &f.SHA256, &f.Status, &f.Version, &f.CreatedAt,
		&f.CreatedBy, &f.UpdatedAt)
	return f, errs.Wrapf(err, "scan bank file")
}

// fileGroup is what one bank file pays: the payments of one house bank and
// payment method, in end-to-end id order. v is how the payments name them:
// nil for a run proposed for one bank, whose one file has neither.
type fileGroup struct {
	bank houseBank
	v    via
	pays []Payment
}

// key is the file's object key: the run's for its one file in a run
// proposed for one bank, as before T45c, else the run's with the bank and
// method, one per group.
func (g fileGroup) key(tenant uuid.UUID, r Run) string {
	name := r.ID.String()
	if g.v.method != nil {
		name += "-" + g.bank.Code + "-" + *g.v.method
	}
	return blobstore.Key(tenant, blobstore.KindBankFiles, name+".xml")
}

// maxMessageID is the longest message id a file of a run with a file per
// group takes: pain.001 holds 35 characters, and its payment information id
// adds "-1" (bankfile.Pain001).
const maxMessageID = 33

// messageID is the file's pain.001 message id, unique per file: the run's,
// with the bank and method when the run has a file per group. bankfile
// clips ids to 35 characters, which would cut the bank and method that tell
// a run's files apart, so a longer one keeps them: the run's part is
// shortened and followed by a hash of it, which keeps runs apart.
func (g fileGroup) messageID(entityCode string, r Run) string {
	id := entityCode + "-" + r.DocumentNumber
	if g.v.method == nil {
		return id
	}
	suffix := "-" + g.bank.Code + "-" + *g.v.method
	if len(id)+len(suffix) <= maxMessageID {
		return id + suffix
	}
	sum := sha256.Sum256([]byte(id))
	hash := "-" + hex.EncodeToString(sum[:4])
	return id[:maxMessageID-len(hash)-len(suffix)] + hash + suffix
}

// fileGroups splits a run's payments by the house bank and method that pay
// them, in the order of their first payments.
func fileGroups(ctx context.Context, tx pgx.Tx, r Run, pays []Payment) ([]fileGroup, error) {
	type key struct {
		bank   uuid.UUID // zero for the run's bank
		method string
	}
	var groups []fileGroup
	at := map[key]int{}
	for _, pay := range pays {
		k := key{method: methodOf(pay.Method)}
		if pay.HouseBankID != nil {
			k.bank = *pay.HouseBankID
		}
		i, ok := at[k]
		if !ok {
			hb, err := payingBank(ctx, tx, r, pay)
			if err != nil {
				return nil, err
			}
			i, at[k] = len(groups), len(groups)
			groups = append(groups, fileGroup{bank: hb, v: via{bank: pay.HouseBankID, method: pay.Method}})
		}
		groups[i].pays = append(groups[i].pays, pay)
	}
	return groups, nil
}

// createFiles writes one bank file per house bank and payment method of
// the run's payments (ADR-0038), in the order of their first payments.
func (e *Endpoints) createFiles(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, pays []Payment) ([]BankFile, error) {
	groups, err := fileGroups(ctx, tx, r, pays)
	if err != nil {
		return nil, err
	}
	files := make([]BankFile, len(groups))
	for i, g := range groups {
		if files[i], err = e.createFile(ctx, tx, p, r, g); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// createFile renders a group's pain.001 file and records it with its hash.
// The file is stamped with its creation time, which becomes the record's
// creation time, so it can be rendered again byte for byte. With object
// storage the file is kept under its key (fileGroup.key), and the record
// refers to it; without, the reference is the hash alone.
func (e *Endpoints) createFile(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, g fileGroup) (BankFile, error) {
	var created time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&created); err != nil {
		return BankFile{}, fmt.Errorf("payments: read the time of the bank file of run %s: %w", r.ID, err)
	}
	content, err := e.render(ctx, p.TenantID, r, g, created)
	if err != nil {
		return BankFile{}, err
	}
	var ref string
	if e.Objects != nil {
		ref = g.key(p.TenantID, r)
		if content, created, err = e.keep(ctx, p.TenantID, r, g, ref, content, created); err != nil {
			return BankFile{}, err
		}
	}
	sum := sha256.Sum256(content)
	if ref == "" {
		ref = "sha256:" + hex.EncodeToString(sum[:])
	}
	// The content is render's, or kept content keep compared with it: it
	// always reads back. Its message id is what a batch booking on the
	// bank's statement carries (T49e).
	t, _ := transferOf(content)
	id := uuid.Must(uuid.NewV7())
	f, err := scanFile(tx.QueryRow(ctx, `WITH f AS (
			INSERT INTO payments.bank_file (tenant_id, id, payment_run_id, format, object_ref, sha256, status, created_at, created_by, house_bank_id, payment_method,
				message_id)
			VALUES ($1, $2, $3, $4, $5, $6, 'created', $7, $8, $10, $11, $13) RETURNING *
		) SELECT f.id, f.payment_run_id, $9::uuid, $12::uuid, f.payment_method, f.format, f.object_ref, f.sha256, f.status, f.version, f.created_at,
			f.created_by, f.updated_at FROM f`,
		p.TenantID, id, r.ID, Format, ref, sum[:], created, p.ID, r.LegalEntityID, g.v.bank, g.v.method, g.bank.ID, t.MessageID))
	if err != nil {
		return BankFile{}, fmt.Errorf("payments: record the bank file of run %s: %w", r.ID, err)
	}
	return f, nil
}

// keep stores the run's bank file under its key and returns the file
// recorded with its creation time. A release step run again after the
// store took the file and the step's transaction was lost (ADR-0012) finds
// the key taken: it records the stored file, once it is this run's file,
// so a retry neither writes a second object nor records another file than
// the one kept. The stored file is the run's when it pays what the run
// pays now (sameTransfer): names and remittance texts may have been
// corrected since, which must not refuse a resumed release for good,
// while a file that pays other amounts or accounts is refused (409
// payments.bank_file_changed). The store not answering is the machinery's
// failure (503), which the release retries.
func (e *Endpoints) keep(ctx context.Context, tenant uuid.UUID, r Run, g fileGroup, key string, content []byte,
	created time.Time) ([]byte, time.Time, error) {
	err := e.Objects.Put(ctx, key, content, blobstore.PutOptions{ContentType: "application/xml"})
	if err == nil {
		return content, created, nil
	}
	if !errors.Is(err, blobstore.ErrExists) {
		return nil, time.Time{}, problem.Unavailable("The file store did not take the bank file; the release tries again.", 30).WithCause(err)
	}
	stored, _, err := e.Objects.Get(ctx, key)
	if err != nil {
		return nil, time.Time{}, problem.Unavailable("The file store did not return the bank file it holds; the release tries again.", 30).WithCause(err)
	}
	changed := problem.Conflict("payments.bank_file_changed", "The file store holds another bank file under run "+r.DocumentNumber+"'s key; it is not recorded.")
	held, err := transferOf(stored)
	if err != nil {
		return nil, time.Time{}, changed
	}
	at, err := time.ParseInLocation("2006-01-02T15:04:05", held.Created, time.UTC)
	if err != nil {
		return nil, time.Time{}, changed
	}
	again, err := e.render(ctx, tenant, r, g, at)
	if err != nil {
		return nil, time.Time{}, err
	}
	// render's output is bankfile.Pain001's, which always reads back: the
	// error can only come from stored content, checked above.
	want, _ := transferOf(again)
	if !sameTransfer(held, want) {
		return nil, time.Time{}, changed
	}
	return stored, at, nil
}

// transfer is what a pain.001 file pays: its message, creation time, the
// debtor account, and each payment's end-to-end id, amount and creditor
// account.
type transfer struct {
	MessageID string `xml:"CstmrCdtTrfInitn>GrpHdr>MsgId"`
	Created   string `xml:"CstmrCdtTrfInitn>GrpHdr>CreDtTm"`
	Debtor    string `xml:"CstmrCdtTrfInitn>PmtInf>DbtrAcct>Id>IBAN"`
	Payments  []struct {
		EndToEndID string `xml:"PmtId>EndToEndId"`
		Amount     struct {
			Value    string `xml:",chardata"`
			Currency string `xml:"Ccy,attr"`
		} `xml:"Amt>InstdAmt"`
		IBAN  string `xml:"CdtrAcct>Id>IBAN"`
		Other string `xml:"CdtrAcct>Id>Othr>Id"`
	} `xml:"CstmrCdtTrfInitn>PmtInf>CdtTrfTxInf"`
}

func transferOf(content []byte) (transfer, error) {
	var t transfer
	err := xml.Unmarshal(content, &t)
	return t, errs.Wrapf(err, "read the bank file")
}

// sameTransfer reports whether two files pay the same: same message, the
// same debtor account, and the same payments to the same accounts.
func sameTransfer(a, b transfer) bool {
	return a.MessageID == b.MessageID && a.Debtor == b.Debtor && slices.Equal(a.Payments, b.Payments)
}

// render writes a group's payments as a pain.001.001.03 credit transfer
// from its house bank, in end-to-end id order, each telling the supplier
// the references of the items it settles.
func (e *Endpoints) render(ctx context.Context, tenant uuid.UUID, r Run, g fileGroup, created time.Time) ([]byte, error) {
	hb, pays := g.bank, g.pays
	// errNoEntity stays an error: the release activity leaves it to its
	// retries (nothing is written), and the download answers 500.
	le, err := e.entityOf(ctx, r)
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
	b := bankfile.Batch{MessageID: g.messageID(le.Code, r), InitiatorName: le.Name, DebtorName: le.Name, DebtorIBAN: iban, DebtorBIC: hb.BIC,
		Currency: r.Currency, Decimals: r.Exponent, ExecutionDate: r.RunDate.Format(time.DateOnly), Created: created}
	for _, pay := range pays {
		remittance, err := e.remittance(ctx, pay, refs)
		if err != nil {
			return nil, err
		}
		d := details[pay.BankAccountID]
		b.Payments = append(b.Payments, bankfile.Payment{EndToEndID: pay.EndToEndID, CreditorName: d.Holder, CreditorAccount: d.Account,
			Amount: pay.Amount, Remittance: remittance})
	}
	content, err := bankfile.Pain001(b)
	if err != nil {
		return nil, problem.Unprocessable("payments.bank_file_invalid", "The bank file cannot be written: "+err.Error()+".")
	}
	return content, nil
}

// remittance is a payment's remittance text: the references of the
// entries its items are on, or a down payment's request (T46c).
func (e *Endpoints) remittance(ctx context.Context, pay Payment, refs map[uuid.UUID]string) (string, error) {
	if pay.Kind == KindDownPayment {
		return e.requestRemittance(ctx, pay)
	}
	var remittance []string
	for _, it := range pay.Items {
		remittance = append(remittance, refs[it.EntryID])
	}
	return strings.Join(remittance, " "), nil
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
	pays, err := paymentsOf(ctx, tx, r.ID)
	if err != nil {
		return nil, err
	}
	groups, err := fileGroups(ctx, tx, r, pays)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.bank.ID == f.HouseBankID && methodOf(g.v.method) == methodOf(f.Method) {
			return e.render(ctx, tenant, r, g, f.CreatedAt)
		}
	}
	return nil, fmt.Errorf("payments: run %s pays nothing through bank file %s", r.ID, f.ID)
}
