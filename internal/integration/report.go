package integration

import "github.com/mustie7iy/agentic-erp/api/gen"

// Report is a migration run's report as it is stored with the run, and as
// the contract's MigrationRunReport declares it (reportToAPI;
// TestReportMatchesContract holds the two to the same keys). The extract's
// counts and the quality gate's findings are written when the run starts;
// the load's part once the gate passed, and the reconciliation's once the
// run is reconciled. Stored reports are decoded leniently (encoding/json),
// so a report written before a key existed still reads, without it.
type Report struct {
	Extract map[string]int `json:"extract"` // rows per SAP table (sap.Extract.Counts)
	Quality []Issue        `json:"quality"`
	*LoadResult
	*Reconciliation
}

// LoadResult is the load's part of a report: the rows it refused or noted,
// and what it created.
type LoadResult struct {
	Refusals []Issue      `json:"load"`
	Loaded   LoadedCounts `json:"loaded"`
}

// LoadedCounts counts what a run's load created, by kind. A kind it created none
// of is left out of the report, as the contract says; the loader names
// every field (TestEveryReportFieldIsWritten).
type LoadedCounts struct {
	Suppliers              int `json:"suppliers,omitempty"`
	SuppliersExtended      int `json:"suppliers_extended,omitempty"`
	SuppliersBlocked       int `json:"suppliers_blocked,omitempty"`
	PurchaseOrders         int `json:"purchase_orders,omitempty"`
	GoodsReceipts          int `json:"goods_receipts,omitempty"`
	SupplierInvoices       int `json:"supplier_invoices,omitempty"`
	PostedInvoicesInvoiced int `json:"posted_invoices_invoiced,omitempty"`
	JournalEntries         int `json:"journal_entries,omitempty"`
	DownPayments           int `json:"down_payments,omitempty"`
	Clearings              int `json:"clearings,omitempty"`
	GRIRPositions          int `json:"gr_ir_positions,omitempty"`
	GRIRTopUps             int `json:"gr_ir_top_ups,omitempty"`
}

// reportToAPI is the report as the contract declares it. The lists of a
// part that ran are present even when empty; the reconciliation's missing
// documents and issues only when it has some, as they are stored.
func reportToAPI(r Report) gen.MigrationRunReport {
	out := gen.MigrationRunReport{Extract: extractToAPI(r.Extract), Quality: listOf(r.Quality, issueToAPI)}
	if r.LoadResult != nil {
		out.Load = listOf(r.Refusals, issueToAPI)
		out.Loaded.SetTo(loadedToAPI(r.Loaded))
	}
	if r.Reconciliation != nil {
		out.Checks = listOf(r.Checks, func(c Check) gen.MigrationCheck {
			return gen.MigrationCheck{CompanyCode: c.CompanyCode, Name: c.Name, Sap: c.SAP, Platform: c.Platform, Pass: c.Pass}
		})
		out.Diffs = listOf(r.Diffs, func(d Diff) gen.MigrationDiff {
			return gen.MigrationDiff{CompanyCode: d.CompanyCode, Key: d.Key, Sap: d.SAP, Platform: d.Platform}
		})
		out.MissingDocuments = r.Missing
		if r.Issues != nil {
			out.Issues = listOf(r.Issues, issueToAPI)
		}
		out.Passed.SetTo(r.Passed)
	}
	return out
}

// extractToAPI names each table sap.Extract.Counts counts; a table a stored
// report predates reads as no rows.
func extractToAPI(c map[string]int) gen.MigrationExtractCounts {
	return gen.MigrationExtractCounts{T001: c["T001"], SKB1: c["SKB1"], LFA1: c["LFA1"], LFB1: c["LFB1"], LFBK: c["LFBK"], EKKO: c["EKKO"],
		EKPO: c["EKPO"], MSEG: c["MSEG"], RBKP: c["RBKP"], RSEG: c["RSEG"], ACDOCA: c["ACDOCA"], T052: c["T052"], T007A: c["T007A"],
		T030: c["T030"], T169G: c["T169G"], T012K: c["T012K"], T074: c["T074"], AGRDEFINE: c["AGR_DEFINE"], AGRTCODES: c["AGR_TCODES"],
		AGRUSERS: c["AGR_USERS"]}
}

func loadedToAPI(l LoadedCounts) gen.MigrationLoadedCounts {
	return gen.MigrationLoadedCounts{Suppliers: count(l.Suppliers), SuppliersExtended: count(l.SuppliersExtended),
		SuppliersBlocked: count(l.SuppliersBlocked), PurchaseOrders: count(l.PurchaseOrders), GoodsReceipts: count(l.GoodsReceipts),
		SupplierInvoices: count(l.SupplierInvoices), PostedInvoicesInvoiced: count(l.PostedInvoicesInvoiced),
		JournalEntries: count(l.JournalEntries), DownPayments: count(l.DownPayments), Clearings: count(l.Clearings),
		GrIrPositions: count(l.GRIRPositions), GrIrTopUps: count(l.GRIRTopUps)}
}

// count is a loaded count as the contract has it: absent when nothing of
// the kind was created.
func count(n int) gen.OptInt {
	if n == 0 {
		return gen.OptInt{}
	}
	return gen.NewOptInt(n)
}

func issueToAPI(i Issue) gen.MigrationIssue {
	return gen.MigrationIssue{Severity: i.Severity, Table: i.Table, Key: i.Key, Message: i.Message}
}

// listOf converts a list, empty rather than nil when there is nothing in it.
func listOf[T, A any](in []T, toAPI func(T) A) []A {
	out := make([]A, 0, len(in))
	for _, v := range in {
		out = append(out, toAPI(v))
	}
	return out
}
