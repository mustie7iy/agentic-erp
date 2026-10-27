package ui

import (
	"context"
	"net/url"
	"testing"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// closeComponents are the period close screens' components (T73i), with
// the variants their branches need.
func closeComponents() map[string]templ.Component {
	r := refusal()
	return map[string]templ.Component{
		"Periods":              Periods(signedIn(), periodList(true)),
		"periods plain":        periodsContent(periodList(false)),
		"periods invalid":      periodsContent(PeriodList{Invalid: true}),
		"periods refused":      periodsContent(PeriodList{Refusal: &r}),
		"periods empty":        periodsContent(PeriodList{}),
		"PeriodFilterForm":     PeriodFilterForm(PeriodFilters{Year: "26", YearError: i18n.PeriodYearInvalid}),
		"optionSelect":         optionSelect("s", FieldLedger, i18n.PeriodLedger, []Option{{Value: "l1", Text: "0L", Selected: true}}),
		"PeriodsTable":         PeriodsTable([]PeriodRow{periodRow(true), periodRow(false)}),
		"PeriodRowView":        PeriodRowView(periodRow(true)),
		"PeriodStatusTag":      PeriodStatusTag(Label{Key: i18n.PeriodStatusLocked}),
		"period status raw":    PeriodStatusTag(Label{Raw: "frozen"}),
		"Period":               Period(signedIn(), periodPage(true)),
		"period plain":         periodContent(periodPage(false)),
		"UnlockBlock":          UnlockBlock("/approvals/1"),
		"PeriodFormView":       PeriodFormView(PeriodForm{Verb: PeriodUnlock, Action: "/x", Text: "t", TextError: i18n.ActionReasonNeeded}),
		"period form lock":     PeriodFormView(PeriodForm{Verb: PeriodLock, Action: "/x"}),
		"ChecksLink":           ChecksLink("/close/checks?x=1"),
		"ChecksSection":        ChecksSection(checksView(true)),
		"checks plain":         ChecksSection(checksView(false)),
		"checks invalid":       checksBody(ChecksView{Invalid: true}),
		"ChecksPage":           ChecksPage(signedIn(), checksView(true)),
		"checksPageContent":    checksPageContent(checksView(false)),
		"GRIRCheckView":        GRIRCheckView(GRIRCheck{Refusal: &r}),
		"GRIRTable":            GRIRTable([]GRIRPosition{grirPosition()}),
		"GRIRRow":              GRIRRow(grirPosition()),
		"LedgerCheckView":      LedgerCheckView(LedgerCheck{Unknown: true}),
		"ledger check refused": LedgerCheckView(LedgerCheck{Refusal: &r}),
		"ledger check clean":   LedgerCheckView(LedgerCheck{Ledger: "0L", Compare: "2L", Trials: []TrialView{{Ledger: "0L", Balanced: true}}}),
		"TrialItem":            TrialItem(TrialView{Ledger: "0L", Balanced: true}),
		"trial unbalanced":     TrialItem(TrialView{Ledger: "2L"}),
		"DifferencesTable":     DifferencesTable([]DifferenceView{difference()}),
		"DifferenceRow":        DifferenceRow(difference()),
		"StatementCheckView":   StatementCheckView(StatementCheck{Refusal: &r}),
		"Revaluation":          Revaluation(signedIn(), revaluationPage(true)),
		"revaluation plain":    revaluationContent(revaluationPage(false)),
		"revaluation refused":  revaluationContent(RevaluationPage{RunsError: &r}),
		"RevaluationFormView":  RevaluationFormView(revaluationForm(true)),
		"revaluation form":     RevaluationFormView(revaluationForm(false)),
		"PreviewSection":       PreviewSection(previewView(true)),
		"preview plain":        PreviewSection(previewView(false)),
		"valuedLists":          valuedLists(nil, nil),
		"PostFormView":         PostFormView(PostForm{CSRF: "c", Key: "k", Company: "c1", Ledger: "l1", KeyDate: "2026-03-31"}),
		"TotalsTable":          TotalsTable([]TotalView{totalView()}),
		"TotalRow":             TotalRow(totalView()),
		"ItemsTable":           ItemsTable([]ItemView{itemView(true), itemView(false)}),
		"ItemRow":              ItemRow(itemView(true)),
		"item balance":         ItemRow(itemView(false)),
		"DirectionTag":         DirectionTag(Label{Key: i18n.DirectionGain}),
		"RevaluationCard":      RevaluationCard(revaluationRow()),
		"RevaluationStatusTag": RevaluationStatusTag(Label{Key: i18n.RevaluationPosted}),
		"RevaluationRun":       RevaluationRun(signedIn(), revaluationRunPage(true)),
		"revaluation run":      revaluationRunContent(revaluationRunPage(false)),
	}
}

// periodRow is a period of the list: a special one whose unlock is asked
// for, or a plain one.
func periodRow(full bool) PeriodRow {
	r := PeriodRow{Path: "/close/periods/1", Year: 2026, Period: 3, Start: day, End: day, Status: Label{Key: i18n.PeriodStatusOpen}, UpdatedAt: day,
		UpdatedBy: Name{Text: "Avery"}}
	if full {
		r.Period, r.Special, r.UnlockPending, r.Status = 13, true, true, Label{Key: i18n.PeriodStatusLocked}
	}
	return r
}

// periodList is the list: everything, or a page of one period.
func periodList(full bool) PeriodList {
	if !full {
		return PeriodList{Rows: []PeriodRow{periodRow(false)}}
	}
	return PeriodList{Filters: PeriodFilters{Companies: []Option{{Value: "c1", Text: "Acme", Selected: true}}, Ledgers: []Option{{Value: "l1", Text: "0L"}},
		Year: "2026"}, Rows: []PeriodRow{periodRow(true), periodRow(false)}, More: "/close?page=t", Revalue: "/close/revaluation"}
}

// periodPage is a period: with everything, or plain and closed.
func periodPage(full bool) PeriodPage {
	h := PeriodHeader{Ledger: "0L Leading ledger", Start: day, End: day, Status: Label{Key: i18n.PeriodStatusClosed}, UpdatedAt: day}
	if !full {
		return PeriodPage{Year: 2026, Period: 3, Header: h, Back: "/close"}
	}
	r := refusal()
	h.Company, h.Special, h.UpdatedBy = "Acme GmbH", true, Name{Text: "Avery"}
	return PeriodPage{Year: 2026, Period: 13, Header: h, Back: "/close", Unlock: "/approvals/1", Actions: []PeriodForm{{Verb: PeriodOpen, Action: "/x"},
		{Verb: PeriodUnlock, Action: "/y", Text: "t"}}, Why: i18n.PeriodNotYours, Checks: "/close/checks", Revalue: "/close/revaluation", Done: i18n.PeriodDoneClosed,
		Refusal: &r, Audit: "/reports/audit?object_id=1"}
}

// grirPosition is an open GR/IR position.
func grirPosition() GRIRPosition {
	return GRIRPosition{Order: "po1", Item: 1, Received: "10", Invoiced: "4", Unit: "EA", Balance: Money{Amount: "-72.00", Currency: "EUR"}, Last: day}
}

// difference is an account that differs between two ledgers.
func difference() DifferenceView {
	m := Money{Amount: "25.00", Currency: "EUR"}
	return DifferenceView{Account: "a1", Currency: "EUR", Balance: m, Compare: m, Difference: m}
}

// checksView is the checks: each with something to look at and more, or
// each clear.
func checksView(full bool) ChecksView {
	v := ChecksView{Ledgers: LedgerCheck{Single: true}, Statements: StatementCheck{Imported: "/bank?status=imported", Gap: "/bank?status=gap"}}
	if !full {
		return v
	}
	m := Money{Amount: "5.00", Currency: "EUR"}
	v.Period = "/close/periods/1"
	v.GRIR = GRIRCheck{Positions: []GRIRPosition{grirPosition()}, More: true, Report: "/reports/gr-ir"}
	v.Ledgers = LedgerCheck{Ledger: "0L", Compare: "2L", Trials: []TrialView{{Ledger: "0L", Debit: m, Credit: m, Balanced: true}, {Ledger: "2L", Debit: m, Credit: m}},
		Differences: []DifferenceView{difference(), {Account: "a2", Entries: "/reports/ledger-reconciliation/entries"}}, More: true, Report: "/reports/ledger-reconciliation"}
	v.Statements.Rows, v.Statements.More = []StatementRow{statementRow(true)}, true
	return v
}

// revaluationForm is the preview's form, with every error, or plain with
// the ledgers.
func revaluationForm(failed bool) RevaluationForm {
	f := RevaluationForm{CSRF: "c", Key: "k", Companies: []Option{{Value: "c1", Text: "Acme", Selected: true}}, KeyDate: "2026-03-31"}
	if failed {
		f.KeyDate, f.CompanyError, f.KeyDateError = "x", i18n.ProposeCompanyNeeded, i18n.ActionDateInvalid
		return f
	}
	f.Ledgers = []Option{{Value: "l1", Text: "0L", Selected: true}}
	return f
}

// totalView is a total with a loss.
func totalView() TotalView {
	m := Money{Amount: "-10.00", Currency: "EUR"}
	return TotalView{Account: "211000", Currency: "USD", Direction: Label{Key: i18n.DirectionLoss}, Items: 1, Amount: m, Booked: m, Valued: m, Difference: m,
		DifferenceGC: m}
}

// itemView is an open item at a rate, or a balance valued without one.
func itemView(item bool) ItemView {
	m := Money{Amount: "-10.00", Currency: "EUR"}
	v := ItemView{Account: "100000", Amount: m, Booked: m, Valued: m, Difference: m, DifferenceGC: m, Direction: Label{Raw: "sideways"}}
	if item {
		v.Account, v.Entry, v.Line, v.Partner, v.Rate, v.Direction = "211000", "e1", 2, "s1", "0.900000000000", Label{Key: i18n.DirectionLoss}
	}
	return v
}

// previewView is a preview: with its totals, items and post, or one that
// values items without a difference and may not be posted.
func previewView(full bool) PreviewView {
	v := PreviewView{KeyDate: day, Year: 2026, Period: 3, RateType: Label{Key: i18n.RateBankSelling}, Items: []ItemView{itemView(false)}}
	if full {
		v.Totals, v.Items, v.Post = []TotalView{totalView()}, []ItemView{itemView(true)}, &PostForm{Company: "c1", KeyDate: "2026-03-31"}
	}
	return v
}

// revaluationRow is one of the company's runs.
func revaluationRow() RevaluationRow {
	return RevaluationRow{Path: "/close/revaluation/1", KeyDate: day, Ledger: "0L Leading ledger", Status: Label{Raw: "reversed"}, Items: 2,
		CreatedBy: Name{Text: "Avery"}, CreatedAt: day}
}

// revaluationPage is the revaluation: everything, or plain without runs.
func revaluationPage(full bool) RevaluationPage {
	if !full {
		return RevaluationPage{}
	}
	r := refusal()
	return RevaluationPage{Form: new(revaluationForm(false)), Refusal: &r, Preview: new(previewView(true)), Runs: []RevaluationRow{revaluationRow()},
		RunsMore: "/close/revaluation?page=t"}
}

// revaluationRunPage is a run: posted with its entries, or one that
// posted none.
func revaluationRunPage(full bool) RevaluationRunPage {
	h := RunSummary{Ledger: "0L", KeyDate: day, RateType: Label{Raw: "spot"}, Status: Label{Key: i18n.RevaluationCancelled}, Items: 1, CreatedAt: day}
	if !full {
		return RevaluationRunPage{Header: h, Back: "/close/revaluation", Items: []ItemView{itemView(false)}}
	}
	h.Company = "Acme GmbH"
	return RevaluationRunPage{Header: h, Back: "/close/revaluation", Done: i18n.RevaluationDonePosted, Valuation: "e1", Reset: "e2",
		Totals: []TotalView{totalView()}, Items: []ItemView{itemView(true)}}
}

// TestCloseHelpers: each status and direction has its colour, the unlock
// alone needs a reason, the paths drop empty values, a decimal outside a
// request is as the API gives it, and the trial balances and checks say
// their result.
func TestCloseHelpers(t *testing.T) {
	for name, tc := range map[string]struct {
		kind func(Label) string
		l    Label
		want string
	}{
		"period open":           {periodStatusKind, Label{Key: i18n.PeriodStatusOpen}, "success"},
		"period closed":         {periodStatusKind, Label{Key: i18n.PeriodStatusClosed}, "warning"},
		"period locked":         {periodStatusKind, Label{Key: i18n.PeriodStatusLocked}, "inactive"},
		"period raw":            {periodStatusKind, Label{Raw: "x"}, ""},
		"revaluation posted":    {revaluationStatusKind, Label{Key: i18n.RevaluationPosted}, "success"},
		"revaluation cancelled": {revaluationStatusKind, Label{Key: i18n.RevaluationCancelled}, "inactive"},
		"revaluation raw":       {revaluationStatusKind, Label{Raw: "x"}, ""},
		"gain":                  {directionKind, Label{Key: i18n.DirectionGain}, "success"},
		"loss":                  {directionKind, Label{Key: i18n.DirectionLoss}, "warning"},
		"none":                  {directionKind, Label{Key: i18n.DirectionNone}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.kind(tc.l); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if !PeriodRequires(PeriodUnlock) || PeriodRequires(PeriodLock) {
		t.Error("PeriodRequires: an unlock needs its reason, a lock none")
	}
	if got := PeriodsPath(url.Values{"fiscal_year": {""}, "ledger_id": {"l1"}}); got != "/close?ledger_id=l1" {
		t.Errorf("PeriodsPath: got %q", got)
	}
	if got := RevaluationFor(url.Values{"key_date": {""}}); got != RevaluationPath {
		t.Errorf("RevaluationFor, nothing chosen: got %q", got)
	}
	if got := decimal(context.Background(), "1234.5"); got != "1234.5" {
		t.Errorf("decimal without a locale: got %q", got)
	}
	if balanced([]TrialView{{Balanced: true}, {}}) || !balanced([]TrialView{{Balanced: true}}) {
		t.Error("balanced: every ledger's trial balance must net to zero")
	}
	if got := yearText(2026); got != "2026" {
		t.Errorf("yearText: got %q", got)
	}
}
