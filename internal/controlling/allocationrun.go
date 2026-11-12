package controlling

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Runs of allocation cycles (ADR-0064 §5, T60f). A distribution's run for
// a fiscal period reads the sender's primary costs of the period per
// account and posts one entry through the ledger's port: per account the
// sender credited with its cost and each receiver debited with its share
// on the same account, the shares split with money.Allocate so they add
// up to the cost to the minor unit. The cycle is held FOR UPDATE from
// before the run is looked for until it is kept, so there is one run per
// cycle and period; the database holds that too.

func init() {
	// A code is checked by its constraint alone. A second run is refused by
	// the application, which holds the cycle FOR UPDATE; the constraint
	// backs that up for any other writer.
	problem.RegisterConstraint("allocation_cycle_code_once", func() *problem.Problem {
		return problem.Conflict("controlling.allocation_cycle_exists", "The legal entity already has an allocation cycle with this code.")
	})
	problem.RegisterConstraint("allocation_run_once", func() *problem.Problem {
		return problem.Conflict("controlling.allocation_run_exists", "The allocation cycle was already run for this period.")
	})
}

// allocationRun is a row of controlling.allocation_run with its cycle's
// code, name, kind and sender; SenderCode is named by the ledger's port
// when the run is shown.
type allocationRun struct {
	ID, CycleID, LegalEntityID uuid.UUID
	CycleCode, CycleName, Kind string
	FiscalYear, FiscalPeriod   int16
	PostingDate                time.Time
	SenderID                   uuid.UUID
	SenderCode                 string
	EntryID                    uuid.UUID
	DocumentNumber             string
	Amount                     money.Minor
	Accounts                   int32
	CreatedAt                  time.Time
	CreatedBy                  uuid.UUID
}

// runSelect reads runs with their cycles, as one relation r: a listing's
// cursor names the run's id without a table (pagination.Keyset).
const runSelect = `SELECT r.id, r.cycle_id, r.legal_entity_id, r.cycle_code, r.cycle_name, r.kind, r.fiscal_year, r.fiscal_period, r.posting_date,
	r.sender_cost_center_id, r.journal_entry_id, r.document_number, r.amount, r.account_count, r.created_at, r.created_by
	FROM (SELECT a.id, a.cycle_id, a.legal_entity_id, c.code, c.name, c.kind, a.fiscal_year, a.fiscal_period, a.posting_date, c.sender_cost_center_id,
			a.journal_entry_id, a.document_number, a.amount, a.account_count, a.created_at, a.created_by
		FROM controlling.allocation_run a JOIN controlling.allocation_cycle c ON c.tenant_id = a.tenant_id AND c.id = a.cycle_id)
		AS r(id, cycle_id, legal_entity_id, cycle_code, cycle_name, kind, fiscal_year, fiscal_period, posting_date, sender_cost_center_id,
			journal_entry_id, document_number, amount, account_count, created_at, created_by)`

func scanRun(r pgx.Row) (allocationRun, error) {
	var a allocationRun
	err := r.Scan(&a.ID, &a.CycleID, &a.LegalEntityID, &a.CycleCode, &a.CycleName, &a.Kind, &a.FiscalYear, &a.FiscalPeriod, &a.PostingDate, &a.SenderID,
		&a.EntryID, &a.DocumentNumber, &a.Amount, &a.Accounts, &a.CreatedAt, &a.CreatedBy)
	return a, errs.Wrapf(err, "controlling: scan allocation run")
}

func runToAPI(r allocationRun, currency string, exp int) gen.AllocationRun {
	return gen.AllocationRun{ID: r.ID, AllocationCycleID: r.CycleID, AllocationCycleCode: r.CycleCode, AllocationCycleName: r.CycleName, Kind: r.Kind,
		LegalEntityID: r.LegalEntityID, FiscalYear: int32(r.FiscalYear), FiscalPeriod: int32(r.FiscalPeriod), PostingDate: r.PostingDate,
		SenderCostCenterID: r.SenderID, SenderCostCenterCode: r.SenderCode, Amount: apitypes.Money(r.Amount, currency, exp), AccountCount: r.Accounts,
		JournalEntryID: r.EntryID, JournalEntryNumber: r.DocumentNumber, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy}
}

// accountCost is the sender's cost of the period on a primary cost
// account, debit positive.
type accountCost struct {
	AccountID uuid.UUID
	Number    string
	Amount    money.Minor
}

// costsPage is how many rows of the sender's actuals one read takes.
const costsPage = 500

// senderCosts are the sender's costs of the period per primary cost
// account, the accounts whose cost is not zero, by account number: what
// the cost center report shows for the center in the period (the ledger's
// actuals, exact at the read), less the accounts that are no primary cost
// accounts (revenue).
func (e *Endpoints) senderCosts(ctx context.Context, le, sender uuid.UUID, period ledger.FiscalPeriod) ([]accountCost, error) {
	q := ledger.ActualsQuery{LegalEntityID: le, FiscalYear: period.FiscalYear, PeriodFrom: period.Number, PeriodTo: period.Number,
		Centers: []uuid.UUID{sender}, Limit: costsPage}
	var actuals []ledger.AssignmentActual
	for {
		page, err := e.Ledger.CostCenterActuals(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("controlling: costs of cost center %s in %d/%d: %w", sender, period.FiscalYear, period.Number, err)
		}
		actuals = append(actuals, page...)
		if len(page) < costsPage {
			break
		}
		q.After = &page[len(page)-1].CenterAccount
	}
	ids := make([]uuid.UUID, len(actuals))
	for i, a := range actuals {
		ids[i] = a.AccountID
	}
	accounts, err := e.Ledger.CostAccounts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("controlling: accounts of cost center %s's costs: %w", sender, err)
	}
	var costs []accountCost
	for _, a := range actuals {
		if acc := accounts[a.AccountID]; acc.PrimaryCost && a.Amount != 0 {
			costs = append(costs, accountCost{AccountID: a.AccountID, Number: acc.Number, Amount: a.Amount})
		}
	}
	slices.SortFunc(costs, func(a, b accountCost) int {
		return cmp.Or(strings.Compare(a.Number, b.Number), strings.Compare(a.AccountID.String(), b.AccountID.String()))
	})
	return costs, nil
}

// distributionLines are a distribution's lines, account by account in
// the order the costs came in: the sender credited with the account's
// cost and each receiver, in the cycle's order, debited with its share
// of it, a receiver whose share rounds to nothing left out; and the total
// moved. An account's shares are its cost times each receiver's part of
// the whole, divided among them by largest remainder (money.Allocate),
// so they add up to the cost to the minor unit and the entry balances
// per account.
func distributionLines(c cycle, costs []accountCost) ([]ledger.PostingLine, money.Minor, error) {
	var lines []ledger.PostingLine
	amounts := make([]money.Minor, len(costs))
	for i, cost := range costs {
		amounts[i] = cost.Amount
		exact := make([]*big.Rat, len(c.Receivers))
		for j, r := range c.Receivers {
			exact[j] = new(big.Rat).Mul(big.NewRat(int64(cost.Amount), 1), big.NewRat(int64(r.ShareBP), wholeBP))
		}
		shares, err := money.Allocate(exact)
		if err != nil {
			return nil, 0, fmt.Errorf("controlling: shares of account %s in allocation cycle %s: %w", cost.Number, c.ID, err)
		}
		lines = append(lines, ledger.PostingLine{AccountID: cost.AccountID, Amount: -cost.Amount, CostCenterID: &c.SenderID})
		for j, share := range shares {
			if share != 0 {
				lines = append(lines, ledger.PostingLine{AccountID: cost.AccountID, Amount: share, CostCenterID: &c.Receivers[j].CostCenterID})
			}
		}
	}
	total, err := money.Sum(amounts...)
	if err != nil {
		return nil, 0, fmt.Errorf("controlling: total of allocation cycle %s: %w", c.ID, err)
	}
	return lines, total, nil
}

// centersValid refuses a run whose sender or a receiver is not valid on
// the posting date, naming it, so that it is refused as the cycle's and
// not as a line of an entry the caller never wrote; the ledger checks the
// centers again under its locks.
func (e *Endpoints) centersValid(ctx context.Context, c cycle, day time.Time) error {
	centers := append([]cycleReceiver{{CostCenterID: c.SenderID, Code: c.SenderCode}}, c.Receivers...)
	for _, center := range centers {
		valid, err := e.Ledger.CostCenterValidOn(ctx, center.CostCenterID, day)
		if err != nil {
			return fmt.Errorf("controlling: cost center %s of allocation cycle %s: %w", center.CostCenterID, c.ID, err)
		}
		if !valid {
			return problem.Conflict("controlling.allocation_center_not_valid", "Cost center "+center.Code+" of the cycle is not valid on "+
				day.Format(time.DateOnly)+", the period's last day.")
		}
	}
	return nil
}

// runOf is the cycle's run of the period, if it has one.
func runOf(ctx context.Context, tx pgx.Tx, cycle uuid.UUID, period ledger.FiscalPeriod) (allocationRun, bool, error) {
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.cycle_id = $1 AND r.fiscal_year = $2 AND r.fiscal_period = $3`, cycle, period.FiscalYear, period.Number))
	if errors.Is(err, pgx.ErrNoRows) {
		return allocationRun{}, false, nil
	}
	if err != nil {
		return allocationRun{}, false, fmt.Errorf("controlling: run of allocation cycle %s in %d/%d: %w", cycle, period.FiscalYear, period.Number, err)
	}
	return r, true, nil
}

// RunAllocationCycle implements POST /v1/allocation-cycles/{id}:run, at
// the cycle's version the caller read.
func (e *Endpoints) RunAllocationCycle(ctx context.Context, req *gen.AllocationCycleRun, params gen.RunAllocationCycleParams) (gen.RunAllocationCycleRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, le, exp, err := e.cycleInCompany(ctx, tx, params.ID, true)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(c.Version); err != nil {
		return nil, err
	}
	period, err := e.regularPeriod(ctx, le.ID, req.FiscalYear, req.FiscalPeriod)
	if err != nil {
		return nil, err
	}
	if earlier, ran, err := runOf(ctx, tx, c.ID, period); err != nil {
		return nil, err
	} else if ran {
		return nil, problem.Conflict("controlling.allocation_run_exists", fmt.Sprintf("The allocation cycle was already run for %d/%d: journal entry %s.",
			period.FiscalYear, period.Number, earlier.DocumentNumber))
	}
	res := gen.AllocationRunResult{AllocationCycleID: c.ID, AllocationCycleCode: c.Code, AllocationCycleName: c.Name, LegalEntityID: le.ID,
		FiscalYear: req.FiscalYear, FiscalPeriod: req.FiscalPeriod}
	costs, err := e.senderCosts(ctx, le.ID, c.SenderID, period)
	if err != nil {
		return nil, err
	}
	if len(costs) == 0 {
		return &gen.AllocationRunResultHeaders{Response: res}, nil
	}
	run, err := e.distribute(ctx, tx, p, c, le, period, costs)
	if err != nil {
		return nil, err
	}
	res.Posted = true
	res.Run.SetTo(runToAPI(run, le.LocalCurrency, exp))
	return &gen.AllocationRunResultHeaders{Response: res}, recordRun(ctx, run, le.LocalCurrency, exp)
}

// distribute posts the sender's costs to the receivers on the period's
// last day and keeps the run.
func (e *Endpoints) distribute(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c cycle, le tenancy.Entity, period ledger.FiscalPeriod,
	costs []accountCost) (allocationRun, error) {
	if err := e.centersValid(ctx, c, period.End); err != nil {
		return allocationRun{}, err
	}
	lines, total, err := distributionLines(c, costs)
	if err != nil {
		return allocationRun{}, err
	}
	id := uuid.Must(uuid.NewV7())
	text := "Distribution " + c.Code
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: le.ID, DocumentType: "SA", DocumentDate: period.End, PostingDate: period.End,
		Currency: le.LocalCurrency, Reference: &c.Code, HeaderText: &text, SourceContext: "controlling", SourceDocumentID: id, Lines: lines})
	if err != nil {
		return allocationRun{}, fmt.Errorf("controlling: post the run of allocation cycle %s: %w", c.ID, err)
	}
	run := allocationRun{ID: id, CycleID: c.ID, LegalEntityID: le.ID, CycleCode: c.Code, CycleName: c.Name, Kind: c.Kind, FiscalYear: period.FiscalYear,
		FiscalPeriod: period.Number, PostingDate: period.End, SenderID: c.SenderID, SenderCode: c.SenderCode, EntryID: entry.ID,
		DocumentNumber: entry.DocumentNumber, Amount: total, Accounts: int32(len(costs)), CreatedBy: p.ID}
	err = tx.QueryRow(ctx, `INSERT INTO controlling.allocation_run (tenant_id, id, cycle_id, legal_entity_id, fiscal_year, fiscal_period, posting_date,
			journal_entry_id, document_number, amount, account_count, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING created_at`, p.TenantID, run.ID, run.CycleID, run.LegalEntityID, run.FiscalYear,
		run.FiscalPeriod, run.PostingDate, run.EntryID, run.DocumentNumber, run.Amount, run.Accounts, run.CreatedBy).Scan(&run.CreatedAt)
	if err != nil {
		return allocationRun{}, fmt.Errorf("controlling: keep the run of allocation cycle %s: %w", c.ID, err)
	}
	return run, nil
}

// recordRun audits a run and publishes it.
func recordRun(ctx context.Context, r allocationRun, currency string, exp int) error {
	day := r.PostingDate.Format(time.DateOnly)
	detail := map[string]any{"allocation_run_id": r.ID, "legal_entity_id": r.LegalEntityID, "code": r.CycleCode, "kind": r.Kind, "fiscal_year": r.FiscalYear,
		"fiscal_period": r.FiscalPeriod, "posting_date": day, "sender_cost_center_id": r.SenderID, "sender_cost_center_code": r.SenderCode,
		"amount": r.Amount.Plain(exp), "currency": currency, "account_count": r.Accounts, "journal_entry_id": r.EntryID, "journal_entry_number": r.DocumentNumber}
	return audit.Record(ctx, "controlling.allocation_cycle.run", "controlling.allocation_cycle", r.CycleID, detail,
		outbox.Event{Context: "controlling", Name: "allocation_run_posted", AggregateType: "controlling.allocation_cycle", AggregateID: r.CycleID,
			Payload: map[string]any{"allocation_run_id": r.ID, "allocation_cycle_id": r.CycleID, "legal_entity_id": r.LegalEntityID, "code": r.CycleCode,
				"name": r.CycleName, "kind": r.Kind, "fiscal_year": r.FiscalYear, "fiscal_period": r.FiscalPeriod, "posting_date": day,
				"sender_cost_center_id": r.SenderID, "sender_cost_center_code": r.SenderCode, "amount": money.EventAmount(r.Amount, currency, exp),
				"account_count": r.Accounts, "journal_entry_id": r.EntryID, "journal_entry_number": r.DocumentNumber}})
}

// shownRuns names the runs' senders and gives each its company's
// currency: runs are listed across the companies the caller may act in.
func (e *Endpoints) shownRuns(ctx context.Context, tx pgx.Tx, runs []allocationRun) (func(allocationRun) gen.AllocationRun, error) {
	senders := make([]uuid.UUID, len(runs))
	type unit struct {
		currency string
		exp      int
	}
	units := map[uuid.UUID]unit{}
	for i, r := range runs {
		senders[i] = r.SenderID
		if _, ok := units[r.LegalEntityID]; ok {
			continue
		}
		le, exp, _, err := e.company(ctx, tx, r.LegalEntityID) // the runs read are of companies the caller may act in
		if err != nil {
			return nil, fmt.Errorf("controlling: company of allocation run %s: %w", r.ID, err)
		}
		units[r.LegalEntityID] = unit{le.LocalCurrency, exp}
	}
	refs, err := e.Ledger.CostCenterRefs(ctx, senders)
	if err != nil {
		return nil, fmt.Errorf("controlling: senders of allocation runs: %w", err)
	}
	return func(r allocationRun) gen.AllocationRun {
		r.SenderCode = refs[r.SenderID].Code
		return runToAPI(r, units[r.LegalEntityID].currency, units[r.LegalEntityID].exp)
	}, nil
}

// GetAllocationRun implements GET /v1/allocation-runs/{id}.
func (e *Endpoints) GetAllocationRun(ctx context.Context, params gen.GetAllocationRunParams) (gen.GetAllocationRunRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, params.ID))
	if err := authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("allocation run"), "controlling: read allocation run %s", params.ID); err != nil {
		return nil, err
	}
	shown, err := e.shownRuns(ctx, tx, []allocationRun{r})
	if err != nil {
		return nil, err
	}
	out := shown(r)
	return &out, nil
}

var runSort = pagination.Sort{Default: "posting_date", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "posting_date", Column: "r.posting_date", Type: "date"}}}

// ListAllocationRuns implements GET /v1/allocation-runs.
func (e *Endpoints) ListAllocationRuns(ctx context.Context, params gen.ListAllocationRunsParams) (gen.ListAllocationRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, cycleID := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.AllocationCycleID)
	year, period := apitypes.Ptr[int32](params.FiscalYear), apitypes.Ptr[int32](params.FiscalPeriod)
	scope := fmt.Sprintf("listAllocationRuns|%v|%v|%d|%d", le, cycleID, params.FiscalYear.Value, params.FiscalPeriod.Value)
	ks, err := e.Pages.Keyset(runSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(runSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR r.cycle_id = $4) AND ($5::int IS NULL OR r.fiscal_year = $5) AND ($6::int IS NULL OR r.fiscal_period = $6)`,
		[]any{access.All, access.IDs, le, cycleID, year, period})
	rows, _ := tx.Query(ctx, q, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (allocationRun, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("controlling: list allocation runs: %w", err)
	}
	shown, err := e.shownRuns(ctx, tx, runs)
	if err != nil {
		return nil, err
	}
	items, next := apitypes.PageOf(ks, runs, func(r allocationRun) (string, uuid.UUID) { return r.PostingDate.Format(time.DateOnly), r.ID }, shown)
	return &gen.AllocationRunPage{Items: items, NextPageToken: next}, nil
}
