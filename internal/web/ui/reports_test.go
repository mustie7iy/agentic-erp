package ui

import (
	"net/url"
	"testing"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// reportComponents are the reports' components (T73j), with the variants
// their branches need.
func reportComponents() map[string]templ.Component {
	r := refusal()
	m := Money{Amount: "-119.00", Currency: "EUR"}
	filters := LedgerFilters{Action: TrialBalancePath, Companies: []Option{{Value: "c1", Text: "Acme GmbH", Selected: true}},
		Ledgers: []Option{{Value: "l1", Text: "0L"}}, Compare: []Option{{Value: "l2", Text: "2L"}}, Year: "26", YearError: i18n.PeriodYearInvalid,
		Period: "17", PeriodError: i18n.ReportPeriodInvalid}
	items := ItemFilters{Companies: []Option{{Value: "c1", Text: "Acme GmbH"}}, Accounts: []Option{{Value: "a1", Text: "211000"}}, Supplier: "BP1",
		SupplierError: i18n.OpenItemsSupplierUnknown, Suppliers: true, KeyDate: "2026-04-15"}
	return map[string]templ.Component{
		"Reports":             Reports(signedIn(), ReportLinks{TrialBalance: "/t", OpenItems: "/o", GRIR: "/g", LedgerRecon: "/l", Banks: []HouseBank{{Code: "MAIN"}}, Audit: true}),
		"reports unread":      reportsContent(ReportLinks{BanksUnread: true}),
		"reports none":        reportsContent(ReportLinks{}),
		"DownloadLink":        DownloadLink("/x.csv"),
		"LedgerFilterForm":    LedgerFilterForm(filters),
		"ledger filters bare": LedgerFilterForm(LedgerFilters{Action: LedgerReconPath}),
		"TrialBalance": TrialBalance(signedIn(), TrialBalancePage{Filters: filters, Rows: []BalanceRow{{Account: "211000", Period: 3, Currency: "EUR",
			Debit: m, Credit: m, DebitLC: m, CreditLC: m}}, More: "/more", CSV: "/x.csv"}),
		"trial balance plain":   trialBalanceContent(TrialBalancePage{Rows: []BalanceRow{{Account: "a"}}}),
		"trial balance invalid": trialBalanceContent(TrialBalancePage{Invalid: true}),
		"trial balance refused": trialBalanceContent(TrialBalancePage{Refusal: &r}),
		"trial balance empty":   trialBalanceContent(TrialBalancePage{}),
		"OpenItems": OpenItems(signedIn(), OpenItemsPage{Filters: items, Supplier: "BP1 Supplier", Aging: []AgingRow{
			{Bucket: Label{Key: i18n.AgingNotDue}, Currency: "EUR", Items: 1, Amount: m}, {Bucket: Label{Key: i18n.AgingOverdue}, From: 1, To: 30, Items: 2},
			{Bucket: Label{Key: i18n.AgingOverdue}, From: 91, Items: 3}, {Bucket: Label{Key: i18n.AgingNoDue}}, {Bucket: Label{Raw: "disputed"}}},
			Rows: []OpenItemRow{{Entry: "/journal/entries/1", Number: "5100000123", Partner: "BP1", Reference: "INV-1", Posting: day, Due: &day, Discount: &day,
				Overdue: 6, DownPay: true, Amount: m, AmountLC: m}, {Number: "2"}}, More: "/more", CSV: "/x.csv"}),
		"open items invalid": openItemsContent(OpenItemsPage{Invalid: true}),
		"open items refused": openItemsContent(OpenItemsPage{Refusal: &r}),
		"open items empty":   openItemsContent(OpenItemsPage{}),
		"open items plain":   openItemsContent(OpenItemsPage{Rows: []OpenItemRow{{Number: "1"}}}),
		"GRIRReport":         GRIRReport(signedIn(), GRIRPage{Companies: []Option{{Value: "c1"}}, All: true, Positions: []GRIRPosition{grirPosition()}, More: "/m", CSV: "/x.csv"}),
		"grir invalid":       grirContent(GRIRPage{Invalid: true}),
		"grir refused":       grirContent(GRIRPage{Refusal: &r}),
		"grir empty":         grirContent(GRIRPage{}),
		"grir plain":         grirContent(GRIRPage{Positions: []GRIRPosition{grirPosition()}}),
		"LedgerRecon": LedgerRecon(signedIn(), LedgerReconPage{Filters: filters, Ledger: "0L", Compare: "2L", Trials: []TrialView{{Ledger: "0L", Balanced: true}},
			Differences: []DifferenceView{difference()}, More: "/m"}),
		"recon invalid": ledgerReconContent(LedgerReconPage{Invalid: true}),
		"recon unknown": ledgerReconContent(LedgerReconPage{Unknown: true}),
		"recon single":  ledgerReconContent(LedgerReconPage{Single: true}),
		"recon refused": ledgerReconContent(LedgerReconPage{Refusal: &r}),
		"recon clean":   ledgerReconContent(LedgerReconPage{Ledger: "0L", Compare: "2L"}),
		"recon plain":   ledgerReconContent(LedgerReconPage{Differences: []DifferenceView{difference()}}),
		"ReconEntries": ReconEntries(signedIn(), ReconEntriesPage{Back: "/b", Account: "100000", Currency: "EUR", Ledger: "0L", Compare: "2L",
			Rows: []ReconEntryRow{{Ledger: "2L", Entry: "/journal/entries/1", Number: "100042", Line: 1, Posting: day, Period: 3, Amount: m, AmountLC: m}}, More: "/m"}),
		"recon entries invalid": reconEntriesContent(ReconEntriesPage{Invalid: true}),
		"recon entries refused": reconEntriesContent(ReconEntriesPage{Refusal: &r}),
		"recon entries empty":   reconEntriesContent(ReconEntriesPage{}),
		"recon entries plain":   reconEntriesContent(ReconEntriesPage{Rows: []ReconEntryRow{{Number: "1"}}}),
		"AuditForm":             AuditForm("x", i18n.AuditObjectInvalid),
		"Audit": Audit(signedIn(), AuditPage{Object: "1", Rows: []AuditRow{{Seq: 1, At: day, Actor: Name{Text: "Avery"}, Kind: Label{Key: i18n.AuditKindHuman},
			Action: "ledger.journal_entry.post", Outcome: "succeeded", Address: "203.0.113.24", Details: []AuditDetail{{Name: "lines", Value: "2"}}},
			{Action: "x.y"}}, More: "/m"}),
		"audit refused":        auditContent(AuditPage{Object: "1", Refusal: &r}),
		"audit empty":          auditContent(AuditPage{Object: "1"}),
		"audit form alone":     auditContent(AuditPage{}),
		"audit invalid":        auditContent(AuditPage{Object: "x", ObjectError: i18n.AuditObjectInvalid}),
		"AuditLinkView":        AuditLinkView("/reports/audit?object_id=1"),
		"Tally":                Tally(10000),
		"checkReport":          checkReport("/reports/gr-ir"),
		"reportItem":           reportItem("/t", i18n.TrialBalanceTitle, i18n.ReportsTrialBalanceLead),
		"reportBack":           reportBack(),
		"BalancesTable":        BalancesTable([]BalanceRow{{Account: "a"}}),
		"BalanceRowView":       BalanceRowView(BalanceRow{Account: "a", Period: 3}),
		"OpenItemFilterForm":   OpenItemFilterForm(items),
		"item filters refused": OpenItemFilterForm(ItemFilters{Supplier: "BP1", SupplierError: i18n.EntryLookupRefused}),
		"item filters bare":    OpenItemFilterForm(ItemFilters{}),
		"AgingTable":           AgingTable([]AgingRow{{Bucket: Label{Key: i18n.AgingNotDue}}}),
		"AgingRowView":         AgingRowView(AgingRow{Bucket: Label{Key: i18n.AgingOverdue}, From: 1, To: 30}),
		"OpenItemsTable":       OpenItemsTable([]OpenItemRow{{Number: "1"}}),
		"OpenItemRowView":      OpenItemRowView(OpenItemRow{Number: "1", Due: &day, Overdue: 2}),
		"ReconEntriesTable":    ReconEntriesTable([]ReconEntryRow{{Number: "1"}}),
		"ReconEntryRowView":    ReconEntryRowView(ReconEntryRow{Number: "1"}),
		"AuditCard":            AuditCard(AuditRow{Action: "x.y", Address: "203.0.113.24", Details: []AuditDetail{{Name: "n", Value: "v"}}}),
	}
}

// TestReportFor: a report's address holds the query's values, those
// left empty out.
func TestReportFor(t *testing.T) {
	if got := ReportFor(GRIRPath, url.Values{FieldCompany: {"c1"}, FieldAllGRIR: {""}}); got != "/reports/gr-ir?legal_entity_id=c1" {
		t.Errorf("got %q", got)
	}
}

// TestAgingKind: each bucket's colour, none for one the catalog does not
// name.
func TestAgingKind(t *testing.T) {
	for k, want := range map[i18n.Key]string{i18n.AgingNotDue: "success", i18n.AgingOverdue: "warning", i18n.AgingNoDue: "", "": ""} {
		if got := agingKind(Label{Key: k}); got != want {
			t.Errorf("agingKind(%q): got %q, want %q", k, got, want)
		}
	}
}
