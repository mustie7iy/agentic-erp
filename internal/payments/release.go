package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ReleaseRun is the F110 release, by a person other than the proposer:
// every supplier is checked again on the run date, each payment is posted
// with the entry that clears its items, and the bank file is produced. A
// supplier that can no longer be paid stops the whole release, so a run is
// posted completely or not at all.
func (e *Endpoints) ReleaseRun(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, comment string) (Run, error) {
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
	if err := requireProposed(r, "released"); err != nil {
		return Run{}, err
	}
	if err := decider(r, p); err != nil {
		return Run{}, err
	}
	pays, err := paymentsOf(ctx, tx, r.ID)
	if err != nil {
		return Run{}, err
	}
	if len(pays) == 0 {
		return Run{}, problem.Conflict("payments.nothing_to_pay", "The run has no payments; reject it.")
	}
	hb, _, err := loadHouseBank(ctx, tx, r.HouseBankID)
	if err != nil {
		return Run{}, err
	}
	if err := e.recheck(ctx, r, pays); err != nil {
		return Run{}, err
	}
	text := "Payment run " + r.DocumentNumber
	for _, pay := range pays {
		items := make([]ledger.ItemRef, len(pay.Items))
		for i, it := range pay.Items {
			items[i] = ledger.ItemRef{FiscalYear: it.FiscalYear, EntryID: it.EntryID, LineNo: it.LineNo}
		}
		entry, err := e.Postings.Settle(ctx, ledger.Settlement{LegalEntityID: r.LegalEntityID, DocumentType: DocumentType, PostingDate: r.RunDate,
			Currency: r.Currency, BankAccountID: hb.GLAccountID, Reference: &pay.EndToEndID, HeaderText: &text, SourceContext: "payments",
			SourceDocumentID: pay.ID, Items: items})
		if err != nil {
			return Run{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payments.payment SET journal_entry_year = $2, journal_entry_id = $3 WHERE id = $1`,
			pay.ID, entry.FiscalYear, entry.ID); err != nil {
			return Run{}, err
		}
	}
	file, err := e.createFile(ctx, tx, r, hb, pays)
	if err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'released', decided_by = $2, decided_at = now(), version = version + 1,
		updated_by = $2 WHERE id = $1`, r.ID, p.ID); err != nil {
		return Run{}, err
	}
	released, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	return released, record(ctx, "payments.payment_run.release", r.ID, map[string]any{"comment": comment, "payments": len(pays), "bank_file_id": file.ID},
		fileEvent(file, "bank_file_created", map[string]any{"format": file.Format, "sha256": hex.EncodeToString(file.SHA256), "status": file.Status}),
		runEvent(released, "payment_run_released", map[string]any{"house_bank_id": released.HouseBankID, "run_date": released.RunDate.Format(time.DateOnly),
			"total_amount": amount(released.Total, released.Currency, released.Exponent), "payment_count": len(pays), "workflow_request_id": nil,
			"released_at": released.DecidedAt.UTC().Format(time.RFC3339Nano)}))
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
// and frees its items for later runs.
func RejectRun(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Run, error) {
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
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'rejected', decided_by = $2, decided_at = now(), version = version + 1,
		updated_by = $2 WHERE id = $1`, r.ID, p.ID); err != nil {
		return Run{}, err
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
	return f, err
}

// createFile renders the run's pain.001 file and records it with its hash.
// The file is stamped with the transaction's time, which becomes the
// record's creation time, so it can be rendered again byte for byte.
func (e *Endpoints) createFile(ctx context.Context, tx pgx.Tx, r Run, hb houseBank, pays []Payment) (BankFile, error) {
	p, _ := reqctx.PrincipalFrom(ctx) // ReleaseRun runs in a request
	var created time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&created); err != nil {
		return BankFile{}, err
	}
	content, err := e.render(ctx, r, hb, pays, created)
	if err != nil {
		return BankFile{}, err
	}
	sum := sha256.Sum256(content)
	id := uuid.Must(uuid.NewV7())
	return scanFile(tx.QueryRow(ctx, `WITH f AS (
			INSERT INTO payments.bank_file (tenant_id, id, payment_run_id, format, object_ref, sha256, status, created_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, 'created', $7, $8) RETURNING *
		) SELECT f.id, f.payment_run_id, $9::uuid, f.format, f.object_ref, f.sha256, f.status, f.version, f.created_at, f.created_by, f.updated_at FROM f`,
		p.TenantID, id, r.ID, Format, "sha256:"+hex.EncodeToString(sum[:]), sum[:], created, p.ID, r.LegalEntityID))
}

// render writes the run's payments as a pain.001.001.03 credit transfer
// from the house bank, in end-to-end id order, each telling the supplier
// the references of the items it settles.
func (e *Endpoints) render(ctx context.Context, r Run, hb houseBank, pays []Payment, created time.Time) ([]byte, error) {
	p, _ := reqctx.PrincipalFrom(ctx) // callers run in a request
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID)
	if err != nil {
		return nil, err
	}
	iban, err := e.Keys.Open(p.TenantID, hb.IBANCipher)
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
			Amount: money.Minor(pay.Amount), Remittance: strings.Join(remittance, " ")})
	}
	content, err := bankfile.Pain001(b)
	if err != nil {
		return nil, problem.Unprocessable("payments.bank_file_invalid", "The bank file cannot be written: "+err.Error()+".")
	}
	return content, nil
}

// fileOf renders a bank file again from its run.
func (e *Endpoints) fileOf(ctx context.Context, tx pgx.Tx, f BankFile) ([]byte, error) {
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
	return e.render(ctx, r, hb, pays, f.CreatedAt)
}
