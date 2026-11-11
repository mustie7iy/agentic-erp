package problem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestConstructors(t *testing.T) {
	cases := []struct {
		p      *Problem
		status int
		code   string
	}{
		{BadRequest("request.bad", "x", FieldError{Field: "a", Code: "b", Message: "c"}), 400, "request.bad"},
		{Unauthorized("x"), 401, "authn.invalid_token"},
		{Forbidden("identity.permission_missing", "x"), 403, "identity.permission_missing"},
		{NotFound("x"), 404, "request.not_found"},
		{Conflict("c.d", "x"), 409, "c.d"},
		{PreconditionFailed("x"), 412, "request.version_mismatch"},
		{PreconditionRequired("If-Match"), 428, "request.precondition_required"},
		{Unprocessable("u.v", "x"), 422, "u.v"},
		{Unavailable("x", 2), 503, "platform.unavailable"},
		{Internal(errors.New("boom")), 500, "platform.internal"},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			if c.p.Status != c.status {
				t.Errorf("status: got %d, want %d", c.p.Status, c.status)
			}
			if c.p.Code != c.code {
				t.Errorf("code: got %q, want %q", c.p.Code, c.code)
			}
			if !strings.HasPrefix(c.p.Type, TypeBase) {
				t.Errorf("type: got %q, want a prefix of %q", c.p.Type, TypeBase)
			}
		})
	}
	if p := New(409, "payables.duplicate_invoice", "t", "d"); p.Type != TypeBase+"payables/duplicate-invoice" {
		t.Fatal(p.Type)
	}
	if p := PreconditionRequired("If-Match"); !strings.Contains(p.Detail, "If-Match") {
		t.Fatal(p.Detail)
	}
}

// TestReference: a referenced resource that is not found is 422 on the
// referring field; any other error passes through.
func TestReference(t *testing.T) {
	var p *Problem
	if !errors.As(Reference(fmt.Errorf("read: %w", NotFound("x")), "role_id", "role", "no role with this id"), &p) {
		t.Fatal("not found: got no problem, want one")
	}
	if p.Status != 422 || p.Code != "request.reference_not_found" {
		t.Errorf("not found: got %d %s, want 422 request.reference_not_found", p.Status, p.Code)
	}
	if p.Detail != "No role with this id." {
		t.Errorf("detail: got %q, want %q", p.Detail, "No role with this id.")
	}
	if want := (FieldError{Field: "role_id", Code: "not_found", Message: "no role with this id"}); len(p.Errors) != 1 || p.Errors[0] != want {
		t.Errorf("field errors: got %+v, want [%+v]", p.Errors, want)
	}
	other := Conflict("x.y", "No.")
	if got := Reference(other, "role_id", "role", "no role with this id"); got != other { //nolint:errorlint // the same error must come back, not a wrapper
		t.Errorf("other error: got %v, want %v unchanged", got, other)
	}
}

// TestNotFoundOf: the 404 of a lookup by id names the resource in the
// wording every context uses.
func TestNotFoundOf(t *testing.T) {
	got := NotFoundOf("purchase order")
	if got.Status != 404 {
		t.Errorf("status: got %d, want 404", got.Status)
	}
	if got.Code != "request.not_found" {
		t.Errorf("code: got %q, want request.not_found", got.Code)
	}
	if want := "No purchase order with this id."; got.Detail != want {
		t.Errorf("detail: got %q, want %q", got.Detail, want)
	}
}

// TestInvalid: a 422 with the code, naming the field with the detail as
// its message.
func TestInvalid(t *testing.T) {
	p := Invalid("x.unknown_unit", "items[0].unit", "The unit is not known.")
	if p.Status != 422 || p.Code != "x.unknown_unit" || p.Detail != "The unit is not known." {
		t.Errorf("got %d %s %q, want 422 x.unknown_unit and the message as its detail", p.Status, p.Code, p.Detail)
	}
	if want := (FieldError{Field: "items[0].unit", Code: "invalid", Message: "The unit is not known."}); len(p.Errors) != 1 || p.Errors[0] != want {
		t.Errorf("field errors: got %+v, want [%+v]", p.Errors, want)
	}
}

// TestReferenceNotFound: the refusal for a lookup that reports "not found"
// with a bool is the one Reference makes from a 404.
func TestReferenceNotFound(t *testing.T) {
	got := ReferenceNotFound("role_id", "role", "no role with this id")
	var want *Problem
	if !errors.As(Reference(NotFound("x"), "role_id", "role", "no role with this id"), &want) {
		t.Fatal("Reference: got no problem, want one")
	}
	if got.Status != want.Status || got.Code != want.Code || got.Detail != want.Detail {
		t.Errorf("got %d %s %q, want %d %s %q", got.Status, got.Code, got.Detail, want.Status, want.Code, want.Detail)
	}
	if len(got.Errors) != 1 || got.Errors[0] != want.Errors[0] {
		t.Errorf("field errors: got %+v, want %+v", got.Errors, want.Errors)
	}
}

// TestRequireText: a text of white space alone is 422 on its field, as
// the column's check would refuse it; any other text passes.
func TestRequireText(t *testing.T) {
	for name, c := range map[string]struct {
		value   string
		refused bool
	}{
		"spaces":            {"   ", true},
		"empty":             {"", true},
		"a tab and newline": {"\t\n", true},
		"a text":            {"Rent", false},
		"a padded text":     {"  Rent ", false},
	} {
		t.Run(name, func(t *testing.T) {
			err := RequireText("description", c.value)
			if !c.refused {
				if err != nil {
					t.Errorf("got %v, want nil", err)
				}
				return
			}
			var p *Problem
			if !errors.As(err, &p) {
				t.Fatalf("got %v, want a problem", err)
			}
			if p.Status != 422 || p.Code != "request.validation_failed" {
				t.Errorf("got %d %s, want 422 request.validation_failed", p.Status, p.Code)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != "description" {
				t.Errorf("field errors: got %+v, want one on description", p.Errors)
			}
		})
	}
}

func TestErrorAndCause(t *testing.T) {
	cause := errors.New("disk on fire")
	p := Internal(cause)
	if !errors.Is(p, cause) {
		t.Errorf("errors.Is(%v, cause) = false, want true", p)
	}
	if got := Cause(p); got != cause { //nolint:errorlint // Cause must return the cause itself, not an error that wraps it
		t.Errorf("Cause: got %v, want %v", got, cause)
	}
	if got := p.Error(); !strings.Contains(got, "disk on fire") {
		t.Errorf("Error() = %q, want it to contain %q", got, "disk on fire")
	}
	q := NotFound("missing")
	if got := q.Error(); strings.Contains(got, "<nil>") {
		t.Errorf("Error() = %q, want no <nil>", got)
	}
	if got := Cause(q); got != nil {
		t.Errorf("Cause of a problem without one: got %v, want nil", got)
	}
}

func pgErr(code, constraint string) error {
	return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code, ConstraintName: constraint, Message: "secret internals"})
}

func TestFromTranslatesDatabaseErrors(t *testing.T) {
	RegisterConstraint("supplier_invoice_no_duplicates", func() *Problem {
		return Conflict("payables.duplicate_invoice", "Duplicate supplier invoice.")
	})
	cases := map[error]struct {
		status int
		code   string
	}{
		pgErr("23505", "supplier_invoice_no_duplicates"): {409, "payables.duplicate_invoice"},
		pgErr("23505", "other_unique"):                   {409, "request.duplicate"},
		pgErr("23503", ""):                               {422, "request.reference_not_found"},
		pgErr("23514", ""):                               {409, "request.rule_violated"},
		pgErr("23P01", "x"):                              {409, "request.rule_violated"},
		pgErr("22P02", ""):                               {422, "request.invalid_value"},
		pgErr("40001", ""):                               {503, "platform.unavailable"},
		pgErr("55P03", ""):                               {409, "request.in_progress"},
		pgErr("57014", ""):                               {503, "platform.unavailable"},
		pgErr("42501", ""):                               {500, "platform.internal"},
		pgErr("57P01", ""):                               {503, "platform.unavailable"}, // terminated by the administrator (failover)
		pgErr("08006", ""):                               {503, "platform.unavailable"}, // connection failure
		pgErr("25006", ""):                               {503, "platform.unavailable"}, // a former writer after a failover
		&pgconn.ConnectError{}:                           {503, "platform.unavailable"},
		&net.OpError{Op: "write", Err: errors.New("broken pipe")}: {503, "platform.unavailable"},
		fmt.Errorf("read: %w", io.ErrUnexpectedEOF):               {503, "platform.unavailable"},
		syscall.ECONNRESET:           {503, "platform.unavailable"},
		context.DeadlineExceeded:     {503, "platform.unavailable"},
		errors.New("plain"):          {500, "platform.internal"},
		Conflict("own.code", "kept"): {409, "own.code"},
	}
	for err, want := range cases {
		// The name comes from the SQLSTATE or the type: a bare ConnectError's
		// Error method dereferences its nil cause.
		name := fmt.Sprintf("%T", err)
		if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) {
			name = pe.Code
		}
		t.Run(name, func(t *testing.T) {
			p := From(err)
			if p.Status != want.status {
				t.Errorf("status: got %d, want %d", p.Status, want.status)
			}
			if p.Code != want.code {
				t.Errorf("code: got %q, want %q", p.Code, want.code)
			}
			if strings.Contains(p.Detail, "secret internals") {
				t.Errorf("database message leaked: %s", p.Detail)
			}
		})
	}
	if p := From(pgErr("55P03", "")); p.RetryAfter != 1 {
		t.Fatal("lock timeouts must ask the client to retry")
	}
	lost := From(pgErr("57P01", ""))
	if lost.RetryAfter != 1 {
		t.Errorf("a lost connection must ask the client to retry: RetryAfter got %d, want 1", lost.RetryAfter)
	}
	if !strings.Contains(lost.Detail, "connection was lost") {
		t.Errorf("lost connection detail: got %q, want it to contain %q", lost.Detail, "connection was lost")
	}
}

// TestFromReadOnly: in a read-only transaction 25006 is a write the code
// should not attempt, a bug, not a failover (ADR-0040).
func TestFromReadOnly(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
	}{
		"a write in a read-only transaction": {pgErr("25006", ""), 500},
		"a lost connection":                  {pgErr("57P01", ""), 503},
		"a missing reference":                {pgErr("23503", ""), 422},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if p := FromReadOnly(c.err); p.Status != c.status {
				t.Errorf("status: got %d, want %d", p.Status, c.status)
			}
		})
	}
	if p := From(pgErr("25006", "")); p.RetryAfter != 1 {
		t.Errorf("a former writer must ask the client to retry: RetryAfter got %d, want 1", p.RetryAfter)
	}
}

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/operations/x", nil)
	r = r.WithContext(reqctx.WithTraceID(r.Context(), "trace-9"))
	p := Unauthorized("token expired")
	p.RetryAfter = 3
	Write(rec, r, p)
	if rec.Code != 401 {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type: got %q, want %q", got, "application/problem+json")
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After: got %q, want %q", got, "3")
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "invalid_token") {
		t.Errorf("WWW-Authenticate: got %q, want it to contain %q", got, "invalid_token")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	for field, want := range map[string]any{
		"instance": "/v1/operations/x", "trace_id": "trace-9", "code": "authn.invalid_token", "status": float64(401),
	} {
		if got[field] != want {
			t.Errorf("%s: got %v, want %v", field, got[field], want)
		}
	}
	// Without a request the instance is left as set.
	rec = httptest.NewRecorder()
	q := NotFound("x")
	q.Instance = "/given"
	Write(rec, nil, q)
	if got := rec.Body.String(); !strings.Contains(got, "/given") {
		t.Errorf("body: got %s, want the instance /given", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After: got %q, want none", got)
	}
}

// TestWriteSharedProblem: two requests refused at once with the same
// problem value (as the package-level refusals of review:T53f4 were) each
// get their own instance and trace id, and the value is left as it was;
// run with -race, a stamp on the shared value is a data race. The cause's
// copy is TestWithCauseCopies'.
func TestWriteSharedProblem(t *testing.T) {
	shared := Unprocessable("cash.amount_too_large", "The amounts add up to more than an amount can hold.")
	requests := map[string]string{"/v1/tenant-a/position": "trace-a", "/v1/tenant-b/position": "trace-b"}
	type response struct {
		path string
		body map[string]any
	}
	responses := make(chan response, len(requests))
	var wg sync.WaitGroup
	for path, trace := range requests {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			Write(rec, r.WithContext(reqctx.WithTraceID(r.Context(), trace)), shared)
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Errorf("decode %s: %v", rec.Body, err)
			}
			responses <- response{path, got}
		})
	}
	wg.Wait()
	close(responses)
	for got := range responses {
		if got.body["instance"] != got.path {
			t.Errorf("%s: instance got %v, want %s", got.path, got.body["instance"], got.path)
		}
		if got.body["trace_id"] != requests[got.path] {
			t.Errorf("%s: trace id got %v, want %s", got.path, got.body["trace_id"], requests[got.path])
		}
	}
	if shared.Instance != "" {
		t.Errorf("shared problem's instance: got %q, want none", shared.Instance)
	}
	if shared.TraceID != "" {
		t.Errorf("shared problem's trace id: got %q, want none", shared.TraceID)
	}
}

// TestWriteHeaderCopies: a header value the response changes after Write
// is not the problem's own, which another request may send.
func TestWriteHeaderCopies(t *testing.T) {
	p := Unauthorized("token expired")
	want := p.Header.Get("Www-Authenticate")
	rec := httptest.NewRecorder()
	Write(rec, nil, p)
	rec.Header()["Www-Authenticate"][0] = `Basic realm="x"`
	if got := p.Header.Get("Www-Authenticate"); got != want {
		t.Errorf("the problem's Www-Authenticate: got %q, want %q", got, want)
	}
}

// TestWithCauseCopies: WithCause leaves the problem it is called on as it
// was, so a cause recorded for one request is not another's.
func TestWithCauseCopies(t *testing.T) {
	p := NotFound("x")
	first, second := errors.New("first"), errors.New("second")
	for want, got := range map[error]*Problem{first: p.WithCause(first), second: p.WithCause(second)} {
		if !errors.Is(Cause(got), want) {
			t.Errorf("cause: got %v, want %v", Cause(got), want)
		}
	}
	if Cause(p) != nil {
		t.Errorf("the original's cause: got %v, want none", Cause(p))
	}
}
