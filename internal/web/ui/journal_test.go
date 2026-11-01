package ui

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// journalComponents are the manual journal entry screens' components
// (T73h), with the variants their branches need.
func journalComponents() map[string]templ.Component {
	r := refusal()
	return map[string]templ.Component{
		"Parked":              Parked(signedIn(), parkedList(true)),
		"parked plain":        parkedContent(parkedList(false)),
		"parked invalid":      parkedContent(ParkedList{Invalid: true}),
		"parked refused":      parkedContent(ParkedList{Refusal: &r}),
		"parked empty":        parkedContent(ParkedList{Entries: "/journal/entries"}),
		"KindTabs":            KindTabs([]StatusTab{{Label: i18n.ParkedKindAll, Path: "/journal", Current: true}}),
		"ParkedCard":          ParkedCard(parkedRow(true)),
		"parked card plain":   ParkedCard(parkedRow(false)),
		"ParkedStatusTag":     ParkedStatusTag(Label{Key: i18n.ParkedStatusPosted}),
		"Entries":             Entries(signedIn(), entryList(true)),
		"entries plain":       entriesContent(entryList(false)),
		"entries invalid":     entriesContent(EntryList{Invalid: true}),
		"entries refused":     entriesContent(EntryList{Refusal: &r}),
		"entries empty":       entriesContent(EntryList{}),
		"EntryFilterForm":     EntryFilterForm(entryList(true).Filters),
		"centerSelect":        centerSelect("c", FieldCostCenter, i18n.EntryLineCostCenter, []Option{{Value: "c1", Text: "CC100", Selected: true}}),
		"EntryCard":           EntryCard(entryRow(true)),
		"entry card plain":    EntryCard(entryRow(false)),
		"Entry":               Entry(signedIn(), entryPage(true)),
		"entry plain":         entryContent(entryPage(false)),
		"entry read only":     entryContent(EntryPage{Number: "1", Why: i18n.EntryNotYours}),
		"EntryLinks":          EntryLinks(entryPage(true).Header),
		"JournalLinesTable":   JournalLinesTable([]JournalLine{journalLine(true), journalLine(false)}, true),
		"parked lines":        JournalLinesTable([]JournalLine{journalLine(false)}, false),
		"JournalLineRow":      JournalLineRow(journalLine(true)),
		"codeOrNone":          codeOrNone(""),
		"ReverseFormView":     ReverseFormView(ReverseForm{Action: "/x", Date: "d", Text: "t", DateError: i18n.ActionDateInvalid, TextError: i18n.EntryTextTooLong}),
		"ParkedEntry":         ParkedEntry(signedIn(), parkedPage(true)),
		"parked entry plain":  parkedEntryContent(parkedPage(false)),
		"ParkedActions":       ParkedActions(ParkedPage{Why: i18n.ParkedOthers}),
		"ParkedFormView":      ParkedFormView(ParkedForm{Verb: ParkedWithdraw, Action: "/x", Request: "r1", Text: "t", TextError: i18n.ActionReasonNeeded}),
		"parked form post":    ParkedFormView(ParkedForm{Verb: ParkedPost, Action: "/x"}),
		"parked form cancel":  ParkedFormView(ParkedForm{Verb: ParkedCancel, Action: "/x"}),
		"UploadLinesView":     UploadLinesView(linesUpload(true)),
		"upload lines plain":  UploadLinesView(linesUpload(false)),
		"RowErrorItem":        RowErrorItem(RowError{Row: 2, Column: "amount", Error: i18n.EntryAmountInvalid}),
		"row error refused":   RowErrorItem(RowError{Row: 3, Error: i18n.EntryFieldRefused, Reason: "not_allowed"}),
		"EntryFormPage":       EntryFormPage(signedIn(), entryForm(true)),
		"entry form new":      entryFormContent(entryForm(false)),
		"entry form no co":    entryFormContent(EntryForm{Back: "/journal"}),
		"EntryFormView":       EntryFormView(entryForm(true)),
		"companyChoice":       companyChoice(EntryForm{Companies: []Option{{Value: "c1", Text: "Acme"}}, CompanyError: i18n.ProposeCompanyNeeded}),
		"EntryLines":          EntryLines(entryForm(true)),
		"entry lines refused": EntryLines(EntryForm{Action: "/x", LinesError: i18n.EntryLinesTooFew}),
		"EntryLineView":       EntryLineView("/x", lineInput2(true)),
		"entry line plain":    EntryLineView("/x", lineInput2(false)),
		"RefusedItem":         RefusedItem(FieldRefusal{Field: i18n.EntryLineCostCenter, Reason: "not_allowed"}),
		"refused item whole":  RefusedItem(FieldRefusal{Reason: "unbalanced"}),
		"entry form full":     EntryFormView(fullEntryForm()),
		"currencyField":       currencyField(fullEntryForm()),
		"currency plain":      currencyField(EntryForm{}),
		"postToField":         postToField(fullEntryForm()),
		"entryFileView":       entryFileView(*fullEntryForm().File),
		"entry file plain":    entryFileView(LinesUpload{}),
		"EntryTotalsView":     EntryTotalsView(fullEntryForm().Totals),
		"EntryTotalsBody":     EntryTotalsBody(EntryTotals{Debits: "1", Credits: "1", Difference: "0"}),
		"EntryLinesSwap":      EntryLinesSwap(fullEntryForm()),
		"entryButtons":        entryButtons(fullEntryForm()),
		"entryButtonsBody":    entryButtonsBody(entryForm(true)),
	}
}

// parkedRow is a parked entry of the list: an agent's proposal with
// everything, or plain.
func parkedRow(full bool) ParkedRow {
	r := ParkedRow{Path: "/journal/parked/1", Status: Label{Raw: "frozen"}, DocType: "SA", Posting: day, Lines: 2, CreatedBy: Name{Text: "Avery"},
		Created: day}
	if full {
		r.Company, r.Header, r.Debits, r.Unowned = "Acme GmbH", "Accrual", &Money{Amount: "10.00", Currency: "EUR"}, true
		r.Status, r.CreatedBy = Label{Key: i18n.ParkedStatusParked}, Name{Text: "Matcher", Agent: true}
	}
	return r
}

// parkedList is the list: everything, or a page of one entry.
func parkedList(full bool) ParkedList {
	if !full {
		return ParkedList{Rows: []ParkedRow{parkedRow(false)}}
	}
	return ParkedList{Tabs: []StatusTab{{Label: i18n.InvoicesStatusAll, Path: "/journal", Current: true}}, Kinds: []StatusTab{{Label: i18n.ParkedKindAgent,
		Path: "/journal?created_by_kind=agent"}}, Rows: []ParkedRow{parkedRow(true), parkedRow(false)}, More: "/journal?page=t", New: "/journal/new",
		Entries: "/journal/entries"}
}

// entryRow is a posted entry of the journal: a reversed one with
// everything, or a plain reversal.
func entryRow(full bool) EntryRow {
	r := EntryRow{Path: "/journal/entries/1", Number: "100001", DocType: "SA", Posting: day, Source: Label{Raw: "treasury"}, Reversal: true}
	if full {
		r.Header, r.Debits, r.Source, r.Reversed, r.Reversal = "Accrual", &Money{Amount: "10.00", Currency: "EUR"}, Label{Key: i18n.SourceLedger}, true, false
	}
	return r
}

// entryList is the journal: everything, or a page of one entry.
func entryList(full bool) EntryList {
	if !full {
		return EntryList{Rows: []EntryRow{entryRow(false)}}
	}
	options := []Option{{Value: "c1", Text: "Acme", Selected: true}}
	return EntryList{Filters: EntryFilters{Companies: options, Ledgers: options, CostCenters: []Option{{Value: "cc1", Text: "CC100"}}, ProfitCenters: options,
		Number: "1", From: "x", FromError: i18n.FilterDateInvalid}, Rows: []EntryRow{entryRow(true), entryRow(false)}, More: "/journal/entries?page=t"}
}

// journalLine is a line: posted with everything, or a parked one plain.
func journalLine(full bool) JournalLine {
	l := JournalLine{No: 1, Account: "520000 Office Supplies", Amount: Money{Amount: "10.00", Currency: "EUR"}}
	if full {
		l.AmountLC, l.CostCenter, l.ProfitCenter, l.Segment, l.Partner, l.TaxCode, l.Text = &Money{Amount: "10.00", Currency: "EUR"}, "CC100", "PC10",
			"SEG1", "p1", "V1", "Paper"
	}
	return l
}

// entryPage is a posted entry: with everything, or plain.
func entryPage(full bool) EntryPage {
	h := EntryHeader{Ledger: "0L", Year: 2026, Period: 3, DocType: "SA", DocDate: day, Posting: day, Currency: "EUR", Local: "EUR", Group: "EUR",
		RateLC: "1", RateGC: "1", Source: Label{Raw: "treasury"}, PostedBy: Name{Text: "Avery"}, PostedAt: day}
	if !full {
		return EntryPage{Number: "100001", Header: h, Lines: []JournalLine{journalLine(false)}, Back: "/journal/entries"}
	}
	r := refusal()
	h.Company, h.TaxDate, h.Reference, h.Header, h.Debits = "Acme GmbH", &day, "R1", "Accrual", &Money{Amount: "10.00", Currency: "EUR"}
	h.Parked, h.ReversalOf, h.ReversedBy = "/journal/parked/1", "/journal/entries/2", "/journal/entries/3"
	return EntryPage{Number: "100001", Header: h, Lines: []JournalLine{journalLine(true)}, Reverse: &ReverseForm{Action: "/x"}, Done: i18n.EntryDoneReversed,
		Refusal: &r, Back: "/journal/entries", Audit: "/reports/audit?object_id=1"}
}

// linesUpload is the upload of an entry's lines: refused with its rows
// and more, or plain.
func linesUpload(failed bool) LinesUpload {
	u := LinesUpload{Action: "/x", CSRF: "c", ETag: `"1"`, Key: "k"}
	if failed {
		u.FileError, u.Again, u.More = i18n.CSVFileNeeded, true, 3
		u.Rows = []RowError{{Row: 2, Column: "account", Error: i18n.EntryAccountUnknown}}
	}
	return u
}

// parkedPage is a parked entry: an agent's proposal with every decision,
// or a plain one with nothing to do.
func parkedPage(full bool) ParkedPage {
	h := ParkedHeader{Status: Label{Key: i18n.ParkedStatusPending}, DocType: "SA", DocDate: day, Posting: day, Currency: "EUR", CreatedBy: Name{Text: "A"},
		Created: day, Owner: &Name{Text: "Avery"}}
	if !full {
		return ParkedPage{Header: h, Lines: []JournalLine{journalLine(false)}, Back: "/journal"}
	}
	r := refusal()
	h.Company, h.TaxDate, h.Reference, h.Header, h.Debits, h.Owner = "Acme GmbH", &day, "R1", "Accrual", &Money{Amount: "10.00", Currency: "EUR"}, nil
	return ParkedPage{Header: h, Lines: []JournalLine{journalLine(true)}, Edit: "/journal/parked/1/edit", Upload: new(linesUpload(true)),
		Actions: []ParkedForm{{Verb: ParkedPost, Action: "/x"}}, Why: i18n.ParkedWaits, TakeOn: true, Approval: "/approvals/1",
		Journal: "/journal/entries/1", Done: i18n.ParkedDonePosted, Refusal: &r, Back: "/journal", Audit: "/reports/audit?object_id=1"}
}

// lineInput2 is a line of the form: with every error and refusal, focused
// as just added, or plain.
func lineInput2(failed bool) LineInput {
	l := LineInput{No: 1, Account: "520000", Amount: "10.00"}
	if failed {
		l.Errors = map[string]i18n.Key{FieldLineAccount: i18n.EntryAccountUnknown, FieldLineAmount: i18n.EntryAmountInvalid}
		l.Refused, l.Focus = []FieldRefusal{{Field: i18n.EntryLineCostCenter, Reason: "not_allowed"}, {Reason: "unbalanced"}}, true
	}
	return l
}

// entryForm is the form: a change with everything, or a new entry.
func entryForm(edit bool) EntryForm {
	f := EntryForm{Action: "/journal/new", CSRF: "c", Key: "k", Companies: []Option{{Value: "c1", Text: "Acme", Selected: true}}, DocType: "SA",
		Lines: []LineInput{lineInput2(false), {No: 2}}, Accounts: []Option{{Value: "520000", Text: "520000 Office Supplies"}}, Back: "/journal"}
	if edit {
		r := refusal()
		f.Edit, f.ETag, f.Company, f.Companies, f.Refusal = true, `"1"`, "Acme GmbH", nil, &r
		f.Errors = map[string]i18n.Key{FieldDocType: i18n.EntryDocTypeInvalid}
		f.Lines[0] = lineInput2(true)
	}
	return f
}

// fullEntryForm is a new entry's form with everything T73h2 added: its
// company changed, its currency's company and errors, its ledgers, its
// totals with lines not counted, the post and a file with wrong rows.
func fullEntryForm() EntryForm {
	f := entryForm(false)
	f.FilledFor, f.FilledOn, f.Changed, f.Local, f.CanPost = "c1", "2026-01-01", true, "CAD", true
	f.Errors = map[string]i18n.Key{FieldCurrency: i18n.ProposeCurrencyInvalid, FieldPostTo: i18n.EntryPostToInvalid}
	f.PostTo = []Option{{}, {Value: PostToLedger + "l1", Text: "0L Leading", Selected: true}, {Value: PostToGroup + "ALL", Text: "ALL Every"}}
	f.Totals = EntryTotals{Debits: "10.00", Credits: "9.00", Difference: "1.00", Currency: "CAD", Uncounted: 2}
	f.File = &LinesUpload{FileError: i18n.CSVFileNeeded, Rows: []RowError{{Row: 2, Column: "amount", Error: i18n.EntryAmountInvalid}}, More: 3, Again: true}
	return f
}

// TestJournalHelpers: each status has its colour, the cancellation and
// the withdrawal alone need a reason, the paths drop empty values, the
// form's lines are named, valued, suggested and focused as they should
// be, and none of several options chosen is any.
func TestJournalHelpers(t *testing.T) {
	for name, tc := range map[string]struct {
		l    Label
		want string
	}{
		"posted":    {Label{Key: i18n.ParkedStatusPosted}, "success"},
		"pending":   {Label{Key: i18n.ParkedStatusPending}, "warning"},
		"rejected":  {Label{Key: i18n.ParkedStatusRejected}, "inactive"},
		"cancelled": {Label{Key: i18n.ParkedStatusCancelled}, "inactive"},
		"parked":    {Label{Key: i18n.ParkedStatusParked}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := parkedStatusKind(tc.l); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if !ParkedRequires(ParkedCancel) || !ParkedRequires(ParkedWithdraw) || ParkedRequires(ParkedPost) {
		t.Error("ParkedRequires: a cancellation and a withdrawal need their reason, a post none")
	}
	if got := JournalFor(url.Values{"status": {""}}); got != JournalPath {
		t.Errorf("JournalFor: got %q", got)
	}
	if got := EntriesFor(url.Values{"ledger_id": {"l1"}}); got != "/journal/entries?ledger_id=l1" {
		t.Errorf("EntriesFor: got %q", got)
	}
	if got := LineFieldLabel("cost_center_id"); got != i18n.EntryLineCostCenter {
		t.Errorf("LineFieldLabel: got %q", got)
	}
	if got := LineFieldLabel("cost_center_code"); got != "" {
		t.Errorf("LineFieldLabel of a field the form has not: got %q", got)
	}
	l := LineInput{No: 2, Account: "a", Amount: "b", CostCenter: "c", ProfitCenter: "d", Segment: "e", Partner: "f", TaxCode: "g", TaxArea: "h",
		TaxBase: "i", Quantity: "j", Unit: "k", Text: "l", Focus: true}
	var got []string
	for _, fd := range lineFields {
		got = append(got, lineValue(l, fd.Name))
	}
	if strings.Join(got, "") != "abcdefghijkl" {
		t.Errorf("lineValue: got %v", got)
	}
	if a := lineInput("/journal/new", l, FieldLineAccount, "a"); a["list"] != "entry-accounts" || a["autofocus"] != true || a["id"] != "line-2-line_account" {
		t.Errorf("the account's input: got %v", a)
	}
	if a := lineInput("/journal/new", LineInput{No: 2}, FieldLineAccount, ""); a["autofocus"] != nil {
		t.Errorf("an account not just added: got %v", a)
	}
	if a := lineInput("/journal/new", l, FieldLineAmount, "b"); a["inputmode"] != "decimal" || a["hx-target"] != "#entry-totals" ||
		a["hx-vals"] != `{"op":"totals"}` {
		t.Errorf("the amount's input: got %v, want decimal, its change asking the totals again", a)
	}
	for _, name := range []string{FieldLineTaxBase, FieldLineQuantity} {
		if a := lineInput("/journal/new", l, name, ""); a["inputmode"] != "decimal" || a["hx-post"] != nil {
			t.Errorf("%s's input: got %v, want decimal, asking nothing", name, a)
		}
	}
	for value, want := range map[string]string{"": "Every ledger the company keeps", PostToLedger + "x": "Ledger 0L Leading", PostToGroup + "G1": "Ledger group 0L Leading", "book:x": "book:x"} {
		var b strings.Builder
		if err := postToText(Option{Value: value, Text: "0L Leading"}).Render(locale(t), &b); err != nil || b.String() != want {
			t.Errorf("the ledgers' choice %q: got %q (%v), want %q", value, b.String(), err, want)
		}
	}
	if !anyChosen([]Option{{Value: "a"}}) || anyChosen([]Option{{Value: "a", Selected: true}}) {
		t.Error("anyChosen: none chosen is any")
	}
	if a := emptyOption(false); a["selected"] != nil {
		t.Errorf("emptyOption, another chosen: got %v", a)
	}
	if (EntryForm{Lines: make([]LineInput, MaxFormLines)}).CanAdd() {
		t.Error("CanAdd: a full form takes no more lines")
	}
	var out bytes.Buffer
	if err := refusedText(FieldRefusal{Field: i18n.EntryLineSegment, Reason: "mismatch"}).Render(locale(t), &out); err != nil ||
		out.String() != "Segment: refused by the ERP (mismatch)." {
		t.Errorf("refusedText: got %q %v", out.String(), err)
	}
}
