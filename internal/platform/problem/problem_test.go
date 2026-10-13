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
		if c.p.Status != c.status || c.p.Code != c.code || !strings.HasPrefix(c.p.Type, TypeBase) {
			t.Errorf("%+v", c.p)
		}
	}
	if p := New(409, "payables.duplicate_invoice", "t", "d"); p.Type != TypeBase+"payables/duplicate-invoice" {
		t.Fatal(p.Type)
	}
	if p := PreconditionRequired("If-Match"); !strings.Contains(p.Detail, "If-Match") {
		t.Fatal(p.Detail)
	}
}

func TestErrorAndCause(t *testing.T) {
	cause := errors.New("disk on fire")
	p := Internal(cause)
	if !errors.Is(p, cause) || Cause(p) != cause || !strings.Contains(p.Error(), "disk on fire") { //nolint:errorlint // Cause must return the cause itself, not an error that wraps it
		t.Fatal(p.Error())
	}
	if q := NotFound("missing"); strings.Contains(q.Error(), "<nil>") || Cause(q) != nil {
		t.Fatal(q.Error())
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
		&pgconn.ConnectError{}:                           {503, "platform.unavailable"},
		&net.OpError{Op: "write", Err: errors.New("broken pipe")}: {503, "platform.unavailable"},
		fmt.Errorf("read: %w", io.ErrUnexpectedEOF):               {503, "platform.unavailable"},
		syscall.ECONNRESET:           {503, "platform.unavailable"},
		context.DeadlineExceeded:     {503, "platform.unavailable"},
		errors.New("plain"):          {500, "platform.internal"},
		Conflict("own.code", "kept"): {409, "own.code"},
	}
	for err, want := range cases {
		p := From(err)
		if p.Status != want.status || p.Code != want.code {
			t.Errorf("%v → %d %s, want %d %s", err, p.Status, p.Code, want.status, want.code)
		}
		if strings.Contains(p.Detail, "secret internals") {
			t.Errorf("database message leaked: %s", p.Detail)
		}
	}
	if p := From(pgErr("55P03", "")); p.RetryAfter != 1 {
		t.Fatal("lock timeouts must ask the client to retry")
	}
	if p := From(pgErr("57P01", "")); p.RetryAfter != 1 || !strings.Contains(p.Detail, "connection was lost") {
		t.Fatal("a lost connection must ask the client to retry")
	}
}

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/operations/x", nil)
	r = r.WithContext(reqctx.WithTraceID(r.Context(), "trace-9"))
	p := Unauthorized("token expired")
	p.RetryAfter = 3
	Write(rec, r, p)
	if rec.Code != 401 || rec.Header().Get("Content-Type") != "application/problem+json" ||
		rec.Header().Get("Retry-After") != "3" || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Fatal(rec.Code, rec.Header())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if got["instance"] != "/v1/operations/x" || got["trace_id"] != "trace-9" || got["code"] != "authn.invalid_token" || got["status"] != float64(401) {
		t.Fatal(got)
	}
	// Without a request the instance is left as set.
	rec = httptest.NewRecorder()
	q := NotFound("x")
	q.Instance = "/given"
	Write(rec, nil, q)
	if !strings.Contains(rec.Body.String(), "/given") || rec.Header().Get("Retry-After") != "" {
		t.Fatal(rec.Body.String())
	}
}
