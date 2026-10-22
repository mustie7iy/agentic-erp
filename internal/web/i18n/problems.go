package i18n

// Problems are the API's problem codes the workspace explains in the
// person's language, each with its message (ADR-0052 G7, §8). A code not
// here is shown with the API's English title, as the problem's fallback;
// T73l translates every code the screens' operations list. The
// contract-drift test holds each code here to one the operations the
// screens call can answer (x-problem-codes).
var Problems = map[string]Key{
	"workflow.already_approved":      ProblemAlreadyApproved,
	"workflow.not_pending":           ProblemNotPending,
	"workflow.own_request":           ProblemOwnRequest,
	"workflow.role_missing":          ProblemRoleMissing,
	"workflow.segregation_of_duties": ProblemSoDObject,
	"workflow.request_pending":       ProblemRequestPending,
	"request.version_mismatch":       ProblemVersion,
	"identity.permission_missing":    ProblemPermission,
	"identity.sod_conflict":          ProblemSoD,
	"identity.agent_not_allowed":     ProblemAgent,
	"request.not_found":              ProblemNotFound,
	"request.invalid_body":           ProblemInvalidBody,
	"request.validation_failed":      ProblemValidation,
	"request.idempotency_key_reused": ProblemKeyReused,
	"ledger.period_not_open":         ProblemPeriodNotOpen,
}

// InvoiceProblems are the codes the invoice screens (T73e) explain in
// their own words, looked up before Problems: the payables codes, and the
// general ones whose Problems message speaks of an approval request.
var InvoiceProblems = map[string]Key{
	"request.version_mismatch":        ProblemInvoiceVersion,
	"request.not_found":               ProblemInvoiceNotFound,
	"request.invalid_body":            ProblemInvoiceBody,
	"request.validation_failed":       ProblemInvoiceValidation,
	"request.idempotency_key_reused":  ProblemInvoiceKeyReused,
	"workflow.request_pending":        ProblemInvoicePending,
	"ledger.period_not_open":          ProblemInvoicePeriod,
	"identity.agent_not_allowed":      ProblemInvoiceAgent,
	"payables.match_outdated":         ProblemMatchOutdated,
	"payables.match_required":         ProblemMatchRequired,
	"payables.tax_mismatch":           ProblemTaxMismatch,
	"payables.invalid_status":         ProblemInvalidStatus,
	"payables.amounts_mismatch":       ProblemAmountsMismatch,
	"payables.supplier_not_usable":    ProblemSupplierBlocked,
	"payables.no_purchase_order":      ProblemNoOrder,
	"payables.invoice_cleared":        ProblemInvoiceCleared,
	"payables.invoice_in_payment_run": ProblemInPaymentRun,
	"agents.already_decided":          ProblemAlreadyDecided,
}
