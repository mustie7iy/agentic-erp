// Package problem is the platform's error model: RFC 9457 Problem Details
// with a stable, documented `code` that clients branch on (04-api-design
// §4.6). Every error that leaves the service is a *Problem; database errors
// are translated here so raw database messages never reach a client.
//
// A problem is one request's answer, never a shared value: a refusal that
// many requests give is a function returning a new problem each call, not
// a package-level variable (internal/layout's TestNoSharedProblems refuses
// one). Write stamps the request's instance and trace id, and WithCause
// records its cause, on a copy, so even a problem reached by two requests
// is not changed by either. The copy is shallow: Errors, MatchIssues,
// CalculatedTax, CreditLimit and Header stay shared with the original, so
// a helper that rewrites a problem it is given (ledger's asItems and
// asAsset) relies on the problem being its request's alone (review:T53f4:
// a shared one carried the first refused request's instance and trace id
// to every later one, of any tenant, and raced).
package problem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TypeBase prefixes the `type` URI of every problem.
const TypeBase = "https://errors.example.com/"

// FieldError points at one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Money is an amount as the API writes it: a decimal string with the
// currency's minor-unit digits, and the currency.
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// CalculatedTax are the figures of a supplier invoice whose stated tax its
// codes' rates do not bear out (payables.tax_mismatch): carried as fields
// so a client shows them in its own words, which it may not do with the
// English detail (ADR-0052, decision D3).
type CalculatedTax struct {
	Stated     Money     `json:"stated"`
	Calculated Money     `json:"calculated"`
	Codes      []CodeTax `json:"codes"`
}

// CodeTax is the tax one code's rates give.
type CodeTax struct {
	TaxCode string `json:"tax_code"`
	Tax     Money  `json:"tax"`
}

// MatchIssue is one issue of a supplier invoice's three-way match, as the
// API's MatchIssue: its code, the invoice item it concerns, and its
// explanation.
type MatchIssue struct {
	Code    string `json:"code"`
	ItemNo  *int32 `json:"item_no,omitempty"`
	Message string `json:"message"`
}

// FailingCheck is a check task of a period close whose check failed when
// a sign-off ran it again (closing.checks_failing, ADR-0058 §7): its task,
// its check and what it found, as the API's FailingCheck.
type FailingCheck struct {
	TaskCode string `json:"task_code"`
	TaskName string `json:"task_name"`
	Check    string `json:"check"`
	Summary  string `json:"summary"`
}

// CreditLimit are the figures of a credit refused because the credits
// naming an invoice would exceed its gross amount
// (payables.credit_exceeds_invoice, product owner 2026-11-10): the
// invoice, its gross, what posted credits naming it already credit, and
// the gross of the one refused, all in the invoice's currency.
type CreditLimit struct {
	InvoiceID     uuid.UUID `json:"invoice_id"`
	InvoiceNumber string    `json:"invoice_number"`
	InvoiceGross  Money     `json:"invoice_gross"`
	Credited      Money     `json:"credited"`
	Attempted     Money     `json:"attempted"`
}

// Problem is an RFC 9457 problem document.
type Problem struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Code     string       `json:"code"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	TraceID  string       `json:"trace_id,omitempty"`
	Errors   []FieldError `json:"errors,omitempty"`
	// CalculatedTax are payables.tax_mismatch's figures (ADR-0052,
	// decision D3), set by that problem alone.
	CalculatedTax *CalculatedTax `json:"calculated_tax,omitempty"`
	// MatchIssues are payables.match_outdated's issues (ADR-0052,
	// decision D3), set by that problem alone: those the invoice's match
	// at posting finds that its basis did not show.
	MatchIssues []MatchIssue `json:"match_issues,omitempty"`
	// FailingChecks are closing.checks_failing's failing checks (ADR-0058
	// §7), set by that problem alone.
	FailingChecks []FailingCheck `json:"failing_checks,omitempty"`
	// CreditLimit are payables.credit_exceeds_invoice's figures, set by
	// that problem alone.
	CreditLimit *CreditLimit `json:"credit_limit,omitempty"`

	// RetryAfter, in seconds, is sent as the Retry-After header when set.
	RetryAfter int `json:"-"`
	// Header holds extra response headers (for example WWW-Authenticate).
	Header http.Header `json:"-"`
	// cause is the underlying error, for logs only.
	cause error
}

func (p *Problem) Error() string {
	if p.cause != nil {
		return fmt.Sprintf("%s (%d): %s: %v", p.Code, p.Status, p.Detail, p.cause)
	}
	return fmt.Sprintf("%s (%d): %s", p.Code, p.Status, p.Detail)
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (p *Problem) Unwrap() error { return p.cause }

// WithCause is p with the underlying error recorded for logging, as a
// copy: p itself is left as it was (the package doc).
func (p *Problem) WithCause(err error) *Problem {
	q := *p
	q.cause = err
	return &q
}

// New builds a problem. The type URI is derived from the code:
// "payables.duplicate_invoice" → TypeBase + "payables/duplicate-invoice".
func New(status int, code, title, detail string) *Problem {
	slug := strings.ReplaceAll(strings.ReplaceAll(code, ".", "/"), "_", "-")
	return &Problem{Type: TypeBase + slug, Title: title, Status: status, Code: code, Detail: detail}
}

func BadRequest(code, detail string, errs ...FieldError) *Problem {
	p := New(http.StatusBadRequest, code, "Bad request", detail)
	p.Errors = errs
	return p
}

func Unauthorized(detail string) *Problem {
	p := New(http.StatusUnauthorized, "authn.invalid_token", "Unauthorized", detail)
	p.Header = http.Header{"Www-Authenticate": {`Bearer error="invalid_token"`}}
	return p
}

func Forbidden(code, detail string) *Problem {
	return New(http.StatusForbidden, code, "Forbidden", detail)
}

func NotFound(detail string) *Problem {
	return New(http.StatusNotFound, "request.not_found", "Not found", detail)
}

// NotFoundOf is the 404 for a lookup by id of the resource named by what
// ("No purchase order with this id."), the wording every context uses.
func NotFoundOf(what string) *Problem { return NotFound("No " + what + " with this id.") }

func Conflict(code, detail string) *Problem {
	return New(http.StatusConflict, code, "Conflict", detail)
}

func PreconditionFailed(detail string) *Problem {
	return New(http.StatusPreconditionFailed, "request.version_mismatch", "Precondition failed", detail)
}

func PreconditionRequired(header string) *Problem {
	return New(http.StatusPreconditionRequired, "request.precondition_required", "Precondition required",
		header+" header is required for this operation.")
}

func Unprocessable(code, detail string, errs ...FieldError) *Problem {
	p := New(http.StatusUnprocessableEntity, code, "Unprocessable request", detail)
	p.Errors = errs
	return p
}

// Reference turns "not found" (404) from reading a resource the request
// refers to into 422 request.reference_not_found on the referring field:
// the request's content is wrong, not its URL. what names the resource in
// the detail ("No legal entity with this id."), message is the field
// error's message; any other error passes through unchanged.
func Reference(err error, field, what, message string) error {
	if From(err).Status == http.StatusNotFound {
		return ReferenceNotFound(field, what, message)
	}
	return err
}

// ReferenceNotFound is Reference's 422 request.reference_not_found for a
// caller that learnt the resource is missing from a lookup's ok = false
// rather than from a 404.
func ReferenceNotFound(field, what, message string) *Problem {
	return MissingReference("No "+what+" with this id.",
		FieldError{Field: field, Code: "not_found", Message: message})
}

// RequireText refuses a text given as white space alone: 422
// request.validation_failed naming field, nil for any other text. A
// column such a text is written to checks that its trimmed text is not
// empty, which would refuse it as 409 request.rule_violated without the
// field, so a context calls RequireText before the write.
func RequireText(field, value string) error {
	if strings.TrimSpace(value) != "" {
		return nil
	}
	return Unprocessable("request.validation_failed", "The request is not valid: "+field+" is a text, not white space alone.",
		FieldError{Field: field, Code: "invalid", Message: "a text, not white space alone"})
}

// MissingReference is the 422 request.reference_not_found that every
// missing reference is reported as, with the caller's detail and field
// errors.
func MissingReference(detail string, errs ...FieldError) *Problem {
	return Unprocessable("request.reference_not_found", detail, errs...)
}

func Unavailable(detail string, retryAfter int) *Problem {
	p := New(http.StatusServiceUnavailable, "platform.unavailable", "Service unavailable", detail)
	p.RetryAfter = retryAfter
	return p
}

func Internal(err error) *Problem {
	return New(http.StatusInternalServerError, "platform.internal", "Internal error",
		"An unexpected error occurred. Quote the trace id when reporting it.").WithCause(err)
}

// constraint maps a database constraint name to the problem a context wants
// clients to see when it is violated.
var (
	constraintsMu sync.RWMutex
	constraints   = map[string]func() *Problem{}
)

// RegisterConstraint lets a context translate its own constraint violations
// into documented codes, for example
// "supplier_invoice_no_duplicates" → 409 payables.duplicate_invoice.
func RegisterConstraint(name string, p func() *Problem) {
	constraintsMu.Lock()
	defer constraintsMu.Unlock()
	constraints[name] = p
}

// From converts any error into a problem. Problems pass through unchanged;
// database errors become stable codes; anything else is an internal error.
func From(err error) *Problem {
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Unavailable("The request did not complete in time; retry with the same Idempotency-Key.", 1).WithCause(err)
	}
	if lostConnection(err) {
		return Unavailable("The database connection was lost; retry with the same Idempotency-Key.", 1).WithCause(err)
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return Internal(err)
	}
	// Class 08 (connection exception) and 57P0x (the server shutting down or
	// terminating the session): the transaction is gone, the request can be retried.
	if strings.HasPrefix(pg.Code, "08") || strings.HasPrefix(pg.Code, "57P0") {
		return Unavailable("The database connection was lost; retry with the same Idempotency-Key.", 1).WithCause(err)
	}
	// read_only_sql_transaction in a read-write transaction: the statement
	// reached a former writer during a failover, before PgBouncer moved to
	// the new one; nothing was written (ADR-0040). FromReadOnly keeps it a
	// 500 in a read-only transaction.
	if pg.Code == readOnlySQLTransaction {
		return Unavailable("The database is failing over; retry with the same Idempotency-Key.", 1).WithCause(err)
	}
	if pg.ConstraintName != "" {
		constraintsMu.RLock()
		f, ok := constraints[pg.ConstraintName]
		constraintsMu.RUnlock()
		if ok {
			return f().WithCause(err)
		}
	}
	switch pg.Code {
	case "23505": // unique_violation
		return Conflict("request.duplicate", "A resource with the same unique values already exists.").WithCause(err)
	case "23503": // foreign_key_violation
		return MissingReference("The request refers to a resource that does not exist.").WithCause(err)
	case "23514", "23P01": // check_violation, exclusion_violation (also raised by business-rule triggers)
		return Conflict("request.rule_violated", "The request violates a business rule.").WithCause(err)
	case "23502", "22P02", "22001", "22003", "22007", "22008": // not null, invalid text, too long, out of range, dates
		return Unprocessable("request.invalid_value", "A value in the request is missing or invalid.").WithCause(err)
	case "40001", "40P01": // serialization_failure, deadlock_detected
		return Unavailable("A concurrent change interfered; retry with the same Idempotency-Key.", 1).WithCause(err)
	case "55P03": // lock_not_available (lock_timeout)
		p := Conflict("request.in_progress", "The resource is being changed by another request; retry shortly.")
		p.RetryAfter = 1
		return p.WithCause(err)
	case "57014": // query_canceled (statement_timeout)
		return Unavailable("The request took too long and was cancelled.", 1).WithCause(err)
	}
	return Internal(err)
}

// readOnlySQLTransaction is SQLSTATE 25006.
const readOnlySQLTransaction = "25006"

// FromReadOnly is From for an error of a read-only transaction, where
// 25006 is a write the code should not attempt, a bug that stays a 500
// rather than a failover to retry (ADR-0040).
func FromReadOnly(err error) *Problem {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == readOnlySQLTransaction {
		return Internal(err)
	}
	return From(err)
}

// lostConnection reports a connection to the database failing or closing
// under a request (a failover, a terminated backend, a network fault):
// nothing of the request's transaction survives, so it is safe to retry.
func lostConnection(err error) bool {
	var connect *pgconn.ConnectError
	var netErr net.Error
	return pgconn.SafeToRetry(err) || errors.As(err, &connect) || errors.As(err, &netErr) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// Cause returns the logged cause of a problem, if any.
func Cause(p *Problem) error { return p.cause }

// Write sends a problem as application/problem+json, with the request's
// path as its instance and the request's trace id unless p sets them. It
// sends a copy: p itself is left as it was (the package doc).
func Write(w http.ResponseWriter, r *http.Request, given *Problem) {
	p := *given
	if r != nil {
		if p.Instance == "" {
			p.Instance = r.URL.Path
		}
		if p.TraceID == "" {
			p.TraceID = reqctx.TraceID(r.Context())
		}
	}
	h := w.Header()
	for k, vs := range p.Header {
		h[k] = slices.Clone(vs) // the response's own: a later Add must not reach p's
	}
	if p.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(p.RetryAfter))
	}
	h.Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(&p) // the status is already sent; a failed write cannot be reported
}
