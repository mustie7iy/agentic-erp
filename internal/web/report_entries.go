package web

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// accountEntries is a trial balance account's entries (T73j2): the
// journal entries of the ledger, year and period with a line on the
// account (listJournalEntries by account_id and fiscal_period), each of
// their lines on it, linked to its entry. With getCurrentPrincipal, the
// company's day (when the address names no year), listLedgers and
// listAccounts it makes five calls. The way back is the trial balance of
// the row's period.
func (s *Server) accountEntries(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	var f ui.LedgerFilters
	lq, valid := parseLedgerQuery(q, &f, s.yearOfCompany(ctx, m))
	account, err := uuid.Parse(q.Get(ui.FieldAccount))
	ap := ui.AccountEntriesPage{Back: ui.ReportFor(ui.TrialBalancePath, without(q, ui.FieldAccount)), Year: f.Year, Period: f.Period}
	if !valid || err != nil || f.YearError != "" || f.PeriodError != "" {
		ap.Invalid = true
		s.render(w, r, http.StatusBadRequest, ui.AccountEntries(s.page(i18n.AccountEntriesTitle, ui.ReportsPath, p, m), ap))
		return
	}
	if lq.ledger != uuid.Nil {
		ap.Ledger = ledgerCode(s.ledgers(ctx, m), lq.ledger)
	}
	ap.Account = accountText(s.accounts(ctx, m, lq.company), account)
	params := gen.ListJournalEntriesParams{LegalEntityID: gen.NewOptUUID(lq.company), LedgerID: lq.ledgerPtr(), FiscalYear: gen.NewOptInt32(lq.year),
		FiscalPeriod: lq.period, AccountID: gen.NewOptUUID(account), PageSize: gen.NewOptInt32(reportPageSize)}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	status := http.StatusOK
	res, err := s.API.ListJournalEntries(ctx, params)
	if page, isPage := res.(*gen.JournalEntryPage); isPage {
		ap.Rows = accountEntryRows(page.Items, account)
		if next := page.NextPageToken.Or(""); next != "" {
			ap.More = ui.ReportFor(ui.AccountEntriesPath, with(q, "page", next))
		}
	} else {
		prob, show := s.refusal(w, r, p, "listJournalEntries", res, err)
		if !show {
			return
		}
		ap.Refusal, status = explain(i18n.ReportRefusedTitle, prob, m), int(prob.Status)
	}
	s.render(w, r, status, ui.AccountEntries(s.page(i18n.AccountEntriesTitle, ui.ReportsPath, p, m), ap))
}

// accountEntryRows are the entries' lines on the account, each with its
// text (the line's, else its entry's).
func accountEntryRows(entries []gen.JournalEntry, account uuid.UUID) []ui.AccountEntryRow {
	var out []ui.AccountEntryRow
	for _, e := range entries {
		for _, l := range e.Lines {
			if l.AccountID != account {
				continue
			}
			out = append(out, ui.AccountEntryRow{Entry: entryPath(e.ID), Number: e.DocumentNumber, Line: l.LineNo, Posting: e.PostingDate,
				Period: e.FiscalPeriod, Text: l.LineText.Or(e.HeaderText.Or("")), Amount: moneyOf(l.Amount), AmountLC: moneyOf(l.AmountLc)})
		}
	}
	return out
}
