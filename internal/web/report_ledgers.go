package web

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The ledger's reports (ADR-0052 §2): a company's trial balance in a
// ledger and fiscal year (listBalances), which with getCurrentPrincipal,
// listLegalEntities, listLedgers and listAccounts makes five calls; the
// reconciliation of two ledgers it keeps (listLedgerReconciliation, ADR-0045),
// five as well; and the entries behind one account's difference
// (listLedgerReconciliationEntries), four with the ledgers and accounts.
// Their figures are the API's balances as they stand at the moment of the
// read (07 §7.3); the workspace adds none of its own.

// ledgerQuery is what a ledger report is of: a company, a ledger (nil:
// the leading one), the ledger it is compared with, a fiscal year and a
// period (unset: the whole year).
type ledgerQuery struct {
	company, ledger, compare uuid.UUID
	year                     int32
	period                   gen.OptInt32
}

// ledgerPtr is the query's ledger for the API, unset for the leading one.
func (lq ledgerQuery) ledgerPtr() gen.OptUUID {
	if lq.ledger == uuid.Nil {
		return gen.OptUUID{}
	}
	return gen.NewOptUUID(lq.ledger)
}

// values are the query as a report's address names it.
func (lq ledgerQuery) values() url.Values {
	q := url.Values{ui.FieldCompany: {lq.company.String()}, ui.FieldYear: {strconv.Itoa(int(lq.year))}}
	if lq.ledger != uuid.Nil {
		q.Set(ui.FieldLedger, lq.ledger.String())
	}
	if lq.compare != uuid.Nil {
		q.Set(ui.FieldCompare, lq.compare.String())
	}
	if p, ok := lq.period.Get(); ok {
		q.Set(ui.FieldPeriod, strconv.Itoa(int(p)))
	}
	return q
}

// parseLedgerQuery reads a ledger report's query into its filters (whose
// options are the companies and ledgers to choose from, the first company
// and the leading ledger chosen until the query names others, this year
// by the company's date (year) until it names one): valid is false when
// the company, the ledgers or the page are not what an address of the
// report holds; a year or period that is none is its filter's error.
func parseLedgerQuery(q url.Values, f *ui.LedgerFilters, year func(company string) string) (ledgerQuery, bool) {
	company, ledger := askedOrFirst(q.Get(ui.FieldCompany), f.Companies), askedOrFirst(q.Get(ui.FieldLedger), f.Ledgers)
	var lq ledgerQuery
	id, err := uuid.Parse(company)
	valid := err == nil
	lq.company = id
	for _, v := range []struct {
		value string
		set   *uuid.UUID
	}{{ledger, &lq.ledger}, {q.Get(ui.FieldCompare), &lq.compare}} {
		if v.value != "" {
			id, err := uuid.Parse(v.value)
			valid = valid && err == nil
			*v.set = id
		}
	}
	typed := q.Get(ui.FieldYear)
	if typed == "" {
		typed = year(company)
	}
	f.Year, lq.year, f.YearError = yearOf(typed)
	f.Period = q.Get(ui.FieldPeriod)
	lq.period, f.PeriodError = periodParam(f.Period)
	return lq, valid
}

// trialBalance is a company's trial balance: a page of its totals per
// account, period and transaction currency in the ledger and year (the
// first company, the leading ledger and this year until chosen).
func (s *Server) trialBalance(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	tp := ui.TrialBalancePage{Filters: ui.LedgerFilters{Action: ui.TrialBalancePath, Companies: companyOptions(s.companies(ctx, m), q.Get(ui.FieldCompany)),
		Ledgers: ledgerOptions(s.ledgers(ctx, m), q.Get(ui.FieldLedger))}}
	lq, valid := parseLedgerQuery(q, &tp.Filters, s.yearOfCompany(ctx, m))
	status := http.StatusOK
	switch {
	case !valid:
		tp.Invalid, status = true, http.StatusBadRequest
	case tp.Filters.YearError != "" || tp.Filters.PeriodError != "":
		status = http.StatusUnprocessableEntity
	default:
		res, err := s.API.ListBalances(ctx, balanceParams(lq, reportPageSize, q.Get("page")))
		page, isPage := res.(*gen.BalancePage)
		if !isPage {
			prob, show := s.refusal(w, r, p, "listBalances", res, err)
			if !show {
				return
			}
			tp.Refusal, status = explain(i18n.ReportRefusedTitle, prob, m), int(prob.Status)
			break
		}
		tp.Rows = balanceRows(page.Items, s.accounts(ctx, m, lq.company))
		if next := page.NextPageToken.Or(""); next != "" {
			tp.More = ui.ReportFor(ui.TrialBalancePath, with(q, "page", next))
		}
		tp.CSV = ui.ReportFor(ui.TrialBalancePath+ui.CSVSuffix, lq.values())
	}
	s.render(w, r, status, ui.TrialBalance(s.page(i18n.TrialBalanceTitle, ui.ReportsPath, p, m), tp))
}

// balanceParams are the API's parameters of a trial balance's page.
func balanceParams(lq ledgerQuery, size int32, token string) gen.ListBalancesParams {
	params := gen.ListBalancesParams{LegalEntityID: lq.company, LedgerID: lq.ledgerPtr(), FiscalYear: lq.year, FiscalPeriod: lq.period,
		PageSize: gen.NewOptInt32(size)}
	if token != "" {
		params.PageToken = gen.NewOptString(token)
	}
	return params
}

// balanceRows are a page of a trial balance, its accounts named.
func balanceRows(items []gen.Balance, accounts map[uuid.UUID]string) []ui.BalanceRow {
	out := make([]ui.BalanceRow, 0, len(items))
	for _, b := range items {
		out = append(out, ui.BalanceRow{Account: accountText(accounts, b.AccountID), Period: b.FiscalPeriod, Currency: b.Currency, Debit: moneyOf(b.Debit),
			Credit: moneyOf(b.Credit), DebitLC: moneyOf(b.DebitLc), CreditLC: moneyOf(b.CreditLc)})
	}
	return out
}

// ledgerRecon is the reconciliation of two ledgers a company keeps: the
// ledger chosen (the leading one until then) with the one compared (the
// leading one for a parallel ledger, the company's first parallel one for
// the leading), each ledger's trial balance check and a page of the
// accounts that differ, each linked to the entries behind it.
func (s *Server) ledgerRecon(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	companies, ledgers := s.companies(ctx, m), s.ledgers(ctx, m)
	lp := ui.LedgerReconPage{Filters: ui.LedgerFilters{Action: ui.LedgerReconPath, Companies: companyOptions(companies, q.Get(ui.FieldCompany))}}
	kept := keptBy(ledgers, askedOrFirst(q.Get(ui.FieldCompany), lp.Filters.Companies))
	ledger, compare, found := compared(kept, chosenLedger(kept, q.Get(ui.FieldLedger)))
	switch {
	case found:
		q = defaulted(q, kept, map[string]uuid.UUID{ui.FieldLedger: ledger.ID, ui.FieldCompare: compare.ID})
		lp.Filters.Ledgers, lp.Filters.Compare = ledgerOptions(kept, q.Get(ui.FieldLedger)), ledgerOptions(kept, q.Get(ui.FieldCompare))
	case ledgers == nil && q.Get(ui.FieldLedger) != "" && q.Get(ui.FieldCompare) != "":
		found = true // the ledgers could not be read: the query's, by id
	case ledgers == nil:
		lp.Unknown = true
	}
	lq, valid := parseLedgerQuery(q, &lp.Filters, s.yearOfCompany(ctx, m))
	status := http.StatusOK
	switch {
	case !valid:
		lp.Invalid, status = true, http.StatusBadRequest
	case lp.Unknown:
	case !found:
		lp.Single = true
	case lp.Filters.YearError != "" || lp.Filters.PeriodError != "":
		status = http.StatusUnprocessableEntity
	default:
		if status, ok = s.reconcileLedgers(w, r, p, m, lq, kept, &lp); !ok {
			return
		}
	}
	s.render(w, r, status, ui.LedgerRecon(s.page(i18n.LedgerReconTitle, ui.ReportsPath, p, m), lp))
}

// keptBy are the ledgers the company keeps, the leading one first; none
// when the company is no id.
func keptBy(ledgers []gen.Ledger, company string) []gen.Ledger {
	id, err := uuid.Parse(company)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(ledgers), func(l gen.Ledger) bool { return !slices.Contains(l.LegalEntityIds, id) })
}

// chosenLedger is the ledger the query names, the leading one (first of
// kept) when it names none or one the company does not keep (defaulted
// then replaces it in the query).
func chosenLedger(kept []gen.Ledger, asked string) uuid.UUID {
	if i := slices.IndexFunc(kept, func(l gen.Ledger) bool { return l.ID.String() == asked }); i >= 0 {
		return kept[i].ID
	}
	if len(kept) > 0 {
		return kept[0].ID
	}
	return uuid.Nil
}

// defaulted is a copy of q, its page kept, with each key that names none
// of the kept ledgers set to its default: the form keeps the ledgers of
// the company it was sent for when the company changed.
func defaulted(q url.Values, kept []gen.Ledger, defaults map[string]uuid.UUID) url.Values {
	out := maps.Clone(q)
	for k, id := range defaults {
		if !slices.ContainsFunc(kept, func(l gen.Ledger) bool { return l.ID.String() == out.Get(k) }) {
			out.Set(k, id.String())
		}
	}
	return out
}

// reconcileLedgers reads a page of the reconciliation into the page, and
// the page's status; ok is false when the answer is already written.
func (s *Server) reconcileLedgers(w http.ResponseWriter, r *http.Request, p *person, m me, lq ledgerQuery, kept []gen.Ledger, lp *ui.LedgerReconPage) (int, bool) {
	q := r.URL.Query()
	params := gen.ListLedgerReconciliationParams{LegalEntityID: lq.company, LedgerID: lq.ledgerPtr(), CompareLedgerID: lq.compare, FiscalYear: lq.year,
		FiscalPeriod: lq.period, PageSize: gen.NewOptInt32(reportPageSize)}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	res, err := s.API.ListLedgerReconciliation(r.Context(), params)
	page, ok := res.(*gen.LedgerReconciliationPage)
	if !ok {
		prob, show := s.refusal(w, r, p, "listLedgerReconciliation", res, err)
		if !show {
			return 0, false
		}
		lp.Refusal = explain(i18n.ReportRefusedTitle, prob, m)
		return int(prob.Status), true
	}
	lp.Ledger, lp.Compare = ledgerCode(kept, lq.ledger), ledgerCode(kept, lq.compare)
	codes := map[uuid.UUID]string{page.LedgerID: lp.Ledger, page.CompareLedgerID: lp.Compare}
	for _, tb := range page.TrialBalances {
		lp.Trials = append(lp.Trials, ui.TrialView{Ledger: codes[tb.LedgerID], Debit: moneyOf(tb.DebitLc), Credit: moneyOf(tb.CreditLc), Balanced: tb.Balanced})
	}
	lp.Differences = differenceViews(page.Items, s.accounts(r.Context(), m, lq.company), lq)
	if next := page.NextPageToken.Or(""); next != "" {
		lp.More = ui.ReportFor(ui.LedgerReconPath, with(q, "page", next))
	}
	return http.StatusOK, true
}

// ledgerCode is a ledger's code by id, its id when it is not kept.
func ledgerCode(kept []gen.Ledger, id uuid.UUID) string {
	if i := slices.IndexFunc(kept, func(l gen.Ledger) bool { return l.ID == id }); i >= 0 {
		return kept[i].Code
	}
	return id.String()
}

// differenceViews are a reconciliation's differences, their accounts
// named and each linked to the entries behind it.
func differenceViews(items []gen.LedgerDifference, accounts map[uuid.UUID]string, lq ledgerQuery) []ui.DifferenceView {
	out := make([]ui.DifferenceView, 0, len(items))
	for _, d := range items {
		q := lq.values()
		q.Set(ui.FieldAccount, d.AccountID.String())
		q.Set(ui.FieldCurrency, d.Currency)
		out = append(out, ui.DifferenceView{Account: accountText(accounts, d.AccountID), Currency: d.Currency, Balance: moneyOf(d.Balance),
			Compare: moneyOf(d.CompareBalance), Difference: moneyOf(d.Difference), Entries: ui.ReportFor(ui.ReconEntriesPath, q)})
	}
	return out
}

// reconEntries is the entries behind one account's difference between
// two ledgers: each ledger's lines whose entry has no copy in the other,
// each linked to its entry.
func (s *Server) reconEntries(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	var f ui.LedgerFilters
	lq, valid := parseLedgerQuery(q, &f, s.yearOfCompany(ctx, m))
	account, err := uuid.Parse(q.Get(ui.FieldAccount))
	ep := ui.ReconEntriesPage{Currency: q.Get(ui.FieldCurrency), Back: ui.ReportFor(ui.LedgerReconPath, without(without(q, ui.FieldAccount), ui.FieldCurrency))}
	if !valid || err != nil || lq.compare == uuid.Nil || f.YearError != "" || f.PeriodError != "" || !currencyCode.MatchString(ep.Currency) {
		ep.Invalid = true
		s.render(w, r, http.StatusBadRequest, ui.ReconEntries(s.page(i18n.ReconEntriesTitle, ui.ReportsPath, p, m), ep))
		return
	}
	ledgers := s.ledgers(ctx, m)
	ep.Account, ep.Ledger, ep.Compare = accountText(s.accounts(ctx, m, lq.company), account), ledgerCode(ledgers, lq.ledger), ledgerCode(ledgers, lq.compare)
	params := gen.ListLedgerReconciliationEntriesParams{LegalEntityID: lq.company, LedgerID: lq.ledgerPtr(), CompareLedgerID: lq.compare,
		FiscalYear: lq.year, FiscalPeriod: lq.period, AccountID: account, Currency: gen.NewOptString(ep.Currency), PageSize: gen.NewOptInt32(reportPageSize)}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	status := http.StatusOK
	res, err := s.API.ListLedgerReconciliationEntries(ctx, params)
	if page, isPage := res.(*gen.LedgerReconciliationEntryPage); isPage {
		for _, e := range page.Items {
			ep.Rows = append(ep.Rows, ui.ReconEntryRow{Ledger: ledgerCode(ledgers, e.LedgerID), Entry: entryPath(e.JournalEntryID), Number: e.DocumentNumber,
				Line: e.LineNo, Posting: e.PostingDate, Period: e.FiscalPeriod, Amount: moneyOf(e.Amount), AmountLC: moneyOf(e.AmountLc)})
		}
		if next := page.NextPageToken.Or(""); next != "" {
			ep.More = ui.ReportFor(ui.ReconEntriesPath, with(q, "page", next))
		}
	} else {
		prob, show := s.refusal(w, r, p, "listLedgerReconciliationEntries", res, err)
		if !show {
			return
		}
		ep.Refusal, status = explain(i18n.ReportRefusedTitle, prob, m), int(prob.Status)
	}
	s.render(w, r, status, ui.ReconEntries(s.page(i18n.ReconEntriesTitle, ui.ReportsPath, p, m), ep))
}
