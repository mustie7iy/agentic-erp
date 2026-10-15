// Package problem is the platform's error model: RFC 9457 Problem Details
// with a stable, documented `code` that clients branch on (04-api-design
// §4.6). Every error that leaves the service is a *Problem; database errors
// are translated here so raw database messages never reach a client.
package problem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"

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

// WithCause records the underlying error for logging.
func (p *Problem) WithCause(err error) *Problem { p.cause = err; return p }

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
	return Unprocessable("request.reference_not_found", "No "+what+" with this id.",
		FieldError{Field: field, Code: "not_found", Message: message})
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
		return Unprocessable("request.reference_not_found", "The request refers to a resource that does not exist.").WithCause(err)
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

// Write sends a problem as application/problem+json.
func Write(w http.ResponseWriter, r *http.Request, p *Problem) {
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
		h[k] = vs
	}
	if p.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(p.RetryAfter))
	}
	h.Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p) // the status is already sent; a failed write cannot be reported
}
