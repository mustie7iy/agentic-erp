package payables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// expenseRefs is what expense reports name by id, as they are now: their
// companies (and each company's configuration, for its expense types'
// names), employees, accounts and cost centers.
type expenseRefs struct {
	companies map[uuid.UUID]tenancy.Entity
	settings  map[uuid.UUID]*config.Company
	employees map[uuid.UUID]partner.Named
	accounts  map[uuid.UUID]ledger.AccountRef
	centers   map[uuid.UUID]ledger.CenterName
}

// expenseReferences reads what the reports name, each once.
func (e *Endpoints) expenseReferences(ctx context.Context, reports []ExpenseReport) (expenseRefs, error) {
	refs := expenseRefs{companies: map[uuid.UUID]tenancy.Entity{}, settings: map[uuid.UUID]*config.Company{}}
	var employees, accounts, centers []uuid.UUID
	for _, r := range reports {
		employees = append(employees, r.EmployeeID)
		for _, l := range r.Lines {
			accounts = append(accounts, l.AccountID)
			if l.CostCenterID != nil {
				centers = append(centers, *l.CostCenterID)
			}
		}
		if _, ok := refs.companies[r.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a report's legal entity exists
		if err != nil {
			return expenseRefs{}, err
		}
		if refs.settings[r.LegalEntityID], err = e.company(ctx, le.Code); err != nil {
			return expenseRefs{}, err
		}
		refs.companies[r.LegalEntityID] = le
	}
	var err error
	if refs.employees, err = e.Partners.Names(ctx, employees); err != nil {
		return expenseRefs{}, fmt.Errorf("payables: employees of %d expense reports: %w", len(reports), err)
	}
	if refs.accounts, err = e.ExpenseLedger.AccountNames(ctx, accounts); err != nil {
		return expenseRefs{}, err
	}
	if refs.centers, err = e.ExpenseLedger.CenterNames(ctx, centers); err != nil {
		return expenseRefs{}, err
	}
	return refs, nil
}

// expenseAPI is an expense report as the API gives it, with what it names.
func expenseAPI(r ExpenseReport, refs expenseRefs) gen.ExpenseReport {
	le, employee := refs.companies[r.LegalEntityID], refs.employees[r.EmployeeID]
	gross, tax, net := r.Totals()
	amount := func(m interface{ Plain(int) string }) gen.Money {
		return gen.Money{Amount: m.Plain(r.Exponent), Currency: r.Currency}
	}
	var year *int32
	if r.JournalEntryYear != nil {
		y := int32(*r.JournalEntryYear)
		year = &y
	}
	out := gen.ExpenseReport{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), DocumentNumber: r.DocumentNumber, LegalEntityID: r.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, EmployeeID: r.EmployeeID, EmployeeNumber: employee.Number, EmployeeName: employee.Name, ReportDate: r.ReportDate,
		Description: apitypes.OptString(r.Description), GrossAmount: amount(gross), TaxAmount: amount(tax), NetAmount: amount(net), Status: r.Status,
		PostingDate: apitypes.OptDate(r.PostingDate), JournalEntryID: apitypes.OptUUID(r.JournalEntryID), JournalEntryYear: apitypes.OptInt32(year),
		PaymentTermCode: apitypes.OptString(r.PaymentTermCode), NetDueDate: apitypes.OptDate(r.NetDueDate), Lines: make([]gen.ExpenseReportLine, len(r.Lines))}
	co := refs.settings[r.LegalEntityID]
	for i, l := range r.Lines {
		account := refs.accounts[l.AccountID]
		line := gen.ExpenseReportLine{LineNo: l.LineNo, ExpenseDate: l.ExpenseDate.Time, ExpenseType: l.ExpenseType, AccountID: l.AccountID,
			AccountNumber: account.Number, AccountName: account.Name, Amount: amount(l.Amount), TaxCode: apitypes.OptString(l.TaxCode),
			TaxAmount: amount(l.TaxAmount), NetAmount: amount(l.Amount - l.TaxAmount), CostCenterID: apitypes.OptUUID(l.CostCenterID),
			Description: apitypes.OptString(l.Description)}
		if co != nil {
			if t, ok := co.ExpenseType(l.ExpenseType); ok {
				line.ExpenseTypeName = gen.NewOptString(t.Name)
			}
		}
		if l.CostCenterID != nil {
			c := refs.centers[*l.CostCenterID]
			line.CostCenterCode, line.CostCenterName = gen.NewOptString(c.Code), gen.NewOptString(c.Name)
		}
		out.Lines[i] = line
	}
	return out
}

// expenseAnswer is a report as an operation answers it.
func (e *Endpoints) expenseAnswer(ctx context.Context, r ExpenseReport) (gen.ExpenseReport, error) {
	refs, err := e.expenseReferences(ctx, []ExpenseReport{r})
	if err != nil {
		return gen.ExpenseReport{}, err
	}
	return expenseAPI(r, refs), nil
}

// CreateExpenseReport implements POST /v1/expense-reports.
func (e *Endpoints) CreateExpenseReport(ctx context.Context, req *gen.ExpenseReportCreate, _ gen.CreateExpenseReportParams) (gen.CreateExpenseReportRes, error) {
	in := ExpenseInput{LegalEntityID: req.LegalEntityID, EmployeeID: req.EmployeeID, Description: apitypes.Ptr[string](req.Description),
		Lines: make([]ExpenseLineInput, len(req.Lines))}
	for i, l := range req.Lines {
		in.Lines[i] = ExpenseLineInput{ExpenseDate: l.ExpenseDate, ExpenseType: l.ExpenseType, Amount: Amount{l.Amount.Amount, l.Amount.Currency},
			TaxCode: apitypes.Ptr[string](l.TaxCode), CostCenterID: apitypes.Ptr[uuid.UUID](l.CostCenterID), Description: apitypes.Ptr[string](l.Description)}
	}
	r, err := e.EnterExpenseReport(ctx, in)
	if err != nil {
		return nil, err
	}
	out, err := e.expenseAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.ExpenseReportHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/expense-reports/" + r.ID.String()),
		Response: out}, nil
}

// GetExpenseReport implements GET /v1/expense-reports/{id}.
func (e *Endpoints) GetExpenseReport(ctx context.Context, params gen.GetExpenseReportParams) (gen.GetExpenseReportRes, error) {
	r, err := GetExpenseReport(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.expenseAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.GetExpenseReportOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

// PostExpenseReport implements POST /v1/expense-reports/{id}:post.
func (e *Endpoints) PostExpenseReport(ctx context.Context, req gen.OptExpenseReportPost, params gen.PostExpenseReportParams) (gen.PostExpenseReportRes, error) {
	r, err := e.PostExpense(ctx, params.ID, etag.IfMatch(params.IfMatch), apitypes.Ptr[time.Time](req.Value.PostingDate))
	if err != nil {
		return nil, err
	}
	out, err := e.expenseAnswer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &gen.PostExpenseReportOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: out}, nil
}

var expenseSort = pagination.Sort{Default: "report_date", Fields: []pagination.Field{
	{Name: "report_date", Column: "r.report_date", Type: "date"}, {Name: "document_number", Column: "r.document_number", Type: "text"},
}}

// ListExpenseReports implements GET /v1/expense-reports: the expense
// reports of the companies the caller may act in.
func (e *Endpoints) ListExpenseReports(ctx context.Context, params gen.ListExpenseReportsParams) (gen.ListExpenseReportsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, employee := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.EmployeeID)
	scope := fmt.Sprintf("listExpenseReports|%s|%s|%s|%s", le, employee, params.Status.Value, params.DocumentNumber.Value)
	ks, err := e.Pages.Keyset(expenseSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("payables: page of expense reports: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("payables: company access for expense reports: %w", err)
	}
	query, args := ks.SQL(expenseSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR r.employee_id = $4) AND ($5 = '' OR r.status = $5) AND ($6 = '' OR r.document_number = $6)`,
		[]any{access.All, access.IDs, le, employee, params.Status.Value, params.DocumentNumber.Value})
	rows, _ := tx.Query(ctx, query, args...)
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ExpenseReport, error) { return scanExpense(row) })
	if err != nil {
		return nil, fmt.Errorf("payables: list expense reports: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, found, func(r ExpenseReport) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return r.DocumentNumber, r.ID
		}
		return r.ReportDate.Format(time.DateOnly), r.ID
	}, func(page []ExpenseReport) (expenseRefs, error) { return e.expenseReferences(ctx, page) }, expenseAPI)
	if err != nil {
		return nil, err
	}
	return &gen.ExpenseReportPage{Items: items, NextPageToken: next}, nil
}
