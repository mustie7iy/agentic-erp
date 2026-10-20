package payments

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/bankstatement"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Bank statements (ADR-0047, T49c) are imported from a bank's file for one
// house bank: the file's statements, each of the bank's account and
// currency, are kept with their lines. A file is imported once per house
// bank and content hash, so the same file again is the first import; a
// statement is imported once per key (bankstatement.Statement.Key), so
// another file with an imported statement is refused. Each statement's
// opening balance is compared with the closing balance of the bank's
// statement before it, by closing date: a difference imports it as Gap. A
// statement imported between two others (a missing one, or an older one
// imported later) becomes the next one's previous statement, which is
// compared again: its gap closes, or one opens. Imports of one house bank
// wait for each other.

// MaxStatementFile is the largest statement file imported, below the
// request limit (uow.MaxBody) so the form around it fits.
const MaxStatementFile = 4 << 20

// Bank statement statuses.
const (
	StatementImported   = "imported"
	StatementGap        = "gap"
	StatementReconciled = "reconciled"
)

func init() {
	problem.RegisterConstraint("bank_statement_key_once", func() *problem.Problem {
		return problem.Conflict("payments.statement_conflict", "A statement with the statement id, account and sequence of one in this file "+
			"was already imported, from another file or earlier in this one.")
	})
}

// Statement is a row of payments.bank_statement with its lines; amounts
// are minor units of Currency, whose minor-unit digits are Exponent.
type Statement struct {
	ID, HouseBankID, LegalEntityID uuid.UUID
	Format                         string
	FileSHA256                     []byte
	FilePosition                   int32
	MessageID                      *string
	Ref, Sequence, Account         string
	Currency                       string
	Exponent                       int
	Opening, Closing               money.Minor
	OpeningDate, ClosingDate       time.Time
	LineCount                      int32
	PreviousID                     *uuid.UUID
	Status                         string
	Version                        int32
	CreatedAt, UpdatedAt           time.Time
	CreatedBy, UpdatedBy           uuid.UUID
	Lines                          []StatementLine // read with the statement, not in lists
}

// StatementLine is a row of payments.bank_statement_line.
type StatementLine struct {
	LineNo                                         int32
	Amount                                         money.Minor
	Reversal                                       bool
	BookingDate, ValueDate                         time.Time
	TransactionCode, BankReference, OwnerReference string
	Text, Status                                   string
	Details                                        []LineDetail
}

// LineDetail is a transaction of a line, as stored in its details.
type LineDetail struct {
	Amount              money.Minor `json:"amount"`
	MessageID           string      `json:"message_id,omitempty"`
	PaymentInfoID       string      `json:"payment_information_id,omitempty"`
	InstructionID       string      `json:"instruction_id,omitempty"`
	EndToEndID          string      `json:"end_to_end_id,omitempty"`
	TransactionCode     string      `json:"transaction_code,omitempty"`
	CounterpartyName    string      `json:"counterparty_name,omitempty"`
	CounterpartyIBAN    string      `json:"counterparty_iban,omitempty"`
	CounterpartyAccount string      `json:"counterparty_account,omitempty"`
	CounterpartyBIC     string      `json:"counterparty_bic,omitempty"`
	Remittance          string      `json:"remittance,omitempty"`
}

// statementBank is what an import needs of its house bank.
type statementBank struct {
	ID, LegalEntityID uuid.UUID
	Currency          string
	IBAN              string
}

// statementFormats are the formats an upload names, and their parsers.
var statementFormats = map[string]func([]byte, currency.Exponents) ([]bankstatement.Statement, error){
	"camt.053": bankstatement.ParseCamt053,
	"mt940":    bankstatement.ParseMT940,
}

// ImportStatements imports the statements of a bank's file for the house
// bank. created is false when the house bank imported the same file
// before: its statements are returned as they are now.
func (e *Endpoints) ImportStatements(ctx context.Context, houseBank uuid.UUID, format string, file []byte) (_ []Statement, sum []byte, created bool, err error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	if p.Kind == reqctx.KindAgent { // agents read statements; people and integrations import them (ADR-0047)
		return nil, nil, false, problem.Forbidden("identity.agent_not_allowed", "Bank statements are imported by people and integrations; agents read them.")
	}
	parse, ok := statementFormats[format]
	if !ok {
		return nil, nil, false, problem.Unprocessable("request.validation_failed", "The format is not one statements are read from.",
			problem.FieldError{Field: "format", Code: "invalid", Message: "camt.053 or mt940"})
	}
	if len(file) == 0 || len(file) > MaxStatementFile {
		return nil, nil, false, problem.Unprocessable("request.validation_failed", "A statement file has 1 byte to 4 MB.",
			problem.FieldError{Field: "file", Code: "invalid_length", Message: "1 byte to 4 MB"})
	}
	bank, err := e.statementBank(ctx, tx, p.TenantID, houseBank)
	if err != nil {
		return nil, nil, false, err
	}
	digest := sha256.Sum256(file)
	sum = digest[:]
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('payments.bank_statement:' || $1::text, 0))`, houseBank); err != nil {
		return nil, nil, false, fmt.Errorf("payments: lock statements of house bank %s: %w", houseBank, err)
	}
	if before, err := fileStatements(ctx, tx, houseBank, sum); err != nil || len(before) > 0 {
		return before, sum, false, err
	}
	exps, err := currency.Load(ctx, tx)
	if err != nil {
		return nil, nil, false, fmt.Errorf("payments: import statements: %w", err)
	}
	parsed, err := parse(file, exps)
	if err != nil {
		return nil, nil, false, problem.Unprocessable("payments.statement_unreadable", "The file cannot be read as "+format+".",
			problem.FieldError{Field: "file", Code: "unreadable", Message: strings.TrimPrefix(err.Error(), bankstatement.ErrMalformed.Error()+": ")})
	}
	for i, s := range parsed {
		if err := importStatement(ctx, tx, p, bank, sum, int32(i+1), s, exps[s.Currency]); err != nil {
			return nil, nil, false, err
		}
	}
	out, err := fileStatements(ctx, tx, houseBank, sum)
	return out, sum, true, err
}

// statementBank reads the house bank, which must be in a legal entity the
// caller may act in, with its IBAN opened.
func (e *Endpoints) statementBank(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID) (statementBank, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return statementBank{}, err
	}
	b := statementBank{ID: id}
	var sealed []byte
	err = tx.QueryRow(ctx, `SELECT legal_entity_id, currency, iban_cipher FROM payments.house_bank WHERE id = $1`, id).Scan(&b.LegalEntityID, &b.Currency, &sealed)
	if err = authz.Visible(err, access, b.LegalEntityID, problem.ReferenceNotFound("house_bank_id", "house bank", "a house bank of a company you may access"),
		"payments: read house bank %s", id); err != nil {
		return statementBank{}, err
	}
	if b.IBAN, err = e.Keys.Open(ctx, tenant, sealed); err != nil {
		return statementBank{}, fmt.Errorf("payments: open the IBAN of house bank %s: %w", id, err)
	}
	return b, nil
}

// importStatement inserts one statement of the file at its position, after
// the bank's statement before it, and its lines, and closes the gap of the
// bank's statement after it that it was missing.
func importStatement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, bank statementBank, sum []byte, position int32, s bankstatement.Statement,
	exp int) error {
	if fieldcrypt.Normalize(s.Account.Identifier()) != fieldcrypt.Normalize(bank.IBAN) || s.Currency != bank.Currency {
		return problem.Unprocessable("payments.statement_not_of_bank", fmt.Sprintf("Statement %s is of account %s in %s; the house bank's account is in %s.",
			s.ID, s.Account.Identifier(), s.Currency, bank.Currency), problem.FieldError{Field: "file", Code: "other_account", Message: "a statement of the house bank's account"})
	}
	var prevID *uuid.UUID
	var prevClosing money.Minor
	err := tx.QueryRow(ctx, `SELECT id, closing_balance FROM payments.bank_statement WHERE house_bank_id = $1 AND closing_date <= $2
		ORDER BY closing_date DESC, id DESC LIMIT 1`, bank.ID, s.Opening.Date).Scan(&prevID, &prevClosing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("payments: previous statement of house bank %s: %w", bank.ID, err)
	}
	status := StatementImported
	if prevID != nil && prevClosing != s.Opening.Amount {
		status = StatementGap
	}
	id := uuid.Must(uuid.NewV7())
	var msg *string
	if s.MessageID != "" {
		msg = &s.MessageID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.bank_statement (tenant_id, id, house_bank_id, legal_entity_id, format, file_sha256, file_position,
			statement_key, message_id, statement_ref, sequence, account, currency, opening_balance, opening_date, closing_balance, closing_date,
			line_count, previous_statement_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $21)`,
		p.TenantID, id, bank.ID, bank.LegalEntityID, s.Format, sum, position, s.Key(), msg, s.ID, s.Sequence, s.Account.Identifier(), s.Currency,
		s.Opening.Amount, s.Opening.Date, s.Closing.Amount, s.Closing.Date, len(s.Entries), prevID, status, p.ID); err != nil {
		return fmt.Errorf("payments: insert statement %s of house bank %s: %w", s.ID, bank.ID, err)
	}
	if err := insertLines(ctx, tx, p, id, s.Entries); err != nil {
		return err
	}
	if err := recordStatement(ctx, id, bank, sum, s, exp, status, prevID); err != nil {
		return err
	}
	return relinkNext(ctx, tx, p, bank, id, s)
}

// insertLines inserts the statement's lines in the bank's order.
func insertLines(ctx context.Context, tx pgx.Tx, p reqctx.Principal, statement uuid.UUID, entries []bankstatement.Entry) error {
	n := len(entries)
	amounts, reversals, booked, valued := make([]int64, n), make([]bool, n), make([]time.Time, n), make([]time.Time, n)
	codes, bankRefs, ownerRefs, texts, details := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	for i, en := range entries {
		amounts[i], reversals[i], booked[i], valued[i] = int64(en.Amount), en.Reversal, en.BookingDate, en.ValueDate
		codes[i], bankRefs[i], ownerRefs[i], texts[i] = en.TransactionCode, en.BankReference, en.OwnerReference, en.Text
		b, _ := json.Marshal(lineDetails(en.Details)) // strings and integers always encode
		details[i] = string(b)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, is_reversal, booking_date, value_date,
			transaction_code, bank_reference, owner_reference, text, details, created_by, updated_by)
		SELECT $1, $2, l.n, l.amount, l.reversal, l.booked, l.valued, l.code, l.bank_ref, l.owner_ref, l.text, l.details, $3, $3
		FROM unnest($4::bigint[], $5::boolean[], $6::date[], $7::date[], $8::text[], $9::text[], $10::text[], $11::text[], $12::jsonb[])
			WITH ORDINALITY AS l(amount, reversal, booked, valued, code, bank_ref, owner_ref, text, details, n)`,
		p.TenantID, statement, p.ID, amounts, reversals, booked, valued, codes, bankRefs, ownerRefs, texts, details); err != nil {
		return fmt.Errorf("payments: insert lines of statement %s: %w", statement, err)
	}
	return nil
}

// lineDetails are the transactions as stored; never nil, so a line without
// any stores [].
func lineDetails(ds []bankstatement.Detail) []LineDetail {
	out := make([]LineDetail, len(ds))
	for i, d := range ds {
		out[i] = LineDetail{Amount: d.Amount, MessageID: d.MessageID, PaymentInfoID: d.PaymentInfoID, InstructionID: d.InstructionID,
			EndToEndID: d.EndToEndID, TransactionCode: d.TransactionCode, CounterpartyName: d.Counterparty.Name, CounterpartyIBAN: d.Counterparty.IBAN,
			CounterpartyAccount: d.Counterparty.Account, CounterpartyBIC: d.Counterparty.BIC, Remittance: d.Remittance}
	}
	return out
}

// relinkNext makes the statement just imported the previous statement of
// the bank's next one, when it comes between that one and its previous
// statement (a missing statement, or an older one imported later), and
// compares their balances again: the next statement is in gap exactly when
// its opening balance is not the new closing balance (a reconciled one
// stays reconciled).
func relinkNext(ctx context.Context, tx pgx.Tx, p reqctx.Principal, bank statementBank, id uuid.UUID, s bankstatement.Statement) error {
	var next uuid.UUID
	var was, now string
	err := tx.QueryRow(ctx, `WITH next AS (
			SELECT n.id, n.status FROM payments.bank_statement n
			LEFT JOIN payments.bank_statement pr ON pr.tenant_id = n.tenant_id AND pr.id = n.previous_statement_id
			WHERE n.house_bank_id = $1 AND n.id <> $2 AND n.opening_date >= $3 AND (pr.id IS NULL OR pr.closing_date <= $3)
			ORDER BY n.opening_date, n.id LIMIT 1)
		UPDATE payments.bank_statement b SET previous_statement_id = $2, version = b.version + 1, updated_by = $5,
			status = CASE WHEN b.status = 'reconciled' THEN b.status WHEN b.opening_balance = $4 THEN 'imported' ELSE 'gap' END
		FROM next WHERE b.id = next.id
		RETURNING b.id, next.status, b.status`, bank.ID, id, s.Closing.Date, s.Closing.Amount, p.ID).Scan(&next, &was, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("payments: the statement after statement %s: %w", id, err)
	}
	var events []outbox.Event
	if name := map[string]string{StatementImported: "bank_statement_gap_closed", StatementGap: "bank_statement_gap_found"}[now]; name != "" && was != now {
		events = append(events, outbox.Event{Context: "payments", Name: name, AggregateType: "payments.bank_statement", AggregateID: next,
			Payload: map[string]any{"bank_statement_id": next, "house_bank_id": bank.ID, "legal_entity_id": bank.LegalEntityID,
				"previous_statement_id": id, "status": now}})
	}
	return record(ctx, "payments.bank_statement.relink", next, map[string]any{"previous_statement_id": id, "previous_status": was, "status": now}, events...)
}

// recordStatement audits the import and publishes BankStatementImported.
func recordStatement(ctx context.Context, id uuid.UUID, bank statementBank, sum []byte, s bankstatement.Statement, exp int, status string, prev *uuid.UUID) error {
	payload := map[string]any{"bank_statement_id": id, "house_bank_id": bank.ID, "legal_entity_id": bank.LegalEntityID, "format": s.Format,
		"file_sha256": sha256Hex(sum), "statement_ref": s.ID, "sequence": s.Sequence, "line_count": len(s.Entries),
		"opening_balance": map[string]any{"amount": s.Opening.Amount.Plain(exp), "currency": s.Currency},
		"closing_balance": map[string]any{"amount": s.Closing.Amount.Plain(exp), "currency": s.Currency}, "previous_statement_id": prev, "status": status}
	return record(ctx, "payments.bank_statement.import", id, map[string]any{"statement_ref": s.ID, "sequence": s.Sequence, "file_sha256": sha256Hex(sum),
		"status": status}, outbox.Event{Context: "payments", Name: "bank_statement_imported", AggregateType: "payments.bank_statement", AggregateID: id, Payload: payload})
}

const statementSelect = `SELECT s.id, s.house_bank_id, s.legal_entity_id, s.format, s.file_sha256, s.file_position, s.message_id, s.statement_ref,
	s.sequence, s.account, s.currency, c.exponent, s.opening_balance, s.closing_balance, s.opening_date, s.closing_date, s.line_count,
	s.previous_statement_id, s.status, s.version, s.created_at, s.updated_at, s.created_by, s.updated_by
	FROM payments.bank_statement s JOIN ref.currency c ON c.code = s.currency`

func scanStatement(row pgx.Row) (Statement, error) {
	var s Statement
	err := row.Scan(&s.ID, &s.HouseBankID, &s.LegalEntityID, &s.Format, &s.FileSHA256, &s.FilePosition, &s.MessageID, &s.Ref, &s.Sequence, &s.Account,
		&s.Currency, &s.Exponent, &s.Opening, &s.Closing, &s.OpeningDate, &s.ClosingDate, &s.LineCount, &s.PreviousID, &s.Status, &s.Version,
		&s.CreatedAt, &s.UpdatedAt, &s.CreatedBy, &s.UpdatedBy)
	return s, errs.Wrapf(err, "scan bank statement")
}

// fileStatements reads the statements the house bank imported from the
// file, in its order, with their lines.
func fileStatements(ctx context.Context, tx pgx.Tx, houseBank uuid.UUID, sum []byte) ([]Statement, error) {
	rows, _ := tx.Query(ctx, statementSelect+` WHERE s.house_bank_id = $1 AND s.file_sha256 = $2 ORDER BY s.file_position`, houseBank, sum)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Statement, error) { return scanStatement(r) })
	if err != nil {
		return nil, fmt.Errorf("payments: statements of file %x: %w", sum, err)
	}
	for i := range out {
		if out[i].Lines, err = statementLines(ctx, tx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// statementLines reads a statement's lines in the bank's order.
func statementLines(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]StatementLine, error) {
	rows, _ := tx.Query(ctx, `SELECT line_no, amount, is_reversal, booking_date, value_date, transaction_code, bank_reference, owner_reference,
		text, status, details FROM payments.bank_statement_line WHERE statement_id = $1 ORDER BY line_no`, id)
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StatementLine, error) {
		var l StatementLine
		err := r.Scan(&l.LineNo, &l.Amount, &l.Reversal, &l.BookingDate, &l.ValueDate, &l.TransactionCode, &l.BankReference, &l.OwnerReference,
			&l.Text, &l.Status, &l.Details)
		return l, errs.Wrapf(err, "scan statement line")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: lines of statement %s: %w", id, err)
	}
	return append([]StatementLine{}, lines...), nil // never nil: the statement was read with its lines
}

func statementNotFound() error { return problem.NotFoundOf("bank statement") }

// GetStatement returns a bank statement of a legal entity the caller may
// access, with its lines.
func GetStatement(ctx context.Context, id uuid.UUID) (Statement, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Statement{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Statement{}, err
	}
	s, err := scanStatement(tx.QueryRow(ctx, statementSelect+` WHERE s.id = $1`, id))
	if err = authz.Visible(err, access, s.LegalEntityID, statementNotFound(), "payments: read bank statement %s", id); err != nil {
		return Statement{}, err
	}
	s.Lines, err = statementLines(ctx, tx, id)
	return s, err
}

// sha256Hex is a file's hash as the API shows it.
func sha256Hex(sum []byte) string { return fmt.Sprintf("%x", sum) }
