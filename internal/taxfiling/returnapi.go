package taxfiling

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
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// PrepareTaxReturn implements POST /v1/tax-registrations/{id}/returns.
func (e *Endpoints) PrepareTaxReturn(ctx context.Context, req *gen.TaxReturnPrepare, params gen.PrepareTaxReturnParams) (gen.PrepareTaxReturnRes, error) {
	kind := req.Kind.Or(Original)
	if kind != Original && kind != Amendment {
		return nil, problem.Unprocessable("tax.invalid_kind", "A return is an original or an amendment.",
			problem.FieldError{Field: "kind", Code: "invalid", Message: "original or amendment"})
	}
	r, err := e.prepare(ctx, params.ID, req.PeriodStart, kind)
	if err != nil {
		return nil, err
	}
	return &gen.TaxReturnHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/tax-returns/" + r.ID.String()),
		Response: returnToAPI(r)}, nil
}

// GetTaxReturn implements GET /v1/tax-returns/{id}: a return of a
// registration the caller sees.
func (e *Endpoints) GetTaxReturn(ctx context.Context, params gen.GetTaxReturnParams) (gen.GetTaxReturnRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readReturn(ctx, tx, params.ID, &access, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetTaxReturnOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: returnToAPI(r)}, nil
}

var returnSort = pagination.Sort{Default: "period_start", Fields: []pagination.Field{{Name: "period_start", Column: "period_start", Type: "date"}}}

// ListTaxReturns implements GET /v1/tax-registrations/{id}/returns: the
// returns of a registration the caller sees, by period.
func (e *Endpoints) ListTaxReturns(ctx context.Context, params gen.ListTaxReturnsParams) (gen.ListTaxReturnsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	start := apitypes.Ptr[time.Time](params.PeriodStart)
	scope := fmt.Sprintf("listTaxReturns|%s|%v|%s", params.ID, start, params.Status.Value)
	ks, err := e.Pages.Keyset(returnSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	var seen bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM tax.registration WHERE `+visible+` AND id = $3)`, access.All, access.IDs, params.ID).
		Scan(&seen); err != nil {
		return nil, fmt.Errorf("taxfiling: tax registration %s: %w", params.ID, err)
	}
	if !seen {
		return nil, problem.NotFoundOf("tax registration")
	}
	q, args := ks.SQL(returnSelect+` WHERE registration_id = $1 AND ($2::date IS NULL OR period_start = $2) AND ($3 = '' OR status = $3)`,
		[]any{params.ID, start, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	returns, err := pgx.CollectRows(rows, scanReturn)
	if err != nil {
		return nil, fmt.Errorf("taxfiling: returns of tax registration %s: %w", params.ID, err)
	}
	items, next := apitypes.PageOf(ks, returns, func(r Return) (string, uuid.UUID) { return r.PeriodStart.Format(time.DateOnly), r.ID }, returnToAPI)
	return &gen.TaxReturnPage{Items: items, NextPageToken: next}, nil
}

// FileTaxReturn implements POST /v1/tax-returns/{id}:file.
func (e *Endpoints) FileTaxReturn(ctx context.Context, req *gen.TaxReturnFile, params gen.FileTaxReturnParams) (gen.FileTaxReturnRes, error) {
	r, err := e.file(ctx, params.ID, etag.IfMatch(params.IfMatch), req.ConfirmationNumber, req.FiledOn)
	if err != nil {
		return nil, err
	}
	return &gen.FileTaxReturnOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: returnToAPI(r)}, nil
}

// WithdrawTaxReturn implements POST /v1/tax-returns/{id}:withdraw.
func (e *Endpoints) WithdrawTaxReturn(ctx context.Context, req *gen.ReasonInput, params gen.WithdrawTaxReturnParams) (gen.WithdrawTaxReturnRes, error) {
	r, err := e.withdraw(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return &gen.FileTaxReturnOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: returnToAPI(r)}, nil
}

func returnToAPI(r Return) gen.TaxReturn {
	m := func(v money.Minor) gen.Money { return apitypes.Money(v, r.Currency, r.Exponent) }
	out := gen.TaxReturn{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), RegistrationID: r.RegistrationID, RegistrationCode: r.RegistrationCode, Kind: r.Kind,
		AmendsID: apitypes.OptUUID(r.AmendsID), PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Status: r.Status, ReturnType: r.ReturnType,
		ReturnName: r.ReturnName, Pack: gen.TaxReturnPack{Country: r.PackCountry, Version: r.PackVersion}, Currency: r.Currency,
		ItemCount: r.ItemCount, ItemsHash: r.ItemsHash, ApprovalRequestID: r.ApprovalRequestID, ApprovedBy: apitypes.OptUUID(r.ApprovedBy),
		PriorPeriod: gen.TaxReturnPriorPeriod{ItemCount: r.PriorItemCount, Tax: m(r.PriorTax), AboveThreshold: r.AboveThreshold},
		Boxes:       make([]gen.TaxReturnSnapshotBox, len(r.Boxes))}
	if r.PriorThreshold != nil {
		out.PriorPeriod.Threshold = gen.NewOptMoney(m(*r.PriorThreshold))
	}
	if r.ApprovedAt != nil {
		out.ApprovedAt = gen.NewOptDateTime(*r.ApprovedAt)
	}
	for i, b := range r.Boxes {
		box := gen.TaxReturnSnapshotBox{Code: b.Code, Label: b.Label, Kind: b.Kind, Amount: m(b.Amount), PriorPeriodAmount: m(b.PriorPeriodAmount),
			CorrectedAmount: m(b.CorrectedAmount)}
		if b.ItemCount != nil {
			box.ItemCount = gen.NewOptInt64(*b.ItemCount)
		}
		out.Boxes[i] = box
	}
	if r.Status == Filed {
		out.Filing = gen.NewOptTaxReturnFiling(gen.TaxReturnFiling{ConfirmationNumber: *r.ConfirmationNumber, FiledOn: *r.FiledOn, RecordedBy: *r.FiledBy,
			RecordedAt: *r.FiledAt})
	}
	if r.DecidedBy != nil {
		out.Decision = gen.NewOptTaxReturnDecision(gen.TaxReturnDecision{DecidedBy: *r.DecidedBy, DecidedAt: *r.DecidedAt,
			Comment: apitypes.OptString(r.DecisionComment)})
	}
	return out
}
