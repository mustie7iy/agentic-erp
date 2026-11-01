package taxfiling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Registration is a stored tax registration.
type Registration struct {
	ID                     uuid.UUID
	Code                   string
	LegalEntityID          uuid.UUID
	Members                []uuid.UUID
	Authority, NumberLast4 string
	ReturnType, Frequency  string
	FirstPeriodStart       time.Time
	LastPeriodEnd          *time.Time
	Active                 bool
}

const registrationSelect = `SELECT id, code, legal_entity_id, group_member_ids, authority, number_last4, return_type, frequency,
	first_period_start, last_period_end, is_active FROM tax.registration`

// visible is the condition on registrations covering a company the caller
// may act in ($1 all companies, $2 the legal entities otherwise).
const visible = `($1 OR legal_entity_id = ANY($2) OR group_member_ids && $2::uuid[])`

func scanRegistration(row pgx.CollectableRow) (Registration, error) {
	var r Registration
	err := row.Scan(&r.ID, &r.Code, &r.LegalEntityID, &r.Members, &r.Authority, &r.NumberLast4, &r.ReturnType, &r.Frequency,
		&r.FirstPeriodStart, &r.LastPeriodEnd, &r.Active)
	return r, errs.Wrapf(err, "scan tax registration")
}

var registrationSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "code", Type: "text"}}}

// ListTaxRegistrations implements GET /v1/tax-registrations: the
// registrations covering a company the caller may act in, the number by its
// last four characters, each company named by its code.
func (e *Endpoints) ListTaxRegistrations(ctx context.Context, params gen.ListTaxRegistrationsParams) (gen.ListTaxRegistrationsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	scope := fmt.Sprintf("listTaxRegistrations|%s|%s", le, params.Authority.Value)
	ks, err := e.Pages.Keyset(registrationSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(registrationSelect+` WHERE `+visible+` AND ($3::uuid IS NULL OR legal_entity_id = $3 OR $3 = ANY(group_member_ids))
		AND ($4 = '' OR authority = $4)`, []any{access.All, access.IDs, le, params.Authority.Value})
	rows, _ := tx.Query(ctx, q, args...)
	regs, err := pgx.CollectRows(rows, scanRegistration)
	if err != nil {
		return nil, fmt.Errorf("taxfiling: list tax registrations: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, regs, func(r Registration) (string, uuid.UUID) { return r.Code, r.ID },
		e.companyCodes(ctx), registrationToAPI)
	if err != nil {
		return nil, fmt.Errorf("taxfiling: list tax registrations: %w", err)
	}
	return &gen.TaxRegistrationPage{Items: items, NextPageToken: next}, nil
}

// companyCodes reads the company codes of the legal entities a page of
// registrations names, each once.
func (e *Endpoints) companyCodes(ctx context.Context) func([]Registration) (map[uuid.UUID]string, error) {
	return func(regs []Registration) (map[uuid.UUID]string, error) {
		codes := map[uuid.UUID]string{}
		for _, r := range regs {
			for _, id := range append([]uuid.UUID{r.LegalEntityID}, r.Members...) {
				if _, done := codes[id]; done {
					continue
				}
				le, ok, err := e.LegalEntities.Entity(ctx, id)
				if err != nil {
					return nil, err
				}
				if !ok { // legal entities are never deleted, and Setup names only existing ones
					return nil, fmt.Errorf("no legal entity %s for tax registration %s", id, r.Code)
				}
				codes[id] = le.Code
			}
		}
		return codes, nil
	}
}

func registrationToAPI(r Registration, codes map[uuid.UUID]string) gen.TaxRegistration {
	members := make([]gen.TaxRegistrationGroupMembersItem, len(r.Members))
	for i, id := range r.Members {
		members[i] = gen.TaxRegistrationGroupMembersItem{LegalEntityID: id, CompanyCode: codes[id]}
	}
	out := gen.TaxRegistration{ID: r.ID, Code: r.Code, LegalEntityID: r.LegalEntityID, CompanyCode: codes[r.LegalEntityID], GroupMembers: members,
		Authority: r.Authority, NumberLast4: r.NumberLast4, ReturnType: r.ReturnType, Frequency: r.Frequency,
		FirstPeriodStart: r.FirstPeriodStart, Status: "active"}
	if r.LastPeriodEnd != nil {
		out.LastPeriodEnd = gen.NewOptDate(*r.LastPeriodEnd)
	}
	if !r.Active {
		out.Status = "inactive"
	}
	return out
}

var periodSort = pagination.Sort{Default: "start_date", Fields: []pagination.Field{{Name: "start_date", Column: "start_date", Type: "date"}}}

// ListTaxPeriods implements GET /v1/tax-registrations/{id}/periods: the
// registration's periods starting by start_date_to, or by its company's
// date today in the company's time zone, which the answer names.
func (e *Endpoints) ListTaxPeriods(ctx context.Context, params gen.ListTaxPeriodsParams) (gen.ListTaxPeriodsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	to := apitypes.Ptr[time.Time](params.StartDateTo)
	ks, err := e.Pages.Keyset(periodSort, fmt.Sprintf("listTaxPeriods|%s|%v", params.ID, to), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, registrationSelect+` WHERE `+visible+` AND id = $3`, access.All, access.IDs, params.ID)
	r, err := pgx.CollectExactlyOneRow(rows, scanRegistration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem.NotFoundOf("tax registration")
	}
	if err != nil {
		return nil, fmt.Errorf("taxfiling: tax registration %s: %w", params.ID, err)
	}
	out := gen.TaxPeriodPage{RegistrationID: r.ID, RegistrationCode: r.Code}
	if to == nil {
		day, err := config.EntityDay(ctx, e.Settings, e.companyCode, r.LegalEntityID, e.Now())
		if err != nil {
			return nil, err
		}
		to, out.TimeZone = &day.Date, gen.NewOptString(day.Zone.String())
	}
	out.StartDateTo = *to
	if out.Items, out.NextPageToken, err = e.periodPage(ctx, tx, ks, r, *to); err != nil {
		return nil, err
	}
	return &out, nil
}

// companyCode is the legal entity's company code, for config.EntityDay.
func (e *Endpoints) companyCode(ctx context.Context, le uuid.UUID) (string, bool, error) {
	ent, ok, err := e.LegalEntities.Entity(ctx, le)
	return ent.Code, ok, err
}

// periodPage pages the registration's periods starting by to: they are
// computed (config.TaxPeriods) and paged as a listing's rows are.
func (e *Endpoints) periodPage(ctx context.Context, tx pgx.Tx, ks *pagination.Keyset, r Registration, to time.Time) ([]gen.TaxPeriod, gen.OptString, error) {
	periods := config.TaxPeriods(r.FirstPeriodStart, r.Frequency, r.LastPeriodEnd, to)
	starts, ends := make([]time.Time, len(periods)), make([]time.Time, len(periods))
	for i, p := range periods {
		starts[i], ends[i] = p.Start, p.End
	}
	q, args := ks.SQL(`SELECT start_date, end_date FROM (SELECT s AS start_date, e AS end_date, $1::uuid AS id
		FROM unnest($2::date[], $3::date[]) AS x(s, e)) p WHERE true`, []any{r.ID, starts, ends})
	rows, _ := tx.Query(ctx, q, args...)
	page, err := pgx.CollectRows(rows, pgx.RowToStructByPos[config.TaxPeriod])
	if err != nil {
		return nil, gen.OptString{}, fmt.Errorf("taxfiling: periods of tax registration %s: %w", r.Code, err)
	}
	items, next := apitypes.PageOf(ks, page, func(p config.TaxPeriod) (string, uuid.UUID) { return p.Start.Format(time.DateOnly), r.ID },
		func(p config.TaxPeriod) gen.TaxPeriod { return gen.TaxPeriod{StartDate: p.Start, EndDate: p.End} })
	return items, next, nil
}
