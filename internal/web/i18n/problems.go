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

// BankProblems are the codes the bank statement screens (T73g) explain
// in their own words, looked up before Problems: the payments codes of a
// statement's import and of its lines' decisions, and the general ones
// in a statement's words.
var BankProblems = map[string]Key{
	"request.version_mismatch":             ProblemStatementVersion,
	"request.validation_failed":            ProblemBankValidation,
	"ledger.period_not_open":               ProblemBankPeriod,
	"payments.statement_conflict":          ProblemStatementConflict,
	"payments.statement_unreadable":        ProblemStatementUnreadable,
	"payments.statement_not_of_bank":       ProblemStatementNotOfBank,
	"payments.statement_gap":               ProblemStatementGap,
	"payments.statement_bank_not_posting":  ProblemBankNotPosting,
	"payments.statement_amount_differs":    ProblemAmountDiffers,
	"payments.statement_posting_rule_gone": ProblemRuleGone,
	"payments.statement_incoming_gone":     ProblemIncomingGone,
	"payments.statement_line_posting":      ProblemLinePosting,
	"payments.statement_match_target":      ProblemMatchTarget,
	"payments.statement_reconciled":        ProblemStatementReconciled,
	"payments.statement_line_not_matched":  ProblemLineNotMatched,
	"payments.statement_line_no_proposal":  ProblemNoProposal,
}

// PaymentProblems are the codes the payment run screens (T73f) explain
// in their own words, looked up before Problems: the payments codes of a
// run's proposal, release, rejection, second approval and bank files, a
// cancelled operation's, and the general ones in a run's words.
var PaymentProblems = map[string]Key{
	"request.version_mismatch":               ProblemRunVersion,
	"request.validation_failed":              ProblemRunValidation,
	"identity.sod_conflict":                  ProblemRunSoD,
	"payments.currency_mismatch":             ProblemCurrencyMismatch,
	"payments.currency_required":             ProblemCurrencyRequired,
	"payments.house_bank_inactive":           ProblemHouseBankInactive,
	"payments.legal_entity_not_active":       ProblemCompanyInactive,
	"ledger.no_fiscal_period":                ProblemNoFiscalPeriod,
	"payments.invalid_status":                ProblemRunStatus,
	"payments.nothing_to_pay":                ProblemNothingToPay,
	"payments.proposal_outdated":             ProblemProposalOutdated,
	"payments.discount_lapsed":               ProblemDiscountLapsed,
	"payments.release_approval_required":     ProblemApprovalRequired,
	"payments.release_approval_pending":      ProblemApprovalPending,
	"payments.release_approval_outdated":     ProblemApprovalOutdated,
	"payments.release_already_approved":      ProblemAlreadyApprovedRun,
	"payments.release_approval_not_required": ProblemApprovalNotRequired,
	"payments.release_in_progress":           ProblemReleaseInProgress,
	"payments.release_role_missing":          ProblemReleaseRole,
	"ledger.no_exchange_rate":                ProblemNoExchangeRate,
	"ledger.item_reserved":                   ProblemItemReserved,
	"platform.operation_not_cancellable":     ProblemNotCancellable,
}
