package leases

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Active is the status of a lease contract once it is activated
// (ADR-0073 §7).
const Active = "active"

// The account determination keys an activation posts to (ADR-0073 §7).
const (
	RightOfUseKey = "right_of_use_asset"
	LiabilityKey  = "lease_liability"
)

// Postings posts journal entries (ledger context).
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
}

// Accounts reads the account determination (ledger context).
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
}

// Schedule is the contract's schedule: the stored one of an active
// contract, computed from its payments for a draft (ADR-0073 §8).
func (c Contract) Schedule(ctx context.Context, tx pgx.Tx) (Schedule, error) {
	if c.Status == Draft {
		return c.Terms().Schedule(c.Payments, c.annualRate()), nil
	}
	rows, _ := tx.Query(ctx, `SELECT l.line_no, l.period_start, l.period_end, p.due_date, l.opening_liability, l.payment, l.interest,
			l.closing_liability, l.depreciation, l.right_of_use_closing
		FROM leases.schedule_line l
		JOIN leases.payment p ON p.tenant_id = l.tenant_id AND p.contract_id = l.contract_id AND p.payment_no = l.line_no
		WHERE l.contract_id = $1 ORDER BY l.line_no`, c.ID)
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Line, error) {
		var l Line
		return l, r.Scan(&l.No, &l.Start, &l.End, &l.PaymentDate, &l.Opening, &l.Payment, &l.Interest, &l.Closing, &l.Depreciation,
			&l.RightOfUseCarrying)
	})
	if err != nil {
		return Schedule{}, fmt.Errorf("leases: schedule of contract %s: %w", c.ID, err)
	}
	s := Schedule{Liability: *c.Liability, RightOfUse: *c.RightOfUse, Lines: lines}
	s.CommencementPayment = s.RightOfUse - s.Liability
	return s, nil
}

// lockDraft reads a draft contract of a company the caller may act in
// under its row lock, FOR NO KEY UPDATE: its schedule's lines reference
// it, and their foreign key's FOR KEY SHARE does not wait for this lock
// (ADR-0008). Another company's is not found; another version is 412 and
// an active one 409 leases.invalid_status.
func lockDraft(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Contract, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contract{}, fmt.Errorf("leases: company access for contract %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `SELECT FROM leases.contract WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
		return Contract{}, fmt.Errorf("leases: lock contract %s: %w", id, err)
	}
	c, err := load(ctx, tx, id)
	if err != nil {
		return Contract{}, err
	}
	if !access.Allows(c.LegalEntityID) {
		return Contract{}, problem.NotFoundOf("lease contract")
	}
	if err := ifMatch(c.Version); err != nil {
		return Contract{}, err
	}
	if c.Status != Draft {
		return Contract{}, problem.Conflict("leases.invalid_status", "Lease contract "+c.DocumentNumber+" is "+c.Status+"; only a draft is activated.")
	}
	return c, nil
}

// leaseAccounts are the company's right-of-use asset and lease liability
// accounts, or 422 leases.no_lease_accounts naming the keys it lacks.
func (e *Endpoints) leaseAccounts(ctx context.Context, le uuid.UUID, company string) (rightOfUse, liability uuid.UUID, err error) {
	var missing []string
	ids := map[string]*uuid.UUID{RightOfUseKey: &rightOfUse, LiabilityKey: &liability}
	for _, key := range []string{RightOfUseKey, LiabilityKey} {
		id, ok, err := e.Accounts.Determination(ctx, le, key)
		if err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("leases: account %s of company %s: %w", key, company, err)
		}
		if !ok {
			missing = append(missing, key)
		}
		*ids[key] = id
	}
	if len(missing) > 0 {
		return uuid.Nil, uuid.Nil, problem.Unprocessable("leases.no_lease_accounts", "Company "+company+"'s account determination has no "+
			strings.Join(missing, " and ")+" account; a lease is activated once the configuration names both (companies[].account_determination).")
	}
	return rightOfUse, liability, nil
}

// Activate activates a draft lease contract once (ADR-0073 §7): under its
// row lock, its schedule is computed from its payments and stored, and one
// entry dated the commencement date debits the right-of-use asset account
// and credits the lease liability account with the right-of-use amount
// (the liability at commencement and a payment due on the commencement
// date, which the liability holds until the lessor's invoice for it is
// posted). The contract becomes active, names the amounts and the entry
// and is fixed. The audit record and LeaseContractChanged are written in
// the same transaction.
func (e *Endpoints) Activate(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	c, err := lockDraft(ctx, tx, id, ifMatch)
	if err != nil {
		return Contract{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID) // a stored contract's legal entity exists
	if err != nil {
		return Contract{}, fmt.Errorf("leases: company of contract %s: %w", id, err)
	}
	rightOfUse, liability, err := e.leaseAccounts(ctx, le.ID, le.Code)
	if err != nil {
		return Contract{}, err
	}
	s := c.Terms().Schedule(c.Payments, c.annualRate())
	text := "Lease contract " + c.DocumentNumber + ": right-of-use asset at commencement"
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: le.ID, DocumentType: "SA", DocumentDate: c.CommencementDate,
		PostingDate: c.CommencementDate, Currency: c.Currency, Reference: &c.DocumentNumber, HeaderText: &text, SourceContext: "leases",
		SourceDocumentID: c.ID, Lines: []ledger.PostingLine{{AccountID: rightOfUse, Amount: s.RightOfUse}, {AccountID: liability, Amount: -s.RightOfUse}}})
	if err != nil {
		return Contract{}, fmt.Errorf("leases: post the activation of contract %s: %w", c.DocumentNumber, err)
	}
	if err := keep(ctx, tx, p, c, s, entry); err != nil {
		return Contract{}, err
	}
	after, err := load(ctx, tx, id)
	if err != nil {
		return Contract{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{after.LessorID})
	if err != nil {
		return Contract{}, fmt.Errorf("leases: lessor of contract %s: %w", id, err)
	}
	before := Draft
	return after, record(ctx, "leases.contract.activate", after, le, names[after.LessorID].Number, &before)
}

// keep stores the schedule's lines and marks the contract active with the
// amounts and the entry its activation posted.
func keep(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Contract, s Schedule, entry ledger.Entry) error {
	n := len(s.Lines)
	nos, starts, ends := make([]int32, n), make([]time.Time, n), make([]time.Time, n)
	amounts := make([][]int64, 6)
	for i := range amounts {
		amounts[i] = make([]int64, n)
	}
	for i, l := range s.Lines {
		nos[i], starts[i], ends[i] = l.No, l.Start, l.End
		for j, a := range []money.Minor{l.Opening, l.Payment, l.Interest, l.Closing, l.Depreciation, l.RightOfUseCarrying} {
			amounts[j][i] = int64(a)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO leases.schedule_line (tenant_id, contract_id, line_no, period_start, period_end, opening_liability, payment,
			interest, closing_liability, depreciation, right_of_use_closing)
		SELECT $1, $2, x.* FROM unnest($3::int[], $4::date[], $5::date[], $6::bigint[], $7::bigint[], $8::bigint[], $9::bigint[], $10::bigint[], $11::bigint[])
			AS x (line_no, period_start, period_end, opening, payment, interest, closing, depreciation, carrying)`,
		p.TenantID, c.ID, nos, starts, ends, amounts[0], amounts[1], amounts[2], amounts[3], amounts[4], amounts[5]); err != nil {
		return fmt.Errorf("leases: keep the schedule of contract %s: %w", c.DocumentNumber, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE leases.contract SET status = $2, version = version + 1, updated_by = $3, lease_liability = $4,
			right_of_use_amount = $5, journal_entry_id = $6, journal_entry_year = $7, journal_entry_number = $8
		WHERE id = $1 AND status = $9`, c.ID, Active, p.ID, s.Liability, s.RightOfUse, entry.ID, entry.FiscalYear, entry.DocumentNumber, Draft); err != nil {
		return fmt.Errorf("leases: activate contract %s: %w", c.DocumentNumber, err)
	}
	return nil
}
