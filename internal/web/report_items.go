package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The items' reports (ADR-0052 §2): the supplier open items of a company
// with their due dates and their aging on a key date (listOpenItems,
// G4), which with getCurrentPrincipal, listLegalEntities, the company's
// chart (listAccounts) and the supplier named (listBusinessPartners by
// number) makes five calls; and a company's GR/IR positions
// (listGrIrBalances), three.

// The aging buckets the catalog names.
var agingBuckets = map[string]i18n.Key{"not_due": i18n.AgingNotDue, "overdue": i18n.AgingOverdue, "no_due_date": i18n.AgingNoDue}

// itemQuery is what the open items are of: the company, the
// reconciliation account (nil: every one), the supplier, the last posting
// and due dates and the key date of the aging.
type itemQuery struct {
	company, account, supplier uuid.UUID
	postingTo, dueTo           gen.OptDate
	key                        time.Time
}

// values are the query as the report's address names it, the supplier
// by its number.
func (iq itemQuery) values(f ui.ItemFilters) url.Values {
	q := url.Values{ui.FieldCompany: {iq.company.String()}, ui.FieldKeyDate: {f.KeyDate}}
	for k, v := range map[string]string{ui.FieldSupplier: f.Supplier, ui.FieldPostingDateTo: f.PostingTo, ui.FieldDueDateTo: f.DueTo} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if iq.account != uuid.Nil {
		q.Set(ui.FieldAccount, iq.account.String())
	}
	return q
}

// params are the API's parameters of a page of the items, aged when age.
func (iq itemQuery) params(size int32, token string, age bool) gen.ListOpenItemsParams {
	params := gen.ListOpenItemsParams{LegalEntityID: gen.NewOptUUID(iq.company), PostingDateTo: iq.postingTo, DueDateTo: iq.dueTo,
		PageSize: gen.NewOptInt32(size), OrderBy: gen.NewOptString("posting_date")}
	if age {
		params.AgingKeyDate = gen.NewOptDate(iq.key)
	}
	for _, v := range []struct {
		id  uuid.UUID
		set *gen.OptUUID
	}{{iq.account, &params.AccountID}, {iq.supplier, &params.PartnerID}} {
		if v.id != uuid.Nil {
			*v.set = gen.NewOptUUID(v.id)
		}
	}
	if token != "" {
		params.PageToken = gen.NewOptString(token)
	}
	return params
}

// openItems is the supplier open items: the company's (the first until
// chosen) on its first supplier reconciliation account by number until
// another is chosen, of the supplier the number names, their aging on
// the key date (today until chosen) and a page of them.
func (s *Server) openItems(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	op := ui.OpenItemsPage{}
	iq, valid := s.itemQuery(ctx, m, q, &op)
	status := http.StatusOK
	switch {
	case !valid:
		op.Invalid, status = true, http.StatusBadRequest
	case filterErrors(op.Filters):
		status = http.StatusUnprocessableEntity
	default:
		res, err := s.API.ListOpenItems(ctx, iq.params(reportPageSize, q.Get("page"), true))
		page, isPage := res.(*gen.OpenItemPageHeaders)
		if !isPage {
			prob, show := s.refusal(w, r, p, "listOpenItems", res, err)
			if !show {
				return
			}
			op.Refusal, status = explain(i18n.ReportRefusedTitle, prob, m), int(prob.Status)
			break
		}
		op.Aging = agingRows(page.Response.Aging)
		op.Rows = itemRows(page.Response.Items, iq, op.Supplier)
		if next := page.Response.NextPageToken.Or(""); next != "" {
			op.More = ui.ReportFor(ui.OpenItemsPath, with(q, "page", next))
		}
		op.CSV = ui.ReportFor(ui.OpenItemsPath+ui.CSVSuffix, iq.values(op.Filters))
	}
	s.render(w, r, status, ui.OpenItems(s.page(i18n.OpenItemsTitle, ui.ReportsPath, p, m), op))
}

// filterErrors reports whether a filter of the open items is wrong.
func filterErrors(f ui.ItemFilters) bool {
	return f.SupplierError != "" || f.PostingError != "" || f.DueError != "" || f.KeyDateError != ""
}

// itemQuery reads the open items' query into the page's filters: the
// companies and the company's supplier reconciliation accounts to choose
// from (an account the company does not hold, the company's first), and
// the supplier looked up by its number. valid is false when the company
// or an account named while the chart is unread are no ids; a filter that
// is wrong is its field's error.
func (s *Server) itemQuery(ctx context.Context, m me, q url.Values, op *ui.OpenItemsPage) (itemQuery, bool) {
	f := ui.ItemFilters{Companies: companyOptions(s.companies(ctx, m), q.Get(ui.FieldCompany)), Supplier: strings.TrimSpace(q.Get(ui.FieldSupplier)),
		Suppliers: m.may(permReadSuppliers), PostingTo: q.Get(ui.FieldPostingDateTo), DueTo: q.Get(ui.FieldDueDateTo), KeyDate: q.Get(ui.FieldKeyDate)}
	var iq itemQuery
	id, err := uuid.Parse(askedOrFirst(q.Get(ui.FieldCompany), f.Companies))
	valid := err == nil
	iq.company = id
	asked := q.Get(ui.FieldAccount)
	if valid {
		f.Accounts = s.supplierAccounts(ctx, m, iq.company, asked)
	}
	if f.Accounts != nil && !slices.ContainsFunc(f.Accounts, func(o ui.Option) bool { return o.Value == asked }) {
		asked = "" // another company's, kept by the form when the company changed: the company's first
	}
	if account := askedOrFirst(asked, f.Accounts); account != "" {
		iq.account, err = uuid.Parse(account)
		valid = valid && err == nil
	}
	if f.KeyDate == "" {
		f.KeyDate = s.today()
	}
	var key gen.OptDate
	key, f.KeyDateError = dateParam(f.KeyDate)
	iq.key = key.Value
	iq.postingTo, f.PostingError = dateParam(f.PostingTo)
	iq.dueTo, f.DueError = dateParam(f.DueTo)
	if f.Supplier != "" && f.Suppliers {
		iq.supplier, op.Supplier, f.SupplierError = s.supplierOf(ctx, f.Supplier)
	}
	op.Filters = f
	return iq, valid
}

// supplierAccounts are the company's supplier reconciliation accounts by
// number to choose from, the chosen one marked; nil when the chart cannot
// be read or holds none, so the items of every reconciliation account are
// listed.
func (s *Server) supplierAccounts(ctx context.Context, m me, company uuid.UUID, chosen string) []ui.Option {
	var out []ui.Option
	for _, a := range s.chartOf(ctx, m, company).byID {
		if a.ReconciliationKind == "supplier" {
			out = append(out, ui.Option{Value: a.ID.String(), Text: a.Number + " " + a.Name, Selected: a.ID.String() == chosen})
		}
	}
	if len(out) == 0 {
		return nil
	}
	sortOptions(out)
	return out
}

// supplierOf is the supplier a number names (listBusinessPartners), with
// its number and name, or why there is none: not known, the lookup
// refused (a permission the person lacks) or failed (logged).
func (s *Server) supplierOf(ctx context.Context, number string) (uuid.UUID, string, i18n.Key) {
	res, err := s.API.ListBusinessPartners(ctx, gen.ListBusinessPartnersParams{Number: gen.NewOptString(number)})
	page, ok := res.(*gen.BusinessPartnerPageHeaders)
	switch {
	case ok && len(page.Response.Items) > 0:
		b := page.Response.Items[0]
		return b.ID, b.Number + " " + b.Name, ""
	case ok:
		return uuid.Nil, "", i18n.OpenItemsSupplierUnknown
	}
	if prob, isProblem := problemOf(res); isProblem && prob.Status == http.StatusForbidden {
		return uuid.Nil, "", i18n.EntryLookupRefused
	}
	s.Log.WarnContext(ctx, "web: a report's supplier could not be looked up", "answer", res, "error", err)
	return uuid.Nil, "", i18n.OpenItemsSupplierUnread
}

// agingRows are the items' aging, every page's.
func agingRows(buckets []gen.OpenItemAging) []ui.AgingRow {
	out := make([]ui.AgingRow, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, ui.AgingRow{Bucket: labelOf(agingBuckets, b.Bucket), From: b.DaysFrom.Or(0), To: b.DaysTo.Or(0), Currency: b.Currency,
			Items: b.ItemCount, Amount: moneyOf(b.AmountLc)})
	}
	return out
}

// itemRows are a page of open items on the key date: each item's net due
// date and the last day of its earliest discount still open, and the days
// it is overdue; its partner by id, but the supplier the filter named.
func itemRows(items []gen.OpenItem, iq itemQuery, supplier string) []ui.OpenItemRow {
	out := make([]ui.OpenItemRow, 0, len(items))
	for _, o := range items {
		row := ui.OpenItemRow{Entry: entryPath(o.JournalEntryID), Number: o.DocumentNumber, Partner: idText(o.PartnerID), Reference: o.Reference.Or(""),
			Posting: o.PostingDate, DownPay: o.SpecialGl.Or("") == "A", Amount: moneyOf(o.Amount), AmountLC: moneyOf(o.AmountLc)}
		if id, ok := o.PartnerID.Get(); ok && id == iq.supplier {
			row.Partner = supplier
		}
		if t, ok := o.PaymentTerms.Get(); ok {
			row.Due, row.Discount, row.Overdue = &t.NetDueDate, openDiscount(t.Discounts, iq.key), daysPast(iq.key, t.NetDueDate)
		}
		out = append(out, row)
	}
	return out
}

// openDiscount is the last day of the earliest discount still open on the
// key date, nil for none.
func openDiscount(tiers []gen.CashDiscountTier, key time.Time) *time.Time {
	for _, d := range tiers {
		if !d.Until.Before(key) {
			return &d.Until
		}
	}
	return nil
}

// daysPast is how many days after the due date the key date is, 0 when it
// is not after it; both are dates, so the days are whole.
func daysPast(key, due time.Time) int {
	return max(0, int(key.Sub(due)/(24*time.Hour)))
}

// grirReport is a company's GR/IR positions: those still open, or every
// one when asked, latest posted last.
func (s *Server) grirReport(w http.ResponseWriter, r *http.Request, p *person) {
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	gp := ui.GRIRPage{Companies: companyOptions(s.companies(ctx, m), q.Get(ui.FieldCompany)), All: q.Get(ui.FieldAllGRIR) != ""}
	company, err := uuid.Parse(askedOrFirst(q.Get(ui.FieldCompany), gp.Companies))
	status := http.StatusOK
	if err != nil {
		gp.Invalid, status = true, http.StatusBadRequest
	} else {
		res, err := s.API.ListGrIrBalances(ctx, grirParams(company, gp.All, reportPageSize, q.Get("page")))
		if page, isPage := res.(*gen.GrIrBalancePageHeaders); isPage {
			gp.Positions = grirCheck(page.Response).Positions
			if next := page.Response.NextPageToken.Or(""); next != "" {
				gp.More = ui.ReportFor(ui.GRIRPath, with(q, "page", next))
			}
			gp.CSV = ui.ReportFor(ui.GRIRPath+ui.CSVSuffix, grirValues(company, gp.All))
		} else {
			prob, show := s.refusal(w, r, p, "listGrIrBalances", res, err)
			if !show {
				return
			}
			gp.Refusal, status = explain(i18n.ReportRefusedTitle, prob, m), int(prob.Status)
		}
	}
	s.render(w, r, status, ui.GRIRReport(s.page(i18n.GRIRReportTitle, ui.ReportsPath, p, m), gp))
}

// grirParams are the API's parameters of a page of GR/IR positions.
func grirParams(company uuid.UUID, all bool, size int32, token string) gen.ListGrIrBalancesParams {
	params := gen.ListGrIrBalancesParams{LegalEntityID: company, OpenOnly: gen.NewOptBool(!all), PageSize: gen.NewOptInt32(size),
		OrderBy: gen.NewOptString("last_posting_date")}
	if token != "" {
		params.PageToken = gen.NewOptString(token)
	}
	return params
}

// grirValues are the GR/IR report's query.
func grirValues(company uuid.UUID, all bool) url.Values {
	q := url.Values{ui.FieldCompany: {company.String()}}
	if all {
		q.Set(ui.FieldAllGRIR, "1")
	}
	return q
}
