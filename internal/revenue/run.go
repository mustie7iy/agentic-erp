package revenue

import (
	"context"
	"errors"
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
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The recognition run (ADR-0068 §9, T81d). For a company and one of its
// regular fiscal periods it brings every active contract to its
// cumulative position at the period's last day. Billing credited each
// order item's revenue account with what was billed (sales, ADR-0067
// §9); per obligation the run moves that account by the revenue
// recognized to the day (the schedule, schedule.go) less the revenue
// billed to it, less what earlier runs posted for the obligation. The
// contract's position, the sum of recognized less billed over its
// obligations, stands on the company's contract asset account when it is
// positive and on its contract liability account when it is negative:
// the entry takes the position the last run left off its account and
// puts the new one on, so a contract never shows both. The posted
// invoice stays the receivable and output tax is not touched.
//
// The amounts are cumulative, so a run is idempotent per contract and
// period: a contract whose recognized less billed did not move since its
// last posting posts nothing, and a delivery or a billing document dated
// into a period already run is caught up by the next run. A company's
// runs follow one another under one lock, one per period, each for a
// later period than the last; a run is not reversed.
//
// That a contract is presented as one contract asset or one contract
// liability is ADR-0068 §2's statement 6 about IFRS 15, from memory and
// not yet confirmed by the accounting specialist.

// MaxRunContracts is the most active contracts of a company one run
// reads (ADR-0068 §9): a run is one request and one transaction, as a
// collection run is (collections.MaxRunItems). Larger runs wait for the
// run as a workflow.
const MaxRunContracts = 2000

// The account determination keys of the run's two accounts
// (companies[].account_determination).
const (
	contractAsset     = "contract_asset"
	contractLiability = "contract_liability"
)

func init() {
	// The run checks both under the company's lock; the constraints are
	// the database's rules and answer alike.
	problem.RegisterConstraint("recognition_run_once", func() *problem.Problem {
		return problem.Conflict("revenue.recognition_run_exists", "Revenue recognition was already run for this company and period.")
	})
	problem.RegisterConstraint("recognition_run_in_order", func() *problem.Problem {
		return problem.Conflict("revenue.recognition_period_not_later", "The company has a revenue recognition run for this period or a later one; a run is for a later period than the last.")
	})
}

// ObligationFigures are an obligation's figures in a run's posting:
// Recognized and Billed are cumulative to the run's posting date, and
// Revenue is what the entry moved the revenue account AccountID by, a
// credit when positive.
type ObligationFigures struct {
	No          int32       `json:"obligation_no"`
	Description string      `json:"description"`
	AccountID   uuid.UUID   `json:"account_id"`
	Recognized  money.Minor `json:"recognized_amount"`
	Billed      money.Minor `json:"billed_amount"`
	Revenue     money.Minor `json:"revenue_amount"`
}

// Posting is a row of revenue.recognition_posting with its obligations:
// what a run posted for one contract. Recognized and Billed are the
// contract's cumulative amounts to the run's posting date and Revenue
// what the entry moved its revenue by.
type Posting struct {
	ID               uuid.UUID
	ContractID       uuid.UUID
	SalesOrderID     uuid.UUID
	SalesOrderNumber string
	CustomerID       uuid.UUID
	Recognized       money.Minor
	Billed           money.Minor
	Revenue          money.Minor
	EntryID          uuid.UUID
	EntryYear        int16
	DocumentNumber   string
	Obligations      []ObligationFigures
}

// position splits recognized less billed into the contract asset and the
// contract liability it stands as: one of the two is zero.
func position(recognized, billed money.Minor) (asset, liability money.Minor) {
	if d := recognized - billed; d > 0 {
		return d, 0
	}
	return 0, billed - recognized
}

// Run is a row of revenue.recognition_run: the amounts are the company's
// totals to PostingDate over the Contracts active contracts read, in
// minor units of Currency; Posted of them moved and were posted, and
// Revenue is what their entries moved revenue by, a credit when
// positive.
// Postings are read by GetRun and given by Recognize.
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
	Recognized    money.Minor
	Billed        money.Minor
	Asset         money.Minor
	Liability     money.Minor
	Revenue       money.Minor
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	Postings      []Posting
}

// runBasis is what a run is made on: the company, the period, and the
// company's contract asset and contract liability accounts.
type runBasis struct {
	le        tenancy.Entity
	exp       int
	period    ledger.FiscalPeriod
	asset     uuid.UUID
	liability uuid.UUID
}

// regularPeriod is the regular posting period of the company's fiscal
// year the run names. A year that is not set up is the ledger's refusal
// (422 ledger.no_fiscal_period); a number that is no regular period of
// the year is refused here (422 revenue.period_not_regular).
func (e *Endpoints) regularPeriod(ctx context.Context, le uuid.UUID, year, number int32) (ledger.FiscalPeriod, error) {
	periods, err := e.Calendar.Periods(ctx, le, int16(year))
	if err != nil {
		return ledger.FiscalPeriod{}, fmt.Errorf("revenue: periods of %s in %d: %w", le, year, err)
	}
	for _, p := range periods {
		if int32(p.Number) == number {
			return p, nil
		}
	}
	return ledger.FiscalPeriod{}, problem.Unprocessable("revenue.period_not_regular",
		fmt.Sprintf("Fiscal period %d is not a regular posting period of the company's fiscal year %d.", number, year),
		problem.FieldError{Field: "fiscal_period", Code: "not_regular", Message: fmt.Sprintf("a regular posting period of %d, 1 to %d", year, len(periods))})
}

// contractAccounts are the company's contract asset and contract
// liability accounts; a company without both is refused (422
// revenue.no_contract_accounts).
func (e *Endpoints) contractAccounts(ctx context.Context, le tenancy.Entity) (asset, liability uuid.UUID, err error) {
	asset, hasAsset, err := e.Accounts.Determination(ctx, le.ID, contractAsset)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("revenue: contract asset account of company %s: %w", le.Code, err)
	}
	liability, hasLiability, err := e.Accounts.Determination(ctx, le.ID, contractLiability)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("revenue: contract liability account of company %s: %w", le.Code, err)
	}
	if !hasAsset || !hasLiability {
		return uuid.Nil, uuid.Nil, problem.Unprocessable("revenue.no_contract_accounts", "Company "+le.Code+
			" has no contract asset account or no contract liability account; a recognition run needs both in the company's account determination "+
			"(contract_asset, contract_liability).")
	}
	return asset, liability, nil
}

// basisOfRun reads what a run of the company and period is made on and
// holds the company's runs: the company is one the caller may act in
// (422 request.reference_not_found), the period a regular one, the
// company has its two accounts, the period is open in every ledger the
// company keeps (the ledger's 409 ledger.period_not_open), and the run
// is for a later period than the company's last (inOrder).
func (e *Endpoints) basisOfRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, legalEntity uuid.UUID, year, number int32) (runBasis, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return runBasis{}, fmt.Errorf("revenue: company access for a recognition run: %w", err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return runBasis{}, fmt.Errorf("revenue: company %s of a recognition run: %w", legalEntity, err)
	}
	if !ok || !access.Allows(le.ID) {
		return runBasis{}, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id that you can access")
	}
	b := runBasis{le: le}
	if b.exp, err = currency.Exponent(ctx, tx, le.LocalCurrency, nil); err != nil {
		return runBasis{}, fmt.Errorf("revenue: currency of company %s: %w", le.Code, err)
	}
	if b.period, err = e.regularPeriod(ctx, le.ID, year, number); err != nil {
		return runBasis{}, err
	}
	if b.asset, b.liability, err = e.contractAccounts(ctx, le); err != nil {
		return runBasis{}, err
	}
	if err := inOrder(ctx, tx, p, le, b.period); err != nil {
		return runBasis{}, err
	}
	if err := e.Postings.Postable(ctx, le.ID, b.period.End); err != nil {
		return runBasis{}, fmt.Errorf("revenue: posting on %s in company %s: %w", b.period.End.Format(time.DateOnly), le.Code, err)
	}
	return b, nil
}

// inOrder holds the company's runs until the transaction ends and
// refuses a run that does not follow them: one for a period already run
// (409 revenue.recognition_run_exists) or for a period before the
// company's last run (409 revenue.recognition_period_not_later). A run
// reads what the runs before it posted and takes it off, so the runs of
// a company must follow one another: the advisory lock is the one the
// database's rule takes when the run is kept (recognition_run_in_order).
func inOrder(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le tenancy.Entity, period ledger.FiscalPeriod) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('revenue.recognition:' || $1::uuid::text || ':' || $2::uuid::text, 0))`,
		p.TenantID, le.ID); err != nil {
		return fmt.Errorf("revenue: hold the recognition runs of company %s: %w", le.Code, err)
	}
	var year, number int16
	var day time.Time
	err := tx.QueryRow(ctx, `SELECT fiscal_year, fiscal_period, posting_date FROM revenue.recognition_run WHERE legal_entity_id = $1
		ORDER BY posting_date DESC LIMIT 1`, le.ID).Scan(&year, &number, &day)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("revenue: last recognition run of company %s: %w", le.Code, err)
	case year == period.FiscalYear && number == period.Number:
		return problem.Conflict("revenue.recognition_run_exists", fmt.Sprintf("Revenue recognition was already run for company %s and %d/%d; "+
			"GET /v1/revenue-recognition-runs?legal_entity_id=&fiscal_year=&fiscal_period= finds the run.", le.Code, year, number))
	case !day.Before(period.End):
		return problem.Conflict("revenue.recognition_period_not_later", fmt.Sprintf("Company %s's last revenue recognition run was for %d/%d, to %s; "+
			"a run is for a later period than the last. What is dated into a period already run is caught up by the next run.",
			le.Code, year, number, day.Format(time.DateOnly)))
	}
	return nil
}

// activeContracts are the company's active contracts by their orders'
// numbers; more than the run's bound are refused (422
// revenue.recognition_run_too_large).
func (e *Endpoints) activeContracts(ctx context.Context, tx pgx.Tx, le tenancy.Entity) ([]Contract, error) {
	limit := e.MaxContracts
	if limit == 0 {
		limit = MaxRunContracts
	}
	rows, _ := tx.Query(ctx, contractSelect+` WHERE k.legal_entity_id = $1 AND k.status = $2 ORDER BY k.sales_order_number, k.id LIMIT $3`, le.ID, Active, limit+1)
	contracts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contract, error) { return scanContract(r) })
	if err != nil {
		return nil, fmt.Errorf("revenue: active contracts of company %s: %w", le.Code, err)
	}
	if len(contracts) > limit {
		return nil, problem.Unprocessable("revenue.recognition_run_too_large", fmt.Sprintf("Company %s has more than %d active revenue contracts, "+
			"the most one run reads; larger runs are not supported yet.", le.Code, limit))
	}
	return contracts, nil
}

// posted is, per contract and obligation, what the runs so far posted
// for the obligation: its recognized less billed at the contract's last
// posting, which holds every obligation of the contract.
func posted(ctx context.Context, tx pgx.Tx, le tenancy.Entity) (map[uuid.UUID]map[int32]money.Minor, error) {
	rows, _ := tx.Query(ctx, `SELECT o.contract_id, o.obligation_no, o.recognized_amount - o.billed_amount
		FROM revenue.recognition_posting_obligation o
		WHERE o.run_id = (SELECT r.id FROM revenue.recognition_posting p JOIN revenue.recognition_run r ON r.tenant_id = p.tenant_id AND r.id = p.run_id
				WHERE p.tenant_id = o.tenant_id AND p.contract_id = o.contract_id ORDER BY r.posting_date DESC LIMIT 1)
			AND EXISTS (SELECT FROM revenue.contract k WHERE k.tenant_id = o.tenant_id AND k.id = o.contract_id AND k.legal_entity_id = $1)`, le.ID)
	out := map[uuid.UUID]map[int32]money.Minor{}
	var contract uuid.UUID
	var no int32
	var amount money.Minor
	_, err := pgx.ForEachRow(rows, []any{&contract, &no, &amount}, func() error {
		if out[contract] == nil {
			out[contract] = map[int32]money.Minor{}
		}
		out[contract][no] = amount
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("revenue: what the recognition runs of company %s posted: %w", le.Code, err)
	}
	return out, nil
}

// billedTo is the order item's revenue billed to the day: the net
// amounts of its billing documents posted on or before it.
func billedTo(item sales.OrderItem, day time.Time) money.Minor {
	var sum money.Minor
	for _, b := range item.Billings {
		if !b.PostingDate.After(day) {
			sum += b.Net // parts of the item's net value, an amount
		}
	}
	return sum
}

// figures are the contract's figures at the day, from its schedule to
// the day and its order's posted billing, and what the runs so far
// posted for its obligations (before): per obligation the cumulative
// revenue recognized and billed and the movement of its revenue account,
// recognized less billed less what was posted; and the contract's sums.
// moved says whether any obligation's movement is not zero: the
// contract's position moves only with one. Every amount is within the
// contract's transaction price, and a movement, the change of
// recognized less the change of billed since the last posting, neither
// of which falls, is too.
func figures(s Schedule, order sales.Order, day time.Time, before map[int32]money.Minor) (p Posting, moved bool) {
	k := s.Contract
	items := make(map[int32]sales.OrderItem, len(order.Items))
	for _, it := range order.Items {
		items[it.ItemNo] = it
	}
	p = Posting{ContractID: k.ID, SalesOrderID: k.SalesOrderID, SalesOrderNumber: k.SalesOrderNumber, CustomerID: k.CustomerID,
		Obligations: make([]ObligationFigures, len(s.Obligations))}
	for i, o := range s.Obligations {
		item := items[o.OrderItemNo]
		f := ObligationFigures{No: o.No, Description: o.Description, AccountID: item.AccountID, Recognized: o.Recognized, Billed: billedTo(item, day)}
		f.Revenue = f.Recognized - f.Billed - before[o.No]
		p.Obligations[i] = f
		p.Recognized, p.Billed, p.Revenue = p.Recognized+f.Recognized, p.Billed+f.Billed, p.Revenue+f.Revenue
		moved = moved || f.Revenue != 0
	}
	return p, moved
}

// entryLines are the lines of a contract's entry (ADR-0068 §9): per
// obligation that moved, its revenue account credited with the movement
// (debited with a negative one); the contract asset account moved from
// the asset the last run left to the new one, and the contract liability
// account from the liability it left to the new one. An account that
// does not move has no line. The lines balance: the position's change is
// the movements' sum.
func entryLines(p Posting, b runBasis) []ledger.PostingLine {
	var lines []ledger.PostingLine
	for _, o := range p.Obligations {
		if o.Revenue != 0 {
			text := fmt.Sprintf("Revenue recognition, obligation %d", o.No)
			lines = append(lines, ledger.PostingLine{AccountID: o.AccountID, Amount: -o.Revenue, Text: &text})
		}
	}
	asset, liability := position(p.Recognized, p.Billed)
	// The position before is the position now less the movement.
	assetBefore, liabilityBefore := position(p.Recognized-p.Revenue, p.Billed)
	if d := asset - assetBefore; d != 0 {
		text := "Contract asset"
		lines = append(lines, ledger.PostingLine{AccountID: b.asset, Amount: d, Text: &text})
	}
	if d := liability - liabilityBefore; d != 0 {
		text := "Contract liability"
		lines = append(lines, ledger.PostingLine{AccountID: b.liability, Amount: -d, Text: &text})
	}
	return lines
}

// read computes the company's active contracts at the period's last
// day: the run with its totals, and the postings of the contracts that
// moved, not posted yet. A day a contract's schedule has no fiscal
// period for is refused naming the contract's order (422
// revenue.no_fiscal_period); totals beyond an amount are refused (422
// revenue.amount_too_large).
func (e *Endpoints) read(ctx context.Context, tx pgx.Tx, b runBasis) (Run, error) {
	contracts, err := e.activeContracts(ctx, tx, b.le)
	if err != nil {
		return Run{}, err
	}
	before, err := posted(ctx, tx, b.le)
	if err != nil {
		return Run{}, err
	}
	run := Run{ID: uuid.Must(uuid.NewV7()), LegalEntityID: b.le.ID, FiscalYear: b.period.FiscalYear, FiscalPeriod: b.period.Number,
		PostingDate: b.period.End, Currency: b.le.LocalCurrency, Exponent: b.exp, Contracts: int32(len(contracts))}
	cal := &calendar{source: e.Calendar, company: b.le}
	recognized, billed := make([]money.Minor, len(contracts)), make([]money.Minor, len(contracts))
	assets, liabilities, revenue := make([]money.Minor, len(contracts)), make([]money.Minor, len(contracts)), make([]money.Minor, len(contracts))
	for i, k := range contracts {
		order, _, err := e.SalesOrders.Order(ctx, k.SalesOrderID) // a stored contract's sales order exists
		if err != nil {
			return Run{}, fmt.Errorf("revenue: sales order of contract %s: %w", k.ID, err)
		}
		s, err := cal.scheduleOf(ctx, k, order, b.period.End)
		if err != nil {
			if p := problem.From(err); p.Code == "revenue.no_fiscal_period" {
				return Run{}, problem.Unprocessable("revenue.no_fiscal_period", "Revenue contract of sales order "+k.SalesOrderNumber+": "+p.Detail)
			}
			return Run{}, err
		}
		p, moved := figures(s, order, b.period.End, before[k.ID])
		recognized[i], billed[i] = p.Recognized, p.Billed
		assets[i], liabilities[i] = position(p.Recognized, p.Billed)
		if moved {
			run.Postings = append(run.Postings, p)
			revenue[i] = p.Revenue
		}
	}
	run.Posted = int32(len(run.Postings))
	return totalled(run, recognized, billed, assets, liabilities, revenue)
}

// totalled is the run with the contracts' amounts added up, and the
// movements of revenue of those that moved; sums beyond an amount are
// refused (422 revenue.amount_too_large).
func totalled(run Run, recognized, billed, assets, liabilities, revenue []money.Minor) (Run, error) {
	for _, t := range []struct {
		sum     *money.Minor
		amounts []money.Minor
	}{{&run.Recognized, recognized}, {&run.Billed, billed}, {&run.Asset, assets}, {&run.Liability, liabilities}, {&run.Revenue, revenue}} {
		sum, err := money.Sum(t.amounts...)
		if err != nil {
			return Run{}, problem.Unprocessable("revenue.amount_too_large", "The company's contracts add up to more than an amount holds.").WithCause(err)
		}
		*t.sum = sum
	}
	return run, nil
}

// Recognize runs revenue recognition for the company and the fiscal
// period and posts it, in the request's transaction (ADR-0068 §9): the
// run is kept with its totals, each contract that moved posts one entry
// to every ledger the company keeps, dated the period's last day, and
// keeps its figures, and the run's audit record and RevenueRecognized
// are written with them. The refusals are basisOfRun's and read's, and
// whatever the ledger refuses of an entry.
func (e *Endpoints) Recognize(ctx context.Context, legalEntity uuid.UUID, year, number int32) (Run, tenancy.Entity, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, tenancy.Entity{}, err
	}
	b, err := e.basisOfRun(ctx, tx, p, legalEntity, year, number)
	if err != nil {
		return Run{}, tenancy.Entity{}, err
	}
	run, err := e.read(ctx, tx, b)
	if err != nil {
		return Run{}, tenancy.Entity{}, err
	}
	run.CreatedBy = p.ID
	err = tx.QueryRow(ctx, `INSERT INTO revenue.recognition_run (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, posting_date, currency,
			contract_count, posting_count, recognized_amount, billed_amount, contract_asset_amount, contract_liability_amount, revenue_amount, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING created_at`, p.TenantID, run.ID, run.LegalEntityID, run.FiscalYear,
		run.FiscalPeriod, run.PostingDate, run.Currency, run.Contracts, run.Posted, run.Recognized, run.Billed, run.Asset, run.Liability, run.Revenue,
		run.CreatedBy).Scan(&run.CreatedAt)
	if err != nil {
		return Run{}, tenancy.Entity{}, fmt.Errorf("revenue: keep the recognition run of company %s for %d/%d: %w", b.le.Code, run.FiscalYear, run.FiscalPeriod, err)
	}
	for i := range run.Postings {
		if err := e.post(ctx, tx, p, b, run, &run.Postings[i]); err != nil {
			return Run{}, tenancy.Entity{}, err
		}
	}
	return run, b.le, recordRun(ctx, run, b.le)
}

// post posts the contract's entry and keeps the posting with its
// obligations' figures: the entry is dated the period's last day, in the
// company's local currency, with the sales order's number as its
// reference and the posting as its source document, in every ledger the
// company keeps.
func (e *Endpoints) post(ctx context.Context, tx pgx.Tx, pr reqctx.Principal, b runBasis, run Run, p *Posting) error {
	p.ID = uuid.Must(uuid.NewV7())
	text := fmt.Sprintf("Revenue recognition %d/%d", run.FiscalYear, run.FiscalPeriod)
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: b.le.ID, DocumentType: "SA", DocumentDate: run.PostingDate, PostingDate: run.PostingDate,
		Currency: run.Currency, Reference: &p.SalesOrderNumber, HeaderText: &text, SourceContext: "revenue", SourceDocumentID: p.ID, Lines: entryLines(*p, b)})
	if err != nil {
		return fmt.Errorf("revenue: post the recognition of sales order %s's contract: %w", p.SalesOrderNumber, err)
	}
	p.EntryID, p.EntryYear, p.DocumentNumber = entry.ID, entry.FiscalYear, entry.DocumentNumber
	if _, err := tx.Exec(ctx, `INSERT INTO revenue.recognition_posting (tenant_id, run_id, contract_id, id, recognized_amount, billed_amount, revenue_amount,
			journal_entry_id, journal_entry_year, document_number) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		pr.TenantID, run.ID, p.ContractID, p.ID, p.Recognized, p.Billed, p.Revenue, p.EntryID, p.EntryYear, p.DocumentNumber); err != nil {
		return fmt.Errorf("revenue: keep the recognition posting of contract %s: %w", p.ContractID, err)
	}
	n := len(p.Obligations)
	numbers, accounts := make([]int32, n), make([]uuid.UUID, n)
	recognized, billed, revenue := make([]int64, n), make([]int64, n), make([]int64, n)
	for i, o := range p.Obligations {
		numbers[i], accounts[i], recognized[i], billed[i], revenue[i] = o.No, o.AccountID, int64(o.Recognized), int64(o.Billed), int64(o.Revenue)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO revenue.recognition_posting_obligation (tenant_id, run_id, contract_id, obligation_no, account_id,
			recognized_amount, billed_amount, revenue_amount)
		SELECT $1, $2, $3, o.obligation_no, o.account_id, o.recognized, o.billed, o.revenue
		FROM unnest($4::int[], $5::uuid[], $6::bigint[], $7::bigint[], $8::bigint[]) AS o (obligation_no, account_id, recognized, billed, revenue)`,
		pr.TenantID, run.ID, p.ContractID, numbers, accounts, recognized, billed, revenue); err != nil {
		return fmt.Errorf("revenue: keep the obligations of contract %s's recognition posting: %w", p.ContractID, err)
	}
	return nil
}

// recordRun adds the run's audit record and publishes RevenueRecognized
// in the same transaction (ADR-0007, ADR-0011): the run with its counts
// and totals and what it moved revenue by, not its postings.
func recordRun(ctx context.Context, r Run, le tenancy.Entity) error {
	day := r.PostingDate.Format(time.DateOnly)
	amount := func(m money.Minor) map[string]string { return money.EventAmount(m, r.Currency, r.Exponent) }
	payload := map[string]any{"revenue_recognition_run_id": r.ID, "legal_entity_id": r.LegalEntityID, "legal_entity_code": le.Code,
		"fiscal_year": r.FiscalYear, "fiscal_period": r.FiscalPeriod, "posting_date": day, "contract_count": r.Contracts, "posting_count": r.Posted,
		"recognized_amount": amount(r.Recognized), "billed_amount": amount(r.Billed), "contract_asset_amount": amount(r.Asset),
		"contract_liability_amount": amount(r.Liability), "revenue_amount": amount(r.Revenue)}
	entries := make([]map[string]any, len(r.Postings))
	for i, p := range r.Postings {
		entries[i] = map[string]any{"revenue_contract_id": p.ContractID, "sales_order_number": p.SalesOrderNumber, "journal_entry_id": p.EntryID,
			"journal_entry_number": p.DocumentNumber, "revenue_amount": p.Revenue.Plain(r.Exponent)}
	}
	detail := map[string]any{"legal_entity_id": r.LegalEntityID, "fiscal_year": r.FiscalYear, "fiscal_period": r.FiscalPeriod, "posting_date": day,
		"currency": r.Currency, "contract_count": r.Contracts, "posting_count": r.Posted, "recognized_amount": r.Recognized.Plain(r.Exponent),
		"billed_amount": r.Billed.Plain(r.Exponent), "contract_asset_amount": r.Asset.Plain(r.Exponent),
		"contract_liability_amount": r.Liability.Plain(r.Exponent), "revenue_amount": r.Revenue.Plain(r.Exponent), "postings": entries}
	if err := audit.Record(ctx, "revenue.recognition_run.create", "revenue.recognition_run", r.ID, detail,
		outbox.Event{Context: "revenue", Name: "revenue_recognized", AggregateType: "revenue.recognition_run", AggregateID: r.ID, Payload: payload}); err != nil {
		return fmt.Errorf("revenue: record recognition run %s: %w", r.ID, err)
	}
	return nil
}
