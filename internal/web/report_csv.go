package web

import (
	"context"
	"encoding/csv"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A report's CSV download (ADR-0052 §8, G5): the API's pages the filters
// select, read with the person's own token, up to maxExportRows rows, the
// filters in the first line and the amounts as the API's decimal strings.
// The rows are read in full before the first byte is written, so that a
// report over the limit is said on a page of its own rather than cut
// short in a file that looks whole; longer exports are T63's.

// The export's limits: maxExportRows rows, read in pages of the API's
// largest size; at most maxExportPages pages (more when the API returns
// short pages), which with a report's other calls stays within the API's
// burst of a principal's calls (04 §4.11, 100 at 50 a second).
const (
	maxExportRows  = 10000
	exportPageSize = 500
	maxExportPages = 25
)

// failure is an API call that answered no page.
type failure struct {
	operation string
	res       any
	err       error
}

// export is one report's download: its file's name, its title and
// filters (the first line), its columns, the report's page to go back to,
// and how a page of its rows is read.
type export struct {
	file    string
	title   i18n.Key
	filters url.Values
	columns []i18n.Key
	back    string
	read    func(ctx context.Context, token string) (rows [][]string, next string, failed *failure)
}

// download reads every row of the export and writes them as CSV; a
// refusal is said on a page of its own, as are filters over the limit.
func (s *Server) download(w http.ResponseWriter, r *http.Request, p *person, m me, x export) {
	var rows [][]string
	token := ""
	for pages := 1; ; pages++ {
		page, next, failed := x.read(r.Context(), token)
		if failed != nil {
			if prob, show := s.refusal(w, r, p, failed.operation, failed.res, failed.err); show {
				s.render(w, r, int(prob.Status), ui.RefusalPage(s.page(x.title, ui.ReportsPath, p, m), i18n.ReportRefusedTitle, x.back,
					i18n.ReportBackToReport, *explain(i18n.ReportRefusedTitle, prob, m)))
			}
			return
		}
		rows = append(rows, page...)
		if len(rows) > maxExportRows || next != "" && (len(rows) == maxExportRows || pages >= maxExportPages) {
			s.render(w, r, http.StatusUnprocessableEntity, ui.Problem(s.page(x.title, ui.ReportsPath, p, m), i18n.ReportTooManyTitle,
				i18n.ReportTooManyBody, x.back, i18n.ReportBackToReport))
			return
		}
		if next == "" {
			break
		}
		token = next
	}
	s.writeCSV(w, r, x, rows)
}

// writeCSV writes the export's rows: UTF-8 with a byte order mark, which
// spreadsheet programs need to read the accents of a French company's
// names, its title and filters first, then its columns in the person's
// language. Each cell is safe for a spreadsheet (safeCell).
func (s *Server) writeCSV(w http.ResponseWriter, r *http.Request, x export, rows [][]string) {
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+x.file+`.csv"`)
	h.Set("Cache-Control", "no-store")
	ctx := r.Context()
	first := []string{i18n.T(ctx, x.title)}
	for _, k := range slices.Sorted(maps.Keys(x.filters)) {
		first = append(first, k+"="+x.filters.Get(k))
	}
	columns := make([]string, len(x.columns))
	for i, c := range x.columns {
		columns[i] = i18n.T(ctx, c)
	}
	cw := csv.NewWriter(w)
	_, _ = w.Write([]byte(byteOrderMark)) // a write's error is the client's, which cw.Error reports for the rows below
	for _, row := range append([][]string{first, columns}, rows...) {
		for i, cell := range row {
			row[i] = safeCell(cell)
		}
		_ = cw.Write(row) // a csv.Writer keeps the first error, read below
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.Log.WarnContext(ctx, "web: a report's download was cut short", "report", x.file, "error", err)
	}
}

// byteOrderMark is UTF-8's, the file's first bytes.
const byteOrderMark = "\xef\xbb\xbf"

// numeric is a cell a spreadsheet reads as a number or a date, as it is:
// an amount as the API writes it, or an ISO date.
var numeric = regexp.MustCompile(`^(-?[0-9]+(\.[0-9]+)?|[0-9]{4}-[0-9]{2}-[0-9]{2})$`)

// safeCell is a cell a spreadsheet does not run: a text a person or an
// agent typed (an account's name, a reference) that begins as a formula
// does is kept as text by a leading apostrophe (OWASP, CSV injection);
// numbers and dates are as they are.
func safeCell(v string) string {
	if v == "" || numeric.MatchString(v) {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + v
	}
	return v
}

// trialBalanceCSV is the trial balance's download.
func (s *Server) trialBalanceCSV(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	var f ui.LedgerFilters
	lq, valid := parseLedgerQuery(r.URL.Query(), &f, s.yearOfCompany(r.Context(), m))
	back := ui.ReportFor(ui.TrialBalancePath, r.URL.Query())
	if !valid || f.YearError != "" || f.PeriodError != "" {
		s.downloadInvalid(w, r, p, m, i18n.TrialBalanceTitle, back)
		return
	}
	accounts := s.accounts(r.Context(), m, lq.company)
	s.download(w, r, p, m, export{file: "trial-balance-" + strconv.Itoa(int(lq.year)), title: i18n.TrialBalanceTitle, filters: lq.values(), back: back,
		columns: []i18n.Key{i18n.BalanceAccount, i18n.BalancePeriod, i18n.BalanceCurrency, i18n.BalanceDebit, i18n.BalanceCredit,
			i18n.BalanceDebitLC, i18n.BalanceCreditLC},
		read: func(ctx context.Context, token string) ([][]string, string, *failure) {
			res, err := s.API.ListBalances(ctx, balanceParams(lq, exportPageSize, token))
			page, ok := res.(*gen.BalancePage)
			if !ok {
				return nil, "", &failure{"listBalances", res, err}
			}
			var rows [][]string
			for _, b := range page.Items {
				rows = append(rows, []string{accountText(accounts, b.AccountID), strconv.Itoa(int(b.FiscalPeriod)), b.Currency, b.Debit.Amount,
					b.Credit.Amount, b.DebitLc.Amount, b.CreditLc.Amount})
			}
			return rows, page.NextPageToken.Or(""), nil
		}})
}

// downloadInvalid answers a download whose filters are not a report's.
func (s *Server) downloadInvalid(w http.ResponseWriter, r *http.Request, p *person, m me, title i18n.Key, back string) {
	s.render(w, r, http.StatusBadRequest, ui.Problem(s.page(title, ui.ReportsPath, p, m), i18n.ReportRefusedTitle, i18n.FilterInvalid, back,
		i18n.ReportBackToReport))
}

// openItemsCSV is the open items' download, each item on the key date.
func (s *Server) openItemsCSV(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	var op ui.OpenItemsPage
	q := r.URL.Query()
	iq, valid := s.itemQuery(r.Context(), m, q, &op)
	back := ui.ReportFor(ui.OpenItemsPath, q)
	if !valid || filterErrors(op.Filters) {
		s.downloadInvalid(w, r, p, m, i18n.OpenItemsTitle, back)
		return
	}
	s.download(w, r, p, m, export{file: "open-items-" + op.Filters.KeyDate, title: i18n.OpenItemsTitle, filters: iq.values(op.Filters), back: back,
		columns: []i18n.Key{i18n.OpenItemDocument, i18n.EntryLinePartner, i18n.EntryReference, i18n.EntryPostingDate, i18n.OpenItemDue,
			i18n.OpenItemDiscount, i18n.OpenItemOverdue, i18n.EntryLineAmount, i18n.BalanceCurrency, i18n.EntryLineAmountLC, i18n.BalanceCurrency},
		read: func(ctx context.Context, token string) ([][]string, string, *failure) {
			res, err := s.API.ListOpenItems(ctx, iq.params(exportPageSize, token, false))
			page, ok := res.(*gen.OpenItemPage)
			if !ok {
				return nil, "", &failure{"listOpenItems", res, err}
			}
			var rows [][]string
			for _, it := range itemRows(page.Items, iq, op.Supplier) {
				rows = append(rows, []string{it.Number, it.Partner, it.Reference, isoDay(it.Posting), dayOrNone(it.Due), dayOrNone(it.Discount),
					strconv.Itoa(it.Overdue), it.Amount.Amount, it.Amount.Currency, it.AmountLC.Amount, it.AmountLC.Currency})
			}
			return rows, page.NextPageToken.Or(""), nil
		}})
}

// isoDay is a date as the API writes it.
func isoDay(t time.Time) string { return t.Format(time.DateOnly) }

// dayOrNone is a date as the API writes it, "" for none.
func dayOrNone(t *time.Time) string {
	if t == nil {
		return ""
	}
	return isoDay(*t)
}

// grirCSV is the GR/IR positions' download.
func (s *Server) grirCSV(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	back := ui.ReportFor(ui.GRIRPath, q)
	company, err := uuid.Parse(q.Get(ui.FieldCompany))
	if err != nil {
		s.downloadInvalid(w, r, p, m, i18n.GRIRReportTitle, back)
		return
	}
	all := q.Get(ui.FieldAllGRIR) != ""
	s.download(w, r, p, m, export{file: "gr-ir", title: i18n.GRIRReportTitle, filters: grirValues(company, all), back: back,
		columns: []i18n.Key{i18n.GRIROrder, i18n.GRIRItem, i18n.GRIRReceived, i18n.GRIRInvoiced, i18n.BalanceCurrency, i18n.GRIRBalance,
			i18n.GRIRLast},
		read: func(ctx context.Context, token string) ([][]string, string, *failure) {
			res, err := s.API.ListGrIrBalances(ctx, grirParams(company, all, exportPageSize, token))
			page, ok := res.(*gen.GrIrBalancePage)
			if !ok {
				return nil, "", &failure{"listGrIrBalances", res, err}
			}
			var rows [][]string
			for _, b := range page.Items {
				rows = append(rows, []string{b.PurchaseOrderID.String(), strconv.Itoa(int(b.PoItemNo)), b.ReceivedQuantity.Value + " " + b.ReceivedQuantity.Unit,
					b.InvoicedQuantity.Value + " " + b.InvoicedQuantity.Unit, b.Balance.Currency, b.Balance.Amount, isoDay(b.LastPostingDate)})
			}
			return rows, page.NextPageToken.Or(""), nil
		}})
}
