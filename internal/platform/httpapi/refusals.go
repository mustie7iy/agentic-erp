package httpapi

import (
	"github.com/google/uuid"
	"github.com/ogen-go/ogen/middleware"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// RefusalsAudited are the writes whose refusals the API audits on their
// document (ADR-0011 As built, review:batch74), by operation: the action
// a refusal is recorded under (the one the write is audited under when it
// succeeds) and the document it is about, the operation's {id}. A write on
// a document's part (an order's change, a delivery) is recorded on the
// document, under the part's action. They are the writes on the documents
// the support agent diagnoses by their audit trail (supportagent.Documents,
// ADR-0072 As built T108b), which a test holds to the contract; a write of
// another context is not listed until a reader of its refusals needs it.
var RefusalsAudited = map[string]audit.Entry{
	"updateSupplierInvoice":            {Action: "payables.supplier_invoice.update", ObjectType: "payables.supplier_invoice"},
	"cancelSupplierInvoice":            {Action: "payables.supplier_invoice.cancel", ObjectType: "payables.supplier_invoice"},
	"clearSupplierInvoiceDownPayments": {Action: "payables.supplier_invoice.clear_down_payments", ObjectType: "payables.supplier_invoice"},
	"holdSupplierInvoice":              {Action: "payables.supplier_invoice.hold", ObjectType: "payables.supplier_invoice"},
	"matchSupplierInvoice":             {Action: "payables.supplier_invoice.match", ObjectType: "payables.supplier_invoice"},
	"postSupplierInvoice":              {Action: "payables.supplier_invoice.post", ObjectType: "payables.supplier_invoice"},
	"releaseSupplierInvoice":           {Action: "payables.supplier_invoice.release", ObjectType: "payables.supplier_invoice"},
	"reverseSupplierInvoice":           {Action: "payables.supplier_invoice.reverse", ObjectType: "payables.supplier_invoice"},
	"postCustomerInvoice":              {Action: "receivables.customer_invoice.post", ObjectType: "receivables.customer_invoice"},
	"reverseJournalEntry":              {Action: "ledger.journal_entry.reverse", ObjectType: "ledger.journal_entry"},
	"updatePurchaseOrder":              {Action: "procurement.purchase_order.update", ObjectType: "procurement.purchase_order"},
	"cancelPurchaseOrder":              {Action: "procurement.purchase_order.cancel", ObjectType: "procurement.purchase_order"},
	"closePurchaseOrder":               {Action: "procurement.purchase_order.close", ObjectType: "procurement.purchase_order"},
	"releasePurchaseOrder":             {Action: "procurement.purchase_order.release", ObjectType: "procurement.purchase_order"},
	"createPurchaseOrderChange":        {Action: "procurement.purchase_order_change.create", ObjectType: "procurement.purchase_order"},
	"updatePurchaseOrderChange":        {Action: "procurement.purchase_order_change.update", ObjectType: "procurement.purchase_order"},
	"cancelPurchaseOrderChange":        {Action: "procurement.purchase_order_change.cancel", ObjectType: "procurement.purchase_order"},
	"submitPurchaseOrderChange":        {Action: "procurement.purchase_order_change.submit", ObjectType: "procurement.purchase_order"},
	"createGrIrClearing":               {Action: "procurement.gr_ir_clearing.create", ObjectType: "procurement.purchase_order"},
	"confirmSalesOrder":                {Action: "sales.sales_order.confirm", ObjectType: "sales.sales_order"},
	"releaseSalesOrderCreditBlock":     {Action: "sales.sales_order.release_credit_block", ObjectType: "sales.sales_order"},
	"confirmSalesDelivery":             {Action: "sales.delivery.confirm", ObjectType: "sales.sales_order"},
	"submitPaymentRun":                 {Action: "payments.payment_run.submit", ObjectType: "payments.payment_run"},
	"releasePaymentRun":                {Action: "payments.payment_run.release", ObjectType: "payments.payment_run"},
	"rejectPaymentRun":                 {Action: "payments.payment_run.reject", ObjectType: "payments.payment_run"},
	"updateChangeRequest":              {Action: "config.change_request.update", ObjectType: "config.change_request"},
	"submitChangeRequest":              {Action: "config.change_request.submit", ObjectType: "config.change_request"},
	"simulateChangeRequest":            {Action: "config.change_request.simulate", ObjectType: "config.change_request"},
	"withdrawChangeRequest":            {Action: "config.change_request.withdraw", ObjectType: "config.change_request"},
	"activateChangeRequest":            {Action: "config.change_request.activate", ObjectType: "config.change_request"},
	"cancelOperation":                  {Action: "platform.operation.cancel", ObjectType: "platform.operation"},
}

// auditRefusals audits the refusal of a write listed in refused on its
// document (audit.RefuseProblem: a business outcome, recorded once, with
// the operation's id in its detail), in the request's unit of work, which
// keeps it while it rolls the write back. A handler that audited the
// refusal itself, with more detail, is not recorded again.
func auditRefusals(refused map[string]audit.Entry) middleware.Middleware {
	return func(req middleware.Request, next middleware.Next) (middleware.Response, error) {
		resp, err := next(req)
		e, listed := refused[req.OperationID]
		// A body the contract's rules refuse may be refused before the
		// handler reads the document, whose {id} may then be none.
		if err == nil || !listed || problem.From(err).Code == "request.validation_failed" {
			return resp, err
		}
		v, _ := req.Params.Path("id")
		id, ok := v.(uuid.UUID)
		if !ok { // every listed operation's {id} is a uuid, which the contract test holds
			return resp, err
		}
		e.ObjectID, e.Detail = id, map[string]any{"operation": req.OperationID}
		return resp, audit.RefuseProblem(req.Context, e, err)
	}
}
