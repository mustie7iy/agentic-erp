package web

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The downloads of the ledger reconciliation and of a document's audit
// history (ADR-0052 G5, T73j4), read and written as T73j's are
// (report_csv.go): every page the filters select, up to 10,000 rows,
// within a download's calls.

// ledgerReconCSV is the ledger reconciliation's download: the accounts
// that differ between the two ledgers, per transaction currency, with
// the difference in the company's and the group's currency. The first
// line names both ledgers by code beside their ids (ledger and
// compare_ledger, the API's own when the address names no ledger), so
// the file says which balance is which. Each ledger's trial balance
// check, which the API gives on the first page alone, is the report's
// page's.
func (s *Server) ledgerReconCSV(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f ui.LedgerFilters
	lq, valid := parseLedgerQuery(q, &f, s.yearOfCompany(r.Context(), m))
	back := ui.ReportFor(ui.LedgerReconPath, q)
	if !valid || lq.compare == uuid.Nil || lq.compare == lq.ledger || f.YearError != "" || f.PeriodError != "" {
		s.downloadInvalid(w, r, p, m, i18n.LedgerReconTitle, back)
		return
	}
	ledgers, filters := s.ledgers(r.Context(), m), lq.values()
	s.download(w, r, p, m, export{file: "ledger-reconciliation-" + strconv.Itoa(int(lq.year)), title: i18n.LedgerReconTitle, filters: filters,
		back: back, columns: []i18n.Key{i18n.DifferenceAccount, i18n.DifferenceCurrency, i18n.DifferenceBalance, i18n.DifferenceCompare,
			i18n.DifferenceAmount, i18n.DifferenceLC, i18n.BalanceCurrency, i18n.DifferenceGC, i18n.BalanceCurrency},
		read: func(ctx context.Context, token string) ([][]string, string, *failure) {
			params := gen.ListLedgerReconciliationParams{LegalEntityID: lq.company, LedgerID: lq.ledgerPtr(), CompareLedgerID: lq.compare,
				FiscalYear: lq.year, FiscalPeriod: lq.period, PageSize: gen.NewOptInt32(exportPageSize)}
			if token != "" {
				params.PageToken = gen.NewOptString(token)
			}
			res, err := s.API.ListLedgerReconciliation(ctx, params)
			page, ok := res.(*gen.LedgerReconciliationPage)
			if !ok {
				return nil, "", &failure{"listLedgerReconciliation", res, err}
			}
			filters.Set(fieldLedgerCode, ledgerCode(ledgers, page.LedgerID))
			filters.Set(fieldCompareCode, ledgerCode(ledgers, page.CompareLedgerID))
			var rows [][]string
			for _, d := range page.Items {
				rows = append(rows, []string{d.AccountNumber + " " + d.AccountName, d.Currency, d.Balance.Amount, d.CompareBalance.Amount,
					d.Difference.Amount, d.DifferenceLc.Amount, d.DifferenceLc.Currency, d.DifferenceGc.Amount, d.DifferenceGc.Currency})
			}
			return rows, page.NextPageToken.Or(""), nil
		}})
}

// The first line's names of the reconciled ledgers' codes.
const (
	fieldLedgerCode  = "ledger"
	fieldCompareCode = "compare_ledger"
)

// auditCSV is a document's audit history's download: its events newest
// first, as the history shows them, the actor by name and by id.
func (s *Server) auditCSV(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	back := ui.ReportFor(ui.AuditPath, q)
	id, err := uuid.Parse(q.Get(ui.FieldObject))
	if err != nil {
		s.downloadInvalid(w, r, p, m, i18n.AuditTitle, back)
		return
	}
	var actors []uuid.UUID // each row's actor, in the rows' order
	s.download(w, r, p, m, export{file: "audit-" + id.String(), title: i18n.AuditTitle, filters: url.Values{ui.FieldObject: {id.String()}}, back: back,
		columns: []i18n.Key{i18n.AuditSeq, i18n.AuditAt, i18n.AuditActor, i18n.AuditActorID, i18n.AuditActorKind, i18n.AuditAction,
			i18n.AuditOutcome, i18n.AuditAddress, i18n.AuditDetails},
		read: func(ctx context.Context, token string) ([][]string, string, *failure) {
			params := gen.ListAuditEventsParams{ObjectID: gen.NewOptUUID(id), PageSize: gen.NewOptInt32(exportPageSize)}
			if token != "" {
				params.PageToken = gen.NewOptString(token)
			}
			res, err := s.API.ListAuditEvents(ctx, params)
			page, ok := res.(*gen.AuditEventPage)
			if !ok {
				return nil, "", &failure{"listAuditEvents", res, err}
			}
			var rows [][]string
			for _, e := range page.Items {
				actors = append(actors, e.ActorID)
				rows = append(rows, []string{strconv.FormatInt(e.Seq, 10), e.OccurredAt.UTC().Format(time.RFC3339), "", e.ActorID.String(),
					kindText(ctx, e.ActorKind), e.Action, e.Outcome, e.ClientAddress.Or(""), detailJSON(e.Detail)})
			}
			return rows, page.NextPageToken.Or(""), nil
		},
		finish: func(ctx context.Context, rows [][]string) {
			names := s.names(ctx, p, firstActors(actors))
			for i := range rows {
				rows[i][2] = names[actors[i]].Text
			}
		}})
}

// firstActors are the first namesPerCall actors of a history, each once:
// the download names them in one read, within its calls (a document's
// history has a few; any beyond are given by id alone).
func firstActors(actors []uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, a := range actors {
		if len(out) == namesPerCall {
			break
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// kindText is an actor's kind in the person's language, as the API names
// it when the catalog does not.
func kindText(ctx context.Context, kind string) string {
	if l := labelOf(actorKinds, kind); l.Key != "" {
		return i18n.T(ctx, l.Key)
	}
	return kind
}

// detailJSON is an event's details as one JSON object, its names sorted,
// each value as the API gave it: data, not a text to read.
func detailJSON(d gen.AuditEventDetail) string {
	var e jx.Encoder
	e.ObjStart()
	for _, k := range slices.Sorted(maps.Keys(d)) {
		e.FieldStart(k)
		e.Raw(d[k])
	}
	e.ObjEnd()
	return e.String()
}
