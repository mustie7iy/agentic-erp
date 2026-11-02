package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A parked invoice's change (ADR-0052 §7, T73e2, T73e3): the form shows the
// invoice's header and lines as they are, and sends what the person
// changed as a merge patch (updateSupplierInvoice) with the ETag the form
// was opened with and its own idempotency key. Only what the person
// changed is sent: each field travels with its value when the form was
// opened, so a value someone else changed meanwhile is never sent back
// over theirs, and a field sent as it was would cost a match (the API
// matches an invoice on an order again after any change, and a change
// unlinks an approved exception). Lines are changed, added and left out,
// each naming its order item, unit, account and centers (the account by
// number, the centers by code, as in SAP); the API replaces the lines as a
// whole, so they are sent whole. Lines added or left out have no identity
// to put another person's change on field by field: when both people
// changed the lines, the form shows the invoice's lines as they are now,
// without the person's changes to them, and says so (rebaseItems). The
// form reads getCurrentPrincipal, getSupplierInvoice, the company's
// accounts, cost centers and profit centers: five calls. Sending it reads
// the same, looks up the codes the pages do not hold (entry_lookup.go), and
// puts the person's changes on the invoice as it is now, which is also what
// a refused form shows, at the invoice's version now. With htmx, a line
// added or left out swaps the lines' section alone.

// maxItemText is the longest description of an invoice's line
// (SupplierInvoiceItem.description).
const maxItemText = 200

// invoiceTyped is what the person typed in an invoice's change form, what
// the form asks (its op), what is wrong with it, and the API's refusal of
// it.
type invoiceTyped struct {
	reference, invoiceDate, postingDate, taxDate string
	gross, tax, ifMatch, op                      string
	errs                                         map[string]i18n.Key
	linesErr                                     i18n.Key
	items                                        []itemTyped
	refusal                                      *ui.Refusal
	opened                                       *invoiceTyped // the values the form sends as opened (FieldWas)
}

// itemTyped is one line as typed: the invoice's line it shows (from 1,
// its place when the form was opened; 0 for one added in the form), its
// fields, what is wrong with them and what the API refused of it, whether
// it has the focus, and, checked, what is sent.
type itemTyped struct {
	from                                                    int
	orderItem, description, quantity, unit, amount, taxCode string
	account, costCenter, profitCenter                       string
	errs                                                    map[string]i18n.Key
	refused                                                 []refusedField
	focus                                                   bool
	sent                                                    gen.SupplierInvoiceItem
}

// field is the line's field by its name in the form.
func (it *itemTyped) field(name string) *string {
	switch name {
	case ui.FieldItemOrder:
		return &it.orderItem
	case ui.FieldItemDescription:
		return &it.description
	case ui.FieldItemQuantity:
		return &it.quantity
	case ui.FieldItemUnit:
		return &it.unit
	case ui.FieldItemAmount:
		return &it.amount
	case ui.FieldItemTaxCode:
		return &it.taxCode
	case ui.FieldItemAccount:
		return &it.account
	case ui.FieldItemCostCenter:
		return &it.costCenter
	default:
		return &it.profitCenter
	}
}

// blank reports whether the line was added in the form and holds nothing:
// it is left out when the form is sent.
func (it itemTyped) blank() bool {
	if it.from != 0 {
		return false
	}
	for _, name := range ui.ItemFieldNames() {
		if strings.TrimSpace(*it.field(name)) != "" {
			return false
		}
	}
	return true
}

// same reports whether two lines hold the same values, as typed.
func (it itemTyped) same(other itemTyped) bool {
	for _, name := range ui.ItemFieldNames() {
		if strings.TrimSpace(*it.field(name)) != strings.TrimSpace(*other.field(name)) {
			return false
		}
	}
	return true
}

// sameItems reports whether two lists of lines hold the same values.
func sameItems(a, b []itemTyped) bool {
	return slices.EqualFunc(a, b, itemTyped.same)
}

// kept reports whether the lines are the n the form was opened with, in
// their places: none added or left out.
func kept(items []itemTyped, n int) bool {
	if len(items) != n {
		return false
	}
	for i, it := range items {
		if it.from != i+1 {
			return false
		}
	}
	return true
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

// invoiceCodes are the company's accounts by number and its cost and
// profit centers by code, as a page of each list holds them, with their
// finders: the form shows a line's ids by them, and a code the pages do
// not hold is looked up when it is sent. An id they do not hold is shown
// as it is, and sent so it is taken as it is (lookUp).
type invoiceCodes struct {
	chart          chart
	costCenters    book
	profitCenters  book
	ccFind, pcFind finder
}

// invoiceCodes reads the company's accounts, cost centers and profit
// centers (entry_lookup.go).
func (s *Server) invoiceCodes(ctx context.Context, m me, company uuid.UUID) invoiceCodes {
	c := invoiceCodes{chart: s.chartOf(ctx, m, company)}
	c.costCenters, c.ccFind = s.costCenters(ctx, company)
	c.profitCenters, c.pcFind = s.profitCenters(ctx, company)
	return c
}

// account is an account as the form shows it: its number, else its id.
func (c invoiceCodes) account(id gen.OptUUID) string {
	if a, ok := c.chart.byID[id.Value]; ok && id.Set {
		return a.Number
	}
	return idText(id)
}

// codeIn is a center as the form shows it: its code in the book, else
// its id.
func codeIn(b book, id gen.OptUUID) string {
	for code, known := range b.ids {
		if id.Set && known == id.Value {
			return code
		}
	}
	return idText(id)
}

// invoiceTypedOf is the form as the invoice is, its amounts and quantities
// as a person of the locale types them, its accounts and centers by the
// company's codes.
func invoiceTypedOf(inv gen.SupplierInvoice, tag string, loc i18n.Locale, codes invoiceCodes) invoiceTyped {
	t := invoiceTyped{reference: inv.SupplierReference, invoiceDate: inv.InvoiceDate.Format(time.DateOnly), gross: loc.AmountInput(inv.GrossAmount.Amount),
		tax: loc.AmountInput(inv.TaxAmount.Amount), ifMatch: tag}
	if d, ok := inv.PostingDate.Get(); ok {
		t.postingDate = d.Format(time.DateOnly)
	}
	if d, ok := inv.TaxDate.Get(); ok {
		t.taxDate = d.Format(time.DateOnly)
	}
	for i, it := range inv.Items {
		line := itemTyped{from: i + 1, description: it.Description.Or(""), quantity: loc.AmountInput(it.Quantity.Value), unit: it.Quantity.Unit,
			amount: loc.AmountInput(it.Amount.Amount), taxCode: it.TaxCode.Or(""), account: codes.account(it.AccountID),
			costCenter: codeIn(codes.costCenters, it.CostCenterID), profitCenter: codeIn(codes.profitCenters, it.ProfitCenterID)}
		if n, ok := it.PoItemNo.Get(); ok {
			line.orderItem = strconv.Itoa(int(n))
		}
		t.items = append(t.items, line)
	}
	opened := t
	t.opened = &opened
	return t
}

// readInvoiceForm reads the posted form's fields whose names have the
// prefix: its header, and its lines in their order; "" for what was typed
// (with what the form asks and the invoice's line each shows),
// ui.FieldWas for the values the form was opened with, the invoice's lines
// in their places.
func readInvoiceForm(f url.Values, prefix string) invoiceTyped {
	t := invoiceTyped{reference: f.Get(prefix + ui.FieldSupplierReference), invoiceDate: f.Get(prefix + ui.FieldInvoiceDate),
		postingDate: f.Get(prefix + ui.FieldPostingDate), taxDate: f.Get(prefix + ui.FieldTaxDate), gross: f.Get(prefix + ui.FieldGross),
		tax: f.Get(prefix + ui.FieldTax)}
	at := func(name string, i int) string {
		if v := f[prefix+name]; i < len(v) {
			return v[i]
		}
		return ""
	}
	for i := range len(f[prefix+ui.FieldItemAmount]) {
		it := itemTyped{from: i + 1}
		if prefix == "" {
			it.from, _ = strconv.Atoi(at(ui.FieldItemFrom, i)) // a line added has none: 0
			t.op = f.Get(ui.FieldOp)
		}
		for _, name := range ui.ItemFieldNames() {
			*it.field(name) = at(name, i)
		}
		t.items = append(t.items, it)
	}
	return t
}

// leaveOut drops the lines whose boxes leave them out (by their place in
// the form, from 1); removed is the place of the first, 0 for none.
func (t *invoiceTyped) leaveOut(boxes []string) (removed int) {
	left := map[string]bool{}
	for _, b := range boxes {
		left[b] = true
	}
	var out []itemTyped
	for i, it := range t.items {
		if !left[strconv.Itoa(i+1)] {
			out = append(out, it)
		} else if removed == 0 {
			removed = len(out) + 1
		}
	}
	t.items = out
	return removed
}

// edited is the form after its lines were changed but not sent: a line
// added, focused, or the line now in the first removed line's place
// focused; never fewer than one line.
func (t *invoiceTyped) edited(add bool, removed int) {
	if add && len(t.items) < ui.MaxFormLines {
		t.items = append(t.items, itemTyped{focus: true})
	}
	if len(t.items) == 0 {
		t.items = []itemTyped{{}}
	}
	if removed > 0 {
		t.items[min(removed, len(t.items))-1].focus = true
	}
}

// What rebase did with the lines: took the person's, put the person's
// changes on the invoice's line by line, took the invoice's (the person
// changed none), or took the invoice's over the person's changes, which
// are lost (both people changed the lines, and some were added or left
// out).
type linesTaken int

const (
	linesTyped linesTaken = iota
	linesMerged
	linesNow
	linesLost
)

// rebase is the invoice as it is now (now) with the person's own changes
// on it: each header field whose typed value is not the one the form was
// opened with, and the lines as rebaseItems takes them.
func rebase(typed, opened, now invoiceTyped) (invoiceTyped, linesTaken) {
	out := now
	out.reference, out.invoiceDate = pick(typed.reference, opened.reference, now.reference), pick(typed.invoiceDate, opened.invoiceDate, now.invoiceDate)
	out.postingDate, out.taxDate = pick(typed.postingDate, opened.postingDate, now.postingDate), pick(typed.taxDate, opened.taxDate, now.taxDate)
	out.gross, out.tax = pick(typed.gross, opened.gross, now.gross), pick(typed.tax, opened.tax, now.tax)
	var taken linesTaken
	out.items, taken = rebaseItems(typed.items, opened.items, now.items)
	return out, taken
}

// pick is a field's value with the person's change on it: what they typed
// when it is not what the form was opened with (o), else the value now.
func pick(t, o, n string) string {
	if strings.TrimSpace(t) != strings.TrimSpace(o) {
		return t
	}
	return n
}

// rebaseItems are the lines with the person's changes on the invoice's
// lines now: the person's lines when nobody else changed the lines since
// the form was opened; the invoice's when the person changed none; while
// neither added nor left out a line, each line with the person's changes
// on it field by field, as the header's; else, both having changed the
// lines and lines added or left out, which have no identity to put a
// change on, the invoice's lines, the person's changes to them lost
// (ADR-0052 T73e3, as the entry form's lines).
func rebaseItems(typed, opened, now []itemTyped) ([]itemTyped, linesTaken) {
	untouched := kept(typed, len(opened))
	switch {
	case sameItems(opened, now):
		return typed, linesTyped
	case untouched && sameItems(typed, opened):
		return now, linesNow
	case !untouched || len(now) != len(opened):
		return now, linesLost
	}
	out := make([]itemTyped, len(now))
	for i := range now {
		out[i] = now[i]
		for _, name := range ui.ItemFieldNames() {
			*out[i].field(name) = pick(*typed[i].field(name), *opened[i].field(name), *now[i].field(name))
		}
	}
	return out, linesMerged
}

// asTyped is a form not sent, or refused before any call, as the person
// sent it: what they typed, with the checks' errors (checked, its lines
// taken as rebase says), at the version and with the values it was opened
// with. It does not take the invoice's version now, which would skip the
// 412 that tells the person of a change by someone else meanwhile. A form
// left without lines gets an empty one to type in.
func asTyped(typed, opened invoiceTyped, ifMatch string, checked invoiceTyped, taken linesTaken) invoiceTyped {
	typed.ifMatch, typed.opened, typed.errs, typed.linesErr = ifMatch, &opened, checked.errs, checked.linesErr
	if taken == linesTyped || taken == linesMerged { // as many lines, in their places
		for i := range typed.items {
			typed.items[i].errs = checked.items[i].errs
		}
	}
	if len(typed.items) == 0 {
		typed.items = []itemTyped{{}}
	}
	return typed
}

// invoiceChange checks the form in the person's locale, before the API is
// called so what was typed stays on the page, looks its codes up once
// nothing else is wrong, and makes the merge patch of what changed against
// the invoice as it is now: nil when nothing did. ok is false when
// anything is wrong, said on its field.
func (s *Server) invoiceChange(ctx context.Context, loc i18n.Locale, t *invoiceTyped, inv gen.SupplierInvoice, codes invoiceCodes) (*gen.SupplierInvoiceUpdate, bool) {
	body := &gen.SupplierInvoiceUpdate{}
	t.headerChange(loc, inv, body)
	if len(t.items) == 0 {
		t.linesErr = i18n.InvoiceEditLinesNone
	}
	for i := range t.items {
		t.items[i].check(loc, baseOf(inv, t.items[i].from), inv.GrossAmount.Currency)
	}
	if len(t.errs) > 0 || t.linesErr != "" || slices.ContainsFunc(t.items, func(it itemTyped) bool { return len(it.errs) > 0 }) ||
		!s.resolveItems(ctx, inv.LegalEntityID, codes, t.items) {
		return nil, false
	}
	body.Items = t.itemsChange(inv)
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

// baseOf is the invoice's line a line of the form shows (from, its place
// from 1), its number left to the API, which numbers lines by their place;
// none for a line added in the form.
func baseOf(inv gen.SupplierInvoice, from int) gen.SupplierInvoiceItem {
	if from < 1 || from > len(inv.Items) {
		return gen.SupplierInvoiceItem{}
	}
	base := inv.Items[from-1]
	base.ItemNo = gen.OptInt32{}
	return base
}

// check checks the line's fields in the locale and makes what is sent of
// it, on the invoice's line it shows (base): its purchase order kept while
// it names an order item (a line added takes the invoice's, as the API
// does), its accounts and centers left to resolveItems. The codes are
// taken in capitals, as the API keeps them.
func (it *itemTyped) check(loc i18n.Locale, base gen.SupplierInvoiceItem, currency string) {
	it.sent = gen.SupplierInvoiceItem{PurchaseOrderID: base.PurchaseOrderID, Amount: gen.Money{Currency: currency}}
	it.orderItem = strings.TrimSpace(it.orderItem)
	switch n, err := strconv.ParseInt(it.orderItem, 10, 32); {
	case it.orderItem == "":
		it.sent.PurchaseOrderID = gen.OptUUID{}
	case err != nil || n < 1:
		it.fail(ui.FieldItemOrder, i18n.InvoiceEditOrderInvalid)
	default:
		it.sent.PoItemNo = gen.NewOptInt32(int32(n))
	}
	it.description = strings.TrimSpace(it.description)
	if utf8.RuneCountInString(it.description) > maxItemText {
		it.fail(ui.FieldItemDescription, i18n.EntryTextTooLong)
	} else if it.description != "" {
		it.sent.Description = gen.NewOptString(it.description)
	}
	it.checkQuantity(loc)
	if a, err := typedAmount(loc, it.amount); err != "" {
		it.fail(ui.FieldItemAmount, err)
	} else {
		it.sent.Amount.Amount = a
	}
	for _, code := range []*string{&it.unit, &it.taxCode, &it.costCenter, &it.profitCenter} {
		*code = strings.ToUpper(strings.TrimSpace(*code))
	}
	it.account = strings.TrimSpace(it.account)
	switch {
	case it.taxCode == "":
	case !taxCode.MatchString(it.taxCode): // JournalLineInput's pattern is SupplierInvoiceItem's
		it.fail(ui.FieldItemTaxCode, i18n.EntryTaxCodeInvalid)
	default:
		it.sent.TaxCode = gen.NewOptString(it.taxCode)
	}
	for field, code := range map[string]string{ui.FieldItemCostCenter: it.costCenter, ui.FieldItemProfit: it.profitCenter} {
		if _, isID := uuid.Parse(code); code != "" && isID != nil && !centerCode.MatchString(code) {
			it.fail(field, i18n.EntryCodeInvalid)
		}
	}
}

// checkQuantity checks the line's quantity, positive in an invoice's
// item, and its unit.
func (it *itemTyped) checkQuantity(loc i18n.Locale) {
	if q, ok := loc.Quantity(it.quantity); ok && !strings.HasPrefix(q, "-") {
		it.sent.Quantity.Value = q
	} else if strings.TrimSpace(it.quantity) == "" {
		it.fail(ui.FieldItemQuantity, i18n.InvoiceEditQuantityNeeded)
	} else {
		it.fail(ui.FieldItemQuantity, i18n.InvoiceEditQuantityInvalid)
	}
	it.unit = strings.ToUpper(strings.TrimSpace(it.unit))
	switch {
	case it.unit == "":
		it.fail(ui.FieldItemUnit, i18n.EntryUnitNeeded)
	case !unitCode.MatchString(it.unit):
		it.fail(ui.FieldItemUnit, i18n.EntryUnitInvalid)
	default:
		it.sent.Quantity.Unit = it.unit
	}
}

// itemCode is one kind of code a line names: its field, the company's
// page of it and its finder, what an unknown one is, the line's code of it
// and where its id goes.
type itemCode struct {
	field   string
	known   book
	find    finder
	unknown i18n.Key
	code    func(itemTyped) string
	set     func(*gen.SupplierInvoiceItem, uuid.UUID)
}

// resolveItems looks up the accounts and centers the lines name in the
// company (lookUp: from the pages read, an id as it is, else one call a
// code within maxCodeLookups) and sets each line's ids, or says on it what
// is not known. It reports whether every line can be sent.
func (s *Server) resolveItems(ctx context.Context, company uuid.UUID, codes invoiceCodes, items []itemTyped) bool {
	lk := &lookups{left: maxCodeLookups}
	kinds := []itemCode{
		{ui.FieldItemAccount, book{ids: codes.chart.byNumber, more: codes.chart.more}, s.accountFinder(ctx, company), i18n.EntryAccountUnknown,
			func(it itemTyped) string { return it.account }, func(in *gen.SupplierInvoiceItem, id uuid.UUID) { in.AccountID = gen.NewOptUUID(id) }},
		{ui.FieldItemCostCenter, codes.costCenters, codes.ccFind, i18n.EntryCostCenterUnknown,
			func(it itemTyped) string { return it.costCenter }, func(in *gen.SupplierInvoiceItem, id uuid.UUID) { in.CostCenterID = gen.NewOptUUID(id) }},
		{ui.FieldItemProfit, codes.profitCenters, codes.pcFind, i18n.EntryProfitCenterUnknown,
			func(it itemTyped) string { return it.profitCenter }, func(in *gen.SupplierInvoiceItem, id uuid.UUID) { in.ProfitCenterID = gen.NewOptUUID(id) }},
	}
	ok := true
	for _, k := range kinds {
		var named []string
		for _, it := range items {
			named = append(named, k.code(it))
		}
		got := lk.lookUp(named, k.known, k.unknown, k.find)
		for i := range items {
			if code := k.code(items[i]); code == "" {
				continue
			} else if f := got[code]; f.err != "" {
				items[i].fail(k.field, f.err)
				ok = false
			} else {
				k.set(&items[i].sent, f.id)
			}
		}
	}
	return ok
}

// itemsChange are the lines as sent, nil when they are the invoice's. A
// line whose order item changed takes the new order item's account and
// cost center, as its hint says, unless the person changed them too.
func (t *invoiceTyped) itemsChange(inv gen.SupplierInvoice) []gen.SupplierInvoiceItem {
	out := make([]gen.SupplierInvoiceItem, len(t.items))
	changed := len(t.items) != len(inv.Items)
	for i, it := range t.items {
		item, base := it.sent, baseOf(inv, it.from)
		if item.PoItemNo.Set && item.PoItemNo != base.PoItemNo {
			if item.AccountID == base.AccountID {
				item.AccountID = gen.OptUUID{}
			}
			if item.CostCenterID == base.CostCenterID {
				item.CostCenterID = gen.OptUUID{}
			}
		}
		out[i] = item
		changed = changed || item != baseOf(inv, i+1)
	}
	if !changed {
		return nil
	}
	return out
}

// view is the form as the page shows it, each line with its errors and
// what the API refused of it, and the lines it was opened with.
func (t invoiceTyped) view(inv gen.SupplierInvoice, p *person, ch chart) ui.InvoiceForm {
	f := ui.InvoiceForm{Action: invoicePath(inv.ID) + "/edit", Back: invoicePath(inv.ID), Number: inv.DocumentNumber, CSRF: p.CSRF, Key: uuid.NewString(),
		ETag: t.ifMatch, Currency: inv.GrossAmount.Currency, Reference: t.reference, InvoiceDate: t.invoiceDate, PostingDate: t.postingDate,
		TaxDate: t.taxDate, Gross: t.gross, Tax: t.tax, Errors: t.errs, LinesError: t.linesErr, Refusal: t.refusal, HasOrder: inv.PurchaseOrderID.Set,
		Accounts: ch.options(),
		Was: map[string]string{ui.FieldSupplierReference: t.opened.reference, ui.FieldInvoiceDate: t.opened.invoiceDate, ui.FieldPostingDate: t.opened.postingDate,
			ui.FieldTaxDate: t.opened.taxDate, ui.FieldGross: t.opened.gross, ui.FieldTax: t.opened.tax}}
	for _, o := range t.opened.items {
		values := map[string]string{}
		for _, name := range ui.ItemFieldNames() {
			values[name] = *o.field(name)
		}
		f.Opened = append(f.Opened, values)
	}
	for i, it := range t.items {
		in := ui.ItemInput{No: i + 1, From: it.from, OrderItem: it.orderItem, Description: it.description, Quantity: it.quantity, Unit: it.unit,
			Amount: it.amount, TaxCode: it.taxCode, Account: it.account, CostCenter: it.costCenter, ProfitCenter: it.profitCenter, Errors: it.errs,
			Focus: it.focus}
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
	codes := s.invoiceCodes(r.Context(), m, inv.LegalEntityID)
	s.showInvoiceForm(w, r, p, m, inv, http.StatusOK, invoiceTypedOf(inv, tag, loc, codes), codes.chart)
}

// showInvoiceForm renders an invoice's change with status; with htmx,
// after its lines changed, its lines' section alone.
func (s *Server) showInvoiceForm(w http.ResponseWriter, r *http.Request, p *person, m me, inv gen.SupplierInvoice, status int, t invoiceTyped, ch chart) {
	w.Header().Add("Vary", "HX-Request")
	f := t.view(inv, p, ch)
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, status, ui.InvoiceItemsSwap(f))
		return
	}
	s.render(w, r, status, ui.InvoiceEdit(s.page(i18n.InvoiceEditTitle, ui.InvoicesPath, p, m), f))
}

// saveInvoice sends what the person changed of a parked invoice
// (sendInvoice), or, when the form's lines were changed instead (a line
// added, or with htmx one left out), renders it again as typed, at the
// version it was opened with.
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
	typed, opened := readInvoiceForm(r.PostForm, ""), readInvoiceForm(r.PostForm, ui.FieldWas)
	removed := typed.leaveOut(r.PostForm[ui.FieldItemRemove])
	if typed.op == ui.OpAdd || r.Header.Get("HX-Request") == "true" {
		typed.edited(typed.op == ui.OpAdd, removed)
		s.showInvoiceForm(w, r, p, m, inv, http.StatusOK, asTyped(typed, opened, ifMatch, invoiceTyped{}, linesNow), s.chartOf(r.Context(), m, inv.LegalEntityID))
		return
	}
	typed.items = slices.DeleteFunc(typed.items, itemTyped.blank)
	s.sendInvoice(w, r, p, m, inv, tag, key, invoiceSent{typed: typed, opened: opened, ifMatch: ifMatch})
}

// invoiceSent is a posted change form: what was typed, the values it was
// opened with, and the ETag it was opened at.
type invoiceSent struct {
	typed, opened invoiceTyped
	ifMatch       string
}

// sendInvoice sends what the person changed at the ETag the form was
// opened with: done, the person goes to the invoice's page, which says so
// (or that nothing changed); refused (a change meanwhile, an invoice no
// longer parked, a value the API does not take), the form says why and
// shows the invoice at its version now with the person's changes on it, so
// sending it again sends those alone. Lines both people changed are shown
// as the invoice has them now, without the person's changes to them, and
// nothing is sent.
func (s *Server) sendInvoice(w http.ResponseWriter, r *http.Request, p *person, m me, inv gen.SupplierInvoice, tag, key string, sent invoiceSent) {
	ctx := r.Context()
	loc, _ := i18n.From(ctx) // every request has its locale (localized)
	codes := s.invoiceCodes(ctx, m, inv.LegalEntityID)
	t, taken := rebase(sent.typed, sent.opened, invoiceTypedOf(inv, tag, loc, codes))
	if taken == linesLost {
		t.linesErr = i18n.InvoiceEditLinesChanged
		s.showInvoiceForm(w, r, p, m, inv, http.StatusConflict, t, codes.chart)
		return
	}
	body, valid := s.invoiceChange(ctx, loc, &t, inv, codes)
	switch {
	case !valid:
		s.showInvoiceForm(w, r, p, m, inv, http.StatusUnprocessableEntity, asTyped(sent.typed, sent.opened, sent.ifMatch, t, taken), codes.chart)
		return
	case body == nil:
		http.Redirect(w, r, invoicePath(inv.ID)+"?done=unchanged", http.StatusSeeOther)
		return
	}
	res, err := s.API.UpdateSupplierInvoice(ctx, body, gen.UpdateSupplierInvoiceParams{ID: inv.ID, IfMatch: sent.ifMatch, IdempotencyKey: key})
	if _, ok := res.(*gen.UpdateSupplierInvoiceOKHeaders); ok {
		http.Redirect(w, r, invoicePath(inv.ID)+"?done=changed", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "updateSupplierInvoice", res, err); show {
		t.refusal = explainIn(i18n.InvoiceProblems, i18n.InvoiceRefusedTitle, prob, m)
		for i, rf := range refusedFields(prob, "items", len(t.items)) {
			t.items[i].refused = rf
		}
		s.showInvoiceForm(w, r, p, m, inv, int(prob.Status), t, codes.chart)
	}
}
