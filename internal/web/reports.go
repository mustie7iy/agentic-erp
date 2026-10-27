package web

import (
	"encoding/json"
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

// The reports (ADR-0052 §2, T73j): the trial balance, the supplier open
// items with their due dates and aging (G4), the GR/IR positions and the
// ledger reconciliation with the entries behind a difference
// (report_ledgers.go, report_items.go), each downloaded as CSV up to
// 10,000 rows (G5, report_csv.go); a house bank's reconciliation report
// is the bank statements' (T73g), linked from the index; a document's
// audit history (this file). Every report reads the figures as the API
// gives them now, with the person's own access: the workspace computes
// no figure of its own. Accounts, ledgers, companies and names that
// cannot be read are no reason to refuse a report: they are logged, and
// shown by id or not offered.

// reportPageSize is how many rows a report's page shows: a page of a
// hundred rows of seven cells stays well under ADR-0052 §10's 100 KB.
const reportPageSize = 100

// auditPageSize is how many events a document's history shows a page.
const auditPageSize = 50

// The actor kinds of an audit event the catalog names.
var actorKinds = map[string]i18n.Key{"human": i18n.AuditKindHuman, "agent": i18n.AuditKindAgent, "service": i18n.AuditKindService,
	"system": i18n.AuditKindSystem}

// mayReport reports whether the person may read any report: the ledger's,
// a bank's reconciliation or the audit trail (advisory, as the API decides
// each call).
func (m me) mayReport() bool {
	return m.may(permReadEntries) || m.may(permReadStatements) || m.may(permReadAudit)
}

// reports is the reports' index: the reports the person may read, and
// each house bank's reconciliation report. With getCurrentPrincipal,
// listHouseBanks and listLegalEntities it makes three calls.
func (s *Server) reports(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	var l ui.ReportLinks
	if m.may(permReadEntries) {
		l.TrialBalance, l.OpenItems, l.GRIR, l.LedgerRecon = ui.TrialBalancePath, ui.OpenItemsPath, ui.GRIRPath, ui.LedgerReconPath
	}
	if m.may(permReadStatements) {
		banks := s.houseBanks(r.Context(), m, uuid.Nil)
		l.Banks, l.BanksUnread = bankViews(banks, s.companies(r.Context(), m)), banks == nil
	}
	l.Audit = m.may(permReadAudit)
	s.render(w, r, http.StatusOK, ui.Reports(s.page(i18n.ReportsTitle, ui.ReportsPath, p, m), l))
}

// auditOf is a document's audit history's page.
func auditOf(id uuid.UUID) string {
	return ui.ReportFor(ui.AuditPath, url.Values{ui.FieldObject: {id.String()}})
}

// auditLink is a document's audit history's page for a person who may
// read the audit trail, else "".
func auditLink(m me, id uuid.UUID) string {
	if !m.may(permReadAudit) {
		return ""
	}
	return auditOf(id)
}

// audit is a document's audit history (listAuditEvents by its id), newest
// first, each event's actor named; without a document, the form that
// names one. With getCurrentPrincipal and listPrincipalNames three calls.
func (s *Server) audit(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	ap := ui.AuditPage{Object: q.Get(ui.FieldObject)}
	status := http.StatusOK
	if ap.Object != "" {
		id, err := uuid.Parse(ap.Object)
		switch {
		case err != nil:
			ap.ObjectError, status = i18n.AuditObjectInvalid, http.StatusUnprocessableEntity
		default:
			if status, ok = s.auditEvents(w, r, p, m, id, &ap); !ok {
				return
			}
		}
	}
	s.render(w, r, status, ui.Audit(s.page(i18n.AuditTitle, ui.ReportsPath, p, m), ap))
}

// auditEvents reads a page of the document's events into the page, and
// the page's status; ok is false when the answer is already written.
func (s *Server) auditEvents(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, ap *ui.AuditPage) (int, bool) {
	q := r.URL.Query()
	params := gen.ListAuditEventsParams{ObjectID: gen.NewOptUUID(id), PageSize: gen.NewOptInt32(auditPageSize)}
	if v := q.Get("page"); v != "" {
		params.PageToken = gen.NewOptString(v)
	}
	res, err := s.API.ListAuditEvents(r.Context(), params)
	page, ok := res.(*gen.AuditEventPage)
	if !ok {
		prob, show := s.refusal(w, r, p, "listAuditEvents", res, err)
		if !show {
			return 0, false
		}
		ap.Refusal = explain(i18n.ReportRefusedTitle, prob, m)
		return int(prob.Status), true
	}
	ids := make([]uuid.UUID, 0, len(page.Items))
	for _, e := range page.Items {
		ids = append(ids, e.ActorID)
	}
	names := s.names(r.Context(), p, ids)
	for _, e := range page.Items {
		ap.Rows = append(ap.Rows, auditRow(e, names))
	}
	if next := page.NextPageToken.Or(""); next != "" {
		ap.More = ui.ReportFor(ui.AuditPath, with(q, "page", next))
	}
	return http.StatusOK, true
}

// auditRow is one event as the history shows it: the API masks the
// client address of all but people (T73c), and the details are data, by
// name.
func auditRow(e gen.AuditEvent, names map[uuid.UUID]ui.Name) ui.AuditRow {
	row := ui.AuditRow{Seq: e.Seq, At: e.OccurredAt, Actor: names[e.ActorID], Kind: labelOf(actorKinds, e.ActorKind), Action: e.Action,
		Outcome: e.Outcome, Address: e.ClientAddress.Or("")}
	for _, k := range slices.Sorted(maps.Keys(e.Detail)) {
		row.Details = append(row.Details, ui.AuditDetail{Name: k, Value: detailText(e.Detail[k])})
	}
	return row
}

// detailText is a detail's value: a string as it is, anything else as
// its JSON.
func detailText(raw []byte) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// yearOf is a fiscal year typed, and its error when it is no year.
func yearOf(typed string) (string, int32, i18n.Key) {
	year, err := strconv.ParseInt(typed, 10, 32)
	if err != nil || year < 1900 || year > 2999 {
		return typed, 0, i18n.PeriodYearInvalid
	}
	return typed, int32(year), ""
}

// periodParam is a posting period typed, none when it is empty.
func periodParam(typed string) (gen.OptInt32, i18n.Key) {
	if typed == "" {
		return gen.OptInt32{}, ""
	}
	n, err := strconv.ParseInt(typed, 10, 32)
	if err != nil || n < 1 || n > 16 {
		return gen.OptInt32{}, i18n.ReportPeriodInvalid
	}
	return gen.NewOptInt32(int32(n)), ""
}
