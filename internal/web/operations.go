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
	"payment_run_release": "payments.run.release",
}

// Operation is one API operation a route calls: its operationId, the
// permission the screen hides it by ("" when it hides nothing by one), and
// whether the route is human-only, as an x-agent: false operation needs
// (the workspace signs people in, never agents).
type Operation struct {
	ID        string
	HiddenBy  string
	HumanOnly bool
}

var (
	opMe        = Operation{ID: "getCurrentPrincipal"}
	opNames     = Operation{ID: "listPrincipalNames"}
	opGet       = Operation{ID: "getApprovalRequest", HiddenBy: permReadApprovals}
	opCompanies = Operation{ID: "listLegalEntities", HiddenBy: permReadCompanies}
	// opInvoice are the reads of an invoice's page (invoice.go).
	opInvoice = []Operation{opMe, {ID: "getSupplierInvoice", HiddenBy: permReadInvoices}, {ID: "listMatchResults", HiddenBy: permReadInvoices},
		{ID: "listRecommendations", HiddenBy: permReadRecommendations}, opNames, opCompanies}
	opHouseBanks = Operation{ID: "listHouseBanks", HiddenBy: permReadStatements}
	// opStatementList are the reads of the statements' list (bank.go),
	// opStatement those of a statement's page (statement.go).
	opStatementList = []Operation{opMe, opHouseBanks, opCompanies, {ID: "listBankStatements", HiddenBy: permReadStatements}}
	opStatement     = []Operation{opMe, {ID: "getBankStatement", HiddenBy: permReadStatements},
		{ID: "listBankStatementMatches", HiddenBy: permReadStatements}, {ID: "getBankStatementRules", HiddenBy: permReadStatements}, opHouseBanks, opNames}
	// opRuns are the reads of the payment runs' list (payments.go), opRun
	// those of a run's page (payment_run.go) and opProgress those of its
	// operation's progress (payment_progress.go).
	opRuns   = []Operation{opMe, opCompanies, opHouseBanks, {ID: "listPaymentRuns", HiddenBy: permReadRuns}, opNames}
	opGetRun = Operation{ID: "getPaymentRun", HiddenBy: permReadRuns}
	opRun    = []Operation{opMe, opGetRun, {ID: "listPayments", HiddenBy: permReadRuns}, {ID: "listPaymentExclusions", HiddenBy: permReadRuns},
		{ID: "listBankFiles", HiddenBy: permReadRuns}, opNames}
	opProgress = []Operation{opMe, opGetRun, {ID: "getOperation", HiddenBy: permReadOperations}}
	// opPeriods are the reads of the periods' list (close.go), opPeriod
	// those of a period's page (period.go) and opChecks those of the
	// pre-close checks (close_checks.go), where the statements not
	// reconciled are read once per status.
	opLedgers    = Operation{ID: "listLedgers", HiddenBy: permReadAccounts}
	opPeriods    = []Operation{opMe, opCompanies, opLedgers, {ID: "listFiscalPeriods", HiddenBy: permReadPeriods}, opNames}
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
)

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
		"GET /{$}":                     {opMe, {ID: "listLegalEntities"}},
		"POST /logout":                 {{ID: "endCurrentSession", HumanOnly: true}},
		"GET /approvals":               {opMe, {ID: "listApprovalRequests", HiddenBy: permReadApprovals}, opNames},
		"GET /approvals/{id}":          {opMe, opGet, opNames},
		"POST /approvals/{id}/approve": {opMe, {ID: "approveApprovalRequest", HiddenBy: permDecide, HumanOnly: true}, opGet, opNames},
		"POST /approvals/{id}/reject":  {opMe, {ID: "rejectApprovalRequest", HiddenBy: permDecide, HumanOnly: true}, opGet, opNames},
		"GET /invoices":                {opMe, opCompanies, {ID: "listSupplierInvoices", HiddenBy: permReadInvoices}, opNames},
		"GET /invoices/{id}":           opInvoice,
		"GET /invoices/{id}/order": {opMe, {ID: "getSupplierInvoice", HiddenBy: permReadInvoices}, {ID: "getPurchaseOrder", HiddenBy: permReadOrders},
			{ID: "listGoodsReceipts", HiddenBy: permReadReceipts}},
		"GET /invoices/{id}/document": {{ID: "getSupplierInvoiceDocument", HiddenBy: permReadInvoices}},
		"POST /invoices/{id}/recommendations/{rec}/decide": append([]Operation{{ID: "decideRecommendation", HiddenBy: permDecideRecommendations,
			HumanOnly: true}}, opInvoice...),
		"GET /bank":                                 opStatementList,
		"POST /bank/statements":                     append([]Operation{{ID: "importBankStatements", HiddenBy: permImportStatements}}, opStatementList...),
		"GET /bank/statements/{id}":                 opStatement,
		"GET /bank/house-banks/{id}/reconciliation": {opMe, opHouseBanks, {ID: "getBankReconciliation", HiddenBy: permReadStatements}},
		"GET /payments":                             opRuns,
		"POST /payments/propose":                    append([]Operation{{ID: "proposePaymentRun", HiddenBy: permProposeRuns}}, opRuns...),
		"GET /payments/{id}":                        opRun,
		"GET /payments/{id}/progress":               opProgress,
		"POST /payments/{id}/cancel":                append([]Operation{{ID: "cancelOperation", HiddenBy: permCancelOperations}}, opProgress...),
		"GET /close":                                opPeriods,
		"GET /close/periods/{id}":                   opPeriod,
		"GET /close/checks":                         opChecks,
		"GET /close/revaluation":                    opRevaluation,
		"POST /close/revaluation/preview": append(append([]Operation{{ID: "previewRevaluation", HiddenBy: permReadEntries}}, opRevaluation...),
			opAccounts),
		"POST /close/revaluation/post": append([]Operation{{ID: "postRevaluationRun", HiddenBy: permPostEntries, HumanOnly: true}}, opRevaluation...),
		"GET /close/revaluation/{id}":  opRevaluationRun,
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
	return screens
}()
