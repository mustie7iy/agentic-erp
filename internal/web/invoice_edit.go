package web

import (
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A parked invoice's change (ADR-0052 §7, T73e2): the form shows the
// invoice's header and lines as they are, and sends what the person
// changed as a merge patch (updateSupplierInvoice) with the ETag the form
// was opened with and its own idempotency key. Only what changed is sent:
// the API matches an invoice on an order again after any change, and a
// change unlinks an approved exception, so a field sent as it was would
// cost a match. Lines are changed, never added or left out: their order
// items, units and accounts are kept as the invoice has them, and the
// lines are sent as a whole, as the merge patch replaces them. The form
// reads getCurrentPrincipal and getSupplierInvoice; sending it reads the
// invoice again, to change what was typed against the invoice as it is.

// maxItemText is the longest description of an invoice's line
// (SupplierInvoiceItem.description).
const maxItemText = 200

// invoiceTyped is what the person typed in an invoice's change form, what
// is wrong with it, and the API's refusal of it.
type invoiceTyped struct {
	reference, invoiceDate, postingDate, taxDate string
	gross, tax, ifMatch                          string
	errs                                         map[string]i18n.Key
	linesErr                                     i18n.Key
	items                                        []itemTyped
	refusal                                      *ui.Refusal
}

// itemTyped is one line as typed, what is wrong with it and what the API
// refused of it.
type itemTyped struct {
	description, quantity, amount, taxCode string
	errs                                   map[string]i18n.Key
	refused                                []refusedField
}

// fail records what is wrong with a header's field.
func (t *invoiceTyped) fail(field string, k i18n.Key) {
	if t.errs == nil {
		t.errs = map[string]i18n.Key{}
	}
	t.errs[field] = k
}

// fail records what is wrong with a line's field.
func (it *itemTyped) fail(field string, k i18n.Key) {
	if it.errs == nil {
		it.errs = map[string]i18n.Key{}
	}
	it.errs[field] = k
}

// invoiceTypedOf is the form as the invoice is, its amounts and quantities as a
// person of the locale types them.
func invoiceTypedOf(inv gen.SupplierInvoice, tag string, loc i18n.Locale) invoiceTyped {
	t := invoiceTyped{reference: inv.SupplierReference, invoiceDate: inv.InvoiceDate.Format(time.DateOnly), gross: loc.AmountInput(inv.GrossAmount.Amount),
		tax: loc.AmountInput(inv.TaxAmount.Amount), ifMatch: tag}
	if d, ok := inv.PostingDate.Get(); ok {
		t.postingDate = d.Format(time.DateOnly)
	}
	if d, ok := inv.TaxDate.Get(); ok {
		t.taxDate = d.Format(time.DateOnly)
	}
	for _, it := range inv.Items {
		t.items = append(t.items, itemTyped{description: it.Description.Or(""), quantity: loc.AmountInput(it.Quantity.Value),
			amount: loc.AmountInput(it.Amount.Amount), taxCode: it.TaxCode.Or("")})
	}
	return t
}

// readInvoiceForm reads the posted form: its header, and its lines in
// their order.
func readInvoiceForm(r *http.Request, ifMatch string) invoiceTyped {
	f := r.PostForm
	t := invoiceTyped{reference: f.Get(ui.FieldSupplierReference), invoiceDate: f.Get(ui.FieldInvoiceDate), postingDate: f.Get(ui.FieldPostingDate),
		taxDate: f.Get(ui.FieldTaxDate), gross: f.Get(ui.FieldGross), tax: f.Get(ui.FieldTax), ifMatch: ifMatch}
	at := func(name string, i int) string {
		if v := f[name]; i < len(v) {
			return v[i]
		}
		return ""
	}
	for i := range len(f[ui.FieldItemAmount]) {
		t.items = append(t.items, itemTyped{description: at(ui.FieldItemDescription, i), quantity: at(ui.FieldItemQuantity, i),
			amount: at(ui.FieldItemAmount, i), taxCode: at(ui.FieldItemTaxCode, i)})
	}
	return t
}

// invoiceChange checks the form in the person's locale, before the API is
// called so what was typed stays on the page, against the invoice as it
// is now, and makes the merge patch of what changed: nil when nothing
// did. ok is false when anything is wrong, said on its field, or when the
// invoice's lines are not the form's (linesErr).
func invoiceChange(loc i18n.Locale, t *invoiceTyped, inv gen.SupplierInvoice) (*gen.SupplierInvoiceUpdate, bool) {
	body := &gen.SupplierInvoiceUpdate{}
	t.headerChange(loc, inv, body)
	body.Items = t.itemsChange(loc, inv)
	if len(t.errs) > 0 || t.linesErr != "" || slices.ContainsFunc(t.items, func(it itemTyped) bool { return len(it.errs) > 0 }) {
		return nil, false
	}
	if !body.SupplierReference.Set && !body.InvoiceDate.Set && !body.PostingDate.Set && !body.TaxDate.Set && !body.GrossAmount.Set &&
		!body.TaxAmount.Set && body.Items == nil {
		return nil, true
	}
	return body, true
}

// headerChange checks the header's fields and sets, on body, those that
// changed.
func (t *invoiceTyped) headerChange(loc i18n.Locale, inv gen.SupplierInvoice, body *gen.SupplierInvoiceUpdate) {
	t.reference = strings.TrimSpace(t.reference)
	switch {
	case t.reference == "":
		t.fail(ui.FieldSupplierReference, i18n.InvoiceEditReferenceNeeded)
	case utf8.RuneCountInString(t.reference) > maxReference:
		t.fail(ui.FieldSupplierReference, i18n.EntryReferenceTooLong)
	case t.reference != inv.SupplierReference:
		body.SupplierReference = gen.NewOptString(t.reference)
	}
	if d, err := requiredDate(strings.TrimSpace(t.invoiceDate)); err != "" {
		t.fail(ui.FieldInvoiceDate, err)
	} else if !d.Equal(inv.InvoiceDate) {
		body.InvoiceDate = gen.NewOptDate(d)
	}
	body.PostingDate = t.optionalDate(ui.FieldPostingDate, t.postingDate, inv.PostingDate)
	body.TaxDate = t.optionalDate(ui.FieldTaxDate, t.taxDate, inv.TaxDate)
	body.GrossAmount = t.amount(loc, ui.FieldGross, t.gross, inv.GrossAmount)
	body.TaxAmount = t.amount(loc, ui.FieldTax, t.tax, inv.TaxAmount)
}

// optionalDate is an optional date's change: unset when it is as it was,
// null when it was cleared.
func (t *invoiceTyped) optionalDate(field, typed string, was gen.OptDate) gen.OptNilDate {
	d, err := dateParam(strings.TrimSpace(typed))
	switch {
	case err != "":
		t.fail(field, i18n.ActionDateInvalid)
	case !d.Set && was.Set:
		return gen.OptNilDate{Set: true, Null: true}
	case d.Set && (!was.Set || !d.Value.Equal(was.Value)):
		return gen.NewOptNilDate(d.Value)
	}
	return gen.OptNilDate{}
}

// amount is a header amount's change, in the invoice's currency: unset
// when it is as it was.
func (t *invoiceTyped) amount(loc i18n.Locale, field, typed string, was gen.Money) gen.OptMoney {
	a, err := typedAmount(loc, typed)
	switch {
	case err != "":
		t.fail(field, err)
	case a != was.Amount:
		return gen.NewOptMoney(gen.Money{Amount: a, Currency: was.Currency})
	}
	return gen.OptMoney{}
}

// typedAmount reads a required amount, or says what is wrong with it.
func typedAmount(loc i18n.Locale, typed string) (string, i18n.Key) {
	if strings.TrimSpace(typed) == "" {
		return "", i18n.EntryAmountNeeded
	}
	a, ok := loc.Amount(typed)
	if !ok {
		return "", i18n.EntryAmountInvalid
	}
	return a, ""
}

// itemsChange checks each line and returns the invoice's lines with the
// changes, nil when none changed. A form whose lines are not the
// invoice's (it changed since the form was opened) sets linesErr.
func (t *invoiceTyped) itemsChange(loc i18n.Locale, inv gen.SupplierInvoice) []gen.SupplierInvoiceItem {
	if len(t.items) != len(inv.Items) {
		t.linesErr = i18n.InvoiceEditLinesChanged
		return nil
	}
	out := slices.Clone(inv.Items)
	changed := false
	for i := range t.items {
		if t.items[i].change(loc, &out[i]) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return out
}

// change checks the line and sets, on item, what changed; it reports
// whether anything did.
func (it *itemTyped) change(loc i18n.Locale, item *gen.SupplierInvoiceItem) bool {
	before := *item
	it.description = strings.TrimSpace(it.description)
	switch {
	case utf8.RuneCountInString(it.description) > maxItemText:
		it.fail(ui.FieldItemDescription, i18n.EntryTextTooLong)
	case it.description == "":
		item.Description = gen.OptString{}
	default:
		item.Description = gen.NewOptString(it.description)
	}
	if q, ok := loc.Quantity(it.quantity); ok {
		item.Quantity.Value = q
	} else if strings.TrimSpace(it.quantity) == "" {
		it.fail(ui.FieldItemQuantity, i18n.InvoiceEditQuantityNeeded)
	} else {
		it.fail(ui.FieldItemQuantity, i18n.InvoiceEditQuantityInvalid)
	}
	if a, err := typedAmount(loc, it.amount); err != "" {
		it.fail(ui.FieldItemAmount, err)
	} else {
		item.Amount.Amount = a
	}
	it.taxCode = strings.ToUpper(strings.TrimSpace(it.taxCode))
	switch {
	case it.taxCode == "":
		item.TaxCode = gen.OptString{}
	case !taxCode.MatchString(it.taxCode): // JournalLineInput's pattern is SupplierInvoiceItem's
		it.fail(ui.FieldItemTaxCode, i18n.EntryTaxCodeInvalid)
	default:
		item.TaxCode = gen.NewOptString(it.taxCode)
	}
	return item.Description != before.Description || item.Quantity != before.Quantity || item.Amount != before.Amount || item.TaxCode != before.TaxCode
}

// view is the form as the page shows it, each line with its order item
// and unit, its errors and what the API refused of it.
func (t invoiceTyped) view(inv gen.SupplierInvoice, p *person) ui.InvoiceForm {
	f := ui.InvoiceForm{Action: invoicePath(inv.ID) + "/edit", Back: invoicePath(inv.ID), Number: inv.DocumentNumber, CSRF: p.CSRF, Key: uuid.NewString(),
		ETag: t.ifMatch, Currency: inv.GrossAmount.Currency, Reference: t.reference, InvoiceDate: t.invoiceDate, PostingDate: t.postingDate,
		TaxDate: t.taxDate, Gross: t.gross, Tax: t.tax, Errors: t.errs, LinesError: t.linesErr, Refusal: t.refusal}
	// The form shows the invoice's lines alone: the lines of one that are
	// not the invoice's are not shown (saveInvoice), so each has its line.
	for i, it := range t.items {
		in := ui.ItemInput{No: i + 1, OrderItem: inv.Items[i].PoItemNo.Or(0), Unit: inv.Items[i].Quantity.Unit, Description: it.description,
			Quantity: it.quantity, Amount: it.amount, TaxCode: it.taxCode, Errors: it.errs}
		for _, rf := range it.refused {
			in.Refused = append(in.Refused, ui.FieldRefusal{Field: ui.ItemFieldLabel(rf.api), Reason: rf.reason})
		}
		f.Items = append(f.Items, in)
	}
	return f
}

// editInvoice is the form of a parked invoice's change, as the invoice
// is; an invoice no longer parked goes to its page, which says what it is
// now.
func (s *Server) editInvoice(w http.ResponseWriter, r *http.Request, p *person) {
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
	if inv.Status != "parked" {
		http.Redirect(w, r, invoicePath(id), http.StatusSeeOther)
		return
	}
	loc, _ := i18n.From(r.Context()) // every request has its locale (localized)
	s.showInvoiceForm(w, r, p, m, inv, http.StatusOK, invoiceTypedOf(inv, tag, loc))
}

// showInvoiceForm renders an invoice's change with status.
func (s *Server) showInvoiceForm(w http.ResponseWriter, r *http.Request, p *person, m me, inv gen.SupplierInvoice, status int, t invoiceTyped) {
	s.render(w, r, status, ui.InvoiceEdit(s.page(i18n.InvoiceEditTitle, ui.InvoicesPath, p, m), t.view(inv, p)))
}

// saveInvoice sends a parked invoice's change at the ETag the form was
// opened with: done, the person goes to the invoice's page, which says
// so (or that nothing changed); refused (a change meanwhile, an invoice
// no longer parked, a value the API does not take), the form says why,
// keeps what was typed and takes the invoice's version now. A form whose
// lines are no longer the invoice's is shown again as the invoice is.
func (s *Server) saveInvoice(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	ifMatch, key, ok := s.formFields(w, r, invoicePath(id), i18n.OrderBack)
	if !ok {
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
	loc, _ := i18n.From(r.Context()) // every request has its locale (localized)
	t := readInvoiceForm(r, ifMatch)
	body, valid := invoiceChange(loc, &t, inv)
	switch {
	case t.linesErr != "":
		fresh := invoiceTypedOf(inv, tag, loc)
		fresh.linesErr = t.linesErr
		s.showInvoiceForm(w, r, p, m, inv, http.StatusConflict, fresh)
		return
	case !valid:
		s.showInvoiceForm(w, r, p, m, inv, http.StatusUnprocessableEntity, t)
		return
	case body == nil:
		http.Redirect(w, r, invoicePath(id)+"?done=unchanged", http.StatusSeeOther)
		return
	}
	res, err := s.API.UpdateSupplierInvoice(r.Context(), body, gen.UpdateSupplierInvoiceParams{ID: id, IfMatch: t.ifMatch, IdempotencyKey: key})
	if _, ok := res.(*gen.UpdateSupplierInvoiceOKHeaders); ok {
		http.Redirect(w, r, invoicePath(id)+"?done=changed", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "updateSupplierInvoice", res, err); show {
		t.refusal, t.ifMatch = explainIn(i18n.InvoiceProblems, i18n.InvoiceRefusedTitle, prob, m), tag
		for i, rf := range refusedFields(prob, "items", len(t.items)) {
			t.items[i].refused = rf
		}
		s.showInvoiceForm(w, r, p, m, inv, int(prob.Status), t)
	}
}
