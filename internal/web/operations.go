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
)

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
	}
	for verb, op := range actionOps {
		screens["POST /invoices/{id}/"+verb] = append([]Operation{op}, opInvoice...)
	}
	return screens
}()
