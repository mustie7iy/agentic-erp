package leases

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The lease run (ADR-0073 §9, T88c). For a company and one of its regular
// fiscal periods, each active contract posts the lines of its stored
// schedule (T88b) whose payment interval ends on or before the period's
// last day and that no earlier run posted: one entry dated that day, the
// lines' interest debited to interest expense and credited to the lease
// liability, their depreciation debited to depreciation expense and
// credited to accumulated depreciation, the expenses on the contract's
// cost center. A contract with no such line posts nothing. In the usual
// course that is the one line whose interval ends in the period (none in
// a period a quarterly or yearly interval does not end in); a line of a
// contract activated after its period was run is caught up by the next
// run, as a revenue recognition run catches up (decided by default,
// ADR-0073 As built T88c). The lease payment itself is not posted: a
// person posts the lessor's invoice against the lease liability (the
// plan's default 7).
//
// A company's runs follow one another under one lock, one per period,
// each for a later period than the last, so a line is posted once
// (lease_line_posted_once); a run is not reversed.

// MaxRunContracts is the most active contracts of a company one run reads:
// a run is one request and one transaction, as a recognition run is
// (revenue.MaxRunContracts). Larger runs wait for the run as a workflow.
const MaxRunContracts = 2000

// The account determination keys a run posts to besides LiabilityKey
// (ADR-0073 §9).
const (
	InterestKey     = "lease_interest"
	DepreciationKey = "right_of_use_depreciation"
	AccumulatedKey  = "right_of_use_accumulated_depreciation"
)

func init() {
	// The run checks both under the company's lock; the constraints are
	// the database's rules and answer alike.
	problem.RegisterConstraint("lease_run_once", func() *problem.Problem {
		return problem.Conflict("leases.run_exists", "Lease accounting was already run for this company and period.")
	})
	problem.RegisterConstraint("lease_run_in_order", func() *problem.Problem {
		return problem.Conflict("leases.run_period_not_later", "The company has a lease run for this period or a later one; a run is for a later period than the last.")
	})
}

// RunPosting is a row of leases.run_posting with its contract's number,
// lessor and cost center: what a run posted for one contract, its
// schedule lines FirstLine to LastLine, their interest and depreciation
// in minor units of the run's currency, and the entry.
type RunPosting struct {
	ID             uuid.UUID
	ContractID     uuid.UUID
	DocumentNumber string
	LessorID       uuid.UUID
	CostCenterID   uuid.UUID
	FirstLine      int32
	LastLine       int32
	Interest       money.Minor
	Depreciation   money.Minor
	EntryID        uuid.UUID
	EntryYear      int16
	EntryNumber    string
}

// Run is a row of leases.run: Contracts is the active contracts it read,
// Posted those that posted, and Interest and Depreciation what its entries
// posted, in minor units of Currency, the company's local currency.
// Postings are read by GetRun and given by PostRun.
type Run struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	FiscalYear    int16
	FiscalPeriod  int16
	PostingDate   time.Time
	Currency      string
	Exponent      int
	Contracts     int32
	Posted        int32
	Interest      money.Minor
	Depreciation  money.Minor
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	Postings      []RunPosting
}

// runBasis is what a run is made on: the company, its currency's digits,
// the period and the four accounts, in the order runKeys names them.
type runBasis struct {
	le       tenancy.Entity
	exp      int
	period   ledger.FiscalPeriod
	accounts []uuid.UUID
}

// runKeys are the accounts a run posts to: interest expense, the lease
// liability, depreciation expense and accumulated depreciation.
var runKeys = []string{InterestKey, LiabilityKey, DepreciationKey, AccumulatedKey}

// basisOfRun reads what a run of the company and period is made on and
// holds the company's runs: the company is one the caller may act in (422
// request.reference_not_found), the period a regular one (the ledger's
// 422 ledger.no_fiscal_period and ledger.period_not_regular), the company
// has the four accounts (422 leases.no_lease_accounts), the run is for a
// later period than the company's last (leaseRuns), and the period is
// open in every ledger the company keeps (the ledger's 409
// ledger.period_not_open), also when nothing is to post.
func (e *Endpoints) basisOfRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, legalEntity uuid.UUID, year, number int32) (runBasis, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return runBasis{}, fmt.Errorf("leases: company access for a lease run: %w", err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return runBasis{}, fmt.Errorf("leases: company %s of a lease run: %w", legalEntity, err)
	}
	if !ok || !access.Allows(le.ID) {
		return runBasis{}, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id that you can access")
	}
	b := runBasis{le: le}
	if b.exp, err = currency.Exponent(ctx, tx, le.LocalCurrency, nil); err != nil {
		return runBasis{}, fmt.Errorf("leases: currency of company %s: %w", le.Code, err)
	}
	if b.period, err = e.Calendar.RegularPeriod(ctx, le.ID, year, number); err != nil {
		return runBasis{}, fmt.Errorf("leases: period %d/%d of company %s: %w", year, number, le.Code, err)
	}
	if b.accounts, err = e.leaseAccounts(ctx, le.ID, le.Code, runKeys...); err != nil {
		return runBasis{}, err
	}
	if err := runsInOrder(ctx, tx, p, le, b.period); err != nil {
		return runBasis{}, err
	}
	if err := e.Postings.Postable(ctx, le.ID, b.period.End); err != nil {
		return runBasis{}, fmt.Errorf("leases: posting on %s in company %s: %w", b.period.End.Format(time.DateOnly), le.Code, err)
	}
	return b, nil
}

// leaseRuns are the company's lease runs, which follow one another: a
// run posts the lines the runs before it left, and the database's rules
// take the same lock when the run and its postings are kept
// (lease_run_in_order, lease_line_posted_once). A second run for the last
// run's period is 409 leases.run_exists, one for an earlier period 409
// leases.run_period_not_later.
var leaseRuns = ledger.PeriodRuns{Table: pgx.Identifier{"leases", "run"}, Lock: "leases.run", Name: "leases: lease runs"}

// runsInOrder holds the company's lease runs and refuses a run that does
// not follow them (leaseRuns).
func runsInOrder(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le tenancy.Entity, period ledger.FiscalPeriod) error {
	last, order, err := leaseRuns.InOrder(ctx, tx, p.TenantID, le.ID, le.Code, period)
	switch {
	case err != nil:
		return err
	case order == ledger.RunSamePeriod:
		return problem.Conflict("leases.run_exists", fmt.Sprintf("Lease accounting was already run for company %s and %d/%d; "+
			"GET /v1/lease-runs?legal_entity_id=&fiscal_year=&fiscal_period= finds the run.", le.Code, last.FiscalYear, last.Period))
	case order == ledger.RunEarlierPeriod:
		return problem.Conflict("leases.run_period_not_later", fmt.Sprintf("Company %s's last lease run was for %d/%d, to %s; "+
			"a run is for a later period than the last. A line whose interval ended in a period already run is caught up by the next run.",
			le.Code, last.FiscalYear, last.Period, last.PostingDate.Format(time.DateOnly)))
	}
	return nil
}

// due are the number of the company's active contracts, at most the
// run's bound (422 leases.run_too_large), and, by contract number, what
// each has to post: its stored lines that end on or before the day and
// come after the last line a run posted, added up. A contract whose lines
// add up to nothing posts nothing.
func (e *Endpoints) due(ctx context.Context, tx pgx.Tx, le tenancy.Entity, day time.Time) (int32, []RunPosting, error) {
	limit := e.MaxRunContracts
	if limit == 0 {
		limit = MaxRunContracts
	}
	// One statement reads the active contracts, at most one more than the
	// bound, and their lines due, so the count and the lines are of one
	// state: an activation committing between two statements would post a
	// contract the count did not read.
	rows, _ := tx.Query(ctx, `WITH active AS (
			SELECT k.tenant_id, k.id, k.document_number, k.lessor_id, k.cost_center_id FROM leases.contract k
			WHERE k.legal_entity_id = $1 AND k.status = $2 ORDER BY k.document_number, k.id LIMIT $4)
		SELECT (SELECT count(*) FROM active)::int, a.id, a.document_number, a.lessor_id, a.cost_center_id, min(l.line_no), max(l.line_no),
			sum(l.interest)::bigint, sum(l.depreciation)::bigint
		FROM active a JOIN leases.schedule_line l ON l.tenant_id = a.tenant_id AND l.contract_id = a.id
		WHERE l.period_end <= $3
			AND l.line_no > coalesce((SELECT max(p.last_line_no) FROM leases.run_posting p WHERE p.tenant_id = a.tenant_id AND p.contract_id = a.id), 0)
		GROUP BY a.id, a.document_number, a.lessor_id, a.cost_center_id
		HAVING sum(l.interest) <> 0 OR sum(l.depreciation) <> 0
		UNION ALL SELECT (SELECT count(*) FROM active)::int, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL
		ORDER BY 3 NULLS FIRST, 2`, le.ID, Active, day, limit+1)
	var active int32
	var postings []RunPosting
	var r struct {
		id, lessor, center     *uuid.UUID
		number                 *string
		first, last            *int32
		interest, depreciation *money.Minor
	}
	_, err := pgx.ForEachRow(rows, []any{&active, &r.id, &r.number, &r.lessor, &r.center, &r.first, &r.last, &r.interest, &r.depreciation}, func() error {
		if r.id != nil {
			postings = append(postings, RunPosting{ContractID: *r.id, DocumentNumber: *r.number, LessorID: *r.lessor, CostCenterID: *r.center,
				FirstLine: *r.first, LastLine: *r.last, Interest: *r.interest, Depreciation: *r.depreciation})
		}
		return nil
	})
	if err != nil {
		return 0, nil, fmt.Errorf("leases: lines due of company %s to %s: %w", le.Code, day.Format(time.DateOnly), err)
	}
	if int(active) > limit {
		return 0, nil, problem.Unprocessable("leases.run_too_large", fmt.Sprintf("Company %s has more than %d active lease contracts, "+
			"the most one run reads; larger runs are not supported yet.", le.Code, limit))
	}
	return active, postings, nil
}

// entryLines are the lines of a contract's entry (ADR-0073 §9): the
// interest debited to interest expense on the contract's cost center and
// credited to the lease liability, the depreciation debited to
// depreciation expense on the cost center and credited to accumulated
// depreciation. An amount of zero has no lines; the interest of a line
// whose rounding left it below zero reverses the sides.
func entryLines(p RunPosting, accounts []uuid.UUID) []ledger.PostingLine {
	interest, liability, depreciation, accumulated := accounts[0], accounts[1], accounts[2], accounts[3]
	center := p.CostCenterID
	var lines []ledger.PostingLine
	if p.Interest != 0 {
		text := fmt.Sprintf("Lease interest, lines %d to %d", p.FirstLine, p.LastLine)
		lines = append(lines, ledger.PostingLine{AccountID: interest, Amount: p.Interest, CostCenterID: &center, Text: &text},
			ledger.PostingLine{AccountID: liability, Amount: -p.Interest, Text: &text})
	}
	if p.Depreciation != 0 {
		text := fmt.Sprintf("Right-of-use depreciation, lines %d to %d", p.FirstLine, p.LastLine)
		lines = append(lines, ledger.PostingLine{AccountID: depreciation, Amount: p.Depreciation, CostCenterID: &center, Text: &text},
			ledger.PostingLine{AccountID: accumulated, Amount: -p.Depreciation, Text: &text})
	}
	return lines
}

// runTotals are the postings' interest and depreciation added up; sums
// beyond an amount are refused (422 leases.amount_too_large).
func runTotals(postings []RunPosting) (interest, depreciation money.Minor, err error) {
	is, ds := make([]money.Minor, len(postings)), make([]money.Minor, len(postings))
	for i, p := range postings {
		is[i], ds[i] = p.Interest, p.Depreciation
	}
	if interest, err = money.Sum(is...); err == nil {
		depreciation, err = money.Sum(ds...)
	}
	if err != nil {
		return 0, 0, problem.Unprocessable("leases.amount_too_large", "The company's lease postings add up to more than an amount holds.").WithCause(err)
	}
	return interest, depreciation, nil
}

// PostRun runs lease accounting for the company and the fiscal period and
// posts it, in the request's transaction (ADR-0073 §9): the run is kept
// with its totals, each contract with lines due posts one entry to every
// ledger the company keeps, dated the period's last day, and keeps what
// it posted, and the run's audit record and LeaseRunPosted are written
// with them. The refusals are basisOfRun's and due's, totals beyond an
// amount (422 leases.amount_too_large), and whatever the ledger refuses of
// an entry.
func (e *Endpoints) PostRun(ctx context.Context, legalEntity uuid.UUID, year, number int32) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	b, err := e.basisOfRun(ctx, tx, p, legalEntity, year, number)
	if err != nil {
		return Run{}, err
	}
	run := Run{ID: uuid.Must(uuid.NewV7()), LegalEntityID: b.le.ID, FiscalYear: b.period.FiscalYear, FiscalPeriod: b.period.Number,
		PostingDate: b.period.End, Currency: b.le.LocalCurrency, Exponent: b.exp, CreatedBy: p.ID}
	if run.Contracts, run.Postings, err = e.due(ctx, tx, b.le, b.period.End); err != nil {
		return Run{}, err
	}
	run.Posted = int32(len(run.Postings))
	if run.Interest, run.Depreciation, err = runTotals(run.Postings); err != nil {
		return Run{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO leases.run (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, posting_date, currency, contract_count,
			posting_count, interest_amount, depreciation_amount, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING created_at`, p.TenantID, run.ID, run.LegalEntityID, run.FiscalYear,
		run.FiscalPeriod, run.PostingDate, run.Currency, run.Contracts, run.Posted, run.Interest, run.Depreciation, run.CreatedBy).Scan(&run.CreatedAt)
	if err != nil {
		return Run{}, fmt.Errorf("leases: keep the lease run of company %s for %d/%d: %w", b.le.Code, run.FiscalYear, run.FiscalPeriod, err)
	}
	for i := range run.Postings {
		if err := e.postContract(ctx, tx, p, b, run, &run.Postings[i]); err != nil {
			return Run{}, err
		}
	}
	return run, recordRun(ctx, run, b.le)
}

// postContract posts the contract's entry and keeps the posting: the
// entry is dated the period's last day, in the company's local currency,
// with the contract's number as its reference and the posting as its
// source document.
func (e *Endpoints) postContract(ctx context.Context, tx pgx.Tx, pr reqctx.Principal, b runBasis, run Run, p *RunPosting) error {
	p.ID = uuid.Must(uuid.NewV7())
	text := fmt.Sprintf("Lease run %d/%d", run.FiscalYear, run.FiscalPeriod)
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: b.le.ID, DocumentType: "SA", DocumentDate: run.PostingDate, PostingDate: run.PostingDate,
		Currency: run.Currency, Reference: &p.DocumentNumber, HeaderText: &text, SourceContext: "leases", SourceDocumentID: p.ID,
		Lines: entryLines(*p, b.accounts)})
	if err != nil {
		return fmt.Errorf("leases: post the lease run of contract %s: %w", p.DocumentNumber, err)
	}
	p.EntryID, p.EntryYear, p.EntryNumber = entry.ID, entry.FiscalYear, entry.DocumentNumber
	if _, err := tx.Exec(ctx, `INSERT INTO leases.run_posting (tenant_id, run_id, contract_id, id, first_line_no, last_line_no, interest_amount,
			depreciation_amount, journal_entry_id, journal_entry_year, journal_entry_number) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		pr.TenantID, run.ID, p.ContractID, p.ID, p.FirstLine, p.LastLine, p.Interest, p.Depreciation, p.EntryID, p.EntryYear, p.EntryNumber); err != nil {
		return fmt.Errorf("leases: keep the lease run posting of contract %s: %w", p.DocumentNumber, err)
	}
	return nil
}

// recordRun adds the run's audit record and publishes LeaseRunPosted in
// the same transaction (ADR-0007, ADR-0011): the run with its counts and
// totals; the audit record also lists its postings, by contract number.
func recordRun(ctx context.Context, r Run, le tenancy.Entity) error {
	day := r.PostingDate.Format(time.DateOnly)
	payload := map[string]any{"lease_run_id": r.ID, "legal_entity_id": r.LegalEntityID, "legal_entity_code": le.Code, "fiscal_year": r.FiscalYear,
		"fiscal_period": r.FiscalPeriod, "posting_date": day, "contract_count": r.Contracts, "posting_count": r.Posted,
		"interest_amount": money.EventAmount(r.Interest, r.Currency, r.Exponent), "depreciation_amount": money.EventAmount(r.Depreciation, r.Currency, r.Exponent)}
	postings := make([]map[string]any, len(r.Postings))
	for i, p := range r.Postings {
		postings[i] = map[string]any{"lease_contract_id": p.ContractID, "document_number": p.DocumentNumber, "first_line_no": p.FirstLine,
			"last_line_no": p.LastLine, "interest_amount": p.Interest.Plain(r.Exponent), "depreciation_amount": p.Depreciation.Plain(r.Exponent),
			"journal_entry_id": p.EntryID, "journal_entry_number": p.EntryNumber}
	}
	detail := map[string]any{"legal_entity_id": r.LegalEntityID, "fiscal_year": r.FiscalYear, "fiscal_period": r.FiscalPeriod, "posting_date": day,
		"currency": r.Currency, "contract_count": r.Contracts, "posting_count": r.Posted, "interest_amount": r.Interest.Plain(r.Exponent),
		"depreciation_amount": r.Depreciation.Plain(r.Exponent), "postings": postings}
	if err := audit.Record(ctx, "leases.run.post", "leases.run", r.ID, detail,
		outbox.Event{Context: "leases", Name: "lease_run_posted", AggregateType: "leases.run", AggregateID: r.ID, Payload: payload}); err != nil {
		return fmt.Errorf("leases: record lease run %s: %w", r.ID, err)
	}
	return nil
}
