package revenue

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// runSelect reads runs as one relation r with their currency's
// minor-unit digits: a listing's cursor names the run's id without a
// table (pagination.Keyset).
const runSelect = `SELECT r.id, r.legal_entity_id, r.fiscal_year, r.fiscal_period, r.posting_date, r.currency, r.exponent, r.contract_count, r.posting_count,
		r.recognized_amount, r.billed_amount, r.contract_asset_amount, r.contract_liability_amount, r.revenue_amount, r.created_at, r.created_by
	FROM (SELECT n.id, n.legal_entity_id, n.fiscal_year, n.fiscal_period, n.posting_date, n.currency, c.exponent, n.contract_count, n.posting_count,
			n.recognized_amount, n.billed_amount, n.contract_asset_amount, n.contract_liability_amount, n.revenue_amount, n.created_at, n.created_by
		FROM revenue.recognition_run n JOIN ref.currency c ON c.code = n.currency) AS r`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.LegalEntityID, &r.FiscalYear, &r.FiscalPeriod, &r.PostingDate, &r.Currency, &r.Exponent, &r.Contracts, &r.Posted,
		&r.Recognized, &r.Billed, &r.Asset, &r.Liability, &r.Revenue, &r.CreatedAt, &r.CreatedBy)
	return r, errs.Wrapf(err, "scan revenue recognition run")
}

// postingsOf reads the run's postings by their sales orders' numbers,
// each with its obligations by number.
func postingsOf(ctx context.Context, tx pgx.Tx, run uuid.UUID) ([]Posting, error) {
	rows, _ := tx.Query(ctx, `SELECT p.id, p.contract_id, k.sales_order_id, k.sales_order_number, k.customer_id, p.recognized_amount, p.billed_amount,
			p.revenue_amount, p.journal_entry_id, p.journal_entry_year, p.document_number,
			(SELECT json_agg(json_build_object('obligation_no', o.obligation_no, 'description', b.description, 'account_id', o.account_id,
					'recognized_amount', o.recognized_amount, 'billed_amount', o.billed_amount, 'revenue_amount', o.revenue_amount) ORDER BY o.obligation_no)
				FROM revenue.recognition_posting_obligation o JOIN revenue.performance_obligation b ON b.tenant_id = o.tenant_id
					AND b.contract_id = o.contract_id AND b.obligation_no = o.obligation_no
				WHERE o.tenant_id = p.tenant_id AND o.run_id = p.run_id AND o.contract_id = p.contract_id)
		FROM revenue.recognition_posting p JOIN revenue.contract k ON k.tenant_id = p.tenant_id AND k.id = p.contract_id
		WHERE p.run_id = $1 ORDER BY k.sales_order_number, p.contract_id`, run)
	postings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Posting, error) {
		var p Posting
		err := r.Scan(&p.ID, &p.ContractID, &p.SalesOrderID, &p.SalesOrderNumber, &p.CustomerID, &p.Recognized, &p.Billed, &p.Revenue,
			&p.EntryID, &p.EntryYear, &p.DocumentNumber, &p.Obligations)
		return p, errs.Wrapf(err, "scan revenue recognition posting")
	})
	if err != nil {
		return nil, fmt.Errorf("revenue: postings of recognition run %s: %w", run, err)
	}
	return postings, nil
}

// GetRun returns a recognition run of a company the caller may act in
// with its postings; another's is not found, so ids cannot be probed
// (04 §4.9).
func GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("revenue: company access for recognition run %s: %w", id, err)
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	if err := authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("revenue recognition run"), "revenue: read recognition run %s", id); err != nil {
		return Run{}, err
	}
	r.Postings, err = postingsOf(ctx, tx, id)
	return r, err
}

// runNames is what runs name by id, as their consumer reads it
// (REVIEW.md, API-consumer classes): their companies, their postings'
// customers and their obligations' revenue accounts.
type runNames struct {
	companies map[uuid.UUID]tenancy.Entity
	customers map[uuid.UUID]partner.Named
	accounts  map[uuid.UUID]string
}

// runNames reads the runs' companies, once each, and their postings'
// customers and accounts.
func (e *Endpoints) runNames(ctx context.Context, runs []Run) (runNames, error) {
	names := runNames{companies: map[uuid.UUID]tenancy.Entity{}}
	var customers, accounts []uuid.UUID
	for _, r := range runs {
		for _, p := range r.Postings {
			customers = append(customers, p.CustomerID)
			for _, o := range p.Obligations {
				accounts = append(accounts, o.AccountID)
			}
		}
		if _, ok := names.companies[r.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a stored run's legal entity exists
		if err != nil {
			return runNames{}, fmt.Errorf("revenue: company of recognition run %s: %w", r.ID, err)
		}
		names.companies[r.LegalEntityID] = le
	}
	var err error
	if names.customers, err = e.Partners.Names(ctx, customers); err != nil {
		return runNames{}, fmt.Errorf("revenue: customers of %d recognition runs: %w", len(runs), err)
	}
	if names.accounts, err = e.Accounts.AccountNumbers(ctx, accounts); err != nil {
		return runNames{}, fmt.Errorf("revenue: accounts of %d recognition runs: %w", len(runs), err)
	}
	return names, nil
}

// runAPI is a run as the API gives it, every amount in its currency;
// its postings are given when it has them.
func runAPI(r Run, names runNames) gen.RevenueRecognitionRun {
	le := names.companies[r.LegalEntityID]
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, r.Currency, r.Exponent) }
	out := gen.RevenueRecognitionRun{ID: r.ID, LegalEntityID: r.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, FiscalYear: int32(r.FiscalYear),
		FiscalPeriod: int32(r.FiscalPeriod), PostingDate: r.PostingDate, Currency: r.Currency, ContractCount: r.Contracts, PostingCount: r.Posted,
		RecognizedAmount: amount(r.Recognized), BilledAmount: amount(r.Billed), ContractAssetAmount: amount(r.Asset),
		ContractLiabilityAmount: amount(r.Liability), RevenueAmount: amount(r.Revenue), CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy}
	for _, p := range r.Postings {
		customer := names.customers[p.CustomerID]
		asset, liability := position(p.Recognized, p.Billed)
		posting := gen.RevenueRecognitionPosting{RevenueContractID: p.ContractID, SalesOrderID: p.SalesOrderID, SalesOrderNumber: p.SalesOrderNumber,
			CustomerID: p.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name, RecognizedAmount: amount(p.Recognized),
			BilledAmount: amount(p.Billed), ContractAssetAmount: amount(asset), ContractLiabilityAmount: amount(liability), RevenueAmount: amount(p.Revenue),
			JournalEntryID: p.EntryID, JournalEntryYear: int32(p.EntryYear), JournalEntryNumber: p.DocumentNumber,
			Obligations: make([]gen.RevenueRecognitionObligation, len(p.Obligations))}
		for i, o := range p.Obligations {
			posting.Obligations[i] = gen.RevenueRecognitionObligation{ObligationNo: o.No, Description: o.Description, AccountID: o.AccountID,
				AccountNumber: names.accounts[o.AccountID], RecognizedAmount: amount(o.Recognized), BilledAmount: amount(o.Billed), RevenueAmount: amount(o.Revenue)}
		}
		out.Postings = append(out.Postings, posting)
	}
	return out
}

// CreateRevenueRecognitionRun implements POST /v1/revenue-recognition-runs.
func (e *Endpoints) CreateRevenueRecognitionRun(ctx context.Context, req *gen.RevenueRecognitionRunCreate, _ gen.CreateRevenueRecognitionRunParams) (gen.CreateRevenueRecognitionRunRes, error) {
	r, _, err := e.Recognize(ctx, req.LegalEntityID, req.FiscalYear, req.FiscalPeriod)
	if err != nil {
		return nil, err
	}
	names, err := e.runNames(ctx, []Run{r})
	if err != nil {
		return nil, err
	}
	return &gen.RevenueRecognitionRunHeaders{Location: gen.NewOptString("/v1/revenue-recognition-runs/" + r.ID.String()), Response: runAPI(r, names)}, nil
}

// GetRevenueRecognitionRun implements GET /v1/revenue-recognition-runs/{id}.
func (e *Endpoints) GetRevenueRecognitionRun(ctx context.Context, params gen.GetRevenueRecognitionRunParams) (gen.GetRevenueRecognitionRunRes, error) {
	r, err := GetRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	names, err := e.runNames(ctx, []Run{r})
	if err != nil {
		return nil, err
	}
	out := runAPI(r, names)
	return &out, nil
}

var runSort = pagination.Sort{Default: "posting_date", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "posting_date", Column: "r.posting_date", Type: "date"}}}

// ListRevenueRecognitionRuns implements GET /v1/revenue-recognition-runs:
// the runs of the companies the caller may act in, latest first, without
// their postings.
func (e *Endpoints) ListRevenueRecognitionRuns(ctx context.Context, params gen.ListRevenueRecognitionRunsParams) (gen.ListRevenueRecognitionRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, year, period := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[int32](params.FiscalYear), apitypes.Ptr[int32](params.FiscalPeriod)
	scope := fmt.Sprintf("listRevenueRecognitionRuns|%v|%d|%d", le, params.FiscalYear.Value, params.FiscalPeriod.Value)
	ks, err := e.Pages.Keyset(runSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("revenue: page of recognition runs: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("revenue: company access for recognition runs: %w", err)
	}
	query, args := ks.SQL(runSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::int IS NULL OR r.fiscal_year = $4) AND ($5::int IS NULL OR r.fiscal_period = $5)`, []any{access.All, access.IDs, le, year, period})
	rows, _ := tx.Query(ctx, query, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("revenue: list recognition runs: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, runs, func(r Run) (string, uuid.UUID) { return r.PostingDate.Format(time.DateOnly), r.ID },
		func(page []Run) (runNames, error) { return e.runNames(ctx, page) }, runAPI)
	if err != nil {
		return nil, err
	}
	return &gen.RevenueRecognitionRunPage{Items: items, NextPageToken: next}, nil
}
