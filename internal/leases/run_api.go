package leases

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// runSelect reads runs as one relation r with their currency's
// minor-unit digits: a listing's cursor names the run's id without a
// table (pagination.Keyset).
const runSelect = `SELECT r.id, r.legal_entity_id, r.fiscal_year, r.fiscal_period, r.posting_date, r.currency, r.exponent, r.contract_count,
		r.posting_count, r.interest_amount, r.depreciation_amount, r.created_at, r.created_by
	FROM (SELECT n.id, n.legal_entity_id, n.fiscal_year, n.fiscal_period, n.posting_date, n.currency, c.exponent, n.contract_count, n.posting_count,
			n.interest_amount, n.depreciation_amount, n.created_at, n.created_by
		FROM leases.run n JOIN ref.currency c ON c.code = n.currency) AS r`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.LegalEntityID, &r.FiscalYear, &r.FiscalPeriod, &r.PostingDate, &r.Currency, &r.Exponent, &r.Contracts, &r.Posted,
		&r.Interest, &r.Depreciation, &r.CreatedAt, &r.CreatedBy)
	return r, errs.Wrapf(err, "scan lease run")
}

// runPostings reads the run's postings by their contracts' numbers.
func runPostings(ctx context.Context, tx pgx.Tx, run uuid.UUID) ([]RunPosting, error) {
	rows, _ := tx.Query(ctx, `SELECT p.id, p.contract_id, k.document_number, k.lessor_id, k.cost_center_id, p.first_line_no, p.last_line_no,
			p.interest_amount, p.depreciation_amount, p.journal_entry_id, p.journal_entry_year, p.journal_entry_number
		FROM leases.run_posting p JOIN leases.contract k ON k.tenant_id = p.tenant_id AND k.id = p.contract_id
		WHERE p.run_id = $1 ORDER BY k.document_number, p.contract_id`, run)
	postings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RunPosting, error) {
		var p RunPosting
		err := r.Scan(&p.ID, &p.ContractID, &p.DocumentNumber, &p.LessorID, &p.CostCenterID, &p.FirstLine, &p.LastLine, &p.Interest, &p.Depreciation,
			&p.EntryID, &p.EntryYear, &p.EntryNumber)
		return p, errs.Wrapf(err, "scan lease run posting")
	})
	if err != nil {
		return nil, fmt.Errorf("leases: postings of lease run %s: %w", run, err)
	}
	return postings, nil
}

// GetRun returns a lease run of a company the caller may act in with its
// postings; another's is not found, so ids cannot be probed (04 §4.9).
func GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("leases: company access for lease run %s: %w", id, err)
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	if err := authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("lease run"), "leases: read lease run %s", id); err != nil {
		return Run{}, err
	}
	r.Postings, err = runPostings(ctx, tx, id)
	return r, err
}

// runReferences reads what runs name by id: their companies, once each,
// and their postings' lessors and cost centers.
func (e *Endpoints) runReferences(ctx context.Context, runs []Run) (references, error) {
	var companies, lessors, centers []uuid.UUID
	for _, r := range runs {
		companies = append(companies, r.LegalEntityID)
		for _, p := range r.Postings {
			lessors, centers = append(lessors, p.LessorID), append(centers, p.CostCenterID)
		}
	}
	return e.referencesOf(ctx, companies, lessors, centers)
}

// runAPI is a run as the API gives it, every amount in its currency, with
// what it names by id; its postings are given when it has them.
func runAPI(r Run, refs references) gen.LeaseRun {
	le := refs.companies[r.LegalEntityID]
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, r.Currency, r.Exponent) }
	out := gen.LeaseRun{ID: r.ID, LegalEntityID: r.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, FiscalYear: int32(r.FiscalYear),
		FiscalPeriod: int32(r.FiscalPeriod), PostingDate: r.PostingDate, Currency: r.Currency, ContractCount: r.Contracts, PostingCount: r.Posted,
		InterestAmount: amount(r.Interest), DepreciationAmount: amount(r.Depreciation), CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy}
	for _, p := range r.Postings {
		lessor := refs.lessors[p.LessorID]
		out.Postings = append(out.Postings, gen.LeaseRunPosting{LeaseContractID: p.ContractID, DocumentNumber: p.DocumentNumber, LessorID: p.LessorID,
			LessorNumber: lessor.Number, LessorName: lessor.Name, CostCenterID: p.CostCenterID, CostCenterCode: refs.costCenters[p.CostCenterID].Code,
			FirstLineNo: p.FirstLine, LastLineNo: p.LastLine, InterestAmount: amount(p.Interest), DepreciationAmount: amount(p.Depreciation),
			JournalEntryID: p.EntryID, JournalEntryYear: int32(p.EntryYear), JournalEntryNumber: p.EntryNumber})
	}
	return out
}

// runAnswer is a run as the API answers it.
func (e *Endpoints) runAnswer(ctx context.Context, r Run) (gen.LeaseRun, error) {
	refs, err := e.runReferences(ctx, []Run{r})
	if err != nil {
		return gen.LeaseRun{}, err
	}
	return runAPI(r, refs), nil
}

// CreateLeaseRun implements POST /v1/lease-runs.
func (e *Endpoints) CreateLeaseRun(ctx context.Context, req *gen.LeaseRunCreate, _ gen.CreateLeaseRunParams) (gen.CreateLeaseRunRes, error) {
	r, err := e.PostRun(ctx, req.LegalEntityID, req.FiscalYear, req.FiscalPeriod)
	if err != nil {
		return nil, err
	}
	out, err := e.runAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.LeaseRunHeaders{Location: gen.NewOptString("/v1/lease-runs/" + r.ID.String()), Response: out}, nil
}

// GetLeaseRun implements GET /v1/lease-runs/{id}.
func (e *Endpoints) GetLeaseRun(ctx context.Context, params gen.GetLeaseRunParams) (gen.GetLeaseRunRes, error) {
	r, err := GetRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.runAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

var runSort = pagination.Sort{Default: "posting_date", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "posting_date", Column: "r.posting_date", Type: "date"}}}

// ListLeaseRuns implements GET /v1/lease-runs: the runs of the companies
// the caller may act in, latest first, without their postings.
func (e *Endpoints) ListLeaseRuns(ctx context.Context, params gen.ListLeaseRunsParams) (gen.ListLeaseRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, year, period := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[int32](params.FiscalYear), apitypes.Ptr[int32](params.FiscalPeriod)
	scope := fmt.Sprintf("listLeaseRuns|%v|%d|%d", le, params.FiscalYear.Value, params.FiscalPeriod.Value)
	ks, err := e.Pages.Keyset(runSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("leases: page of lease runs: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("leases: company access for lease runs: %w", err)
	}
	query, args := ks.SQL(runSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::int IS NULL OR r.fiscal_year = $4) AND ($5::int IS NULL OR r.fiscal_period = $5)`, []any{access.All, access.IDs, le, year, period})
	rows, _ := tx.Query(ctx, query, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("leases: list lease runs: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, runs, func(r Run) (string, uuid.UUID) { return r.PostingDate.Format(time.DateOnly), r.ID },
		func(page []Run) (references, error) { return e.runReferences(ctx, page) }, runAPI)
	if err != nil {
		return nil, err
	}
	return &gen.LeaseRunPage{Items: items, NextPageToken: next}, nil
}
