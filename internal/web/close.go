package web

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The period close (ADR-0052 §2, T73i): the posting periods of a company
// and ledger in a fiscal year (this file); a period's page with its
// opening and closing, its lock and the request of its unlock, which the
// controller decides in the approvals inbox (T32; period.go); the
// pre-close checks of a period (close_checks.go); and the
// foreign-currency revaluation's preview, its post and its runs
// (ADR-0042; revaluation.go). Segregation of duties is the API's: whoever
// asked for an unlock is refused as its approver, and the inbox says so
// by the problem's code.

// A list page shows periodPageSize periods: a fiscal year has at most
// sixteen, twelve and four special ones. With getCurrentPrincipal,
// listLegalEntities, listLedgers and listPrincipalNames it makes five
// calls (ADR-0052 §10: six).
const periodPageSize = 20

// periodStatuses are the period statuses the catalog names.
var periodStatuses = map[string]i18n.Key{"open": i18n.PeriodStatusOpen, "closed": i18n.PeriodStatusClosed, "locked": i18n.PeriodStatusLocked}

// periodPath is a period's page.
func periodPath(id uuid.UUID) string { return ui.PeriodsBase + "/" + id.String() }

// periodsOf is the periods' list of a company, ledger and year.
func periodsOf(company, ledger uuid.UUID, year int32) string {
	return ui.PeriodsPath(url.Values{ui.FieldCompany: {company.String()}, ui.FieldLedger: {ledger.String()},
		ui.FieldYear: {strconv.Itoa(int(year))}})
}

// periods is the periods' list: of the company, ledger and year the query
// names, else of the first company, the leading ledger and this year.
func (s *Server) periods(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	companies, ledgers := s.companies(r.Context(), m), s.ledgers(r.Context(), m)
	f := ui.PeriodFilters{Companies: companyOptions(companies, q.Get(ui.FieldCompany)), Ledgers: ledgerOptions(ledgers, q.Get(ui.FieldLedger)),
		Year: q.Get(ui.FieldYear)}
	if f.Year == "" {
		f.Year = strconv.Itoa(s.now().Year())
	}
	company, ledger := askedOrFirst(q.Get(ui.FieldCompany), f.Companies), askedOrFirst(q.Get(ui.FieldLedger), f.Ledgers)
	l := ui.PeriodList{Filters: f}
	if m.may(permReadEntries) {
		l.Revalue = ui.RevaluationFor(url.Values{ui.FieldCompany: {company}, ui.FieldLedger: {ledger}})
	}
	params, valid := periodParams(q, company, ledger, &l.Filters)
	status := http.StatusOK
	switch {
	case !valid:
		l.Invalid, status = true, http.StatusBadRequest
	case l.Filters.YearError != "":
		status = http.StatusUnprocessableEntity
	default:
		res, err := s.API.ListFiscalPeriods(r.Context(), params)
		page, ok := res.(*gen.FiscalPeriodPage)
		if !ok {
			prob, show := s.refusal(w, r, p, "listFiscalPeriods", res, err)
			if !show {
				return
			}
			l.Refusal, status = explainIn(i18n.PeriodProblems, i18n.PeriodsRefusedTitle, prob, m), int(prob.Status)
			break
		}
		l.Rows = s.periodRows(r.Context(), p, page.Items)
		if next := page.NextPageToken.Or(""); next != "" {
			l.More = ui.PeriodsPath(with(q, "page", next))
		}
	}
	s.render(w, r, status, ui.Periods(s.page(i18n.CloseTitle, ui.ClosePath, p, m), l))
}

// askedOrFirst is the value asked for, else the first of the options, marked
// chosen; "" without either.
func askedOrFirst(asked string, options []ui.Option) string {
	if asked != "" || len(options) == 0 {
		return asked
	}
	options[0].Selected = true
	return options[0].Value
}

// periodParams are the API's filters of the list, and whether the company
// and the ledger are ids; a year that is none is the filter's error.
func periodParams(q url.Values, company, ledger string, f *ui.PeriodFilters) (gen.ListFiscalPeriodsParams, bool) {
	params := gen.ListFiscalPeriodsParams{PageSize: gen.NewOptInt32(periodPageSize), OrderBy: gen.NewOptString("period")}
	valid := true
	for _, v := range []struct {
		value string
		set   *gen.OptUUID
	}{{company, &params.LegalEntityID}, {ledger, &params.LedgerID}} {
		if v.value != "" {
			id, err := uuid.Parse(v.value)
			valid = valid && err == nil
			*v.set = gen.NewOptUUID(id)
		}
	}
	year, err := strconv.ParseInt(f.Year, 10, 32)
	if err != nil || year < 1900 || year > 2999 {
		f.YearError = i18n.PeriodYearInvalid
	}
	params.FiscalYear = gen.NewOptInt32(int32(year))
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	return params, valid
}

// periodRows are a page of periods, each with who changed it last.
func (s *Server) periodRows(ctx context.Context, p *person, items []gen.FiscalPeriod) []ui.PeriodRow {
	ids := make([]uuid.UUID, 0, len(items))
	for _, fp := range items {
		ids = append(ids, fp.UpdatedBy)
	}
	names := s.names(ctx, p, ids)
	out := make([]ui.PeriodRow, 0, len(items))
	for _, fp := range items {
		out = append(out, ui.PeriodRow{Path: periodPath(fp.ID), Year: fp.FiscalYear, Period: fp.Period, Start: fp.StartDate, End: fp.EndDate,
			Special: fp.IsSpecial, Status: labelOf(periodStatuses, fp.Status), UnlockPending: fp.UnlockRequestID.Set, UpdatedAt: fp.UpdatedAt,
			UpdatedBy: names[fp.UpdatedBy]})
	}
	return out
}

// maxLedgers is the most ledgers a page names, the API's largest page.
const maxLedgers = 500

// ledgers are the tenant's ledgers (listLedgers), the leading one first,
// then by code; none without the permission to list them. A call that
// fails leaves them unnamed, logged: a ledger's name is no reason to
// refuse a page.
func (s *Server) ledgers(ctx context.Context, m me) []gen.Ledger {
	if !m.may(permReadAccounts) {
		return nil
	}
	res, err := s.API.ListLedgers(ctx, gen.ListLedgersParams{PageSize: gen.NewOptInt32(maxLedgers)})
	page, ok := res.(*gen.LedgerPage)
	if !ok {
		s.Log.WarnContext(ctx, "web: the ledgers could not be read", "answer", res, "error", err)
		return nil
	}
	out := page.Items
	slices.SortFunc(out, func(a, b gen.Ledger) int {
		if a.IsLeading != b.IsLeading {
			if a.IsLeading {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Code, b.Code)
	})
	return out
}

// ledgerOptions are the ledgers to choose, the leading one first; nil
// when they could not be read.
func ledgerOptions(ledgers []gen.Ledger, chosen string) []ui.Option {
	if ledgers == nil {
		return nil
	}
	out := make([]ui.Option, 0, len(ledgers))
	for _, l := range ledgers {
		out = append(out, ui.Option{Value: l.ID.String(), Text: ledgerText(l), Selected: l.ID.String() == chosen})
	}
	return out
}

// ledgerText is a ledger as the pages name it: its code and name.
func ledgerText(l gen.Ledger) string { return l.Code + " " + l.Name }

// ledgerName is a ledger's code and name by id, its id when it could not
// be read.
func ledgerName(ledgers []gen.Ledger, id uuid.UUID) string {
	if i := slices.IndexFunc(ledgers, func(l gen.Ledger) bool { return l.ID == id }); i >= 0 {
		return ledgerText(ledgers[i])
	}
	return id.String()
}
