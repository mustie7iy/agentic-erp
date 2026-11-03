package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A depreciation run's work (ADR-0057 §7): four steps, each one
// transaction in the tenant's scope as the run's requester, which the
// workflow DepreciationRun calls as its activities through the
// workflows.Depreciation port (or the in-process runner, without a
// workflow engine). Each is idempotent: run again after a commit whose
// completion was lost, it finds its effect there and does nothing. Each
// of the plan, a batch and the finish first reads the run's Operation
// (liveRun): once it is cancelled the step refuses with 409
// assets.run_cancelled, which the workflow records by failing the run, so
// a cancellation stops the run at its next step and the batches already
// posted stay posted. The plan and a batch, which may run long, read it
// without its lock, so a :cancel during them commits at once (the
// transactions' lock timeout would otherwise answer it 409
// request.in_progress); their closing report leaves a cancelled Operation
// alone. The finish, which succeeds the Operation, reads it under its
// lock.

// lockRun reads a run FOR UPDATE (FOR SHARE with share), whoever may act
// in its company: the run's own work, as its requester.
func lockRun(ctx context.Context, tx pgx.Tx, id uuid.UUID, share bool) (Run, error) {
	lock := " FOR UPDATE"
	if share {
		lock = " FOR SHARE"
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE id = $1`+lock, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, problem.NotFoundOf("depreciation run")
	}
	return r, errs.Wrapf(err, "assets: lock depreciation run %s", id)
}

// liveRun locks a run as lockRun does and, while it is queued or running,
// refuses it once its Operation is cancelled (409 assets.run_cancelled).
// With finishing the Operation is read under its lock, after the run's,
// for a step that finishes the Operation; otherwise as it stands, so a
// :cancel during the step commits at once and stops the run at the next
// one.
func (e *Endpoints) liveRun(ctx context.Context, tx pgx.Tx, id uuid.UUID, share, finishing bool) (Run, error) {
	r, err := lockRun(ctx, tx, id, share)
	if err != nil || (r.Status != RunQueued && r.Status != RunRunning) {
		return r, err
	}
	cancelled, err := e.Runs.Cancelled(ctx, r.OperationID, finishing)
	if err != nil {
		return Run{}, fmt.Errorf("assets: the operation of depreciation run %s: %w", id, err)
	}
	if cancelled {
		return Run{}, problem.Conflict("assets.run_cancelled", fmt.Sprintf("The operation of depreciation run %s was cancelled: the run stopped "+
			"before its next step and failed, the batches already posted staying posted; a new run of the period posts what is left.", id))
	}
	return r, nil
}

// PlanDepreciationRun plans a queued run: it selects the assets the run
// posts (selectDepreciable, as the preview does), writes them in batches
// of batchSize by asset number, sets the run running with the count and
// reports the batches on its Operation. It returns the run's batches in
// order; a run already running returns those it wrote, one posted or
// failed none. A run whose Operation was cancelled is refused (liveRun).
func (e *Endpoints) PlanDepreciationRun(ctx context.Context, id uuid.UUID) ([]int32, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := e.liveRun(ctx, tx, id, false, false)
	if err != nil || r.Status == RunPosted || r.Status == RunFailed {
		return nil, err
	}
	if r.Status == RunQueued {
		if err := e.plan(ctx, tx, p, r); err != nil {
			return nil, err
		}
	}
	rows, _ := tx.Query(ctx, `SELECT batch_no FROM assets.depreciation_batch WHERE run_id = $1 ORDER BY batch_no`, id)
	nos, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	return nos, errs.Wrapf(err, "assets: batches of depreciation run %s", id)
}

// plan writes a queued run's batches and sets it running.
func (e *Endpoints) plan(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) error {
	per, err := e.periodOf(ctx, r.LegalEntityID, r.FiscalYear, r.Period)
	if err != nil {
		return err
	}
	cs, err := selectDepreciable(ctx, tx, r.LegalEntityID, r.LedgerID, per, nil, nil)
	if err != nil {
		return err
	}
	n, size := 0, e.batchSize()
	for i := 0; i < len(cs); i += size {
		ids := make([]uuid.UUID, 0, size)
		for _, c := range cs[i:min(i+size, len(cs))] {
			ids = append(ids, c.asset)
		}
		n++
		if _, err := tx.Exec(ctx, `INSERT INTO assets.depreciation_batch (tenant_id, run_id, batch_no, asset_ids, status) VALUES ($1, $2, $3, $4, 'pending')`,
			p.TenantID, r.ID, n, ids); err != nil {
			return fmt.Errorf("assets: batch %d of depreciation run %s: %w", n, r.ID, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE assets.depreciation_run SET status = 'running', assets_planned = $2, version = version + 1, updated_by = $3
		WHERE id = $1 AND status = 'queued'`, r.ID, len(cs), p.ID); err != nil {
		return fmt.Errorf("assets: plan depreciation run %s: %w", r.ID, err)
	}
	if err := e.Runs.Report(ctx, r.OperationID, 0, int64(n)); err != nil {
		return fmt.Errorf("assets: progress of depreciation run %s: %w", r.ID, err)
	}
	return audit.Record(ctx, "assets.depreciation_run.plan", "assets.depreciation_run", r.ID, map[string]any{"ledger_id": r.LedgerID,
		"fiscal_year": r.FiscalYear, "period": r.Period, "assets_planned": len(cs), "batches": n})
}

// batchSize is the most assets a batch takes: Endpoints.BatchSize, or
// batchSize (decided by default 12).
func (e *Endpoints) batchSize() int {
	if e.BatchSize > 0 {
		return e.BatchSize
	}
	return batchSize
}

// batchWork is a batch being posted: its run, company and period, its
// assets still selectable under their locks with their differences, and
// how many it skipped.
type batchWork struct {
	run     Run
	no      int32
	co      Company
	period  Period
	assets  []*candidate
	skipped int
}

// PostDepreciationBatch posts a pending batch of a running run in one
// transaction (ADR-0057 §7, ADR-0012): it locks the batch and then its
// assets FOR UPDATE in id order, re-checks each one's selection under its
// lock (one no longer selectable is skipped, takes no seq and is
// counted), posts the remaining assets' differences as one entry in the
// run's ledger alone (none when no asset has one), writes a depreciation
// movement per asset with a difference, gives each remaining asset its
// next seq, books it through the run's period, marks the batch posted and
// reports it on the run's Operation. A batch already posted, or of a run
// no longer running, is left alone; a run whose Operation was cancelled
// is refused (liveRun).
func (e *Endpoints) PostDepreciationBatch(ctx context.Context, id uuid.UUID, no int32) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	w, ok, err := e.batchOf(ctx, tx, id, no)
	if err != nil || !ok {
		return err
	}
	lines, err := e.batchLines(ctx, w)
	if err != nil {
		return err
	}
	var entry *PostedEntry
	if len(lines) > 0 {
		entries, err := e.Ledger.Post(ctx, w.entry(lines))
		if err != nil {
			return fmt.Errorf("assets: post batch %d of depreciation run %s: %w", no, id, err)
		}
		entry = &entries[0] // a posting to one ledger has one entry
	}
	if err := writeBatch(ctx, tx, p, w, entry); err != nil {
		return err
	}
	return e.recordBatch(ctx, tx, w, entry)
}

// batchOf reads a run's pending batch, its run running, with the batch's
// assets locked and still selectable and their differences; ok is false
// for a batch already posted or a run no longer running.
func (e *Endpoints) batchOf(ctx context.Context, tx pgx.Tx, id uuid.UUID, no int32) (batchWork, bool, error) {
	r, err := e.liveRun(ctx, tx, id, true, false)
	if err != nil || r.Status != RunRunning {
		return batchWork{}, false, err
	}
	var status string
	var ids []uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT status, asset_ids FROM assets.depreciation_batch WHERE run_id = $1 AND batch_no = $2 FOR UPDATE`, id, no).
		Scan(&status, &ids); err != nil {
		return batchWork{}, false, fmt.Errorf("assets: lock batch %d of depreciation run %s: %w", no, id, err)
	}
	if status == batchPosted {
		return batchWork{}, false, nil
	}
	if _, err := tx.Exec(ctx, `SELECT FROM assets.asset WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids); err != nil {
		return batchWork{}, false, fmt.Errorf("assets: lock the assets of batch %d of depreciation run %s: %w", no, id, err)
	}
	w := batchWork{run: r, no: no}
	if w.co, err = e.companyOf(ctx, r.LegalEntityID); err != nil {
		return batchWork{}, false, err
	}
	if w.period, err = e.periodOf(ctx, r.LegalEntityID, r.FiscalYear, r.Period); err != nil {
		return batchWork{}, false, err
	}
	if w.assets, err = selectDepreciable(ctx, tx, r.LegalEntityID, r.LedgerID, w.period, nil, ids); err != nil {
		return batchWork{}, false, err
	}
	w.skipped = len(ids) - len(w.assets)
	return w, true, differences(ctx, tx, r.LedgerID, w.period, w.assets)
}

// companyOf is a run's company, which was checked when the run started.
func (e *Endpoints) companyOf(ctx context.Context, le uuid.UUID) (Company, error) {
	// Legal entities are never removed, so the run's is found.
	co, _, err := e.Companies.Company(ctx, le)
	return co, errs.Wrapf(err, "assets: legal entity %s", le)
}

// batchLines translates each difference of the batch into group currency
// at the posting date's rate (ADR-0057 §7) and returns the lines the batch
// posts on its assets' classes' accounts and centers (runLines).
func (e *Endpoints) batchLines(ctx context.Context, w batchWork) ([]previewLine, error) {
	var posting []*candidate
	var lc []money.Minor
	for _, c := range w.assets {
		if c.difference != 0 {
			posting, lc = append(posting, c), append(lc, c.difference)
		}
	}
	if len(posting) == 0 {
		return nil, nil
	}
	group, err := e.Companies.GroupCurrency(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: group currency: %w", err)
	}
	gc, err := e.Ledger.GroupAmounts(ctx, w.co.Currency, group, w.period.End, lc)
	if err != nil {
		return nil, fmt.Errorf("assets: group amounts of batch %d of depreciation run %s: %w", w.no, w.run.ID, err)
	}
	for i, c := range posting {
		c.differenceGC = gc[i]
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: configuration: %w", err)
	}
	accounts, err := e.depreciationAccounts(ctx, doc, previewRun{co: w.co, assets: posting})
	if err != nil {
		return nil, err
	}
	return runLines(posting, accounts), nil
}

// entry is the batch's journal entry (ADR-0057 §7): in local currency, to
// the run's ledger alone, document type AF, dated the period's last day,
// the run as its source document, each line keeping its local and group
// amounts.
func (w batchWork) entry(lines []previewLine) Posting {
	text := fmt.Sprintf("Depreciation %d/%02d, batch %d", w.run.FiscalYear, w.run.Period, w.no)
	out := Posting{LegalEntityID: w.co.ID, LedgerID: &w.run.LedgerID, DocumentType: documentTypeAF, PostingDate: w.period.End, DocumentDate: w.period.End,
		Currency: w.co.Currency, HeaderText: &text, DocumentID: w.run.ID, Lines: make([]PostingLine, len(lines))}
	for i, l := range lines {
		lc, gc := l.amount, l.gc
		out.Lines[i] = PostingLine{AccountID: l.account.ID, Amount: l.amount, LC: &lc, GC: &gc, CostCenterID: idOrNil(l.costCenter),
			ProfitCenterID: idOrNil(l.profitCenter), Text: &text}
	}
	return out
}

// idOrNil is the id, nil for uuid.Nil (none).
func idOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// writeBatch gives each asset the batch processed its next seq, books it
// through the run's period in the ledger, writes a depreciation movement
// for each with a difference on the batch's entry, and marks the batch
// posted with what it posted (ADR-0057 §5, §7).
func writeBatch(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w batchWork, entry *PostedEntry) error {
	ids := make([]uuid.UUID, len(w.assets))
	var amount money.Minor
	var moved, movements []uuid.UUID
	var accumulated, accumulatedGC []money.Minor // as the movements record them: a credit of accumulated depreciation
	for i, c := range w.assets {
		ids[i] = c.asset
		if c.difference != 0 {
			amount, moved, movements = amount+c.difference, append(moved, c.asset), append(movements, uuid.Must(uuid.NewV7()))
			accumulated, accumulatedGC = append(accumulated, -c.difference), append(accumulatedGC, -c.differenceGC)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1, updated_by = $2 WHERE id = ANY($1)`, ids, p.ID); err != nil {
		return fmt.Errorf("assets: seq of the assets of batch %d of depreciation run %s: %w", w.no, w.run.ID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE assets.asset_terms SET booked_through_year = $3, booked_through_period = $4, version = version + 1, updated_by = $5
		WHERE asset_id = ANY($1) AND ledger_id = $2 AND (booked_through_year IS NULL OR (booked_through_year, booked_through_period) < ($3, $4))`,
		ids, w.run.LedgerID, w.run.FiscalYear, w.run.Period, p.ID); err != nil {
		return fmt.Errorf("assets: book the assets of batch %d of depreciation run %s through %d/%d: %w", w.no, w.run.ID, w.run.FiscalYear, w.run.Period, err)
	}
	var year *int16
	var id *uuid.UUID
	var number *string
	if entry != nil {
		year, id, number = &entry.FiscalYear, &entry.ID, &entry.DocumentNumber
	}
	if len(moved) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
				posting_date, fiscal_year, fiscal_period, accumulated, accumulated_gc, document_kind, document_id, journal_entry_year, journal_entry_id, created_by)
			SELECT a.tenant_id, x.id, a.id, a.last_seq, a.legal_entity_id, $2, a.class_code, 'depreciation', $3, $3, $4, $5, x.acc, x.gc,
				'depreciation_run', $6, $7, $8, $9
			FROM unnest($1::uuid[], $10::bigint[], $11::bigint[], $12::uuid[]) AS x(asset, acc, gc, id) JOIN assets.asset a ON a.id = x.asset`,
			moved, w.run.LedgerID, w.period.End, w.run.FiscalYear, w.run.Period, w.run.ID, year, id, p.ID, accumulated, accumulatedGC, movements); err != nil {
			return fmt.Errorf("assets: movements of batch %d of depreciation run %s: %w", w.no, w.run.ID, err)
		}
	}
	_, err := tx.Exec(ctx, `UPDATE assets.depreciation_batch SET status = 'posted', amount = $3, assets_posted = $4, assets_skipped = $5,
			journal_entry_year = $6, journal_entry_id = $7, document_number = $8, version = version + 1
		WHERE run_id = $1 AND batch_no = $2 AND status = 'pending'`, w.run.ID, w.no, amount, len(moved), w.skipped, year, id, number)
	return errs.Wrapf(err, "assets: post batch %d of depreciation run %s", w.no, w.run.ID)
}

// recordBatch audits the batch and reports the run's posted batches on
// its Operation.
func (e *Endpoints) recordBatch(ctx context.Context, tx pgx.Tx, w batchWork, entry *PostedEntry) error {
	detail := map[string]any{"run_id": w.run.ID, "batch_no": w.no, "assets": len(w.assets), "assets_skipped": w.skipped}
	if entry != nil {
		detail["journal_entry_id"], detail["document_number"] = entry.ID, entry.DocumentNumber
	}
	var done, total int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'posted'), count(*) FROM assets.depreciation_batch WHERE run_id = $1`, w.run.ID).
		Scan(&done, &total); err != nil {
		return fmt.Errorf("assets: progress of depreciation run %s: %w", w.run.ID, err)
	}
	if err := e.Runs.Report(ctx, w.run.OperationID, done, total); err != nil {
		return fmt.Errorf("assets: progress of depreciation run %s: %w", w.run.ID, err)
	}
	return audit.Record(ctx, "assets.depreciation_run.post_batch", "assets.depreciation_run", w.run.ID, detail)
}

// FinishDepreciationRun sets a running run whose batches are all posted
// posted with its totals, audits it, publishes DepreciationRunCompleted
// and succeeds its Operation. A run no longer running is left alone; one
// whose Operation was cancelled is refused (liveRun), so it is failed
// rather than posted.
func (e *Endpoints) FinishDepreciationRun(ctx context.Context, id uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := e.liveRun(ctx, tx, id, false, true)
	if err != nil || r.Status != RunRunning {
		return err
	}
	var pending int
	if err := tx.QueryRow(ctx, `WITH t AS (SELECT count(*) FILTER (WHERE status = 'pending') AS pending, coalesce(sum(amount), 0) AS amount,
				coalesce(sum(assets_posted), 0) AS posted, coalesce(sum(assets_skipped), 0) AS skipped
			FROM assets.depreciation_batch WHERE run_id = $1),
		u AS (UPDATE assets.depreciation_run r SET status = 'posted', amount = t.amount, assets_posted = t.posted, assets_skipped = t.skipped,
				version = r.version + 1, updated_by = $2
			FROM t WHERE r.id = $1 AND t.pending = 0)
		SELECT pending FROM t`, id, p.ID).Scan(&pending); err != nil {
		return fmt.Errorf("assets: finish depreciation run %s: %w", id, err)
	}
	if pending > 0 {
		return fmt.Errorf("assets: depreciation run %s has %d batches not posted; it is finished after them", id, pending)
	}
	r, err = e.recordRun(ctx, tx, id, "assets.depreciation_run.post")
	if err != nil {
		return err
	}
	return errs.Wrapf(e.Runs.Succeed(ctx, r.OperationID, runPath(id), map[string]any{"run_id": id, "status": r.Status}),
		"assets: succeed the operation of depreciation run %s", id)
}

// FailDepreciationRun sets a queued or running run failed with the
// problem that stopped it, audits it, publishes DepreciationRunCompleted
// and fails its Operation with the problem. Batches already posted stay
// posted. A run already posted or failed is left alone. An Operation
// cancelled is left cancelled, its final status (04 §4.8): the run that
// stopped for it records the cancellation as its problem.
func (e *Endpoints) FailDepreciationRun(ctx context.Context, id uuid.UUID, why *problem.Problem) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := lockRun(ctx, tx, id, false)
	if err != nil || (r.Status != RunQueued && r.Status != RunRunning) {
		return err
	}
	failure, _ := json.Marshal(RunError{Code: why.Code, Detail: why.Detail}) // two strings always encode
	if _, err := tx.Exec(ctx, `UPDATE assets.depreciation_run SET status = 'failed', error = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, failure, p.ID); err != nil {
		return fmt.Errorf("assets: fail depreciation run %s: %w", id, err)
	}
	if r, err = e.recordRun(ctx, tx, id, "assets.depreciation_run.fail"); err != nil {
		return err
	}
	cancelled, err := e.Runs.Cancelled(ctx, r.OperationID, true)
	if err != nil || cancelled {
		return errs.Wrapf(err, "assets: the operation of depreciation run %s", id)
	}
	return errs.Wrapf(e.Runs.Fail(ctx, r.OperationID, why), "assets: fail the operation of depreciation run %s", id)
}

// recordRun reads the run after it ended and audits it under the action,
// publishing DepreciationRunCompleted (api/asyncapi.yaml) with what its
// posted batches posted and their entries.
func (e *Endpoints) recordRun(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string) (Run, error) {
	r, err := lockRun(ctx, tx, id, false)
	if err != nil {
		return Run{}, err
	}
	batches, err := readBatches(ctx, tx, []uuid.UUID{id})
	if err != nil {
		return Run{}, err
	}
	co, err := e.companyOf(ctx, r.LegalEntityID)
	if err != nil {
		return Run{}, err
	}
	l, _, err := e.ledgerOf(ctx, r.LegalEntityID, &r.LedgerID)
	if err != nil {
		return Run{}, err
	}
	exp, err := currency.Exponent(ctx, tx, r.Currency, nil)
	if err != nil {
		return Run{}, fmt.Errorf("assets: currency %s: %w", r.Currency, err)
	}
	payload := runEvent(r, batches[id], co, l, exp)
	return r, audit.Record(ctx, action, "assets.depreciation_run", id, payload,
		outbox.Event{Context: "assets", Name: "depreciation_run_completed", AggregateType: "assets.depreciation_run", AggregateID: id, Payload: payload})
}

// runEvent is DepreciationRunCompleted's payload: the run with what its
// posted batches posted and their entries.
func runEvent(r Run, batches []Batch, co Company, l LedgerRef, exp int) map[string]any {
	amount, posted, skipped := postedTotals(batches)
	entries := []map[string]any{}
	for _, b := range batches {
		if b.EntryID != nil { // set once the batch is posted (depreciation_batch_entry)
			entries = append(entries, map[string]any{"batch_no": b.No, "journal_entry_id": *b.EntryID, "fiscal_year": *b.EntryYear, "document_number": *b.DocumentNumber})
		}
	}
	var planned int32
	if r.Planned != nil {
		planned = *r.Planned
	}
	out := map[string]any{"run_id": r.ID, "legal_entity_id": r.LegalEntityID, "company_code": co.Code, "ledger_id": r.LedgerID, "ledger_code": l.Code,
		"fiscal_year": r.FiscalYear, "period": r.Period, "posting_date": r.PostingDate.Format(time.DateOnly), "status": r.Status, "currency_lc": r.Currency,
		"assets_planned": planned, "assets_posted": posted, "assets_skipped": skipped, "amount": money.EventAmount(amount, r.Currency, exp), "entries": entries}
	if r.Error != nil {
		out["error"] = map[string]any{"code": r.Error.Code, "detail": r.Error.Detail}
	}
	return out
}

// postedTotals are what a run's posted batches posted: the depreciation,
// the assets posted and those skipped. A posted run's own totals are
// these; a running run shows them as they stand, and a failed run too,
// its posted batches staying posted.
func postedTotals(batches []Batch) (money.Minor, int32, int32) {
	var amount money.Minor
	var posted, skipped int32
	for _, b := range batches {
		if b.Status == batchPosted { // its totals are set with the status (depreciation_batch_posted)
			amount, posted, skipped = amount+*b.Amount, posted+*b.Posted, skipped+*b.Skipped
		}
	}
	return amount, posted, skipped
}

// UnfinishedDepreciationRuns lists the runs queued or running in the
// transaction's tenant, by run with its Operation: the work of the
// in-process runner of a cell without a workflow engine, which takes up a
// run whose Operation was cancelled too, so that its next step fails it.
func (e *Endpoints) UnfinishedDepreciationRuns(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT id, operation_id FROM assets.depreciation_run WHERE status IN ('queued', 'running')`)
	out := map[uuid.UUID]uuid.UUID{}
	var run, op uuid.UUID
	_, err := pgx.ForEachRow(rows, []any{&run, &op}, func() error { out[run] = op; return nil })
	return out, errs.Wrapf(err, "assets: unfinished depreciation runs")
}
