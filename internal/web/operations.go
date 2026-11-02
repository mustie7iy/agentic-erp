package web

import "github.com/mustie7iy/agentic-erp/internal/web/ui"

// The workspace's inventory of the API operations its screens call
// (ADR-0052 §10, contract drift). The ogen client makes a call that no
// longer fits the contract fail the build; TestScreensAgainstTheContract
// holds this inventory to the contract besides: every operation exists,
// the permission a screen hides an action by is that operation's
// x-permission, an x-agent: false operation is called from a route marked
// human-only, the approval permissions are the decisions' own
// x-object-permissions, and every problem code the catalog explains is one
// these operations can answer.

// The permissions screens hide by (advisory, ADR-0052 §7): each the
// x-permission of the operation it stands for.
const (
	permReadApprovals         = "workflow.request.read"
	permDecide                = "workflow.request.decide"
	permReadInvoices          = "payables.invoice.read"
	permCaptureInvoices       = "payables.invoice.capture"
	permReadRecommendations   = "agents.session.read"
	permDecideRecommendations = "agents.recommendation.decide"
	permReadOrders            = "procurement.order.read"
	permReadReceipts          = "procurement.receipt.read"
	permReadCompanies         = "tenancy.org.read"
	permReadStatements        = "payments.statement.read"
	permImportStatements      = "payments.statement.import"
	permMatchLines            = "payments.statement.match"
	permPostLines             = "payments.statement.post"
	permReadRuns              = "payments.run.read"
	permProposeRuns           = "payments.run.propose"
	permReleaseRuns           = "payments.run.release"
	permTransmitFiles         = "payments.file.transmit"
	permReadOperations        = "platform.operation.read"
	permCancelOperations      = "platform.operation.cancel"
	permReadPeriods           = "ledger.period.read"
	permManagePeriods         = "ledger.period.manage"
	permLockPeriods           = "ledger.period.lock"
	permReadAccounts          = "ledger.account.read"
	permReadEntries           = "ledger.entry.read"
	permPostEntries           = "ledger.entry.post"
	permParkEntries           = "ledger.entry.park"
	permReadCenters           = "ledger.cost_center.read"
	permCancelRequests        = "workflow.request.cancel"
	permReadAudit             = "audit.event.read"
	permReadSuppliers         = "partner.supplier.read"
)

// objectApprovers are the decisions' x-object-permissions: the permission
// to decide a request on each kind of object, besides permDecide and the
// step's role.
var objectApprovers = map[string]string{
	"requisition": "procurement.requisition.approve", "partner_change": "partner.change.approve",
	"invoice_exception": "payables.invoice.approve", "config_change": "config.change.approve",
	"period_unlock": "ledger.period.unlock.approve", "manual_entry": "ledger.entry.approve",
	"purchase_order": "procurement.order.approve", "order_change": "procurement.order.approve",
	"bank_clearing": "payments.statement.clearing.approve", "center_change": "ledger.cost_center.approve",
	"payment_run_release": "payments.run.release", "gr_ir_clearing": "procurement.gr_ir.clear.approve",
	"tax_return": "tax.return.approve",
}

// Operation is one API operation a route calls: its operationId, the
// permission the screen hides it by ("" when it hides nothing by one),
// whether the route is human-only, as an x-agent: false operation needs
// (the workspace signs people in, never agents), and whether it is a
// lookup: a code a form names turned into its id before the form's write,
// part of the write and not one of the page's reads (T73h).
type Operation struct {
	ID        string
	HiddenBy  string
	HumanOnly bool
	Lookup    bool
	Export    bool // read page by page for a download, up to maxExportPages times (T73j)
	// PerObject marks the read of one type of a request's object (T73d2): a
	// request's page calls the one of its object's type, one read
	// whichever it is.
	PerObject bool
	Write     bool // a form's write besides its first: a new entry posted at once is parked above its limit (T73h2)
}

var (
	opMe        = Operation{ID: "getCurrentPrincipal"}
	opNames     = Operation{ID: "listPrincipalNames"}
	opGet       = Operation{ID: "getApprovalRequest", HiddenBy: permReadApprovals}
	opCompanies = Operation{ID: "listLegalEntities", HiddenBy: permReadCompanies}
	// opToday is a company's date today, the default of a form's dates and
	// a report's year or key date (today.go).
	opToday = Operation{ID: "getLegalEntityToday", HiddenBy: permReadCompanies}
	// opInvoice are the reads of an invoice's page (invoice.go).
	opInvoice = []Operation{opMe, {ID: "getSupplierInvoice", HiddenBy: permReadInvoices}, {ID: "listMatchResults", HiddenBy: permReadInvoices},
		{ID: "listRecommendations", HiddenBy: permReadRecommendations}, opNames, opCompanies}
	// opInvoiceForm are the reads of a parked invoice's change form
	// (invoice_edit.go): its lines name accounts and centers by their codes
	// (invoiceCodes), and sending it looks codes up besides (opLookups).
	opInvoiceForm = []Operation{opMe, {ID: "getSupplierInvoice", HiddenBy: permReadInvoices}, opAccounts,
		{ID: "listCostCenters", HiddenBy: permReadCenters}, {ID: "listProfitCenters", HiddenBy: permReadCenters}}
	opHouseBanks = Operation{ID: "listHouseBanks", HiddenBy: permReadStatements}
	opOperation  = Operation{ID: "getOperation", HiddenBy: permReadOperations}
	// opStatementList are the reads of the statements' list (bank.go),
	// opStatement those of a statement's page (statement.go).
	opStatementList = []Operation{opMe, opHouseBanks, opCompanies, {ID: "listBankStatements", HiddenBy: permReadStatements}}
	opStatement     = []Operation{opMe, {ID: "getBankStatement", HiddenBy: permReadStatements},
		{ID: "listBankStatementMatches", HiddenBy: permReadStatements}, {ID: "getBankStatementRules", HiddenBy: permReadStatements}, opHouseBanks, opNames}
	// opRuns are the reads of the payment runs' list (payments.go), opRun
	// those of a run's page (payment_run.go), opFiles those of its bank
	// files' section and opProgress those of its operation's progress
	// (payment_progress.go).
	opRuns   = []Operation{opMe, opCompanies, opToday, opHouseBanks, {ID: "listPaymentRuns", HiddenBy: permReadRuns}, opNames}
	opGetRun = Operation{ID: "getPaymentRun", HiddenBy: permReadRuns}
	opRun    = []Operation{opMe, opGetRun, {ID: "listPayments", HiddenBy: permReadRuns}, {ID: "listPaymentExclusions", HiddenBy: permReadRuns},
		opCompanies, opNames}
	opFiles    = []Operation{opMe, {ID: "listBankFiles", HiddenBy: permReadRuns}}
	opProgress = []Operation{opMe, opGetRun, {ID: "getOperation", HiddenBy: permReadOperations}}
	// opPeriods are the reads of the periods' list (close.go), opPeriod
	// those of a period's page (period.go) and opChecks those of the
	// pre-close checks (close_checks.go), where the statements not
	// reconciled are read once per status.
	opLedgers    = Operation{ID: "listLedgers", HiddenBy: permReadAccounts}
	opPeriods    = []Operation{opMe, opCompanies, opToday, opLedgers, {ID: "listFiscalPeriods", HiddenBy: permReadPeriods}, opNames}
	opPeriod     = []Operation{opMe, {ID: "getFiscalPeriod", HiddenBy: permReadPeriods}, opCompanies, opLedgers, opNames}
	opStatements = Operation{ID: "listBankStatements", HiddenBy: permReadStatements}
	opChecks     = []Operation{opMe, opLedgers, {ID: "listGrIrBalances", HiddenBy: permReadEntries},
		{ID: "listLedgerReconciliation", HiddenBy: permReadEntries}, opStatements, opStatements}
	// opRevaluation are the reads of the revaluation's page (revaluation.go),
	// opRevaluationRun those of a run's page; a preview's page names the
	// accounts besides.
	opAccounts       = Operation{ID: "listAccounts", HiddenBy: permReadAccounts}
	opRevaluation    = []Operation{opMe, opCompanies, opLedgers, {ID: "listRevaluationRuns", HiddenBy: permReadEntries}, opNames}
	opRevaluationRun = []Operation{opMe, {ID: "getRevaluationRun", HiddenBy: permReadEntries}, opCompanies, opLedgers, opAccounts, opNames}
	// opParkedList are the reads of the parked entries' list (journal.go),
	// opJournal those of the journal (journal_entries.go) and opEntry
	// those of a posted entry's page; opEntryForm those of a new entry's
	// form and opChangeForm a parked one's change (entry_form.go);
	// opParkedEntry those of a parked entry's page (parked.go). Sending a
	// form looks its codes up besides (opLookups, entry_lookup.go).
	opParkedList  = []Operation{opMe, opCompanies, {ID: "listParkedJournalEntries", HiddenBy: permReadEntries}, opNames}
	opJournal     = []Operation{opMe, opCompanies, opLedgers, {ID: "listCostCenters", HiddenBy: permReadCenters}, {ID: "listProfitCenters", HiddenBy: permReadCenters}, {ID: "listJournalEntries", HiddenBy: permReadEntries}}
	opEntry       = []Operation{opMe, {ID: "getJournalEntry", HiddenBy: permReadEntries}, opCompanies, opLedgers, opAccounts, opNames}
	opGetParked   = Operation{ID: "getParkedJournalEntry", HiddenBy: permReadEntries}
	opEntryForm   = []Operation{opMe, opCompanies, opToday, opAccounts, opLedgers, {ID: "listLedgerGroups", HiddenBy: permReadAccounts}}
	opChangeForm  = []Operation{opMe, opGetParked, opCompanies, opAccounts, {ID: "listBusinessPartners", HiddenBy: permReadSuppliers}}
	opParkEntry   = Operation{ID: "parkJournalEntry", HiddenBy: permParkEntries}
	opParkedEntry = []Operation{opMe, opGetParked, opCompanies, opAccounts, opNames, opGet}
	opLookups     = []Operation{{ID: "listAccounts", Lookup: true}, {ID: "listCostCenters", Lookup: true}, {ID: "listProfitCenters", Lookup: true},
		{ID: "listSegments", Lookup: true}, {ID: "listBusinessPartners", Lookup: true}}
	opUpdateParked = Operation{ID: "updateParkedJournalEntry", HiddenBy: permParkEntries}
	// opBalances are the reads of the trial balance (report_ledgers.go),
	// opReconciliation those of the ledger reconciliation and
	// opReconEntries those of the entries behind a difference; opOpenItems
	// those of the supplier open items and opGRIR those of the GR/IR
	// positions (report_items.go). A download reads its report's pages,
	// up to maxExportPages of them (paged, report_csv.go).
	opListBalances = Operation{ID: "listBalances", HiddenBy: permReadEntries}
	opListItems    = Operation{ID: "listOpenItems", HiddenBy: permReadEntries}
	opListGRIR     = Operation{ID: "listGrIrBalances", HiddenBy: permReadEntries}
	opBalances     = []Operation{opMe, opCompanies, opToday, opLedgers, opAccounts, opListBalances}
	opSupplier     = Operation{ID: "listBusinessPartners", HiddenBy: permReadSuppliers}
	opOpenItems    = []Operation{opMe, opCompanies, opToday, opAccounts, opSupplier, opListItems}
	opGRIR         = []Operation{opMe, opCompanies, opListGRIR}
	opListRecon    = Operation{ID: "listLedgerReconciliation", HiddenBy: permReadEntries}
	opReconcile    = []Operation{opMe, opCompanies, opToday, opLedgers, opListRecon}
	opReconEntries = []Operation{opMe, opToday, opLedgers, opAccounts, {ID: "listLedgerReconciliationEntries", HiddenBy: permReadEntries}}
	// opAccountEntries are the reads of a trial balance account's entries
	// (report_entries.go, T73j2).
	opAccountEntries = []Operation{opMe, opToday, opLedgers, opAccounts, {ID: "listJournalEntries", HiddenBy: permReadEntries}}
	// opListAudit is a document's audit history's read (reports.go); its
	// download names the history's actors in one read (T73j4).
	opListAudit = Operation{ID: "listAuditEvents", HiddenBy: permReadAudit}
)

// paged is the operation a download reads page by page.
func paged(op Operation) Operation {
	op.Export = true
	return op
}

// opRequest are the reads of a request's page (approvals.go): the request,
// its people's names and its object, read by its type's operation
// (approval_object.go).
var opRequest = append([]Operation{opMe, opGet, opNames}, objectOps()...)

// decideOps are the operations of the decisions on an approval request, by
// verb, each hidden by its x-permission: its approval and rejection, which
// only people make, and the requester's withdrawal.
var decideOps = map[string]Operation{
	ui.VerbApprove:  {ID: "approveApprovalRequest", HiddenBy: permDecide, HumanOnly: true},
	ui.VerbReject:   {ID: "rejectApprovalRequest", HiddenBy: permDecide, HumanOnly: true},
	ui.VerbWithdraw: {ID: "cancelApprovalRequest", HiddenBy: permCancelRequests},
}

// parkedOps are the operations of the decisions on a parked entry, by
// verb, each hidden by its x-permission: its post, which only people make
// (T35), its cancellation, and the withdrawal of its approval request.
var parkedOps = map[string]Operation{
	ui.ParkedPost:     {ID: "postParkedJournalEntry", HiddenBy: permPostEntries, HumanOnly: true},
	ui.ParkedCancel:   {ID: "cancelParkedJournalEntry", HiddenBy: permParkEntries},
	ui.ParkedWithdraw: {ID: "cancelApprovalRequest", HiddenBy: permCancelRequests},
}

// periodOps are the operations of the decisions on a period, by verb,
// each hidden by its x-permission: opening and closing it, and its lock
// and the request of its unlock, which are people's alone (T32).
var periodOps = map[string]Operation{
	ui.PeriodOpen:   {ID: "openFiscalPeriod", HiddenBy: permManagePeriods},
	ui.PeriodClose:  {ID: "closeFiscalPeriod", HiddenBy: permManagePeriods},
	ui.PeriodLock:   {ID: "lockFiscalPeriod", HiddenBy: permLockPeriods, HumanOnly: true},
	ui.PeriodUnlock: {ID: "unlockFiscalPeriod", HiddenBy: permLockPeriods, HumanOnly: true},
}

// runOps are the operations of the decisions on a payment run, by verb,
// each hidden by its x-permission: its release (and the resumption of a
// release that stopped), its rejection and the request for a second
// approval are people's alone, a bank file's transmission is not.
var runOps = map[string]Operation{
	ui.RunRelease:  {ID: "releasePaymentRun", HiddenBy: permReleaseRuns, HumanOnly: true},
	ui.RunReject:   {ID: "rejectPaymentRun", HiddenBy: permReleaseRuns, HumanOnly: true},
	ui.RunSubmit:   {ID: "submitPaymentRun", HiddenBy: permReleaseRuns, HumanOnly: true},
	ui.RunTransmit: {ID: "transmitBankFile", HiddenBy: permTransmitFiles},
}

// lineOps are the operations of the decisions on a statement's lines, by
// verb, each hidden by its x-permission; all are people's alone.
var lineOps = map[string]Operation{
	ui.LineMatch:   {ID: "matchBankStatementLine", HiddenBy: permMatchLines, HumanOnly: true},
	ui.LineExclude: {ID: "excludeBankStatementLine", HiddenBy: permMatchLines, HumanOnly: true},
	ui.LinePost:    {ID: "postBankStatementLine", HiddenBy: permPostLines, HumanOnly: true},
	ui.LineAccept:  {ID: "acceptBankStatementLineProposal", HiddenBy: permMatchLines, HumanOnly: true},
	ui.LineReject:  {ID: "rejectBankStatementLineProposal", HiddenBy: permMatchLines, HumanOnly: true},
}

// actionOps are the operations of the actions on an invoice, by verb,
// each hidden by its x-permission.
var actionOps = map[string]Operation{
	ui.ActMatch:   {ID: "matchSupplierInvoice", HiddenBy: "payables.invoice.capture"},
	ui.ActPost:    {ID: "postSupplierInvoice", HiddenBy: "payables.invoice.post"},
	ui.ActHold:    {ID: "holdSupplierInvoice", HiddenBy: "payables.invoice.hold"},
	ui.ActRelease: {ID: "releaseSupplierInvoice", HiddenBy: "payables.invoice.review"},
	ui.ActCancel:  {ID: "cancelSupplierInvoice", HiddenBy: "payables.invoice.capture"},
	ui.ActReverse: {ID: "reverseSupplierInvoice", HiddenBy: "payables.invoice.post"},
}

// Screens are the operations each route calls, by its pattern.
var Screens = func() map[string][]Operation {
	screens := map[string][]Operation{
		"GET /{$}":            {opMe, {ID: "listLegalEntities"}},
		"POST /logout":        {{ID: "endCurrentSession", HumanOnly: true}},
		"GET /approvals":      {opMe, {ID: "listApprovalRequests", HiddenBy: permReadApprovals}, opNames},
		"GET /approvals/{id}": opRequest,
		"GET /invoices":       {opMe, opCompanies, {ID: "listSupplierInvoices", HiddenBy: permReadInvoices}, opNames},
		"GET /invoices/{id}":  opInvoice,
		// The capture's form, and its operation's progress (?operation=).
		"GET /invoices/capture": {opMe, opCompanies, opOperation},
		"POST /invoices/capture": append([]Operation{{ID: "createUpload", HiddenBy: permCaptureInvoices},
			{ID: "captureSupplierInvoice", HiddenBy: permCaptureInvoices, Write: true}}, opMe, opCompanies, Operation{ID: "getUpload", HiddenBy: permCaptureInvoices}),
		"GET /invoices/{id}/order": {opMe, {ID: "getSupplierInvoice", HiddenBy: permReadInvoices}, {ID: "getPurchaseOrder", HiddenBy: permReadOrders},
			{ID: "listGoodsReceipts", HiddenBy: permReadReceipts}},
		"GET /invoices/{id}/document": {{ID: "getSupplierInvoiceDocument", HiddenBy: permReadInvoices}},
		"GET /invoices/{id}/edit":     opInvoiceForm,
		"POST /invoices/{id}/edit":    append(append([]Operation{{ID: "updateSupplierInvoice", HiddenBy: permCaptureInvoices}}, opInvoiceForm...), opLookups...),
		"POST /invoices/{id}/recommendations/{rec}/decide": append([]Operation{{ID: "decideRecommendation", HiddenBy: permDecideRecommendations,
			HumanOnly: true}}, opInvoice...),
		"GET /bank":                        opStatementList,
		"POST /bank/statements":            append([]Operation{{ID: "importBankStatements", HiddenBy: permImportStatements}}, opStatementList...),
		"GET /bank/statements/{id}":        opStatement,
		"POST /bank/statements/{id}/rules": append([]Operation{{ID: "matchBankStatement", HiddenBy: permMatchLines}}, opStatement...),
		"GET /bank/statements/{id}/lines/{line}/payments": {opMe, {ID: "getBankStatement", HiddenBy: permReadStatements},
			{ID: "listBankStatementPayments", HiddenBy: permMatchLines, HumanOnly: true}},
		"GET /bank/statements/{id}/file":            {{ID: "getBankStatementFile", HiddenBy: permReadStatements, HumanOnly: true}},
		"GET /bank/house-banks/{id}/reconciliation": {opMe, opHouseBanks, {ID: "getBankReconciliation", HiddenBy: permReadStatements}},
		"GET /payments":                             opRuns,
		"POST /payments/propose":                    append([]Operation{{ID: "proposePaymentRun", HiddenBy: permProposeRuns}}, opRuns...),
		"GET /payments/{id}":                        opRun,
		"GET /payments/{id}/progress":               opProgress,
		"GET /payments/{id}/files":                  opFiles,
		"POST /payments/{id}/cancel":                append([]Operation{{ID: "cancelOperation", HiddenBy: permCancelOperations}}, opProgress...),
		"GET /close":                                opPeriods,
		"GET /close/periods/{id}":                   opPeriod,
		"GET /close/checks":                         opChecks,
		"GET /close/revaluation":                    opRevaluation,
		"POST /close/revaluation/preview": append(append([]Operation{{ID: "previewRevaluation", HiddenBy: permReadEntries}}, opRevaluation...),
			opAccounts),
		"POST /close/revaluation/post": append([]Operation{{ID: "postRevaluationRun", HiddenBy: permPostEntries, HumanOnly: true}}, opRevaluation...),
		"GET /close/revaluation/{id}":  opRevaluationRun,
		"POST /close/revaluation/{id}/cancel": append([]Operation{{ID: "cancelRevaluationRun", HiddenBy: permPostEntries, HumanOnly: true}},
			opRevaluationRun...),
		"GET /journal":     opParkedList,
		"GET /journal/new": opEntryForm,
		"POST /journal/new": append(append([]Operation{opParkEntry, {ID: "postJournalEntry", HiddenBy: permPostEntries, HumanOnly: true, Write: true}},
			opEntryForm...), opLookups...),
		"POST /journal/new/file":                     append(append([]Operation{opParkEntry}, opEntryForm...), opLookups...),
		"GET /journal/parked/{id}":                   opParkedEntry,
		"GET /journal/parked/{id}/edit":              opChangeForm,
		"POST /journal/parked/{id}/edit":             append(append([]Operation{opUpdateParked}, opChangeForm...), opLookups...),
		"POST /journal/parked/{id}/lines":            append(append([]Operation{opUpdateParked}, opParkedEntry...), opLookups...),
		"GET /journal/entries":                       opJournal,
		"GET /journal/entries/{id}":                  opEntry,
		"POST /journal/entries/{id}/reverse":         append([]Operation{{ID: "reverseJournalEntry", HiddenBy: permPostEntries, HumanOnly: true}}, opEntry...),
		"GET /reports":                               {opMe, opHouseBanks, opCompanies},
		"GET /reports/trial-balance":                 opBalances,
		"GET /reports/trial-balance.csv":             {opMe, opToday, opAccounts, paged(opListBalances)},
		"GET /reports/trial-balance/entries":         opAccountEntries,
		"GET /reports/open-items":                    opOpenItems,
		"GET /reports/open-items.csv":                {opMe, opCompanies, opToday, opAccounts, opSupplier, paged(opListItems)},
		"GET /reports/gr-ir":                         opGRIR,
		"GET /reports/gr-ir.csv":                     {opMe, paged(opListGRIR)},
		"GET /reports/ledger-reconciliation":         opReconcile,
		"GET /reports/ledger-reconciliation.csv":     {opMe, opToday, opLedgers, paged(opListRecon)},
		"GET /reports/ledger-reconciliation/entries": opReconEntries,
		"GET /reports/audit":                         {opMe, opListAudit, opNames},
		"GET /reports/audit.csv":                     {opMe, paged(opListAudit), opNames},
	}
	for verb, op := range decideOps {
		screens["POST /approvals/{id}/"+verb] = append([]Operation{op}, opRequest...)
	}
	for verb, op := range actionOps {
		screens["POST /invoices/{id}/"+verb] = append([]Operation{op}, opInvoice...)
	}
	for verb, op := range lineOps {
		screens["POST /bank/statements/{id}/"+verb] = append([]Operation{op}, opStatement...)
	}
	for verb, op := range runOps {
		screens["POST /payments/{id}/"+verb] = append([]Operation{op}, opRun...)
	}
	for verb, op := range periodOps {
		screens["POST /close/periods/{id}/"+verb] = append([]Operation{op}, opPeriod...)
	}
	for verb, op := range parkedOps {
		screens["POST /journal/parked/{id}/"+verb] = append([]Operation{op}, opParkedEntry...)
	}
	return screens
}()
