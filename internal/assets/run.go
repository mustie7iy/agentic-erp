package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Depreciation run statuses (ADR-0057 §5, §7).
const (
	RunQueued  = "queued"
	RunRunning = "running"
	RunPosted  = "posted"
	RunFailed  = "failed"
)

// Batch statuses.
const (
	batchPending = "pending"
	batchPosted  = "posted"
)

// documentTypeAF is the journal entries' document type of depreciation
// runs (number range journal_af, ADR-0057 §3).
const documentTypeAF = "AF"

// batchSize is the most assets a run's batch posts in one entry (decided
// by default 12).
const batchSize = 500

func init() {
	// Two runs of a company and ledger started at once: the second's
	// insert meets the first's queued row (depreciation_run_one_at_a_time).
	problem.RegisterConstraint("depreciation_run_one_at_a_time", func() *problem.Problem {
		return problem.Conflict("assets.run_in_progress", "Another depreciation run of the company and ledger was started at the same time; "+
			"it is queued or running (GET /v1/depreciation-runs?status=queued).")
	})
}

// Run is a stored depreciation run with its batches.
type Run struct {
	ID, LegalEntityID, LedgerID, OperationID uuid.UUID
	FiscalYear, Period                       int16
	PostingDate                              time.Time
	Currency, Status                         string
	Planned, Posted, Skipped                 *int32
	Amount                                   *money.Minor
	Error                                    *RunError
	Version                                  int32
	CreatedAt, UpdatedAt                     time.Time
	CreatedBy                                uuid.UUID
	Batches                                  []Batch
}

// RunError is the problem that failed a run: its code and detail.
type RunError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Batch is a run's batch: its assets' count, and once posted what it
// posted with its journal entry (none when no asset had a difference).
type Batch struct {
	No, Assets      int32
	Status          string
	Amount          *money.Minor
	Posted, Skipped *int32
	EntryYear       *int16
	EntryID         *uuid.UUID
	DocumentNumber  *string
}

// runPath is a run's resource path.
func runPath(id uuid.UUID) string { return "/v1/depreciation-runs/" + id.String() }

// runTarget is what a run (or its preview) of the company, ledger and
// period posts in: the company the caller may act in (404 otherwise), the
// ledger it keeps (422 ledger.ledger_not_kept otherwise; nil: the leading
// ledger) and the regular period (periodOf).
func (e *Endpoints) runTarget(ctx context.Context, le uuid.UUID, ledgerID *uuid.UUID, year, period int32) (Company, LedgerRef, Period, error) {
	co, err := e.company(ctx, le)
	if err != nil {
		return Company{}, LedgerRef{}, Period{}, err
	}
	l, ok, err := e.ledgerOf(ctx, co.ID, ledgerID)
	if err != nil {
		return Company{}, LedgerRef{}, Period{}, err
	}
	if !ok {
		return Company{}, LedgerRef{}, Period{}, invalid("ledger.ledger_not_kept", fmt.Sprintf("Company %s does not keep this ledger.", co.Code), "ledger_id",
			"a ledger the company keeps")
	}
	p, err := e.periodOf(ctx, co.ID, int16(year), int16(period))
	return co, l, p, err
}

// StartDepreciationRun implements POST /v1/depreciation-runs (ADR-0057 §7):
// the run of a company, ledger and regular period, written queued with its
// Operation, which the workflows dispatcher (or the in-process runner)
// carries out.
func (e *Endpoints) StartDepreciationRun(ctx context.Context, req *gen.DepreciationRunStart, _ gen.StartDepreciationRunParams) (gen.StartDepreciationRunRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	co, l, per, err := e.runTarget(ctx, req.LegalEntityID, apitypes.Ptr[uuid.UUID](req.LedgerID), req.FiscalYear, req.Period)
	if err != nil {
		return nil, err
	}
	if err := e.Ledger.PeriodOpen(ctx, co.ID, l.ID, per.FiscalYear, per.Number); err != nil {
		return nil, fmt.Errorf("assets: period %d/%d in ledger %s: %w", per.FiscalYear, per.Number, l.Code, err)
	}
	if err := runsAllow(ctx, tx, co, l, per); err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	op, err := e.Runs.Start(ctx, runPath(id), req)
	if err != nil {
		return nil, fmt.Errorf("assets: operation of depreciation run %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.depreciation_run (tenant_id, id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, posting_date,
			currency, status, operation_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', $9, $10, $10)`,
		p.TenantID, id, co.ID, l.ID, per.FiscalYear, per.Number, per.End, co.Currency, op.ID, p.ID); err != nil {
		return nil, fmt.Errorf("assets: insert depreciation run of %s %d/%d: %w", co.Code, per.FiscalYear, per.Number, err)
	}
	// An audit record that cannot be added fails the request, whose
	// response is then the error's.
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: op},
		audit.Record(ctx, "assets.depreciation_run.start", "assets.depreciation_run", id, map[string]any{"legal_entity_id": co.ID,
			"company_code": co.Code, "ledger_id": l.ID, "ledger_code": l.Code, "fiscal_year": per.FiscalYear, "period": per.Number,
			"posting_date": per.End.Format(time.DateOnly), "operation_id": op.ID})
}

// runsAllow refuses a run while another of the company and ledger is
// queued or running (409 assets.run_in_progress, naming it) and one of a
// period before the latest posted run's in the ledger (409
// assets.run_out_of_order): runs post in period order, each catching up
// what the periods before it left (ADR-0057 §7).
func runsAllow(ctx context.Context, tx pgx.Tx, co Company, l LedgerRef, per Period) error {
	var other uuid.UUID
	var year, period int16
	var status string
	err := tx.QueryRow(ctx, `SELECT id, fiscal_year, fiscal_period, status FROM assets.depreciation_run
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND status IN ('queued', 'running')`, co.ID, l.ID).Scan(&other, &year, &period, &status)
	switch {
	case err == nil:
		return problem.Conflict("assets.run_in_progress", fmt.Sprintf("Depreciation run %s of %d/%d (%s) is %s in company %s and ledger %s; one run of "+
			"a company and ledger runs at a time.", runPath(other), year, period, l.Code, status, co.Code, l.Code))
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("assets: depreciation runs in progress of %s in %s: %w", co.Code, l.Code, err)
	}
	err = tx.QueryRow(ctx, `SELECT fiscal_year, fiscal_period FROM assets.depreciation_run WHERE legal_entity_id = $1 AND ledger_id = $2
		AND status = 'posted' ORDER BY fiscal_year DESC, fiscal_period DESC LIMIT 1`, co.ID, l.ID).Scan(&year, &period)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("assets: latest depreciation run of %s in %s: %w", co.Code, l.Code, err)
	case year > per.FiscalYear || (year == per.FiscalYear && period > per.Number):
		return problem.Conflict("assets.run_out_of_order", fmt.Sprintf("Depreciation of %d/%d is posted in ledger %s of company %s; a run of an earlier "+
			"period is not started after it, and the next run catches up what an earlier period left.", year, period, l.Code, co.Code))
	}
	return nil
}

// runSelect reads runs.
const runSelect = `SELECT id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, posting_date, currency, status, operation_id, assets_planned,
		assets_posted, assets_skipped, amount, error, version, created_at, created_by, updated_at
	FROM assets.depreciation_run`

func scanRun(r pgx.Row) (Run, error) {
	var x Run
	var failure []byte
	err := r.Scan(&x.ID, &x.LegalEntityID, &x.LedgerID, &x.FiscalYear, &x.Period, &x.PostingDate, &x.Currency, &x.Status, &x.OperationID,
		&x.Planned, &x.Posted, &x.Skipped, &x.Amount, &failure, &x.Version, &x.CreatedAt, &x.CreatedBy, &x.UpdatedAt)
	if err == nil && failure != nil {
		x.Error = &RunError{}
		// The run wrote it from a RunError (FailDepreciationRun); a document
		// that does not decode shows as empty rather than failing the read.
		_ = json.Unmarshal(failure, x.Error)
	}
	return x, errs.Wrapf(err, "scan depreciation run")
}

// readRun reads a run of a legal entity the caller may act in with its
// batches; any other is not found.
func readRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Run, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(r.LegalEntityID)):
		return Run{}, problem.NotFoundOf("depreciation run")
	case err != nil:
		return Run{}, fmt.Errorf("assets: depreciation run %s: %w", id, err)
	}
	batches, err := readBatches(ctx, tx, []uuid.UUID{id})
	r.Batches = batches[id]
	return r, err
}

// readBatches reads the batches of runs in order, by run.
func readBatches(ctx context.Context, tx pgx.Tx, runs []uuid.UUID) (map[uuid.UUID][]Batch, error) {
	rows, _ := tx.Query(ctx, `SELECT run_id, batch_no, cardinality(asset_ids), status, amount, assets_posted, assets_skipped, journal_entry_year,
			journal_entry_id, document_number
		FROM assets.depreciation_batch WHERE run_id = ANY($1) ORDER BY run_id, batch_no`, runs)
	out := map[uuid.UUID][]Batch{}
	var run uuid.UUID
	var b Batch
	_, err := pgx.ForEachRow(rows, []any{&run, &b.No, &b.Assets, &b.Status, &b.Amount, &b.Posted, &b.Skipped, &b.EntryYear, &b.EntryID, &b.DocumentNumber},
		func() error {
			out[run] = append(out[run], b)
			b = Batch{} // the next row's pointers are scanned into a new value
			return nil
		})
	return out, errs.Wrapf(err, "assets: batches of %d depreciation runs", len(runs))
}

// GetDepreciationRun implements GET /v1/depreciation-runs/{id}.
func (e *Endpoints) GetDepreciationRun(ctx context.Context, params gen.GetDepreciationRunParams) (gen.GetDepreciationRunRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	v, err := e.runView(ctx, tx, []Run{r})
	if err != nil {
		return nil, err
	}
	return &gen.DepreciationRunHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: v.runAPI(r)}, nil
}

var runSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListDepreciationRuns implements GET /v1/depreciation-runs: the runs of
// the legal entities the caller may act in, filtered by company, ledger,
// fiscal year and status.
func (e *Endpoints) ListDepreciationRuns(ctx context.Context, params gen.ListDepreciationRunsParams) (gen.ListDepreciationRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, ledger, year := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.LedgerID), apitypes.Ptr[int32](params.FiscalYear)
	scope := fmt.Sprintf("listDepreciationRuns|%s|%s|%t %d|%s", le, ledger, params.FiscalYear.Set, params.FiscalYear.Value, params.Status.Value)
	ks, err := e.Pages.Keyset(runSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT * FROM (`+runSelect+`) r WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR legal_entity_id = $3)
		AND ($4::uuid IS NULL OR ledger_id = $4) AND ($5::smallint IS NULL OR fiscal_year = $5) AND ($6 = '' OR status = $6)`,
		[]any{access.All, access.IDs, le, ledger, year, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("assets: list depreciation runs: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, rs, func(r Run) (string, uuid.UUID) { return r.CreatedAt.UTC().Format(time.RFC3339Nano), r.ID },
		func(page []Run) (docView, error) { return e.runPage(ctx, tx, page) }, func(r Run, v docView) gen.DepreciationRun { return v.runAPI(r) })
	if err != nil {
		return nil, err
	}
	return &gen.DepreciationRunPage{Items: items, NextPageToken: next}, nil
}

// runPage reads the batches of a page's runs into them and what they are
// shown with.
func (e *Endpoints) runPage(ctx context.Context, tx pgx.Tx, page []Run) (docView, error) {
	ids := make([]uuid.UUID, len(page))
	for i, r := range page {
		ids[i] = r.ID
	}
	batches, err := readBatches(ctx, tx, ids)
	if err != nil {
		return docView{}, err
	}
	for i := range page {
		page[i].Batches = batches[page[i].ID]
	}
	return e.runView(ctx, tx, page)
}

// runView reads what runs are shown with: their companies, ledgers and
// currencies.
func (e *Endpoints) runView(ctx context.Context, tx pgx.Tx, rs []Run) (docView, error) {
	companies, currencies := make([]uuid.UUID, len(rs)), make([]string, len(rs))
	for i, r := range rs {
		companies[i], currencies[i] = r.LegalEntityID, r.Currency
	}
	return e.docViewOf(ctx, tx, companies, currencies, nil)
}

// optCount is a count once known.
func optCount(n *int32) gen.OptInt32 {
	if n == nil {
		return gen.OptInt32{}
	}
	return gen.NewOptInt32(*n)
}

// runAPI shows a run with its batches.
func (v docView) runAPI(r Run) gen.DepreciationRun {
	amount := func(m *money.Minor) gen.OptMoney {
		if m == nil {
			return gen.OptMoney{}
		}
		return gen.NewOptMoney(v.money(*m, r.Currency))
	}
	out := gen.DepreciationRun{ID: r.ID, LegalEntityID: r.LegalEntityID, CompanyCode: v.companies[r.LegalEntityID].Code, LedgerID: r.LedgerID,
		FiscalYear: int32(r.FiscalYear), Period: int32(r.Period), PostingDate: r.PostingDate, Currency: r.Currency, Status: r.Status,
		OperationID: r.OperationID, AssetsPlanned: optCount(r.Planned), AssetsPosted: optCount(r.Posted), AssetsSkipped: optCount(r.Skipped),
		Amount: amount(r.Amount), Batches: make([]gen.DepreciationRunBatch, len(r.Batches)), Version: r.Version, CreatedAt: r.CreatedAt,
		CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt}
	v.nameLedger(r.LedgerID, &out.LedgerCode, &out.LedgerName)
	if r.Error != nil {
		out.Error = gen.NewOptDepreciationRunError(gen.DepreciationRunError{Code: r.Error.Code, Detail: r.Error.Detail})
		amount, posted, skipped := postedTotals(r.Batches)
		out.Amount, out.AssetsPosted, out.AssetsSkipped = gen.NewOptMoney(v.money(amount, r.Currency)), gen.NewOptInt32(posted), gen.NewOptInt32(skipped)
	}
	for i, b := range r.Batches {
		out.Batches[i] = gen.DepreciationRunBatch{BatchNo: b.No, Status: b.Status, Assets: b.Assets, AssetsPosted: optCount(b.Posted),
			AssetsSkipped: optCount(b.Skipped), Amount: amount(b.Amount), JournalEntryID: apitypes.OptUUID(b.EntryID),
			DocumentNumber: apitypes.OptString(b.DocumentNumber)}
		if b.EntryYear != nil {
			out.Batches[i].JournalEntryFiscalYear = gen.NewOptInt32(int32(*b.EntryYear))
		}
	}
	return out
}
