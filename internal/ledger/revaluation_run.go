package ledger

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
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

// RevaluationDocumentType is the document type of a revaluation run's
// valuation and reset entries (ADR-0042): SA, SAP's standard for the
// valuation run, numbered in the general journal range.
const RevaluationDocumentType = "SA"

// RevaluationRun is a posted foreign-currency revaluation run (ADR-0042).
// Valuation holds its items, and Totals their sums, when they were read.
type RevaluationRun struct {
	ID, LegalEntityID, LedgerID uuid.UUID
	KeyDate                     time.Time
	RateType, Status            string
	ItemCount                   int32
	ValuationYear, ReversalYear *int16
	ValuationID, ReversalID     *uuid.UUID
	CreatedAt                   time.Time
	CreatedBy                   uuid.UUID
	Version                     int32
	Valuation                   *Revaluation
	Totals                      []RevaluationTotal
}

// The item count is read, not kept on the run: the items are inserted
// after the run, which is fixed once posted.
const revaluationRunSelect = `SELECT r.id, r.legal_entity_id, r.ledger_id, r.key_date, r.rate_type, r.status,
		(SELECT count(*) FROM ledger.revaluation_item i WHERE i.tenant_id = r.tenant_id AND i.run_id = r.id)::int,
		r.valuation_entry_year, r.valuation_entry_id, r.reversal_entry_year, r.reversal_entry_id, r.created_at, r.created_by, r.version
	FROM ledger.revaluation_run r`

func scanRevaluationRun(row pgx.Row) (RevaluationRun, error) {
	var r RevaluationRun
	err := errs.Wrapf(row.Scan(&r.ID, &r.LegalEntityID, &r.LedgerID, &r.KeyDate, &r.RateType, &r.Status, &r.ItemCount, &r.ValuationYear,
		&r.ValuationID, &r.ReversalYear, &r.ReversalID, &r.CreatedAt, &r.CreatedBy, &r.Version), "scan revaluation run")
	return r, err
}

// PostRevaluationRun implements POST /v1/revaluation-runs: values what the
// preview values and, in one transaction, posts the valuation entry on the
// key date and its reset on the next day, records the run and its items,
// audits it and publishes RevaluationRunPosted (ADR-0042). Human-only.
func (e *Endpoints) PostRevaluationRun(ctx context.Context, req *gen.RevaluationPreviewRequest, _ gen.PostRevaluationRunParams) (gen.PostRevaluationRunRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, ledgerID, group, err := e.revaluationTarget(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	if le.Status != "active" {
		return nil, problem.Unprocessable("ledger.legal_entity_not_active", "The legal entity is "+le.Status+"; postings need it active.",
			problem.FieldError{Field: "legal_entity_id", Code: "not_active", Message: "an active legal entity"})
	}
	// Every entry bucket of the tenant, exclusively and first (03-data-model,
	// entry locks): clearings and reversals, which take them shared, wait
	// until the run commits, so none changes an item the run values while it
	// runs, and a second run waits and then finds this one posted.
	if err := lockBuckets(ctx, tx, false, allBuckets()); err != nil {
		return nil, err
	}
	if err := revaluationNotPosted(ctx, tx, le.ID, ledgerID, req.KeyDate); err != nil {
		return nil, err
	}
	r, err := valueRevaluation(ctx, tx, le, ledgerID, group, req.KeyDate)
	if err != nil {
		return nil, err
	}
	run, err := e.postRevaluation(ctx, tx, p, le, r)
	if err != nil {
		return nil, err
	}
	rep := revaluationRunToAPI(run)
	return &gen.RevaluationRunHeaders{ETag: gen.NewOptString(etag.Format(run.Version)), Location: gen.NewOptString("/v1/revaluation-runs/" + run.ID.String()), Response: rep}, nil
}

// allBuckets lists every entry lock bucket in ascending order.
func allBuckets() []int32 {
	all := make([]int32, EntryLockBuckets)
	for i := range all {
		all[i] = int32(i)
	}
	return all
}

// revaluationNotPosted refuses a run where one of the legal entity, ledger
// and key date is posted, naming it; the caller holds the entry locks, so
// no run of the tenant commits between this read and its own insert.
func revaluationNotPosted(ctx context.Context, tx pgx.Tx, le, ledgerID uuid.UUID, keyDate time.Time) error {
	rows, _ := tx.Query(ctx, `SELECT id FROM ledger.revaluation_run WHERE legal_entity_id = $1 AND ledger_id = $2 AND key_date = $3 AND status = 'posted'`,
		le, ledgerID, keyDate)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("ledger: revaluation of %s at %s: posted run: %w", le, keyDate.Format(time.DateOnly), err)
	}
	if len(ids) == 0 {
		return nil
	}
	p := problem.Conflict("ledger.revaluation_exists", fmt.Sprintf("Revaluation run %s is posted at %s; cancel it to run again.", ids[0], keyDate.Format(time.DateOnly)))
	p.Header = http.Header{"Location": {"/v1/revaluation-runs/" + ids[0].String()}}
	return p
}

// postRevaluation posts the valuation's differences, when it has any,
// after checking that the key date's period and the next one are open
// (both entries need them, and a run without entries is held to the same
// rule), then records the run with its items and publishes it.
func (e *Endpoints) postRevaluation(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le tenancy.Entity, r Revaluation) (RevaluationRun, error) {
	next := r.KeyDate.AddDate(0, 0, 1)
	for _, day := range []time.Time{r.KeyDate, next} {
		if _, _, _, err := postingPeriod(ctx, tx, le, []uuid.UUID{r.LedgerID}, day); err != nil {
			return RevaluationRun{}, err
		}
	}
	totals, err := r.Totals()
	if err != nil {
		return RevaluationRun{}, err
	}
	run := RevaluationRun{ID: uuid.Must(uuid.NewV7()), LegalEntityID: le.ID, LedgerID: r.LedgerID, KeyDate: r.KeyDate, RateType: r.RateType}
	if len(totals) > 0 {
		lines, err := revaluationLines(ctx, tx, r, totals)
		if err != nil {
			return RevaluationRun{}, err
		}
		// As every posting: the valuation accounts may since have been
		// blocked for posting in the legal entity. The reset posts to the same.
		if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
			return RevaluationRun{}, err
		}
		valuation, err := e.postRevaluationEntry(ctx, tx, p, le, r, run.ID, r.KeyDate, "Foreign-currency revaluation at ", lines)
		if err != nil {
			return RevaluationRun{}, err
		}
		reset, err := e.postRevaluationEntry(ctx, tx, p, le, r, run.ID, next, "Reset of the foreign-currency revaluation at ", negated(lines))
		if err != nil {
			return RevaluationRun{}, err
		}
		run.ValuationYear, run.ValuationID, run.ReversalYear, run.ReversalID = &valuation.FiscalYear, &valuation.ID, &reset.FiscalYear, &reset.ID
	}
	if err := recordRevaluationRun(ctx, tx, p, run, r); err != nil {
		return RevaluationRun{}, err
	}
	posted, err := getRevaluationRun(ctx, tx, run.ID)
	if err != nil {
		return RevaluationRun{}, err
	}
	posted.Valuation, posted.Totals = &r, totals
	return posted, nil
}

// valuationAccounts are where a revalued account's differences post.
type valuationAccounts struct {
	adjustment *uuid.UUID
	gain, loss uuid.UUID
}

// revaluationLines are the valuation entry's lines, two per total in the
// totals' order: the difference on the account itself for a flagged
// account or on its adjustment account for a reconciliation account, and
// its offset on the unrealised gain or loss account by the total's
// direction. The entry is in local currency, so a line's transaction
// amount is its local one; local and group amounts are each the total's
// own difference (ADR-0042), so a group difference of the other sign
// posts to the same account.
func revaluationLines(ctx context.Context, tx pgx.Tx, r Revaluation, totals []RevaluationTotal) ([]Line, error) {
	accounts := map[uuid.UUID]valuationAccounts{}
	rows, _ := tx.Query(ctx, `SELECT account_id, adjustment_account_id, gain_account_id, loss_account_id FROM ledger.fx_valuation_account
		WHERE legal_entity_id = $1 AND is_active`, r.LegalEntityID)
	var id uuid.UUID
	var a valuationAccounts
	_, err := pgx.ForEachRow(rows, []any{&id, &a.adjustment, &a.gain, &a.loss}, func() error {
		v := a
		if a.adjustment != nil {
			// Copied: pgx scans the next row into the same pointer.
			adjustment := *a.adjustment
			v.adjustment = &adjustment
		}
		accounts[id] = v
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: revaluation of %s at %s: valuation accounts: %w", r.LegalEntityID, r.KeyDate.Format(time.DateOnly), err)
	}
	lines := make([]Line, 0, 2*len(totals))
	for _, t := range totals {
		// Every account in scope has an active valuation (revaluationItems).
		a := accounts[t.AccountID]
		target, offset := t.AccountID, a.loss
		if a.adjustment != nil {
			target = *a.adjustment
		}
		if t.Direction == "gain" {
			offset = a.gain
		}
		text := "Revaluation " + t.CurrencyTC + " " + r.KeyDate.Format(time.DateOnly)
		lines = append(lines,
			Line{AccountID: target, TC: t.DifferenceLC, LC: t.DifferenceLC, GC: t.DifferenceGC, Text: &text},
			Line{AccountID: offset, TC: -t.DifferenceLC, LC: -t.DifferenceLC, GC: -t.DifferenceGC, Text: &text})
	}
	return lines, nil
}

// negated is the reset of lines: each amount with the other sign. A
// difference is never the minimum bigint (money.Diff keeps it in range).
func negated(lines []Line) []Line {
	out := make([]Line, len(lines))
	for i, l := range lines {
		l.TC, l.LC, l.GC = -l.TC, -l.LC, -l.GC
		out[i] = l
	}
	return out
}

// postRevaluationEntry posts one of a run's entries through the ledger's
// posting path, in local currency, the run its source document; the
// header's local-to-group rate is the date's, for the record only, since
// the lines' group amounts are valued on their own.
func (e *Endpoints) postRevaluationEntry(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le tenancy.Entity, r Revaluation, run uuid.UUID,
	day time.Time, header string, lines []Line) (Entry, error) {
	_, toGC, err := postingRates(ctx, tx, r.CurrencyLC, r.CurrencyLC, r.CurrencyGC, day, nil)
	if err != nil {
		return Entry{}, err
	}
	text := header + r.KeyDate.Format(time.DateOnly)
	return e.post(ctx, tx, p, draft{entity: le, ledgers: []uuid.UUID{r.LedgerID}, documentType: RevaluationDocumentType, documentDate: day, postingDate: day,
		currencyTC: r.CurrencyLC, currencyLC: r.CurrencyLC, currencyGC: r.CurrencyGC, rateTCLC: "1", rateLCGC: toGC.shown, headerText: &text,
		sourceContext: "ledger", sourceDocumentID: &run, lines: lines})
}

// recordRevaluationRun inserts the run and its items, audits it and
// publishes RevaluationRunPosted.
func recordRevaluationRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run RevaluationRun, r Revaluation) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, valuation_entry_year,
			valuation_entry_id, reversal_entry_year, reversal_entry_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`, p.TenantID, run.ID, run.LegalEntityID, run.LedgerID, run.KeyDate, run.RateType,
		run.ValuationYear, run.ValuationID, run.ReversalYear, run.ReversalID, p.ID); err != nil {
		return fmt.Errorf("ledger: record revaluation run %s: %w", run.ID, err)
	}
	if err := recordRevaluationItems(ctx, tx, p.TenantID, run.ID, r.Items); err != nil {
		return err
	}
	return audit.Record(ctx, "ledger.revaluation_run.post", "ledger.revaluation_run", run.ID,
		map[string]any{"key_date": run.KeyDate.Format(time.DateOnly), "items": len(r.Items), "valuation_entry_id": run.ValuationID, "reversal_entry_id": run.ReversalID},
		outbox.Event{Context: "ledger", Name: "revaluation_run_posted", AggregateType: "ledger.revaluation_run", AggregateID: run.ID,
			Payload: map[string]any{"run_id": run.ID, "legal_entity_id": run.LegalEntityID, "ledger_id": run.LedgerID,
				"key_date": run.KeyDate.Format(time.DateOnly), "rate_type": run.RateType, "item_count": len(r.Items),
				"valuation_entry_id": run.ValuationID, "valuation_entry_year": run.ValuationYear,
				"reversal_entry_id": run.ReversalID, "reversal_entry_year": run.ReversalYear}})
}

// recordRevaluationItems inserts a run's items, numbered in order.
func recordRevaluationItems(ctx context.Context, tx pgx.Tx, tenant, run uuid.UUID, items []RevaluedItem) error {
	n := len(items)
	accounts, partners, entries := make([]uuid.UUID, n), make([]*uuid.UUID, n), make([]*uuid.UUID, n)
	years, lineNos := make([]*int16, n), make([]*int32, n)
	currencies, rates, ratesGC := make([]string, n), make([]*string, n), make([]*string, n)
	froms, fromsGC := make([]*time.Time, n), make([]*time.Time, n)
	tcs, lcs, gcs, valuedLC, valuedGC := make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n)
	for i, it := range items {
		accounts[i], partners[i], currencies[i] = it.AccountID, it.PartnerID, it.CurrencyTC
		if it.Line != nil {
			years[i], entries[i], lineNos[i] = &it.Line.FiscalYear, &it.Line.EntryID, &it.Line.LineNo
		}
		tcs[i], lcs[i], gcs[i], valuedLC[i], valuedGC[i] = it.AmountTC, it.AmountLC, it.AmountGC, it.ValuedLC, it.ValuedGC
		// A balance zero in its currency is valued without rates (valueItems).
		if it.Rate != "" {
			rates[i], froms[i], ratesGC[i] = &it.Rate, &it.RateValidFrom, &it.RateGC
		}
		if !it.RateGCValidFrom.IsZero() {
			fromsGC[i] = &it.RateGCValidFrom
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.revaluation_item (tenant_id, run_id, item_no, account_id, partner_id, fiscal_year, journal_entry_id,
			line_no, currency_tc, amount_tc, amount_lc, amount_gc, rate, rate_valid_from, valued_lc, rate_gc, rate_gc_valid_from, valued_gc)
		SELECT $1, $2, n, a, pa, y, je, ln, c, tc, lc, gc, rt::numeric, f, vl, rg::numeric, fg, vg
		FROM unnest($3::uuid[], $4::uuid[], $5::smallint[], $6::uuid[], $7::int[], $8::text[], $9::bigint[], $10::bigint[], $11::bigint[], $12::text[],
			$13::date[], $14::bigint[], $15::text[], $16::date[], $17::bigint[]) WITH ORDINALITY AS x(a, pa, y, je, ln, c, tc, lc, gc, rt, f, vl, rg, fg, vg, n)`,
		tenant, run, accounts, partners, years, entries, lineNos, currencies, tcs, lcs, gcs, rates, froms, valuedLC, ratesGC, fromsGC, valuedGC); err != nil {
		return fmt.Errorf("ledger: record revaluation run %s: items: %w", run, err)
	}
	return nil
}

// getRevaluationRun loads a run of a legal entity the caller may act in,
// without its items.
func getRevaluationRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (RevaluationRun, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return RevaluationRun{}, err
	}
	run, err := scanRevaluationRun(tx.QueryRow(ctx, revaluationRunSelect+` WHERE r.id = $1`, id))
	if err = authz.Visible(err, access, run.LegalEntityID, problem.NotFoundOf("revaluation run"), "ledger: read revaluation run %s", id); err != nil {
		return RevaluationRun{}, err
	}
	return run, nil
}

// GetRevaluationRun implements GET /v1/revaluation-runs/{id}.
func (e *Endpoints) GetRevaluationRun(ctx context.Context, params gen.GetRevaluationRunParams) (gen.GetRevaluationRunRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	run, err := getRevaluationRun(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	rep, err := e.revaluationRunWithItems(ctx, tx, run)
	if err != nil {
		return nil, err
	}
	return &gen.GetRevaluationRunOKHeaders{ETag: gen.NewOptString(etag.Format(run.Version)), Response: rep}, nil
}

// revaluationRunWithItems renders a run with its items and totals.
func (e *Endpoints) revaluationRunWithItems(ctx context.Context, tx pgx.Tx, run RevaluationRun) (gen.RevaluationRun, error) {
	// A run is recorded for an existing legal entity, which is never deleted.
	le, _, err := e.LegalEntities.Entity(ctx, run.LegalEntityID)
	if err != nil {
		return gen.RevaluationRun{}, err
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return gen.RevaluationRun{}, err
	}
	r, err := revaluationRunItems(ctx, tx, run, le.LocalCurrency, group)
	if err != nil {
		return gen.RevaluationRun{}, err
	}
	if run.Totals, err = r.Totals(); err != nil {
		return gen.RevaluationRun{}, err
	}
	run.Valuation = &r
	return revaluationRunToAPI(run), nil
}

// revaluationRunItems reads a run's items back, with the differences they
// had when valued.
func revaluationRunItems(ctx context.Context, tx pgx.Tx, run RevaluationRun, lc, gc string) (Revaluation, error) {
	r := Revaluation{LegalEntityID: run.LegalEntityID, LedgerID: run.LedgerID, KeyDate: run.KeyDate, RateType: run.RateType, CurrencyLC: lc, CurrencyGC: gc}
	exps, err := exponents(ctx, tx, lc, gc)
	if err != nil {
		return Revaluation{}, err
	}
	r.ExpLC, r.ExpGC = exps[lc], exps[gc]
	rows, _ := tx.Query(ctx, `SELECT i.account_id, i.partner_id, i.fiscal_year, i.journal_entry_id, i.line_no, i.currency_tc, c.exponent, i.amount_tc,
			i.amount_lc, i.amount_gc, coalesce(i.rate::text, ''), i.rate_valid_from, i.valued_lc, coalesce(i.rate_gc::text, ''), i.rate_gc_valid_from, i.valued_gc
		FROM ledger.revaluation_item i JOIN ref.currency c ON c.code = i.currency_tc WHERE i.run_id = $1 ORDER BY i.item_no`, run.ID)
	var it RevaluedItem
	var year *int16
	var entry *uuid.UUID
	var lineNo *int32
	var from, fromGC *time.Time
	_, err = pgx.ForEachRow(rows, []any{&it.AccountID, &it.PartnerID, &year, &entry, &lineNo, &it.CurrencyTC, &it.ExpTC, &it.AmountTC, &it.AmountLC,
		&it.AmountGC, &it.Rate, &from, &it.ValuedLC, &it.RateGC, &fromGC, &it.ValuedGC}, func() error {
		item := it
		item.Line, item.RateValidFrom, item.RateGCValidFrom = nil, time.Time{}, time.Time{}
		if item.PartnerID != nil {
			partner := *item.PartnerID
			item.PartnerID = &partner
		}
		if entry != nil {
			item.Line = &ItemRef{FiscalYear: *year, EntryID: *entry, LineNo: *lineNo}
		}
		if from != nil {
			item.RateValidFrom = *from
		}
		if fromGC != nil {
			item.RateGCValidFrom = *fromGC
		}
		// Valued from these same amounts when the run was posted, so in range.
		item.DifferenceLC, item.DifferenceGC = item.ValuedLC-item.AmountLC, item.ValuedGC-item.AmountGC
		r.Items = append(r.Items, item)
		return nil
	})
	if err != nil {
		return Revaluation{}, fmt.Errorf("ledger: read revaluation run %s: items: %w", run.ID, err)
	}
	return r, nil
}

var revaluationRunSort = pagination.Sort{Default: "key_date", Fields: []pagination.Field{{Name: "key_date", Column: "r.key_date", Type: "date"}}}

// ListRevaluationRuns implements GET /v1/revaluation-runs: the runs of the
// legal entities the caller may act in, without their items.
func (e *Endpoints) ListRevaluationRuns(ctx context.Context, params gen.ListRevaluationRunsParams) (gen.ListRevaluationRunsRes, error) {
	l := entityList{sort: revaluationRunSort, name: "listRevaluationRuns", query: revaluationRunSelect, column: "r.legal_entity_id"}
	l.filter(params.LegalEntityID, params.OrderBy, params.PageSize, params.PageToken)
	key := func(r RevaluationRun) (string, uuid.UUID) { return r.KeyDate.Format(time.DateOnly), r.ID }
	items, next, err := listOfEntities(ctx, e, l, scanRevaluationRun, key, revaluationRunToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.RevaluationRunPage{Items: items, NextPageToken: next}, nil
}

// revaluationRunToAPI renders a run, with its items and totals when they
// were read.
func revaluationRunToAPI(run RevaluationRun) gen.RevaluationRun {
	out := gen.RevaluationRun{ID: run.ID, LegalEntityID: run.LegalEntityID, LedgerID: run.LedgerID, KeyDate: run.KeyDate, RateType: run.RateType,
		Status: run.Status, ItemCount: run.ItemCount, CreatedAt: run.CreatedAt, CreatedBy: run.CreatedBy}
	if run.ValuationID != nil {
		out.ValuationEntryID.SetTo(*run.ValuationID)
		out.ValuationEntryYear.SetTo(int32(*run.ValuationYear))
		out.ReversalEntryID.SetTo(*run.ReversalID)
		out.ReversalEntryYear.SetTo(int32(*run.ReversalYear))
	}
	if run.Valuation != nil {
		out.Items, out.Totals = revaluationItemsToAPI(*run.Valuation, run.Totals)
	}
	return out
}

// CancelRevaluationRun implements POST /v1/revaluation-runs/{id}:cancel:
// reverses the run's valuation and reset entries through the ledger's
// reversal path, the one way around :reverse's refusal of a run's entries,
// and marks the run cancelled, which frees its key date; audited and
// published as RevaluationRunCancelled (ADR-0042). Human-only.
func (e *Endpoints) CancelRevaluationRun(ctx context.Context, req *gen.ReasonInput, params gen.CancelRevaluationRunParams) (gen.CancelRevaluationRunRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	run, err := getRevaluationRun(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	// The entries' buckets first, both at once and in order (03-data-model,
	// entry locks), then the run, so a concurrent cancel waits here.
	if run.ValuationID != nil {
		if err := lockEntries(ctx, tx, false, []uuid.UUID{*run.ValuationID, *run.ReversalID}); err != nil {
			return nil, err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT status, version FROM ledger.revaluation_run WHERE id = $1 FOR UPDATE`, run.ID).Scan(&run.Status, &run.Version); err != nil {
		return nil, fmt.Errorf("ledger: cancel revaluation run %s: lock: %w", run.ID, err)
	}
	if err := etag.Check(params.IfMatch, run.Version); err != nil {
		return nil, err
	}
	if run.Status != "posted" {
		return nil, problem.Conflict("ledger.revaluation_not_posted", "Revaluation run "+run.ID.String()+" is "+run.Status+"; only a posted run is cancelled.")
	}
	reversals, err := e.reverseRevaluationEntries(ctx, tx, p, run)
	if err != nil {
		return nil, err
	}
	if run, err = recordRevaluationCancel(ctx, tx, p, run, req.Reason, reversals); err != nil {
		return nil, err
	}
	rep, err := e.revaluationRunWithItems(ctx, tx, run)
	if err != nil {
		return nil, err
	}
	return &gen.CancelRevaluationRunOKHeaders{ETag: gen.NewOptString(etag.Format(run.Version)), Response: rep}, nil
}

// reverseRevaluationEntries reverses a run's valuation and reset entries,
// each on its own posting date, so both periods must be open as for any
// posting (T32: a closed or locked one refuses the cancel); none for a run
// without entries.
func (e *Endpoints) reverseRevaluationEntries(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run RevaluationRun) ([]Entry, error) {
	if run.ValuationID == nil {
		return nil, nil
	}
	reversals := make([]Entry, 0, 2)
	for _, id := range []uuid.UUID{*run.ValuationID, *run.ReversalID} {
		rev, err := e.reverseRevaluationEntry(ctx, tx, p, run.ID, id)
		if err != nil {
			return nil, err
		}
		reversals = append(reversals, rev)
	}
	return reversals, nil
}

// reverseRevaluationEntry reverses one of a cancelling run's entries,
// audited as every reversal, so the entry's trail shows it reversed.
func (e *Endpoints) reverseRevaluationEntry(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run, id uuid.UUID) (Entry, error) {
	// The run's state, not the entry's, is what the caller matched.
	orig, err := reversibleEntry(ctx, tx, id, func(Entry) error { return nil }, run)
	if err != nil {
		return Entry{}, err
	}
	rev, err := e.postReversal(ctx, tx, p, orig, nil, nil)
	if err != nil {
		return Entry{}, err
	}
	if err := recordReversal(ctx, tx, p, orig, rev); err != nil {
		return Entry{}, err
	}
	return rev, audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.reverse", ObjectType: "ledger.journal_entry", ObjectID: orig.ID,
		Outcome: "succeeded", Detail: map[string]any{"reversal_entry_id": rev.ID, "reversal_document_number": rev.DocumentNumber, "revaluation_run_id": run}})
}

// recordRevaluationCancel marks the locked, posted run cancelled, audits
// it and publishes RevaluationRunCancelled with its entries' reversals.
func recordRevaluationCancel(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run RevaluationRun, reason string, reversals []Entry) (RevaluationRun, error) {
	if _, err := tx.Exec(ctx, `UPDATE ledger.revaluation_run SET status = 'cancelled', updated_by = $2, version = version + 1 WHERE id = $1`,
		run.ID, p.ID); err != nil {
		return RevaluationRun{}, fmt.Errorf("ledger: cancel revaluation run %s: %w", run.ID, err)
	}
	run.Status, run.Version = "cancelled", run.Version+1
	payload := map[string]any{"run_id": run.ID, "legal_entity_id": run.LegalEntityID, "ledger_id": run.LedgerID,
		"key_date": run.KeyDate.Format(time.DateOnly), "reason": reason, "valuation_reversal_entry_id": nil, "valuation_reversal_entry_year": nil,
		"reset_reversal_entry_id": nil, "reset_reversal_entry_year": nil}
	for i, k := range []string{"valuation", "reset"} {
		if len(reversals) > 0 {
			payload[k+"_reversal_entry_id"], payload[k+"_reversal_entry_year"] = reversals[i].ID, reversals[i].FiscalYear
		}
	}
	err := audit.Record(ctx, "ledger.revaluation_run.cancel", "ledger.revaluation_run", run.ID,
		map[string]any{"reason": reason, "valuation_reversal_entry_id": payload["valuation_reversal_entry_id"],
			"reset_reversal_entry_id": payload["reset_reversal_entry_id"]},
		outbox.Event{Context: "ledger", Name: "revaluation_run_cancelled", AggregateType: "ledger.revaluation_run", AggregateID: run.ID, Payload: payload})
	return run, err
}
