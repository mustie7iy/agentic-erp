package web

import (
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The clearing of a supplier's down payments against a posted invoice
// (T73e4, ADR-0037, SAP F-54), a screen of its own linked from the
// invoice's page. It lists the supplier's open down payments in the
// invoice's company, a page of listOpenItems with special G/L indicator A
// (the API's own filter, a page at a time, ADR-0052 §8), and sends those
// the person chose, with the transfer's posting date, as
// clearSupplierInvoiceDownPayments, with the invoice's ETag the screen was
// read at and the form's own idempotency key. Each down payment shows its
// paid request (listDownPaymentRequests, by its payment entry): its number
// and what was paid, tax included, the amount the clearing weighs against
// the invoice's gross amount, where the ledger's item holds the net. One
// the API would refuse whatever is chosen with it (no paid request, a
// request for another order or in another currency) is shown without its
// box and says why (advisory, ADR-0052 §7). Every money rule is the API's:
// each down payment cleared whole, their total with those cleared before
// within the invoice's gross amount, the input tax reversed; the screen
// adds and compares no amounts. It reads getCurrentPrincipal,
// getSupplierInvoice, getLegalEntityToday (the date's default),
// listOpenItems and listDownPaymentRequests: five calls.

// maxDownPayments is how many down payments the screen lists at a time:
// as many as one clearing takes (DownPaymentClearingInput.items).
const maxDownPayments = 100

// maxPaidRequests is how many of the supplier's paid requests the screen
// reads to name its down payments: the API's largest page. A down payment
// whose request is beyond it is shown as one without a request, never
// offered: the screen errs towards offering less.
const maxPaidRequests = 500

// specialDownPayment is the special G/L indicator of a down payment
// (OpenItem.special_gl).
const specialDownPayment = "A"

// downPaymentProblems are the clearing's own messages before the invoice
// screens'.
var downPaymentProblems = func() map[string]i18n.Key {
	out := maps.Clone(i18n.InvoiceProblems)
	maps.Copy(out, i18n.DownPaymentProblems)
	return out
}()

// downPaymentsPath is the clearing's screen of an invoice.
func downPaymentsPath(id uuid.UUID) string { return invoicePath(id) + "/" + ui.DownPaymentsVerb }

// clearable reports whether down payments are cleared against the
// invoice: a posted invoice, not a credit memo or a subsequent debit or
// credit (the operation's own requirement).
func clearable(inv gen.SupplierInvoice) bool {
	return inv.Status == "posted" && inv.Kind.Or("invoice") == "invoice"
}

// mayClear reports whether the person may clear down payments and see
// them (advisory, ADR-0052 §7): the operation's permission, and the
// open items' read.
func mayClear(m me) bool { return m.may(permPostInvoices) && m.may(permReadEntries) }

// downPaymentValue is a down payment's value in the form: its fiscal
// year, journal entry and line, the clearing's ClearingItemRef.
func downPaymentValue(it gen.OpenItem) string {
	return strconv.Itoa(int(it.FiscalYear)) + ":" + it.JournalEntryID.String() + ":" + strconv.Itoa(int(it.LineNo))
}

// downPaymentRef reads a down payment's value back; ok is false when it
// is not one the form offers.
func downPaymentRef(v string) (gen.ClearingItemRef, bool) {
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return gen.ClearingItemRef{}, false
	}
	year, yerr := strconv.ParseInt(parts[0], 10, 32)
	entry, eerr := uuid.Parse(parts[1])
	line, lerr := strconv.ParseInt(parts[2], 10, 32)
	if yerr != nil || eerr != nil || lerr != nil {
		return gen.ClearingItemRef{}, false
	}
	return gen.ClearingItemRef{FiscalYear: int32(year), JournalEntryID: entry, LineNo: int32(line)}, true
}

// clearingTyped is what the person sent: the transfer's posting date, the
// down payments chosen, the page of them the form listed, and what is
// wrong with it.
type clearingTyped struct {
	date, page         string
	chosen             []string
	dateErr, chooseErr i18n.Key
}

// downPayments is the clearing's screen; an invoice that down payments
// cannot be cleared against goes to its own page, which says what it is.
func (s *Server) downPayments(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	inv, tag, ok := s.readInvoice(w, r, p, m, id)
	if !ok {
		return
	}
	if !clearable(inv) {
		http.Redirect(w, r, invoicePath(id), http.StatusSeeOther)
		return
	}
	t := clearingTyped{date: s.today(r.Context(), m, inv.LegalEntityID.String()), page: r.URL.Query().Get(ui.FieldPage)}
	s.showDownPayments(w, r, p, m, inv, tag, http.StatusOK, t, nil)
}

// showDownPayments renders the screen with status: the invoice, the page
// of down payments with those chosen checked, the form at tag with a fresh
// idempotency key, and the API's refusal of what was sent. An invoice no
// longer posted lists none.
func (s *Server) showDownPayments(w http.ResponseWriter, r *http.Request, p *person, m me, inv gen.SupplierInvoice, tag string, status int,
	t clearingTyped, refused *ui.Refusal) {
	v := ui.DownPaymentsPage{Back: invoicePath(inv.ID), Number: inv.DocumentNumber, Supplier: supplierOf(inv), Gross: moneyOf(inv.GrossAmount),
		Closed: !clearable(inv), Action: downPaymentsPath(inv.ID), CSRF: p.CSRF, ETag: tag, Key: uuid.NewString(), Page: t.page, Date: t.date,
		DateError: t.dateErr, ChooseError: t.chooseErr, Refusal: refused}
	if !v.Closed {
		listed, ok := s.downPaymentChoices(w, r, p, m, inv, t, &v)
		if !ok {
			return
		}
		status = max(status, listed)
	}
	s.render(w, r, status, ui.DownPayments(s.page(i18n.DownPaymentsTitle, ui.InvoicesPath, p, m), v))
}

// downPaymentChoices reads the page of the supplier's open down payments
// in the invoice's company into v, each with its paid request and those
// chosen checked, the next page's path in More; a refusal is shown in
// their place, its status returned. ok is false when the answer is
// already written.
func (s *Server) downPaymentChoices(w http.ResponseWriter, r *http.Request, p *person, m me, inv gen.SupplierInvoice, t clearingTyped,
	v *ui.DownPaymentsPage) (int, bool) {
	params := gen.ListOpenItemsParams{LegalEntityID: gen.NewOptUUID(inv.LegalEntityID), PartnerID: gen.NewOptUUID(inv.SupplierID),
		SpecialGl: gen.NewOptString(specialDownPayment), PageSize: gen.NewOptInt32(maxDownPayments)}
	if t.page != "" {
		params.PageToken = gen.NewOptString(t.page)
	}
	refused := func(op string, res any, err error) (int, bool) {
		refusal, shown := s.inPlace(w, r, p, m, downPaymentProblems, op, res, err, i18n.DownPaymentsRefusedTitle, "down-payments-refusal")
		if !shown {
			return 0, false
		}
		v.ItemsRefusal = refusal
		return refusalStatus(res), true
	}
	res, err := s.API.ListOpenItems(r.Context(), params)
	page, ok := res.(*gen.OpenItemPage)
	if !ok {
		return refused("listOpenItems", res, err)
	}
	if len(page.Items) == 0 {
		return 0, true
	}
	rres, err := s.API.ListDownPaymentRequests(r.Context(), gen.ListDownPaymentRequestsParams{LegalEntityID: gen.NewOptUUID(inv.LegalEntityID),
		SupplierID: gen.NewOptUUID(inv.SupplierID), Status: gen.NewOptString("paid"), PageSize: gen.NewOptInt32(maxPaidRequests)})
	requests, ok := rres.(*gen.DownPaymentRequestPage)
	if !ok {
		return refused("listDownPaymentRequests", rres, err)
	}
	byEntry := map[uuid.UUID]gen.DownPaymentRequest{}
	for _, rq := range requests.Items {
		if e, ok := rq.JournalEntryID.Get(); ok {
			byEntry[e] = rq
		}
	}
	chosen := map[string]bool{}
	for _, c := range t.chosen {
		chosen[c] = true
	}
	for _, it := range page.Items {
		c := choiceOf(it, inv, byEntry)
		c.Chosen = c.Value != "" && chosen[c.Value]
		v.Items = append(v.Items, c)
	}
	if next, ok := page.NextPageToken.Get(); ok {
		v.More = downPaymentsPath(inv.ID) + "?" + url.Values{ui.FieldPage: {next}}.Encode()
	}
	return 0, true
}

// choiceOf is an open down payment as the screen shows it, with its paid
// request when one names its entry: offered unless the API would refuse it
// whatever else is chosen.
func choiceOf(it gen.OpenItem, inv gen.SupplierInvoice, byEntry map[uuid.UUID]gen.DownPaymentRequest) ui.DownPaymentChoice {
	c := ui.DownPaymentChoice{Entry: it.DocumentNumber, Reference: it.Reference.Or(""), Posting: it.PostingDate}
	rq, ok := byEntry[it.JournalEntryID]
	if !ok {
		c.Why = i18n.DownPaymentsNoRequest
		return c
	}
	paid, tax := moneyOf(rq.Amount), moneyOf(rq.TaxAmount)
	c.Request, c.Amount, c.Tax, c.Reference = rq.DocumentNumber, &paid, &tax, rq.Reference.Or(c.Reference)
	order, named := rq.PurchaseOrderID.Get()
	switch {
	case rq.Amount.Currency != inv.GrossAmount.Currency:
		c.Why = i18n.DownPaymentsOtherCurrency
	case named && (!inv.PurchaseOrderID.Set || inv.PurchaseOrderID.Value != order):
		c.Why = i18n.DownPaymentsOtherOrder
	default:
		c.Value = downPaymentValue(it)
	}
	return c
}

// clearDownPayments clears the down payments the person chose against the
// invoice, at the ETag the screen was read at: done, the person goes to
// the invoice's page, which says so; refused, the screen says why with
// the choice and date kept, at the invoice's version now. A form the
// checks refuse before any call keeps the version it was read at.
func (s *Server) clearDownPayments(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	ifMatch, key, ok := s.formFields(w, r, invoicePath(id), i18n.OrderBack)
	if !ok {
		return
	}
	t := clearingTyped{date: strings.TrimSpace(r.PostForm.Get(ui.FieldDate)), page: r.PostForm.Get(ui.FieldPage), chosen: r.PostForm[ui.FieldDownPayment]}
	body, ok := t.check()
	if !ok && t.dateErr == "" && t.chooseErr == "" {
		// A down payment the form does not offer: not the page's form.
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.chrome(r, i18n.RefusedTitle), i18n.RefusedTitle, i18n.ProblemInvoiceBody, invoicePath(id), i18n.OrderBack))
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	if body == nil {
		s.showClearing(w, r, p, m, id, ifMatch, http.StatusUnprocessableEntity, t, nil)
		return
	}
	res, err := s.API.ClearSupplierInvoiceDownPayments(r.Context(), body, gen.ClearSupplierInvoiceDownPaymentsParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	if _, ok := res.(*gen.UpdateSupplierInvoiceOKHeaders); ok { // the operation answers the invoice, as a change does
		http.Redirect(w, r, invoicePath(id)+"?done=cleared", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "clearSupplierInvoiceDownPayments", res, err); show {
		s.showClearing(w, r, p, m, id, "", int(prob.Status), t, explainIn(downPaymentProblems, i18n.DownPaymentsRefusedTitle, prob, m))
	}
}

// showClearing reads the invoice and renders the screen again with what
// was sent: at tag, or at the invoice's ETag now when tag is "".
func (s *Server) showClearing(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, tag string, status int, t clearingTyped,
	refused *ui.Refusal) {
	inv, now, ok := s.readInvoice(w, r, p, m, id)
	if !ok {
		return
	}
	if tag == "" {
		tag = now
	}
	s.showDownPayments(w, r, p, m, inv, tag, status, t, refused)
}

// check checks what was sent before the API is called, so it stays on the
// screen: a posting date, and at least one down payment, each one the
// form offers. body is nil when anything is wrong; ok is false, with
// neither error set, when a down payment is not one the form offers.
func (t *clearingTyped) check() (body *gen.DownPaymentClearingInput, ok bool) {
	var items []gen.ClearingItemRef
	for _, c := range t.chosen {
		ref, valid := downPaymentRef(c)
		if !valid {
			return nil, false
		}
		items = append(items, ref)
	}
	if len(items) == 0 {
		t.chooseErr = i18n.DownPaymentsChooseNeeded
	}
	var day gen.OptDate
	if t.date == "" {
		t.dateErr = i18n.DownPaymentsDateNeeded
	} else if day, t.dateErr = dateParam(t.date); t.dateErr != "" {
		t.dateErr = i18n.ActionDateInvalid
	}
	if t.dateErr != "" || t.chooseErr != "" {
		return nil, false
	}
	return &gen.DownPaymentClearingInput{PostingDate: day.Value, Items: items}, true
}
