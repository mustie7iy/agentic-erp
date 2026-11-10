package integration

import "github.com/mustie7iy/agentic-erp/api/gen"

// Report is a migration run's report as it is stored with the run, and as
// the contract's MigrationRunReport declares it (reportToAPI;
// TestReportMatchesContract holds the two to the same keys, the extract's
// tables named in lower case in the contract, extractToAPI). The extract's
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
	Segments               int `json:"segments,omitempty"`
	ProfitCenters          int `json:"profit_centers,omitempty"`
	ProfitCentersExtended  int `json:"profit_centers_extended,omitempty"`
	CostCenters            int `json:"cost_centers,omitempty"`
	Hierarchies            int `json:"hierarchies,omitempty"`
	HierarchyNodes         int `json:"hierarchy_nodes,omitempty"`
	HierarchyPlaces        int `json:"hierarchy_places,omitempty"`
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

// extractToAPI names each table sap.Extract.Counts counts. The stored
// report keys a table by SAP's name (T001, AGR_DEFINE), as reports were
// written before the contract declared them; the contract's JSON names are
// snake_case (04 API style), so it keys each by that name in lower case
// (t001, agr_define), and the conversion maps between them rather than a
// data migration rewriting stored reports. A table a stored report
// predates reads as no rows.
func extractToAPI(c map[string]int) gen.MigrationExtractCounts {
	return gen.MigrationExtractCounts{T001: c["T001"], Skb1: c["SKB1"], Lfa1: c["LFA1"], Adrc: c["ADRC"], Lfb1: c["LFB1"], Lfbk: c["LFBK"], Ekko: c["EKKO"],
		Ekpo: c["EKPO"], Mseg: c["MSEG"], Rbkp: c["RBKP"], Rseg: c["RSEG"], Acdoca: c["ACDOCA"], T052: c["T052"], T007a: c["T007A"],
		T030: c["T030"], T169g: c["T169G"], T012k: c["T012K"], T074: c["T074"], AgrDefine: c["AGR_DEFINE"], AgrTcodes: c["AGR_TCODES"],
		AgrUsers: c["AGR_USERS"], Csks: c["CSKS"], Cskt: c["CSKT"], Cepc: c["CEPC"], Cepct: c["CEPCT"], CepcBukrs: c["CEPC_BUKRS"],
		FaglSegm: c["FAGL_SEGM"], FaglSegmt: c["FAGL_SEGMT"], Tka01: c["TKA01"], Setheader: c["SETHEADER"], Setheadert: c["SETHEADERT"],
		Setnode: c["SETNODE"], Setleaf: c["SETLEAF"]}
}

func loadedToAPI(l LoadedCounts) gen.MigrationLoadedCounts {
	return gen.MigrationLoadedCounts{Suppliers: count(l.Suppliers), SuppliersExtended: count(l.SuppliersExtended),
		SuppliersBlocked: count(l.SuppliersBlocked), PurchaseOrders: count(l.PurchaseOrders), GoodsReceipts: count(l.GoodsReceipts),
		SupplierInvoices: count(l.SupplierInvoices), PostedInvoicesInvoiced: count(l.PostedInvoicesInvoiced),
		JournalEntries: count(l.JournalEntries), DownPayments: count(l.DownPayments), Clearings: count(l.Clearings),
		GrIrPositions: count(l.GRIRPositions), GrIrTopUps: count(l.GRIRTopUps), Segments: count(l.Segments), ProfitCenters: count(l.ProfitCenters),
		ProfitCentersExtended: count(l.ProfitCentersExtended), CostCenters: count(l.CostCenters), Hierarchies: count(l.Hierarchies),
		HierarchyNodes: count(l.HierarchyNodes), HierarchyPlaces: count(l.HierarchyPlaces)}
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
