package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Manual journal entries (ADR-0052 §2, T35, T73h): the parked entries,
// people's and agents' proposals, by status (this file); the entry form,
// a new entry's and a parked one's change, with lines added and left out
// with and without htmx (entry_form.go), whose codes become the API's ids
// (entry_lookup.go); a parked entry's page with its post, which asks for
// another person's approval above the company's limit, its cancellation,
// the withdrawal of its approval request and the upload of its lines from
// a CSV file (parked.go, entry_csv.go); and the journal of posted entries
// with an entry's page and its reversal (journal_entries.go). The
// approval itself is decided in the approvals inbox (T73d), where the
// requester is refused by the code the API answers.

// A list page shows parkedPageSize entries; with getCurrentPrincipal,
// listLegalEntities and listPrincipalNames it makes four calls (ADR-0052
// §10: six).
const parkedPageSize = 25

// The parked entries' statuses and principals' kinds the catalog names,
// the list's tabs (every status first) and its kinds (everyone's, then
// the agents' proposals).
var (
	parkedStatuses = map[string]i18n.Key{"parked": i18n.ParkedStatusParked, "pending_approval": i18n.ParkedStatusPending,
		"posted": i18n.ParkedStatusPosted, "rejected": i18n.ParkedStatusRejected, "cancelled": i18n.ParkedStatusCancelled}
	parkedTabs  = []string{"", "parked", "pending_approval", "posted", "rejected", "cancelled"}
	parkedKinds = map[string]i18n.Key{"": i18n.ParkedKindAll, "agent": i18n.ParkedKindAgent}
	// parkedDone are the notices of what was just done to a parked entry.
	parkedDone = map[string]i18n.Key{"parked": i18n.ParkedDoneParked, "changed": i18n.ParkedDoneChanged, "uploaded": i18n.ParkedDoneUploaded,
		"posted": i18n.ParkedDonePosted, "submitted": i18n.ParkedDoneSubmitted, "cancelled": i18n.ParkedDoneCancelled,
		"withdrawn": i18n.ParkedDoneWithdrawn, "limit": i18n.ParkedDoneLimit}
)

// parkedPath is a parked entry's page, entryPath a posted one's.
func parkedPath(id uuid.UUID) string { return ui.ParkedBase + "/" + id.String() }

func entryPath(id uuid.UUID) string { return ui.EntriesPath + "/" + id.String() }

// parkedEntries is the parked entries' list, by the status and kind the
// query names.
func (s *Server) parkedEntries(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	l := ui.ParkedList{Tabs: parkedStatusTabs(q), Kinds: parkedKindTabs(q)}
	if m.may(permParkEntries) {
		l.New = ui.NewEntryPath
	}
	if m.may(permReadEntries) {
		l.Entries = ui.EntriesPath
	}
	params, valid := parkedParams(q)
	status := http.StatusOK
	if !valid {
		l.Invalid, status = true, http.StatusBadRequest
	} else {
		res, err := s.API.ListParkedJournalEntries(r.Context(), params)
		page, isPage := res.(*gen.ParkedJournalEntryPage)
		if !isPage {
			prob, show := s.refusal(w, r, p, "listParkedJournalEntries", res, err)
			if !show {
				return
			}
			l.Refusal, status = explainIn(i18n.EntryProblems, i18n.ParkedRefusedTitle, prob, m), int(prob.Status)
		} else {
			l.Rows = s.parkedRows(r.Context(), p, m, page.Items)
			if next := page.NextPageToken.Or(""); next != "" {
				l.More = ui.JournalFor(with(q, "page", next))
			}
		}
	}
	s.render(w, r, status, ui.Parked(s.page(i18n.JournalTitle, ui.JournalPath, p, m), l))
}

// parkedParams are the API's filters of the list, and whether the
// company is an id.
func parkedParams(q url.Values) (gen.ListParkedJournalEntriesParams, bool) {
	params := gen.ListParkedJournalEntriesParams{PageSize: gen.NewOptInt32(parkedPageSize)}
	valid := true
	if v := q.Get(ui.FieldCompany); v != "" {
		id, err := uuid.Parse(v)
		valid = err == nil
		params.LegalEntityID = gen.NewOptUUID(id)
	}
	if v := q.Get("status"); v != "" {
		params.Status = gen.NewOptString(v)
	}
	if v := q.Get(ui.FieldKind); v != "" {
		params.CreatedByKind = gen.NewOptString(v)
	}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	return params, valid
}

// parkedStatusTabs are the list's statuses, each keeping the other
// filters; parkedKindTabs its kinds.
func parkedStatusTabs(q url.Values) []ui.StatusTab {
	tabs := make([]ui.StatusTab, 0, len(parkedTabs))
	for _, st := range parkedTabs {
		k := parkedStatuses[st]
		if st == "" {
			k = i18n.InvoicesStatusAll
		}
		tabs = append(tabs, ui.StatusTab{Label: k, Path: ui.JournalFor(with(q, "status", st)), Current: q.Get("status") == st})
	}
	return tabs
}

func parkedKindTabs(q url.Values) []ui.StatusTab {
	tabs := make([]ui.StatusTab, 0, len(parkedKinds))
	for _, kind := range []string{"", "agent"} {
		tabs = append(tabs, ui.StatusTab{Label: parkedKinds[kind], Path: ui.JournalFor(with(q, ui.FieldKind, kind)), Current: q.Get(ui.FieldKind) == kind})
	}
	return tabs
}

// parkedRows are a page of parked entries, each with who parked it.
func (s *Server) parkedRows(ctx context.Context, p *person, m me, items []gen.ParkedJournalEntry) []ui.ParkedRow {
	ids := make([]uuid.UUID, 0, len(items))
	for _, pe := range items {
		ids = append(ids, pe.CreatedBy)
	}
	names, companies := s.names(ctx, p, ids), s.companies(ctx, m)
	out := make([]ui.ParkedRow, 0, len(items))
	for _, pe := range items {
		amounts := make([]gen.Money, 0, len(pe.Lines))
		for _, l := range pe.Lines {
			amounts = append(amounts, l.Amount)
		}
		out = append(out, ui.ParkedRow{Path: parkedPath(pe.ID), Company: companies[pe.LegalEntityID], Status: labelOf(parkedStatuses, pe.Status),
			DocType: pe.DocumentType, Posting: pe.PostingDate, Header: pe.HeaderText.Or(""), Debits: debits(amounts), Lines: len(pe.Lines),
			CreatedBy: names[pe.CreatedBy], Created: pe.CreatedAt, Unowned: !pe.OwnerID.Set && pe.Status == "parked"})
	}
	return out
}

// debits are the sum of the positive amounts, in their one currency: nil
// when there are none, or when they cannot be summed (currencies or
// digits that differ, or no decimal), which the API never gives.
func debits(amounts []gen.Money) *ui.Money {
	if len(amounts) == 0 {
		return nil
	}
	currency := amounts[0].Currency
	_, frac, _ := strings.Cut(amounts[0].Amount, ".")
	var sum money.Minor
	for _, a := range amounts {
		_, f, _ := strings.Cut(a.Amount, ".")
		v, err := money.ParseDecimal(a.Amount, len(frac))
		if err != nil || a.Currency != currency || len(f) != len(frac) {
			return nil
		}
		if v > 0 {
			if sum, err = money.Sum(sum, v); err != nil {
				return nil
			}
		}
	}
	return &ui.Money{Amount: sum.Plain(len(frac)), Currency: currency}
}
