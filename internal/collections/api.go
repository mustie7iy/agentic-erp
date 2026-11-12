package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// companies reads the runs' companies, once each: what a run names by id,
// as its consumer reads it (REVIEW.md, API-consumer classes).
func (e *Endpoints) companies(ctx context.Context, runs []Run) (map[uuid.UUID]tenancy.Entity, error) {
	out := map[uuid.UUID]tenancy.Entity{}
	for _, r := range runs {
		if _, ok := out[r.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a stored run's legal entity exists
		if err != nil {
			return nil, fmt.Errorf("collections: company of dunning run %s: %w", r.ID, err)
		}
		out[r.LegalEntityID] = le
	}
	return out, nil
}

// runToAPI is a run as the API gives it, with its company's code and name.
func runToAPI(r Run, companies map[uuid.UUID]tenancy.Entity) gen.DunningRun {
	le := companies[r.LegalEntityID]
	return gen.DunningRun{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), LegalEntityID: r.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, KeyDate: r.KeyDate,
		Status: r.Status, Currency: r.Currency, NoticeCount: r.NoticeCount, ItemCount: r.ItemCount,
		OverdueAmount: apitypes.Money(r.OverdueAmount, r.Currency, r.Exponent)}
}

// CreateDunningRun implements POST /v1/dunning-runs.
func (e *Endpoints) CreateDunningRun(ctx context.Context, req *gen.DunningRunCreate, _ gen.CreateDunningRunParams) (gen.CreateDunningRunRes, error) {
	r, err := e.ProposeRun(ctx, RunInput{LegalEntityID: req.LegalEntityID, KeyDate: req.KeyDate})
	if err != nil {
		return nil, err
	}
	companies, err := e.companies(ctx, []Run{r})
	if err != nil {
		return nil, err
	}
	return &gen.DunningRunHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/dunning-runs/" + r.ID.String()),
		Response: runToAPI(r, companies)}, nil
}

// GetDunningRun implements GET /v1/dunning-runs/{id}.
func (e *Endpoints) GetDunningRun(ctx context.Context, params gen.GetDunningRunParams) (gen.GetDunningRunRes, error) {
	r, err := GetRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	companies, err := e.companies(ctx, []Run{r})
	if err != nil {
		return nil, err
	}
	return &gen.GetDunningRunOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: runToAPI(r, companies)}, nil
}

var runSort = pagination.Sort{Default: "key_date", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "key_date", Column: "r.key_date", Type: "date"},
}}

// ListDunningRuns implements GET /v1/dunning-runs: the runs of the
// companies the caller may act in, the latest key date first.
func (e *Endpoints) ListDunningRuns(ctx context.Context, params gen.ListDunningRunsParams) (gen.ListDunningRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	from, to := apitypes.Ptr[time.Time](params.KeyDateFrom), apitypes.Ptr[time.Time](params.KeyDateTo)
	scope := fmt.Sprintf("listDunningRuns|%s|%s|%v|%v", le, params.Status.Value, from, to)
	ks, err := e.Pages.Keyset(runSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("collections: page of dunning runs: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: company access for dunning runs: %w", err)
	}
	q, args := ks.SQL(runSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4 = '' OR r.status = $4) AND ($5::date IS NULL OR r.key_date >= $5) AND ($6::date IS NULL OR r.key_date <= $6)`,
		[]any{access.All, access.IDs, le, params.Status.Value, from, to})
	rows, _ := tx.Query(ctx, q, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("collections: list dunning runs: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, runs, func(r Run) (string, uuid.UUID) { return r.KeyDate.Format(time.DateOnly), r.ID },
		func(page []Run) (map[uuid.UUID]tenancy.Entity, error) { return e.companies(ctx, page) }, runToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.DunningRunPage{Items: items, NextPageToken: next}, nil
}

// shown is what a page of notices is shown with: their run, whose
// currency their amounts are in, and their customers' names.
type shown struct {
	run       Run
	customers map[uuid.UUID]partner.Named
}

// noticeToAPI is a notice as the API gives it: its customer by number and
// name, its amount in the run's currency and each item's in its own.
func noticeToAPI(n Notice, s shown) gen.DunningNotice {
	out := gen.DunningNotice{ID: n.ID, DunningRunID: n.RunID, CustomerID: n.CustomerID, CustomerNumber: n.CustomerNumber,
		CustomerName: s.customers[n.CustomerID].Name, Level: n.Level, ItemCount: n.ItemCount,
		OverdueAmount: apitypes.Money(n.OverdueAmount, s.run.Currency, s.run.Exponent), Items: make([]gen.DunningNoticeItem, len(n.Items))}
	for i, it := range n.Items {
		out.Items[i] = gen.DunningNoticeItem{ItemNo: it.ItemNo, FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID, LineNo: it.LineNo,
			DocumentNumber: it.DocumentNumber, Reference: apitypes.OptString(it.Reference), DocumentDate: it.DocumentDate, NetDueDate: it.NetDueDate,
			DaysOverdue: it.DaysOverdue, Amount: apitypes.Money(it.Amount, it.Currency, it.Exponent),
			LocalAmount:   apitypes.Money(it.AmountLC, s.run.Currency, s.run.Exponent),
			PreviousLevel: it.PreviousLevel, Level: it.Level}
	}
	return out
}

var noticeSort = pagination.Sort{Default: "customer_number", Fields: []pagination.Field{
	{Name: "customer_number", Column: "customer_number", Type: "text"},
}}

// ListDunningNotices implements GET /v1/dunning-runs/{id}/notices: the
// notices of a run of a company the caller may act in, by customer
// number, each with its items.
func (e *Endpoints) ListDunningNotices(ctx context.Context, params gen.ListDunningNoticesParams) (gen.ListDunningNoticesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	run, err := GetRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	customer, level := apitypes.Ptr[uuid.UUID](params.CustomerID), apitypes.Ptr[int32](params.Level)
	scope := fmt.Sprintf("listDunningNotices|%s|%s|%v", run.ID, customer, params.Level)
	ks, err := e.Pages.Keyset(noticeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("collections: page of dunning notices: %w", err)
	}
	q, args := ks.SQL(`SELECT id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount
		FROM collections.dunning_notice
		WHERE dunning_run_id = $1 AND ($2::uuid IS NULL OR customer_id = $2) AND ($3::int IS NULL OR level = $3)`, []any{run.ID, customer, level})
	rows, _ := tx.Query(ctx, q, args...)
	notices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Notice, error) {
		var n Notice
		err := r.Scan(&n.ID, &n.RunID, &n.CustomerID, &n.CustomerNumber, &n.Level, &n.ItemCount, &n.OverdueAmount)
		return n, errs.Wrapf(err, "scan dunning notice")
	})
	if err != nil {
		return nil, fmt.Errorf("collections: list the notices of dunning run %s: %w", run.ID, err)
	}
	out, next, err := apitypes.PageWith(ks, notices, func(n Notice) (string, uuid.UUID) { return n.CustomerNumber, n.ID },
		func(page []Notice) (shown, error) { return e.shown(ctx, tx, run, page) }, noticeToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.DunningNoticePage{Items: out, NextPageToken: next}, nil
}

// shown reads what the page's notices are shown with: their items, given
// to them, and their customers' names.
func (e *Endpoints) shown(ctx context.Context, tx pgx.Tx, run Run, page []Notice) (shown, error) {
	if err := items(ctx, tx, page); err != nil {
		return shown{}, err
	}
	customers := make([]uuid.UUID, len(page))
	for i, n := range page {
		customers[i] = n.CustomerID
	}
	names, err := e.Partners.Names(ctx, customers)
	if err != nil {
		return shown{}, fmt.Errorf("collections: customers of %d dunning notices: %w", len(page), err)
	}
	return shown{run: run, customers: names}, nil
}
